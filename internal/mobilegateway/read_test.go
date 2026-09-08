package mobilegateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
)

const (
	readTestDialogID  = "018f0c9e-8f4b-4a6b-8c9d-000000000001"
	readTestMessageID = "018f0c9e-8f4b-4a6b-8c9d-000000000002"
	readTestCursor    = "eyJ2IjoxfQ.c2ln"
	readTestTime      = "2026-08-29T10:00:00Z"
)

type readControllerCall struct {
	method        string
	requestID     string
	target        string
	limit         int
	cursor        string
	afterSequence int64
}

type fakeReadController struct {
	mu sync.Mutex

	calls  []readControllerCall
	errors map[string]error

	dialogs  mobilecontrollerclient.DialogList
	dialog   mobilecontrollerclient.DialogSnapshot
	events   mobilecontrollerclient.EventList
	messages mobilecontrollerclient.MessageList
	tasks    mobilecontrollerclient.TaskList
	task     mobilecontrollerclient.TaskSnapshot
	control  mobilecontrollerclient.ControlSummary
	health   mobilecontrollerclient.Status
	ready    mobilecontrollerclient.Status
}

func validFakeReadController() *fakeReadController {
	dialog := mobilecontrollerclient.Dialog{
		ID: readTestDialogID, Lifecycle: "active", ObjectVersion: 1,
		CreatedAt: readTestTime, UpdatedAt: readTestTime,
	}
	task := mobilecontrollerclient.Task{
		ID: "task-1", DialogID: readTestDialogID, IssueID: "HL-210", ExpectedRevision: 17,
		State: "running", CancelState: "none", CreatedAt: readTestTime, UpdatedAt: readTestTime,
	}
	return &fakeReadController{
		errors: make(map[string]error),
		dialogs: mobilecontrollerclient.DialogList{
			Dialogs: []mobilecontrollerclient.Dialog{dialog}, SnapshotVersion: 1,
			FreshnessAt: readTestTime, EventCheckpoint: readTestCursor,
		},
		dialog: mobilecontrollerclient.DialogSnapshot{
			Dialog: dialog, SnapshotVersion: 1, FreshnessAt: readTestTime,
		},
		events: mobilecontrollerclient.EventList{
			Events: []mobilecontrollerclient.ManagementEvent{{
				EventID: 1, OwnerVersion: 1, DialogID: readTestDialogID, ObjectVersion: 1,
				Kind: "dialog.created", Lifecycle: "active", OccurredAt: readTestTime,
			}}, SnapshotVersion: 1, FreshnessAt: readTestTime, NextCursor: readTestCursor,
		},
		messages: mobilecontrollerclient.MessageList{
			Messages: []mobilecontrollerclient.Message{{
				ID: readTestMessageID, DialogID: readTestDialogID, Sequence: 1,
				Actor: "owner", Kind: "input", Content: "hello", CreatedAt: readTestTime,
			}}, FreshnessAt: readTestTime,
		},
		tasks: mobilecontrollerclient.TaskList{Tasks: []mobilecontrollerclient.Task{task}, FreshnessAt: readTestTime},
		task:  mobilecontrollerclient.TaskSnapshot{Task: task, FreshnessAt: readTestTime},
		control: mobilecontrollerclient.ControlSummary{
			Components:  []mobilecontrollerclient.ControlComponent{{Name: "controller", State: "ready"}},
			Capacity:    []mobilecontrollerclient.ControlCapacity{{Class: "general", InFlight: 1, Limit: 24}},
			FreshnessAt: readTestTime,
		},
		health: mobilecontrollerclient.Status{Status: "ok"},
		ready:  mobilecontrollerclient.Status{Status: "ready"},
	}
}

func (controller *fakeReadController) record(call readControllerCall) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.calls = append(controller.calls, call)
	return controller.errors[call.method]
}

func (controller *fakeReadController) ListDialogs(_ context.Context, requestID string, limit int, cursor string) (mobilecontrollerclient.DialogList, error) {
	err := controller.record(readControllerCall{method: "dialogs", requestID: requestID, limit: limit, cursor: cursor})
	return controller.dialogs, err
}

func (controller *fakeReadController) GetDialog(_ context.Context, requestID, dialogID string) (mobilecontrollerclient.DialogSnapshot, error) {
	err := controller.record(readControllerCall{method: "dialog", requestID: requestID, target: dialogID})
	return controller.dialog, err
}

func (controller *fakeReadController) ListEvents(_ context.Context, requestID string, limit int, after string) (mobilecontrollerclient.EventList, error) {
	err := controller.record(readControllerCall{method: "events", requestID: requestID, limit: limit, cursor: after})
	return controller.events, err
}

func (controller *fakeReadController) ListMessages(_ context.Context, requestID, dialogID string, limit int, afterSequence int64) (mobilecontrollerclient.MessageList, error) {
	err := controller.record(readControllerCall{
		method: "messages", requestID: requestID, target: dialogID, limit: limit, afterSequence: afterSequence,
	})
	return controller.messages, err
}

func (controller *fakeReadController) ListTasks(_ context.Context, requestID string, limit int) (mobilecontrollerclient.TaskList, error) {
	err := controller.record(readControllerCall{method: "tasks", requestID: requestID, limit: limit})
	return controller.tasks, err
}

func (controller *fakeReadController) GetTask(_ context.Context, requestID, taskID string) (mobilecontrollerclient.TaskSnapshot, error) {
	err := controller.record(readControllerCall{method: "task", requestID: requestID, target: taskID})
	return controller.task, err
}

func (controller *fakeReadController) Control(_ context.Context, requestID string) (mobilecontrollerclient.ControlSummary, error) {
	err := controller.record(readControllerCall{method: "control", requestID: requestID})
	return controller.control, err
}

func (controller *fakeReadController) Health(_ context.Context, requestID string) (mobilecontrollerclient.Status, error) {
	err := controller.record(readControllerCall{method: "health", requestID: requestID})
	return controller.health, err
}

func (controller *fakeReadController) Ready(_ context.Context, requestID string) (mobilecontrollerclient.Status, error) {
	err := controller.record(readControllerCall{method: "ready", requestID: requestID})
	return controller.ready, err
}

func (controller *fakeReadController) callSnapshot() []readControllerCall {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return append([]readControllerCall(nil), controller.calls...)
}

type readTestFixture struct {
	handler *ReadHandler
	mux     *http.ServeMux
	auth    *AuthHandler
	session *http.Cookie
}

func newReadTestFixture(t *testing.T, controller ReadController) readTestFixture {
	t.Helper()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)}
	verifier := &fakeTelegramVerifier{identity: mobileauth.TelegramIdentity{
		UserID: 900000000001, AuthDate: clock.Now().Add(time.Second),
		Fingerprint: sha256.Sum256([]byte("read-handler-session")),
	}}
	auth := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
	handler, err := NewReadHandler(auth, controller)
	if err != nil {
		t.Fatalf("NewReadHandler(): %v", err)
	}
	mux := http.NewServeMux()
	if err := auth.Register(mux); err != nil {
		t.Fatal(err)
	}
	if err := handler.Register(mux); err != nil {
		t.Fatal(err)
	}
	bootstrap := responseCookie(t, serveBootstrap(mux, "read-client"), BootstrapCookieName)
	exchange := serve(mux, http.MethodPost, "/api/v1/auth/telegram", "signed-read-init", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin,
		"Cookie": cookieHeader(bootstrap), clientInstanceHeader: "read-client",
	})
	if exchange.Code != http.StatusOK {
		t.Fatalf("session exchange status=%d body=%s", exchange.Code, exchange.Body.String())
	}
	return readTestFixture{
		handler: handler, mux: mux, auth: auth,
		session: responseCookie(t, exchange, SessionCookieName),
	}
}

func (fixture readTestFixture) request(method, target string, authenticated bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, testPublicOrigin+target, nil)
	request.Host = "mobile.example.test"
	if authenticated {
		request.AddCookie(fixture.session)
	}
	response := httptest.NewRecorder()
	fixture.mux.ServeHTTP(response, request)
	return response
}

func TestReadHandlerServesExactTypedControllerRoutes(t *testing.T) {
	t.Parallel()
	controller := validFakeReadController()
	fixture := newReadTestFixture(t, controller)

	for _, test := range []struct {
		path          string
		authenticated bool
	}{
		{path: "/api/v1/dialogs?limit=1&cursor=" + readTestCursor, authenticated: true},
		{path: "/api/v1/dialogs/" + readTestDialogID, authenticated: true},
		{path: "/api/v1/dialogs/" + readTestDialogID + "/messages?limit=2&after_sequence=0", authenticated: true},
		{path: "/api/v1/events?limit=2", authenticated: true},
		{path: "/api/v1/tasks?limit=2", authenticated: true},
		{path: "/api/v1/tasks/task-1", authenticated: true},
		{path: "/api/v1/control", authenticated: true},
		{path: "/api/v1/healthz", authenticated: false},
		{path: "/api/v1/readyz", authenticated: true},
	} {
		response := fixture.request(http.MethodGet, test.path, test.authenticated)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" ||
			response.Header().Get("Content-Security-Policy") == "" ||
			response.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
			response.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s unsafe response headers: %v", test.path, response.Header())
		}
		var parsed envelope
		if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil || parsed.Error != nil ||
			parsed.SchemaVersion != 1 || parsed.RequestID == "" || parsed.Data == nil {
			t.Fatalf("%s invalid public envelope: parsed=%+v err=%v body=%s", test.path, parsed, err, response.Body.String())
		}
		if (test.path == "/api/v1/healthz" || test.path == "/api/v1/readyz") &&
			!strings.Contains(response.Body.String(), `"status":"ok"`) {
			t.Fatalf("%s public probe was not normalized: %s", test.path, response.Body.String())
		}
	}
	calls := controller.callSnapshot()
	if len(calls) != 9 || calls[0].method != "dialogs" || calls[0].limit != 1 || calls[0].cursor != readTestCursor ||
		calls[2].method != "messages" || calls[2].target != readTestDialogID || calls[2].limit != 2 ||
		calls[3].method != "events" || calls[3].limit != 2 || calls[5].target != "task-1" {
		t.Fatalf("typed Controller dispatch mismatch: %+v", calls)
	}
}

func TestReadHandlerRequiresSessionExceptExactLiveness(t *testing.T) {
	t.Parallel()
	controller := validFakeReadController()
	fixture := newReadTestFixture(t, controller)

	if response := fixture.request(http.MethodGet, "/api/v1/dialogs", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Dialog list status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/readyz", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated readiness status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/healthz", false); response.Code != http.StatusOK {
		t.Fatalf("public liveness status=%d body=%s", response.Code, response.Body.String())
	}
	wrongHost := httptest.NewRequest(http.MethodGet, "https://evil.example/api/v1/healthz", nil)
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, wrongHost)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("wrong-host liveness status=%d body=%s", response.Code, response.Body.String())
	}
	calls := controller.callSnapshot()
	if len(calls) != 1 || calls[0].method != "health" {
		t.Fatalf("authentication/Host failures reached Controller: %+v", calls)
	}
}

func TestReadHandlerRejectsMethodPathBodyAndQueryConfusionBeforeDispatch(t *testing.T) {
	t.Parallel()
	controller := validFakeReadController()
	fixture := newReadTestFixture(t, controller)

	options := fixture.request(http.MethodOptions, "/api/v1/tasks", false)
	if options.Code != http.StatusMethodNotAllowed || options.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("method confusion status=%d allow=%q body=%s", options.Code, options.Header().Get("Allow"), options.Body.String())
	}
	for _, target := range []string{
		"/api/v1/dialogs?limit=01", "/api/v1/dialogs?limit=1&limit=2",
		"/api/v1/dialogs?limit=1&",
		"/api/v1/dialogs?cursor=bad", "/api/v1/tasks?unknown=1",
		"/api/v1/events?after=" + readTestCursor + "&after=" + readTestCursor,
		"/api/v1/dialogs/not-a-uuid/messages", "/api/v1/tasks/task%2F1",
		"/api/v1/dialogs/" + readTestDialogID + "/messages/extra", "/api/v1/tasks/",
	} {
		request := httptest.NewRequest(http.MethodGet, testPublicOrigin+target, nil)
		request.Host = "mobile.example.test"
		request.AddCookie(fixture.session)
		response := httptest.NewRecorder()
		fixture.handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	bodyRequest := httptest.NewRequest(http.MethodGet, testPublicOrigin+"/api/v1/tasks", strings.NewReader("unexpected"))
	bodyRequest.Host = "mobile.example.test"
	bodyRequest.AddCookie(fixture.session)
	bodyResponse := httptest.NewRecorder()
	fixture.handler.ServeHTTP(bodyResponse, bodyRequest)
	if bodyResponse.Code != http.StatusBadRequest {
		t.Fatalf("GET body status=%d body=%s", bodyResponse.Code, bodyResponse.Body.String())
	}
	if calls := controller.callSnapshot(); len(calls) != 0 {
		t.Fatalf("invalid public requests reached Controller: %+v", calls)
	}
}

func TestReadHandlerUsesGeneralAndIsolatedReservedCapacity(t *testing.T) {
	t.Parallel()
	controller := validFakeReadController()
	fixture := newReadTestFixture(t, controller)

	generalReleases := acquireReadCapacity(t, fixture.auth.capacity, CapacityGeneral, maximumGeneralInFlight)
	for _, target := range []string{
		"/api/v1/dialogs", "/api/v1/dialogs/" + readTestDialogID,
		"/api/v1/dialogs/" + readTestDialogID + "/messages", "/api/v1/events",
		"/api/v1/tasks",
	} {
		if response := fixture.request(http.MethodGet, target, true); response.Code != http.StatusTooManyRequests {
			t.Fatalf("saturated general route %s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	if response := fixture.request(http.MethodGet, "/api/v1/healthz", false); response.Code != http.StatusOK {
		t.Fatalf("general load consumed health reserve: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/control", true); response.Code != http.StatusOK {
		t.Fatalf("general load consumed status reserve: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/tasks/task-1", true); response.Code != http.StatusOK {
		t.Fatalf("general load consumed Task-status reserve: status=%d body=%s", response.Code, response.Body.String())
	}
	releaseReadCapacity(generalReleases)

	recoveryReleases := acquireReadCapacity(t, fixture.auth.capacity, CapacityRecovery, maximumReservedInFlight)
	for _, target := range []string{"/api/v1/dialogs", "/api/v1/events", "/api/v1/tasks"} {
		if response := fixture.request(http.MethodGet, target, true); response.Code != http.StatusOK {
			t.Fatalf("recovery reserve leaked into ordinary route %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	if response := fixture.request(http.MethodGet, "/api/v1/readyz", true); response.Code != http.StatusOK {
		t.Fatalf("recovery load consumed health reserve: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/control", true); response.Code != http.StatusOK {
		t.Fatalf("recovery load consumed status reserve: status=%d body=%s", response.Code, response.Body.String())
	}
	releaseReadCapacity(recoveryReleases)

	statusReleases := acquireReadCapacity(t, fixture.auth.capacity, CapacityStatus, maximumReservedInFlight)
	if response := fixture.request(http.MethodGet, "/api/v1/control", true); response.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated status control status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/tasks/task-1", true); response.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated status Task detail status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/tasks", true); response.Code != http.StatusOK {
		t.Fatalf("status reserve leaked into ordinary Task list: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/healthz", false); response.Code != http.StatusOK {
		t.Fatalf("status load consumed health reserve: status=%d body=%s", response.Code, response.Body.String())
	}
	releaseReadCapacity(statusReleases)

	healthReleases := acquireReadCapacity(t, fixture.auth.capacity, CapacityHealth, maximumReservedInFlight)
	if response := fixture.request(http.MethodGet, "/api/v1/readyz", true); response.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated health readiness status=%d body=%s", response.Code, response.Body.String())
	}
	if response := fixture.request(http.MethodGet, "/api/v1/control", true); response.Code != http.StatusOK {
		t.Fatalf("health load consumed status reserve: status=%d body=%s", response.Code, response.Body.String())
	}
	releaseReadCapacity(healthReleases)
}

func TestPublicReadResponseCapMatchesFrozenContract(t *testing.T) {
	t.Parallel()
	if maximumPublicReadResponseSize != 1024*1024 {
		t.Fatalf("public response cap = %d, want 1048576", maximumPublicReadResponseSize)
	}
}

func TestPublicReadWriterFailsClosedAboveFrozenCap(t *testing.T) {
	t.Parallel()

	fixture := newReadTestFixture(t, validFakeReadController())
	response := httptest.NewRecorder()
	fixture.handler.writeReadData(response, http.StatusOK, "request-cap-test", map[string]string{
		"value": strings.Repeat("x", maximumPublicReadResponseSize),
	})
	if response.Code != http.StatusServiceUnavailable || response.Body.Len() > maximumPublicReadResponseSize ||
		!strings.Contains(response.Body.String(), `"code":"unavailable"`) ||
		strings.Contains(response.Body.String(), strings.Repeat("x", 128)) {
		t.Fatalf("oversize public projection did not fail closed: status=%d bytes=%d body=%s",
			response.Code, response.Body.Len(), response.Body.String())
	}
}

func TestInvalidAndUnauthenticatedReadsDoNotConsumeReservedCapacity(t *testing.T) {
	t.Parallel()

	controller := validFakeReadController()
	fixture := newReadTestFixture(t, controller)
	statusReleases := acquireReadCapacity(t, fixture.auth.capacity, CapacityStatus, maximumReservedInFlight)
	recoveryReleases := acquireReadCapacity(t, fixture.auth.capacity, CapacityRecovery, maximumReservedInFlight)
	defer releaseReadCapacity(statusReleases)
	defer releaseReadCapacity(recoveryReleases)

	tests := []struct {
		name          string
		method        string
		target        string
		host          string
		body          string
		authenticated bool
		want          int
	}{
		{name: "unauthenticated status", method: http.MethodGet, target: "/api/v1/control", want: http.StatusUnauthorized},
		{name: "invalid status method", method: http.MethodPost, target: "/api/v1/tasks/task-1", authenticated: true, want: http.StatusMethodNotAllowed},
		{name: "invalid recovery host", method: http.MethodGet, target: "/api/v1/events", host: "wrong.example.test", authenticated: true, want: http.StatusBadRequest},
		{name: "invalid recovery body", method: http.MethodGet, target: "/api/v1/tasks", body: "unexpected", authenticated: true, want: http.StatusBadRequest},
		{name: "invalid recovery query", method: http.MethodGet, target: "/api/v1/dialogs?limit=01", authenticated: true, want: http.StatusBadRequest},
		{name: "empty recovery query marker", method: http.MethodGet, target: "/api/v1/dialogs?", authenticated: true, want: http.StatusBadRequest},
		{name: "unsafe message sequence query", method: http.MethodGet, target: "/api/v1/dialogs/" + readTestDialogID + "/messages?after_sequence=9007199254740992", authenticated: true, want: http.StatusBadRequest},
		{name: "unauthenticated recovery", method: http.MethodGet, target: "/api/v1/tasks", want: http.StatusUnauthorized},
	}

	start := make(chan struct{})
	type result struct {
		name   string
		status int
	}
	results := make(chan result, len(tests))
	for _, test := range tests {
		test := test
		go func() {
			<-start
			request := httptest.NewRequest(test.method, testPublicOrigin+test.target, strings.NewReader(test.body))
			request.Host = "mobile.example.test"
			if test.host != "" {
				request.Host = test.host
			}
			if test.authenticated {
				request.AddCookie(fixture.session)
			}
			response := httptest.NewRecorder()
			fixture.mux.ServeHTTP(response, request)
			results <- result{name: test.name, status: response.Code}
		}()
	}
	before := fixture.auth.capacity.Snapshot()
	close(start)
	for range tests {
		actual := <-results
		for _, test := range tests {
			if test.name == actual.name && actual.status != test.want {
				t.Errorf("%s status=%d, want %d", actual.name, actual.status, test.want)
			}
		}
	}
	after := fixture.auth.capacity.Snapshot()
	if !reflect.DeepEqual(after, before) || after.ReservedInFlight != 2*maximumReservedInFlight {
		t.Fatalf("rejected reads changed reserved capacity: before=%+v after=%+v", before, after)
	}
	if calls := controller.callSnapshot(); len(calls) != 0 {
		t.Fatalf("rejected reads reached Controller: %+v", calls)
	}
}

func TestControlPublishesGatewayCapacityAndCountsCurrentStatusRequest(t *testing.T) {
	t.Parallel()

	controller := validFakeReadController()
	controller.control.Capacity = []mobilecontrollerclient.ControlCapacity{
		{Class: "general", InFlight: 7, Limit: 8},
		{Class: "reserve", InFlight: 1, Limit: 2},
	}
	fixture := newReadTestFixture(t, controller)
	general := acquireReadCapacity(t, fixture.auth.capacity, CapacityGeneral, 2)
	recovery := acquireReadCapacity(t, fixture.auth.capacity, CapacityRecovery, 1)
	defer releaseReadCapacity(general)
	defer releaseReadCapacity(recovery)

	response := fixture.request(http.MethodGet, "/api/v1/control", true)
	if response.Code != http.StatusOK {
		t.Fatalf("control status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data mobilecontrollerclient.ControlSummary `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode public control: %v", err)
	}
	want := []mobilecontrollerclient.ControlCapacity{
		{Class: "general", InFlight: 2, Limit: 24},
		// One held recovery slot plus the current control request's status slot.
		{Class: "reserve", InFlight: 2, Limit: 8},
	}
	if !reflect.DeepEqual(payload.Data.Capacity, want) {
		t.Fatalf("public capacity = %+v, want %+v; body=%s", payload.Data.Capacity, want, response.Body.String())
	}
	if len(payload.Data.Components) != 1 || payload.Data.Components[0].Name != "controller" ||
		payload.Data.FreshnessAt != readTestTime {
		t.Fatalf("Controller components/freshness changed: %+v", payload.Data)
	}
	if strings.Contains(response.Body.String(), `"in_flight":7`) || strings.Contains(response.Body.String(), `"in_flight":1`) {
		t.Fatalf("private Controller capacity leaked: %s", response.Body.String())
	}
}

func TestReadHandlerCollapsesControllerErrorsAndRejectsUnsafeOutput(t *testing.T) {
	t.Parallel()

	t.Run("private error", func(t *testing.T) {
		controller := validFakeReadController()
		controller.errors["control"] = errors.New("private Controller database path and token")
		fixture := newReadTestFixture(t, controller)
		response := fixture.request(http.MethodGet, "/api/v1/control", true)
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private") ||
			strings.Contains(response.Body.String(), "token") {
			t.Fatalf("Controller error leaked: status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("existence hiding and resync", func(t *testing.T) {
		controller := validFakeReadController()
		controller.errors["task"] = &mobilecontrollerclient.RemoteError{Status: http.StatusForbidden, Code: "forbidden"}
		controller.errors["events"] = &mobilecontrollerclient.RemoteError{Status: http.StatusGone, Code: "resync_required"}
		fixture := newReadTestFixture(t, controller)
		if response := fixture.request(http.MethodGet, "/api/v1/tasks/task-1", true); response.Code != http.StatusNotFound {
			t.Fatalf("foreign Task existence leaked: status=%d body=%s", response.Code, response.Body.String())
		}
		response := fixture.request(http.MethodGet, "/api/v1/events", true)
		if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), `"code":"resync_required"`) {
			t.Fatalf("event resync mapping changed: status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("unsafe projection", func(t *testing.T) {
		controller := validFakeReadController()
		controller.messages.Messages[0].Content = "private-secret\x00suffix"
		fixture := newReadTestFixture(t, controller)
		response := fixture.request(
			http.MethodGet, "/api/v1/dialogs/"+readTestDialogID+"/messages", true,
		)
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "private-secret") {
			t.Fatalf("unsafe Controller projection leaked: status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("readiness normalization is closed", func(t *testing.T) {
		controller := validFakeReadController()
		controller.ready.Status = "ok"
		fixture := newReadTestFixture(t, controller)
		response := fixture.request(http.MethodGet, "/api/v1/readyz", true)
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), `"status":"ok"`) {
			t.Fatalf("non-Controller readiness vocabulary was accepted: status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestReadHandlerRequiresConfiguredSharedDependencies(t *testing.T) {
	t.Parallel()
	controller := validFakeReadController()
	if _, err := NewReadHandler(nil, controller); err == nil {
		t.Fatal("nil AuthHandler was accepted")
	}
	auth := &AuthHandler{}
	if _, err := NewReadHandler(auth, controller); err == nil {
		t.Fatal("AuthHandler without shared dependencies was accepted")
	}
	if handler, err := NewReadHandler(nil, nil); err == nil || handler != nil {
		t.Fatal("nil read dependencies were accepted")
	}
}

func acquireReadCapacity(t *testing.T, gate *CapacityGate, class CapacityClass, count int) []func() {
	t.Helper()
	releases := make([]func(), 0, count)
	for range count {
		release, ok := gate.Acquire(class)
		if !ok {
			t.Fatalf("capacity class %d rejected within budget", class)
		}
		releases = append(releases, release)
	}
	return releases
}

func releaseReadCapacity(releases []func()) {
	for _, release := range releases {
		release()
	}
}
