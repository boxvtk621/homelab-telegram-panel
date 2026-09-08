package mobilecontrollerclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testRequestID           = "request-018f0c9e"
	testPrincipal           = "owner-a"
	testCapabilitiesVersion = "capabilities-v1"
)

func TestControllerResponseCapMatchesFrozenContract(t *testing.T) {
	t.Parallel()
	if maximumResponseBytes != 1024*1024 {
		t.Fatalf("Controller response cap = %d, want 1048576", maximumResponseBytes)
	}
}

func TestClientReadsExactControllerProjectionOverUnixSockets(t *testing.T) {
	t.Parallel()

	businessPath, stopBusiness := serveUnix(t, func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Host != "controller" ||
			request.Header.Get("X-Request-ID") != testRequestID ||
			request.URL.RequestURI() != "/internal/mobile/v2/dialogs?limit=20" {
			t.Fatalf("unexpected ordinary Controller request: method=%s host=%s uri=%s headers=%v",
				request.Method, request.Host, request.URL.RequestURI(), request.Header)
		}
		writeControllerResponse(response, http.StatusOK, testRequestID,
			`{"dialogs":[{"id":"018f0c9e-8f4b-4a6b-8c9d-000000000001","lifecycle":"active","object_version":2,"created_at":"2026-08-29T10:00:00.000000Z","updated_at":"2026-08-29T10:00:01.000000Z"}],"snapshot_version":2,"freshness_at":"2026-08-29T10:00:02.000000Z"}`, "")
	})
	defer stopBusiness()
	healthPath, stopHealth := serveUnix(t, func(response http.ResponseWriter, request *http.Request) {
		if request.URL.RequestURI() != "/internal/mobile/v2/healthz" {
			t.Fatalf("unexpected health request: %s", request.URL.RequestURI())
		}
		writeControllerResponse(response, http.StatusOK, testRequestID, `{"status":"ok"}`, "")
	})
	defer stopHealth()
	controlPath, stopControl := serveUnix(t, func(response http.ResponseWriter, request *http.Request) {
		t.Fatalf("read reached unused status socket: %s", request.URL.RequestURI())
	})
	defer stopControl()
	recoveryPath, stopRecovery := serveUnix(t, func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Host != "controller" ||
			request.Header.Get("X-Request-ID") != testRequestID ||
			request.URL.RequestURI() != "/internal/mobile/v2/recovery/dialogs?limit=20" {
			t.Fatalf("unexpected Controller request: method=%s host=%s uri=%s headers=%v",
				request.Method, request.Host, request.URL.RequestURI(), request.Header)
		}
		writeControllerResponse(response, http.StatusOK, testRequestID,
			`{"dialogs":[{"id":"018f0c9e-8f4b-4a6b-8c9d-000000000001","lifecycle":"active","object_version":2,"created_at":"2026-08-29T10:00:00.000000Z","updated_at":"2026-08-29T10:00:01.000000Z"}],"snapshot_version":2,"freshness_at":"2026-08-29T10:00:02.000000Z"}`, "")
	})
	defer stopRecovery()

	client, err := New(
		businessPath, healthPath, controlPath, recoveryPath,
		testPrincipal, testCapabilitiesVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	page, err := client.ListDialogs(context.Background(), testRequestID, 20, "")
	if err != nil || len(page.Dialogs) != 1 || page.Dialogs[0].ObjectVersion != 2 || page.SnapshotVersion != 2 {
		t.Fatalf("safe Controller projection did not round-trip: page=%+v err=%v", page, err)
	}
	recovered, err := client.RecoverDialogs(context.Background(), testRequestID, 20, "")
	if err != nil || len(recovered.Dialogs) != 1 || recovered.Dialogs[0].ObjectVersion != 2 || recovered.SnapshotVersion != 2 {
		t.Fatalf("explicit recovery projection did not round-trip: page=%+v err=%v", recovered, err)
	}
	status, err := client.Health(context.Background(), testRequestID)
	if err != nil || status.Status != "ok" {
		t.Fatalf("isolated control endpoint failed: status=%+v err=%v", status, err)
	}
}

func TestClientKeepsOrdinaryAndExplicitRecoveryListTransportsDistinct(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var businessURIs, recoveryURIs []string
	record := func(destination *[]string) http.HandlerFunc {
		return func(response http.ResponseWriter, request *http.Request) {
			mu.Lock()
			*destination = append(*destination, request.URL.RequestURI())
			mu.Unlock()
			writeControllerResponse(response, http.StatusOK, testRequestID, `{}`, "")
		}
	}
	businessPath, stopBusiness := serveUnix(t, record(&businessURIs))
	defer stopBusiness()
	healthPath, stopHealth := serveUnix(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("list read reached health socket")
	})
	defer stopHealth()
	controlPath, stopControl := serveUnix(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("list read reached status socket")
	})
	defer stopControl()
	recoveryPath, stopRecovery := serveUnix(t, record(&recoveryURIs))
	defer stopRecovery()

	client, err := New(businessPath, healthPath, controlPath, recoveryPath, testPrincipal, testCapabilitiesVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	for _, test := range []struct {
		name string
		call func() error
	}{
		{name: "dialogs", call: func() error {
			_, err := client.ListDialogs(context.Background(), testRequestID, 20, "")
			return err
		}},
		{name: "events", call: func() error {
			_, err := client.ListEvents(context.Background(), testRequestID, 20, "")
			return err
		}},
		{name: "tasks", call: func() error {
			_, err := client.ListTasks(context.Background(), testRequestID, 20)
			return err
		}},
		{name: "recover dialogs", call: func() error {
			_, err := client.RecoverDialogs(context.Background(), testRequestID, 20, "")
			return err
		}},
		{name: "recover events", call: func() error {
			_, err := client.RecoverEvents(context.Background(), testRequestID, 20, "")
			return err
		}},
		{name: "recover tasks", call: func() error {
			_, err := client.RecoverTasks(context.Background(), testRequestID, 20)
			return err
		}},
	} {
		if err := test.call(); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(businessURIs, ","), strings.Join([]string{
		"/internal/mobile/v2/dialogs?limit=20",
		"/internal/mobile/v2/events?limit=20",
		"/internal/mobile/v2/tasks?limit=20",
	}, ","); got != want {
		t.Fatalf("ordinary list transport = %q, want %q", got, want)
	}
	if got, want := strings.Join(recoveryURIs, ","), strings.Join([]string{
		"/internal/mobile/v2/recovery/dialogs?limit=20",
		"/internal/mobile/v2/recovery/events?limit=20",
		"/internal/mobile/v2/recovery/tasks?limit=20",
	}, ","); got != want {
		t.Fatalf("recovery list transport = %q, want %q", got, want)
	}
}

func TestClientRejectsUnknownFieldsRequestMismatchAndUnboundedResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		responseID  string
		data        string
		header      func(http.Header)
		wantInvalid bool
	}{
		{
			name: "unknown owner field", responseID: testRequestID,
			data:        `{"tasks":[],"freshness_at":"2026-08-29T10:00:02.000000Z","owner_id":"private-owner"}`,
			wantInvalid: true,
		},
		{
			name: "request mismatch", responseID: "different-request",
			data:        `{"tasks":[],"freshness_at":"2026-08-29T10:00:02.000000Z"}`,
			wantInvalid: true,
		},
		{
			name: "content encoding", responseID: testRequestID,
			data:        `{"tasks":[],"freshness_at":"2026-08-29T10:00:02.000000Z"}`,
			header:      func(header http.Header) { header.Set("Content-Encoding", "gzip") },
			wantInvalid: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newUniformTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
				if test.header != nil {
					test.header(response.Header())
				}
				writeControllerResponse(response, http.StatusOK, test.responseID, test.data, "")
			})
			defer client.CloseIdleConnections()
			_, err := client.ListTasks(context.Background(), testRequestID, 20)
			if !test.wantInvalid || err == nil || !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("invalid Controller response accepted: %v", err)
			}
		})
	}
}

func TestClientRejectsAuthorityDriftOnEverySocketClass(t *testing.T) {
	t.Parallel()

	client := newUniformTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
		writeControllerResponseWithAuthority(
			response, http.StatusOK, testRequestID, `{}`, "",
			"owner-b", testCapabilitiesVersion,
		)
	})
	defer client.CloseIdleConnections()

	tests := []struct {
		name string
		call func() error
	}{
		{name: "business", call: func() error {
			_, err := client.GetDialog(context.Background(), testRequestID, "018f0c9e-8f4b-4a6b-8c9d-000000000001")
			return err
		}},
		{name: "health", call: func() error {
			_, err := client.Health(context.Background(), testRequestID)
			return err
		}},
		{name: "status", call: func() error {
			_, err := client.Control(context.Background(), testRequestID)
			return err
		}},
		{name: "recovery", call: func() error {
			_, err := client.RecoverTasks(context.Background(), testRequestID, 20)
			return err
		}},
	}
	for _, test := range tests {
		if err := test.call(); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("Controller principal drift on %s socket was accepted: %v", test.name, err)
		}
	}
}

func TestClientRejectsChunkedResponseAboveFrozenCap(t *testing.T) {
	t.Parallel()

	client := newUniformTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		if flusher, ok := response.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = response.Write([]byte(strings.Repeat("x", maximumResponseBytes+1)))
	})
	defer client.CloseIdleConnections()

	if _, err := client.ListTasks(context.Background(), testRequestID, 20); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("chunked Controller response above hard cap was accepted: %v", err)
	}
}

func TestClientCollapsesRemoteErrorMessage(t *testing.T) {
	t.Parallel()

	secret := "private-database-error"
	client := newUniformTestClient(t, func(response http.ResponseWriter, _ *http.Request) {
		writeControllerResponse(response, http.StatusNotFound, testRequestID, "", secret)
	})
	defer client.CloseIdleConnections()
	_, err := client.GetTask(context.Background(), testRequestID, "task-1")
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Status != http.StatusNotFound || remote.Code != "not_found" ||
		strings.Contains(err.Error(), secret) {
		t.Fatalf("remote failure leaked or changed: %#v", err)
	}
}

func TestClientEscapesUTF8TaskIdentityAsOnePathSegment(t *testing.T) {
	t.Parallel()

	const taskID = "задача 50% готова"
	client := newUniformTestClient(t, func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/internal/mobile/v2/tasks/"+taskID ||
			request.URL.RawQuery != "" || strings.Contains(request.URL.RequestURI(), " 50% ") {
			t.Fatalf("Task ID was not transported as one escaped segment: path=%q uri=%q", request.URL.Path, request.URL.RequestURI())
		}
		writeControllerResponse(response, http.StatusOK, testRequestID,
			`{"task":{"id":"задача 50% готова","dialog_id":"018f0c9e-8f4b-4a6b-8c9d-000000000001","issue_id":"HL-210","expected_revision":1,"state":"running","cancel_state":"none","created_at":"2026-08-29T10:00:00Z","updated_at":"2026-08-29T10:00:00Z"},"freshness_at":"2026-08-29T10:00:01Z"}`, "")
	})
	defer client.CloseIdleConnections()

	snapshot, err := client.GetTask(context.Background(), testRequestID, taskID)
	if err != nil || snapshot.Task.ID != taskID {
		t.Fatalf("UTF-8 Task identity did not round-trip: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestClientRejectsAmbiguousTaskPathSegmentsBeforeDispatch(t *testing.T) {
	t.Parallel()

	client := newUniformTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("ambiguous Task identity reached Controller transport")
	})
	defer client.CloseIdleConnections()

	for _, taskID := range []string{"", " ", " leading", "trailing ", "path/segment", "query?", "fragment#", "line\nbreak", strings.Repeat("я", 129), string([]byte{0xff})} {
		if _, err := client.GetTask(context.Background(), testRequestID, taskID); err == nil {
			t.Errorf("ambiguous Task ID %q was accepted", taskID)
		}
	}
}

func TestClientRejectsAliasedSocketClasses(t *testing.T) {
	t.Parallel()

	if _, err := New(
		"/tmp/business.sock", "/tmp/health.sock", "/tmp/status.sock", "/tmp/business.sock",
		testPrincipal, testCapabilitiesVersion,
	); err == nil {
		t.Fatal("aliased Controller socket classes were accepted")
	}
	if _, err := New(
		"/tmp/business.sock", "/tmp/health.sock", "/tmp/status.sock", "/tmp/recovery.sock",
		"", testCapabilitiesVersion,
	); err == nil {
		t.Fatal("empty Controller authority binding was accepted")
	}
}

func newUniformTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	paths := make([]string, 0, 4)
	for range 4 {
		path, stop := serveUnix(t, handler)
		paths = append(paths, path)
		t.Cleanup(stop)
	}
	client, err := New(paths[0], paths[1], paths[2], paths[3], testPrincipal, testCapabilitiesVersion)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func serveUnix(t *testing.T, handler http.HandlerFunc) (string, func()) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "hl210-mc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "controller.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: serverReadHeaderTimeoutForTest}
	done := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(done)
	}()
	return path, func() {
		_ = server.Close()
		<-done
	}
}

const serverReadHeaderTimeoutForTest = defaultTimeout

func writeControllerResponse(response http.ResponseWriter, status int, requestID, data, secretMessage string) {
	writeControllerResponseWithAuthority(
		response, status, requestID, data, secretMessage, testPrincipal, testCapabilitiesVersion,
	)
}

func writeControllerResponseWithAuthority(
	response http.ResponseWriter,
	status int,
	requestID, data, secretMessage, principal, capabilitiesVersion string,
) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = response.Write([]byte(`{"schema_version":2,"request_id":"` + requestID +
			`","server_time":"2026-08-29T10:00:03.000000Z","principal":"` + principal +
			`","capabilities_version":"` + capabilitiesVersion + `","data":` + data + `}` + "\n"))
		return
	}
	_, _ = response.Write([]byte(`{"schema_version":2,"request_id":"` + requestID +
		`","server_time":"2026-08-29T10:00:03.000000Z","principal":"` + principal +
		`","capabilities_version":"` + capabilitiesVersion + `","error":{"code":"not_found","message":"` +
		secretMessage + `"}}` + "\n"))
}
