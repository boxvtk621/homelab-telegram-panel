package mobilegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontract"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
)

type DialogCommandController interface {
	CreateDialog(context.Context, string, mobilecontrollerclient.CreateDialogRequest) (mobilecontrollerclient.CreateDialogResult, error)
}

// DialogCommandsHandler adds only dialog.create to the read surface. It owns
// neither a command ledger nor a database; Controller commit is the only ACK.
type DialogCommandsHandler struct {
	reads      *ReadHandler
	controller DialogCommandController
}

func NewDialogCommandsHandler(reads *ReadHandler, controller DialogCommandController) (*DialogCommandsHandler, error) {
	if reads == nil || isNilDependency(controller) {
		return nil, errors.New("dialog command dependency is unavailable")
	}
	return &DialogCommandsHandler{reads: reads, controller: controller}, nil
}

func (handler *DialogCommandsHandler) Register(mux *http.ServeMux) error {
	return handler.reads.registerWith(mux, handler)
}

func (handler *DialogCommandsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != publicAPIPrefix+"/dialogs" {
		handler.reads.ServeHTTP(w, r)
		return
	}
	setSecurityHeaders(w)
	requestID, err := handler.reads.ids.New("request")
	if err != nil || !validRequestID(requestID) {
		handler.reads.writeReadError(w, http.StatusServiceUnavailable, "request-unavailable", "request_id_unavailable")
		return
	}
	if !exactAuthRequest(r, true) || r.Header.Get("Content-Encoding") != "" {
		handler.reads.writeReadError(w, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	session, err := handler.reads.auth.AuthorizeMutation(r)
	if err != nil {
		handler.reads.writeReadError(w, http.StatusUnauthorized, requestID, "authentication_failed")
		return
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 || (contentTypes[0] != "application/json" && contentTypes[0] != "application/json; charset=utf-8") {
		handler.reads.writeReadError(w, http.StatusUnsupportedMediaType, requestID, "unsupported_media_type")
		return
	}
	release, admitted := handler.reads.capacity.Acquire(CapacityGeneral)
	if !admitted {
		w.Header().Set("Retry-After", "1")
		handler.reads.writeReadError(w, http.StatusTooManyRequests, requestID, "rate_limited")
		return
	}
	defer release()
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		handler.reads.writeReadError(w, status, requestID, "invalid_request")
		return
	}
	input, err := decodePublicCreateDialog(payload)
	if err != nil {
		handler.reads.writeReadError(w, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	// The browser cannot choose the actor, channel, capabilities or client
	// namespace. The session supplies the last; the UDS peer supplies the rest.
	input.ClientInstanceID = session.ClientInstanceID
	result, err := handler.controller.CreateDialog(r.Context(), requestID, input)
	if err != nil {
		var remote *mobilecontrollerclient.RemoteError
		if errors.As(err, &remote) && remote.Status == http.StatusConflict {
			handler.reads.writeReadError(w, http.StatusConflict, requestID, "command_conflict")
			return
		}
		handler.reads.writeControllerReadError(w, requestID, err, false)
		return
	}
	snapshot := mobilecontrollerclient.DialogSnapshot{Dialog: result.Dialog, SnapshotVersion: result.SnapshotVersion, FreshnessAt: result.FreshnessAt}
	if result.CommandID != input.CommandID || result.SnapshotVersion != input.ExpectedCollectionVersion+1 ||
		result.Dialog.Lifecycle != "active" || result.Dialog.ObjectVersion != 1 || result.Dialog.ActiveTaskID != "" || result.Dialog.PendingAdmissionID != "" || result.Dialog.AdmissionNextCheckAt != "" ||
		validateDialogSnapshot(snapshot, result.Dialog.ID) != nil {
		handler.reads.writeReadError(w, http.StatusServiceUnavailable, requestID, "outcome_unknown")
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	handler.reads.writeReadData(w, status, requestID, result)
}

func decodePublicCreateDialog(payload []byte) (mobilecontrollerclient.CreateDialogRequest, error) {
	var input mobilecontrollerclient.CreateDialogRequest
	invalid := errors.New("invalid dialog command")
	decoder := json.NewDecoder(bytes.NewReader(payload))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return input, invalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || seen[key] {
			return input, invalid
		}
		seen[key] = true
		switch key {
		case "command_id":
			if decoder.Decode(&input.CommandID) != nil {
				return input, invalid
			}
		case "expected_collection_version":
			var version *int64
			if decoder.Decode(&version) != nil || version == nil {
				return input, invalid
			}
			input.ExpectedCollectionVersion = *version
		default:
			return input, invalid
		}
	}
	if _, err := decoder.Token(); err != nil {
		return input, invalid
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || len(seen) != 2 || mobilecontract.ValidateCanonicalUUID("command ID", input.CommandID) != nil ||
		!safeJSONInteger(input.ExpectedCollectionVersion, false) || input.ExpectedCollectionVersion == 1<<53-1 {
		return input, invalid
	}
	return input, nil
}
