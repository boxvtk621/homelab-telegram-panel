package mobilegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontract"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
)

type SubmissionController interface {
	SubmitTask(context.Context, string, string, mobilecontrollerclient.SubmissionRequest) (mobilecontrollerclient.SubmissionReceipt, error)
}

type SubmissionHandler struct {
	dialogs    *DialogCommandsHandler
	controller SubmissionController
}

func NewSubmissionHandler(dialogs *DialogCommandsHandler, controller SubmissionController) (*SubmissionHandler, error) {
	if dialogs == nil || isNilDependency(controller) {
		return nil, errors.New("submission dependencies unavailable")
	}
	return &SubmissionHandler{dialogs: dialogs, controller: controller}, nil
}

func (handler *SubmissionHandler) Register(mux *http.ServeMux) error {
	return handler.dialogs.reads.registerWith(mux, handler)
}

func (handler *SubmissionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, publicAPIPrefix+"/dialogs/") || !strings.HasSuffix(r.URL.Path, "/submissions") {
		handler.dialogs.ServeHTTP(w, r)
		return
	}
	reads := handler.dialogs.reads
	setSecurityHeaders(w)
	requestID, err := reads.ids.New("request")
	if err != nil || !validRequestID(requestID) {
		reads.writeReadError(w, http.StatusServiceUnavailable, "request-unavailable", "request_id_unavailable")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		reads.writeReadError(w, http.StatusMethodNotAllowed, requestID, "method_not_allowed")
		return
	}
	dialogID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, publicAPIPrefix+"/dialogs/"), "/submissions")
	if !exactAuthRequest(r, true) || r.URL.EscapedPath() != r.URL.Path || mobilecontract.ValidateDialogID(dialogID) != nil || r.Header.Get("Content-Encoding") != "" {
		reads.writeReadError(w, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	session, err := reads.auth.AuthorizeMutation(r)
	if err != nil {
		reads.writeReadError(w, http.StatusUnauthorized, requestID, "authentication_failed")
		return
	}
	types := r.Header.Values("Content-Type")
	if len(types) != 1 || (types[0] != "application/json" && types[0] != "application/json; charset=utf-8") {
		reads.writeReadError(w, http.StatusUnsupportedMediaType, requestID, "unsupported_media_type")
		return
	}
	release, admitted := reads.capacity.Acquire(CapacityGeneral)
	if !admitted {
		w.Header().Set("Retry-After", "1")
		reads.writeReadError(w, http.StatusTooManyRequests, requestID, "rate_limited")
		return
	}
	defer release()
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		reads.writeReadError(w, http.StatusRequestEntityTooLarge, requestID, "invalid_request")
		return
	}
	input, err := decodePublicSubmission(payload)
	if err != nil {
		reads.writeReadError(w, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	input.ClientInstanceID = session.ClientInstanceID
	result, err := handler.controller.SubmitTask(r.Context(), requestID, dialogID, input)
	if err != nil {
		var remote *mobilecontrollerclient.RemoteError
		if errors.As(err, &remote) && remote.Status == http.StatusConflict {
			reads.writeReadError(w, http.StatusConflict, requestID, "command_conflict")
			return
		}
		reads.writeControllerReadError(w, requestID, err, false)
		return
	}
	if !mobilecontrollerclient.ValidSubmissionReceipt(result, dialogID, input.CommandID, input.ExpectedObjectVersion) {
		reads.writeReadError(w, http.StatusServiceUnavailable, requestID, "outcome_unknown")
		return
	}
	status := http.StatusAccepted
	if result.Replayed {
		status = http.StatusOK
	}
	reads.writeReadData(w, status, requestID, result)
}

var submissionIssuePattern = regexp.MustCompile(`^(HL-[1-9][0-9]*)?$`)

func decodePublicSubmission(payload []byte) (mobilecontrollerclient.SubmissionRequest, error) {
	var input mobilecontrollerclient.SubmissionRequest
	invalid := errors.New("invalid submission")
	if !utf8.Valid(payload) {
		return input, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return input, invalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return input, invalid
		}
		seen[key] = true
		switch key {
		case "command_id":
			err = decoder.Decode(&input.CommandID)
		case "expected_object_version":
			err = decoder.Decode(&input.ExpectedObjectVersion)
		case "requested_issue_id":
			var issue *string
			err = decoder.Decode(&issue)
			if issue == nil {
				return input, invalid
			}
			input.RequestedIssueID = *issue
		case "text":
			err = decoder.Decode(&input.Text)
		default:
			return input, invalid
		}
		if err != nil {
			return input, invalid
		}
	}
	if _, err := decoder.Token(); err != nil {
		return input, invalid
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || len(seen) != 4 || mobilecontract.ValidateCanonicalUUID("command", input.CommandID) != nil ||
		input.ExpectedObjectVersion < 1 || input.ExpectedObjectVersion >= 1<<53-1 || len(input.Text) > 2048 || strings.TrimSpace(input.Text) == "" || len(input.RequestedIssueID) > 64 || !submissionIssuePattern.MatchString(input.RequestedIssueID) || strings.ContainsRune(input.Text, 0) {
		return input, invalid
	}
	return input, nil
}
