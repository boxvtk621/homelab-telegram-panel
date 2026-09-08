package mobilegateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontract"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
)

const (
	publicAPIPrefix               = "/api/v1"
	maximumPublicReadResponseSize = 1024 * 1024
)

// ReadController is the exact read-only Controller surface available to the
// public Gateway. Implementations must use the private Controller sockets.
type ReadController interface {
	ListDialogs(context.Context, string, int, string) (mobilecontrollerclient.DialogList, error)
	GetDialog(context.Context, string, string) (mobilecontrollerclient.DialogSnapshot, error)
	ListEvents(context.Context, string, int, string) (mobilecontrollerclient.EventList, error)
	ListMessages(context.Context, string, string, int, int64) (mobilecontrollerclient.MessageList, error)
	ListTasks(context.Context, string, int) (mobilecontrollerclient.TaskList, error)
	GetTask(context.Context, string, string) (mobilecontrollerclient.TaskSnapshot, error)
	Control(context.Context, string) (mobilecontrollerclient.ControlSummary, error)
	Health(context.Context, string) (mobilecontrollerclient.Status, error)
	Ready(context.Context, string) (mobilecontrollerclient.Status, error)
}

// ReadHandler is a non-authoritative public projection. It owns no business
// state and forwards only authenticated, bounded GETs to the Controller client.
type ReadHandler struct {
	auth       *AuthHandler
	controller ReadController
	ids        IDGenerator
	clock      mobileauth.Clock
	capacity   *CapacityGate
}

// NewReadHandler binds reads to the exact AuthHandler dependencies so auth and
// read routes cannot accidentally use separate capacity, clock, or ID domains.
func NewReadHandler(auth *AuthHandler, controller ReadController) (*ReadHandler, error) {
	if auth == nil || isNilDependency(controller) || isNilDependency(auth.ids) ||
		isNilDependency(auth.clock) || auth.capacity == nil {
		return nil, errors.New("mobile read handler dependency is not configured")
	}
	return &ReadHandler{
		auth: auth, controller: controller, ids: auth.ids,
		clock: auth.clock, capacity: auth.capacity,
	}, nil
}

// Register installs only the frozen read-only R1 routes.
func (handler *ReadHandler) Register(mux *http.ServeMux) error {
	return handler.registerWith(mux, handler)
}

func (handler *ReadHandler) registerWith(mux *http.ServeMux, next http.Handler) error {
	if handler == nil || mux == nil {
		return errors.New("mobile read handler or mux is not configured")
	}
	for _, pattern := range []string{
		publicAPIPrefix + "/dialogs", publicAPIPrefix + "/dialogs/",
		publicAPIPrefix + "/events",
		publicAPIPrefix + "/tasks", publicAPIPrefix + "/tasks/",
		publicAPIPrefix + "/control", publicAPIPrefix + "/healthz", publicAPIPrefix + "/readyz",
	} {
		mux.Handle(pattern, next)
	}
	return nil
}

type publicReadRoute uint8

const (
	readRouteDialogs publicReadRoute = iota + 1
	readRouteDialog
	readRouteMessages
	readRouteEvents
	readRouteTasks
	readRouteTask
	readRouteControl
	readRouteHealth
	readRouteReady
)

type matchedReadRoute struct {
	route  publicReadRoute
	target string
}

func (handler *ReadHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(response)
	matched, routeOK := matchPublicReadRoute(request)
	if !routeOK {
		handler.writeReadError(response, http.StatusNotFound, "request-rejected", "not_found")
		return
	}
	requestID, err := handler.ids.New("request")
	if err != nil || !validRequestID(requestID) {
		handler.writeReadError(response, http.StatusServiceUnavailable, "request-unavailable", "request_id_unavailable")
		return
	}
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		handler.writeReadError(response, http.StatusMethodNotAllowed, requestID, "method_not_allowed")
		return
	}
	if request.Host != handler.auth.publicHost || request.ContentLength != 0 || len(request.TransferEncoding) != 0 {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	if err := validatePublicReadRequest(matched, request); err != nil {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	if matched.route != readRouteHealth {
		if _, err := handler.auth.Authenticate(request); err != nil {
			clearCookie(response, SessionCookieName)
			handler.writeReadError(response, http.StatusUnauthorized, requestID, "authentication_failed")
			return
		}
	}
	class := readCapacityClass(matched.route)
	release, admitted := handler.capacity.Acquire(class)
	if !admitted {
		response.Header().Set("Retry-After", "1")
		handler.writeReadError(response, http.StatusTooManyRequests, requestID, "rate_limited")
		return
	}
	defer release()

	switch matched.route {
	case readRouteDialogs:
		handler.listDialogs(response, request, requestID)
	case readRouteDialog:
		handler.getDialog(response, request, requestID, matched.target)
	case readRouteMessages:
		handler.listMessages(response, request, requestID, matched.target)
	case readRouteEvents:
		handler.listEvents(response, request, requestID)
	case readRouteTasks:
		handler.listTasks(response, request, requestID)
	case readRouteTask:
		handler.getTask(response, request, requestID, matched.target)
	case readRouteControl:
		handler.control(response, request, requestID)
	case readRouteHealth:
		handler.health(response, request, requestID)
	case readRouteReady:
		handler.ready(response, request, requestID)
	default:
		handler.writeReadError(response, http.StatusNotFound, requestID, "not_found")
	}
}

func validatePublicReadRequest(matched matchedReadRoute, request *http.Request) error {
	if request.URL.ForceQuery {
		return errors.New("empty query marker is invalid")
	}
	switch matched.route {
	case readRouteDialogs:
		_, _, err := parseDialogsQuery(request.URL.RawQuery)
		return err
	case readRouteDialog:
		if request.URL.RawQuery != "" || mobilecontract.ValidateDialogID(matched.target) != nil {
			return errors.New("Dialog request is invalid")
		}
	case readRouteMessages:
		if mobilecontract.ValidateDialogID(matched.target) != nil {
			return errors.New("message request is invalid")
		}
		_, _, err := parseMessagesQuery(request.URL.RawQuery)
		return err
	case readRouteEvents:
		_, _, err := parseEventsQuery(request.URL.RawQuery)
		return err
	case readRouteTasks:
		_, err := parseTasksQuery(request.URL.RawQuery)
		return err
	case readRouteTask:
		if request.URL.RawQuery != "" || mobilecontract.ValidateTaskReadID(matched.target) != nil {
			return errors.New("Task request is invalid")
		}
	case readRouteControl, readRouteHealth, readRouteReady:
		if request.URL.RawQuery != "" {
			return errors.New("probe request is invalid")
		}
	default:
		return errors.New("read route is invalid")
	}
	return nil
}

func matchPublicReadRoute(request *http.Request) (matchedReadRoute, bool) {
	if request == nil || request.URL == nil || request.URL.RawPath != "" ||
		path.Clean(request.URL.Path) != request.URL.Path || strings.Contains(request.URL.Path, "//") {
		return matchedReadRoute{}, false
	}
	switch request.URL.Path {
	case publicAPIPrefix + "/dialogs":
		return matchedReadRoute{route: readRouteDialogs}, true
	case publicAPIPrefix + "/tasks":
		return matchedReadRoute{route: readRouteTasks}, true
	case publicAPIPrefix + "/events":
		return matchedReadRoute{route: readRouteEvents}, true
	case publicAPIPrefix + "/control":
		return matchedReadRoute{route: readRouteControl}, true
	case publicAPIPrefix + "/healthz":
		return matchedReadRoute{route: readRouteHealth}, true
	case publicAPIPrefix + "/readyz":
		return matchedReadRoute{route: readRouteReady}, true
	}
	if tail, found := strings.CutPrefix(request.URL.Path, publicAPIPrefix+"/dialogs/"); found {
		if dialogID, ok := strings.CutSuffix(tail, "/messages"); ok && dialogID != "" && !strings.Contains(dialogID, "/") {
			return matchedReadRoute{route: readRouteMessages, target: dialogID}, true
		}
		if tail != "" && !strings.Contains(tail, "/") {
			return matchedReadRoute{route: readRouteDialog, target: tail}, true
		}
		return matchedReadRoute{}, false
	}
	if taskID, found := strings.CutPrefix(request.URL.Path, publicAPIPrefix+"/tasks/"); found && taskID != "" && !strings.Contains(taskID, "/") {
		return matchedReadRoute{route: readRouteTask, target: taskID}, true
	}
	return matchedReadRoute{}, false
}

func readCapacityClass(route publicReadRoute) CapacityClass {
	switch route {
	case readRouteHealth, readRouteReady:
		return CapacityHealth
	case readRouteControl, readRouteTask:
		return CapacityStatus
	default:
		// Ordinary lists and details are general traffic. Recovery capacity is
		// reserved for explicit outcome readback, full-snapshot, and resync
		// routes; an ordinary polling request must never borrow it.
		return CapacityGeneral
	}
}

func (handler *ReadHandler) getDialog(response http.ResponseWriter, request *http.Request, requestID, dialogID string) {
	if request.URL.RawQuery != "" || mobilecontract.ValidateDialogID(dialogID) != nil {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	snapshot, err := handler.controller.GetDialog(request.Context(), requestID, dialogID)
	if err != nil {
		handler.writeControllerReadError(response, requestID, err, true)
		return
	}
	if err := validateDialogSnapshot(snapshot, dialogID); err != nil {
		handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
		return
	}
	handler.writeReadData(response, http.StatusOK, requestID, snapshot)
}

func (handler *ReadHandler) listEvents(response http.ResponseWriter, request *http.Request, requestID string) {
	limit, after, err := parseEventsQuery(request.URL.RawQuery)
	if err != nil {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	page, err := handler.controller.ListEvents(request.Context(), requestID, limit, after)
	if err != nil {
		handler.writeControllerReadError(response, requestID, err, false)
		return
	}
	if err := validateEventList(page, limit); err != nil {
		handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
		return
	}
	handler.writeReadData(response, http.StatusOK, requestID, page)
}

func (handler *ReadHandler) listDialogs(response http.ResponseWriter, request *http.Request, requestID string) {
	limit, cursor, err := parseDialogsQuery(request.URL.RawQuery)
	if err != nil {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	page, err := handler.controller.ListDialogs(request.Context(), requestID, limit, cursor)
	if err != nil {
		handler.writeControllerReadError(response, requestID, err, false)
		return
	}
	if err := validateDialogList(page, limit); err != nil {
		handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
		return
	}
	handler.writeReadData(response, http.StatusOK, requestID, page)
}

func (handler *ReadHandler) listMessages(response http.ResponseWriter, request *http.Request, requestID, dialogID string) {
	limit, afterSequence, err := parseMessagesQuery(request.URL.RawQuery)
	if err != nil || mobilecontract.ValidateDialogID(dialogID) != nil {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	page, err := handler.controller.ListMessages(request.Context(), requestID, dialogID, limit, afterSequence)
	if err != nil {
		handler.writeControllerReadError(response, requestID, err, true)
		return
	}
	if err := validateMessageList(page, dialogID, limit, afterSequence); err != nil {
		handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
		return
	}
	handler.writeReadData(response, http.StatusOK, requestID, page)
}

func (handler *ReadHandler) listTasks(response http.ResponseWriter, request *http.Request, requestID string) {
	limit, err := parseTasksQuery(request.URL.RawQuery)
	if err != nil {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	page, err := handler.controller.ListTasks(request.Context(), requestID, limit)
	if err != nil {
		handler.writeControllerReadError(response, requestID, err, false)
		return
	}
	if err := validateTaskList(page, limit); err != nil {
		handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
		return
	}
	handler.writeReadData(response, http.StatusOK, requestID, page)
}

func (handler *ReadHandler) getTask(response http.ResponseWriter, request *http.Request, requestID, taskID string) {
	if request.URL.RawQuery != "" || mobilecontract.ValidateTaskReadID(taskID) != nil {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	snapshot, err := handler.controller.GetTask(request.Context(), requestID, taskID)
	if err != nil {
		handler.writeControllerReadError(response, requestID, err, true)
		return
	}
	if err := validateTaskSnapshot(snapshot, taskID); err != nil {
		handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
		return
	}
	handler.writeReadData(response, http.StatusOK, requestID, snapshot)
}

func (handler *ReadHandler) control(response http.ResponseWriter, request *http.Request, requestID string) {
	if request.URL.RawQuery != "" {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	summary, err := handler.controller.Control(request.Context(), requestID)
	if err != nil {
		handler.writeControllerReadError(response, requestID, err, false)
		return
	}
	if err := validateControl(summary); err != nil {
		handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
		return
	}
	// Controller capacity describes its private UDS runtime and cannot prove the
	// public Gateway's 24+8 admission contract. Replace it only after the typed
	// Controller projection has been validated. The status slot held by this
	// request remains acquired until after the response, so the snapshot counts
	// the current control request honestly.
	capacity := handler.capacity.Snapshot()
	summary.Capacity = []mobilecontrollerclient.ControlCapacity{
		{Class: "general", InFlight: capacity.GeneralInFlight, Limit: capacity.GeneralLimit},
		{Class: "reserve", InFlight: capacity.ReservedInFlight, Limit: capacity.ReservedLimit},
	}
	handler.writeReadData(response, http.StatusOK, requestID, summary)
}

func (handler *ReadHandler) health(response http.ResponseWriter, request *http.Request, requestID string) {
	handler.probe(response, request, requestID, "ok", handler.controller.Health)
}

func (handler *ReadHandler) ready(response http.ResponseWriter, request *http.Request, requestID string) {
	handler.probe(response, request, requestID, "ready", handler.controller.Ready)
}

func (handler *ReadHandler) probe(
	response http.ResponseWriter,
	request *http.Request,
	requestID string,
	expectedControllerStatus string,
	probe func(context.Context, string) (mobilecontrollerclient.Status, error),
) {
	if request.URL.RawQuery != "" {
		handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
		return
	}
	status, err := probe(request.Context(), requestID)
	if err != nil {
		handler.writeControllerReadError(response, requestID, err, false)
		return
	}
	if status.Status != expectedControllerStatus {
		handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
		return
	}
	// The public contract deliberately normalizes both Controller probes to one
	// non-authoritative liveness vocabulary consumed by the Mini App.
	handler.writeReadData(response, http.StatusOK, requestID, mobilecontrollerclient.Status{Status: "ok"})
}

func parseDialogsQuery(raw string) (int, string, error) {
	values, err := parseCanonicalReadQuery(raw, "limit", "cursor")
	if err != nil {
		return 0, "", err
	}
	limit, err := parsePublicLimit(values.Get("limit"))
	if err != nil {
		return 0, "", err
	}
	cursor := values.Get("cursor")
	if cursor != "" && !validPublicCursor(cursor) {
		return 0, "", errors.New("cursor is invalid")
	}
	return limit, cursor, nil
}

func parseMessagesQuery(raw string) (int, int64, error) {
	values, err := parseCanonicalReadQuery(raw, "limit", "after_sequence")
	if err != nil {
		return 0, 0, err
	}
	limit, err := parsePublicLimit(values.Get("limit"))
	if err != nil {
		return 0, 0, err
	}
	after := int64(0)
	if value := values.Get("after_sequence"); value != "" {
		if !canonicalPublicDecimal(value, false) {
			return 0, 0, errors.New("after sequence is invalid")
		}
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil || after > mobilecontract.MaximumSafeJSONInteger {
			return 0, 0, errors.New("after sequence is invalid")
		}
	}
	return limit, after, nil
}

func parseTasksQuery(raw string) (int, error) {
	values, err := parseCanonicalReadQuery(raw, "limit")
	if err != nil {
		return 0, err
	}
	return parsePublicLimit(values.Get("limit"))
}

func parseEventsQuery(raw string) (int, string, error) {
	values, err := parseCanonicalReadQuery(raw, "limit", "after")
	if err != nil {
		return 0, "", err
	}
	limit, err := parsePublicLimit(values.Get("limit"))
	if err != nil {
		return 0, "", err
	}
	after := values.Get("after")
	if after != "" && !validPublicCursor(after) {
		return 0, "", errors.New("event cursor is invalid")
	}
	return limit, after, nil
}

func parseCanonicalReadQuery(raw string, allowed ...string) (url.Values, error) {
	if strings.ContainsAny(raw, "%+;") {
		return nil, errors.New("query encoding is not canonical")
	}
	if raw != "" {
		for _, field := range strings.Split(raw, "&") {
			if field == "" || strings.Count(field, "=") != 1 {
				return nil, errors.New("query framing is not canonical")
			}
		}
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	accepted := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		accepted[key] = struct{}{}
	}
	for key, entries := range values {
		if _, ok := accepted[key]; !ok || len(entries) != 1 || entries[0] == "" {
			return nil, errors.New("query is invalid")
		}
	}
	return values, nil
}

func parsePublicLimit(value string) (int, error) {
	if value == "" {
		return mobilecontract.DefaultPageSize, nil
	}
	if !canonicalPublicDecimal(value, true) {
		return 0, errors.New("limit is invalid")
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit > mobilecontract.MaximumPageSize {
		return 0, errors.New("limit is invalid")
	}
	return limit, nil
}

func canonicalPublicDecimal(value string, positive bool) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') || (positive && value == "0") {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validPublicCursor(value string) bool {
	if value == "" || len(value) > 1024 || strings.Count(value, ".") != 1 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func (handler *ReadHandler) writeControllerReadError(
	response http.ResponseWriter,
	requestID string,
	err error,
	objectScoped bool,
) {
	var remote *mobilecontrollerclient.RemoteError
	if errors.As(err, &remote) {
		switch remote.Status {
		case http.StatusBadRequest:
			handler.writeReadError(response, http.StatusBadRequest, requestID, "invalid_request")
			return
		case http.StatusForbidden:
			if objectScoped {
				handler.writeReadError(response, http.StatusNotFound, requestID, "not_found")
			} else {
				handler.writeReadError(response, http.StatusForbidden, requestID, "forbidden")
			}
			return
		case http.StatusNotFound:
			if objectScoped {
				handler.writeReadError(response, http.StatusNotFound, requestID, "not_found")
				return
			}
		case http.StatusConflict:
			if remote.Code == "resync_required" {
				handler.writeReadError(response, http.StatusConflict, requestID, "resync_required")
				return
			}
		case http.StatusGone:
			if remote.Code == "resync_required" {
				handler.writeReadError(response, http.StatusGone, requestID, "resync_required")
				return
			}
		case http.StatusTooManyRequests:
			response.Header().Set("Retry-After", "1")
			handler.writeReadError(response, http.StatusTooManyRequests, requestID, "rate_limited")
			return
		}
	}
	handler.writeReadError(response, http.StatusServiceUnavailable, requestID, "unavailable")
}

type publicReadEnvelope struct {
	SchemaVersion int            `json:"schema_version"`
	RequestID     string         `json:"request_id"`
	ServerTime    string         `json:"server_time"`
	Data          any            `json:"data,omitempty"`
	Error         *errorEnvelope `json:"error,omitempty"`
}

func (handler *ReadHandler) writeReadData(response http.ResponseWriter, status int, requestID string, data any) {
	handler.writeReadEnvelope(response, status, publicReadEnvelope{
		SchemaVersion: publicSchemaVersion, RequestID: requestID,
		ServerTime: handler.clock.Now().UTC().Format("2006-01-02T15:04:05.000000Z"), Data: data,
	})
}

func (handler *ReadHandler) writeReadError(response http.ResponseWriter, status int, requestID, code string) {
	handler.writeReadEnvelope(response, status, publicReadEnvelope{
		SchemaVersion: publicSchemaVersion, RequestID: requestID,
		ServerTime: handler.clock.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
		Error:      &errorEnvelope{Code: code, Message: "request could not be completed"},
	})
}

func (handler *ReadHandler) writeReadEnvelope(response http.ResponseWriter, status int, value publicReadEnvelope) {
	payload, err := json.Marshal(value)
	if err != nil || len(payload)+1 > maximumPublicReadResponseSize {
		status = http.StatusServiceUnavailable
		value.Data = nil
		value.Error = &errorEnvelope{Code: "unavailable", Message: "request could not be completed"}
		payload, _ = json.Marshal(value)
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_, _ = response.Write(append(payload, '\n'))
}

var _ http.Handler = (*ReadHandler)(nil)
var _ ReadController = (*mobilecontrollerclient.Client)(nil)
