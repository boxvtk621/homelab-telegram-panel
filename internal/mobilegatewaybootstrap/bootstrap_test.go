package mobilegatewaybootstrap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/buildinfo"
	"github.com/boxvtk621/homelab-telegram-panel/internal/identity"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilegatewayconfig"
	"github.com/boxvtk621/homelab-telegram-panel/internal/observability"
)

var fixedTime = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

type loggedEvent struct {
	Name       string         `json:"event"`
	Outcome    string         `json:"outcome"`
	ErrorCode  string         `json:"error_code"`
	Attributes map[string]any `json:"attributes"`
}

func TestExecuteValidateBuildsFullRuntimeWithoutListening(t *testing.T) {
	t.Parallel()

	output := new(bytes.Buffer)
	dependencies := testDependencies(http.NotFoundHandler())
	dependencies.Listen = func(string, string) (net.Listener, error) {
		t.Fatal("validate must not open a listener")
		return nil, errors.New("unreachable")
	}
	code := Execute(context.Background(), []string{"validate"}, validLookup(), output, dependencies)
	if code != ExitOK {
		t.Fatalf("Execute(validate) = %d, want %d; log: %s", code, ExitOK, output.String())
	}
	events := decodeLoggedEvents(t, output)
	if names := loggedEventNames(events); !reflect.DeepEqual(names, []string{"mobile_gateway.configuration.validated"}) {
		t.Fatalf("event names = %v", names)
	}
}

func TestExecuteVersionDoesNotReadConfigurationOrListen(t *testing.T) {
	t.Parallel()

	output := new(bytes.Buffer)
	dependencies := testDependencies(http.NotFoundHandler())
	dependencies.Listen = func(string, string) (net.Listener, error) {
		t.Fatal("version must not listen")
		return nil, errors.New("unreachable")
	}
	code := Execute(context.Background(), []string{"version"}, func(string) (string, bool) {
		t.Fatal("version must not load configuration")
		return "", false
	}, output, dependencies)
	if code != ExitOK {
		t.Fatalf("Execute(version) = %d, want %d", code, ExitOK)
	}
	events := decodeLoggedEvents(t, output)
	if len(events) != 1 || events[0].Name != "mobile_gateway.version.reported" || events[0].Attributes["version"] != buildinfo.Version {
		t.Fatalf("unexpected version event: %#v", events)
	}
}

func TestExecuteRejectsInvalidCommandAndConfiguration(t *testing.T) {
	t.Parallel()

	t.Run("command", func(t *testing.T) {
		output := new(bytes.Buffer)
		code := Execute(context.Background(), []string{"run"}, validLookup(), output, testDependencies(http.NotFoundHandler()))
		if code != ExitUsage {
			t.Fatalf("Execute(run) = %d, want %d", code, ExitUsage)
		}
		events := decodeLoggedEvents(t, output)
		if len(events) != 1 || events[0].ErrorCode != "COMMAND_INVALID" {
			t.Fatalf("unexpected command events: %#v", events)
		}
	})

	t.Run("configuration", func(t *testing.T) {
		output := new(bytes.Buffer)
		code := Execute(context.Background(), []string{"validate"}, func(string) (string, bool) { return "", false }, output, testDependencies(http.NotFoundHandler()))
		if code != ExitUsage {
			t.Fatalf("Execute(validate) = %d, want %d", code, ExitUsage)
		}
		events := decodeLoggedEvents(t, output)
		if len(events) != 1 || events[0].ErrorCode != "CONFIG_INVALID" {
			t.Fatalf("unexpected configuration events: %#v", events)
		}
	})
}

func TestExecuteServeRejectsPrivilegedOrUnknownIdentityBeforeListening(t *testing.T) {
	t.Parallel()

	for _, effectiveUID := range []int{0, -1} {
		dependencies := testDependencies(http.NotFoundHandler())
		dependencies.EffectiveUID = effectiveUID
		dependencies.Listen = func(string, string) (net.Listener, error) {
			t.Fatal("privileged process must not listen")
			return nil, errors.New("unreachable")
		}
		output := new(bytes.Buffer)
		code := Execute(context.Background(), []string{"serve"}, validLookup(), output, dependencies)
		if code != ExitRuntime {
			t.Fatalf("Execute(serve) with uid %d = %d, want %d", effectiveUID, code, ExitRuntime)
		}
		events := decodeLoggedEvents(t, output)
		if len(events) != 1 || events[0].ErrorCode != "PRIVILEGED_PROCESS" {
			t.Fatalf("unexpected events for uid %d: %#v", effectiveUID, events)
		}
	}
}

func TestExecuteServeUsesExactLoopbackAddressAndShutsDownGracefully(t *testing.T) {
	t.Parallel()

	listener := newBlockingListener()
	listened := make(chan struct{})
	dependencies := testDependencies(http.NotFoundHandler())
	dependencies.Listen = func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != "127.0.0.1:18080" {
			t.Errorf("Listen(%q, %q), want tcp and exact configured loopback", network, address)
		}
		close(listened)
		return listener, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	output := new(bytes.Buffer)
	result := make(chan int, 1)
	lookup, _ := controllerIdentityTestLookup(t, "telegram-user:200002", controllerCapabilitiesVersion, nil)
	go func() {
		result <- Execute(ctx, []string{"serve"}, lookup, output, dependencies)
	}()
	select {
	case <-listened:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not open the listener")
	}
	cancel()
	select {
	case code := <-result:
		if code != ExitOK {
			t.Fatalf("Execute(serve) = %d, want %d; log: %s", code, ExitOK, output.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not finish after cancellation")
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("graceful shutdown did not close the listener")
	}
	events := decodeLoggedEvents(t, output)
	if names := loggedEventNames(events); !reflect.DeepEqual(names, []string{
		"mobile_gateway.process.started", "mobile_gateway.process.finished",
	}) {
		t.Fatalf("event names = %v", names)
	}
	if events[1].Outcome != string(observability.OutcomeSucceeded) {
		t.Fatalf("shutdown outcome = %q", events[1].Outcome)
	}
}

func TestExecuteServeRefusesControllerPrincipalDriftBeforeListening(t *testing.T) {
	t.Parallel()

	lookup, _ := controllerIdentityTestLookup(t, "telegram-user:999999", controllerCapabilitiesVersion, nil)
	dependencies := testDependencies(http.NotFoundHandler())
	dependencies.Listen = func(string, string) (net.Listener, error) {
		t.Fatal("identity mismatch must fail before opening the public listener")
		return nil, errors.New("unreachable")
	}
	output := new(bytes.Buffer)
	code := Execute(context.Background(), []string{"serve"}, lookup, output, dependencies)
	if code != ExitRuntime || !strings.Contains(output.String(), "PROCESS_FAILED") {
		t.Fatalf("identity drift did not fail closed: code=%d log=%s", code, output.String())
	}
	if strings.Contains(output.String(), "telegram-user:999999") || strings.Contains(output.String(), "telegram-user:200002") {
		t.Fatalf("principal identity leaked to process log: %s", output.String())
	}
}

func TestPrincipalBoundReadinessRechecksIdentityAfterStartup(t *testing.T) {
	t.Parallel()

	var readyCalls atomic.Int32
	_, configuration := controllerIdentityTestLookup(
		t, "telegram-user:999999", controllerCapabilitiesVersion, &readyCalls,
	)
	client, err := mobilecontrollerclient.New(
		configuration.ControllerBusinessSocket,
		configuration.ControllerHealthSocket,
		configuration.ControllerControlSocket,
		configuration.ControllerRecoverySocket,
		"telegram-user:200002",
		controllerCapabilitiesVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	bound := &principalBoundController{Client: client, expectedPrincipal: "telegram-user:200002", expectedCapabilities: controllerCapabilitiesVersion}
	if _, err := bound.Ready(context.Background(), "readiness-binding-check"); err == nil {
		t.Fatal("readiness accepted changed Controller principal")
	}
	if readyCalls.Load() != 0 {
		t.Fatalf("readiness probe ran after identity mismatch: %d", readyCalls.Load())
	}
}

func TestDialogCreationOptInStartupAndReadiness(t *testing.T) {
	t.Parallel()
	var readyCalls atomic.Int32
	_, cfg := controllerIdentityTestLookup(t, "telegram-user:200002", "mobile-workspace-dialog-create-v1", &readyCalls)
	cfg.DialogCreationEnabled = true
	dependencies := testDependencies(http.NotFoundHandler())
	logger, err := observability.New(new(bytes.Buffer), observability.WithClock(dependencies.Clock))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := buildRuntime(cfg, dependencies, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	if err := runtime.identityCheck(context.Background()); err != nil {
		t.Fatalf("opt-in startup: %v", err)
	}
	bound := &principalBoundController{Client: runtime.controller, expectedPrincipal: "telegram-user:200002", expectedCapabilities: "mobile-workspace-dialog-create-v1"}
	if _, err := bound.Ready(context.Background(), "opt-in-readiness"); err != nil || readyCalls.Load() != 1 {
		t.Fatalf("opt-in ready: calls=%d err=%v", readyCalls.Load(), err)
	}
}

func TestBuildRuntimeWiresAuthNineReadsSessionsAuditAndStaticBoundary(t *testing.T) {
	t.Parallel()

	staticCalls := 0
	static := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		staticCalls++
		response.WriteHeader(http.StatusNoContent)
	})
	dependencies := testDependencies(static)
	loggerOutput := new(bytes.Buffer)
	logger, err := observability.New(loggerOutput, observability.WithClock(dependencies.Clock))
	if err != nil {
		t.Fatalf("observability.New(): %v", err)
	}
	runtime, err := buildRuntime(validConfiguration(), dependencies, logger)
	if err != nil {
		t.Fatalf("buildRuntime(): %v", err)
	}
	defer runtime.close()

	business, health, control, recovery, err := runtime.controller.SocketPaths()
	if err != nil {
		t.Fatalf("SocketPaths(): %v", err)
	}
	if business != "/tmp/fixik-mobile-business-missing.sock" ||
		health != "/tmp/fixik-mobile-health-missing.sock" ||
		control != "/tmp/fixik-mobile-control-missing.sock" ||
		recovery != "/tmp/fixik-mobile-recovery-missing.sock" {
		t.Fatalf("unexpected isolated Controller sockets: %q %q %q %q", business, health, control, recovery)
	}
	if runtime.sessions == nil || runtime.sessions.ActiveCount() != 0 {
		t.Fatal("runtime must start with an empty in-memory session store")
	}
	identityResult := mobileauth.TelegramIdentity{
		UserID: 200002, AuthDate: fixedTime.Add(2 * time.Second), Fingerprint: [32]byte{1},
	}
	_, _, session, err := runtime.sessions.Exchange(identityResult, "test-client")
	if err != nil {
		t.Fatalf("session Exchange(): %v", err)
	}
	if session.CapabilityVersion != capabilityVersion || runtime.sessions.ActiveCount() != 1 {
		t.Fatalf("session capability/count = %d/%d", session.CapabilityVersion, runtime.sessions.ActiveCount())
	}

	readPaths := []string{
		"/api/v1/dialogs",
		"/api/v1/dialogs/dialog-1",
		"/api/v1/dialogs/dialog-1/messages",
		"/api/v1/events",
		"/api/v1/tasks",
		"/api/v1/tasks/task-1",
		"/api/v1/control",
		"/api/v1/healthz",
		"/api/v1/readyz",
	}
	for _, requestPath := range readPaths {
		request := httptest.NewRequest(http.MethodGet, "https://mobile.example.test"+requestPath, nil)
		response := httptest.NewRecorder()
		runtime.server.Handler.ServeHTTP(response, request)
		if response.Code == http.StatusNotFound {
			t.Errorf("read route %s was not registered", requestPath)
		}
	}

	bootstrapRequest := httptest.NewRequest(http.MethodGet, "https://mobile.example.test/api/v1/auth/bootstrap", nil)
	bootstrapRequest.Header.Set("X-Fixik-Client-Instance", "browser-1")
	bootstrapResponse := httptest.NewRecorder()
	runtime.server.Handler.ServeHTTP(bootstrapResponse, bootstrapRequest)
	if bootstrapResponse.Code != http.StatusOK || len(bootstrapResponse.Result().Cookies()) != 1 {
		t.Fatalf("bootstrap status/cookies = %d/%d; body: %s", bootstrapResponse.Code, len(bootstrapResponse.Result().Cookies()), bootstrapResponse.Body.String())
	}

	staticResponse := httptest.NewRecorder()
	runtime.server.Handler.ServeHTTP(staticResponse, httptest.NewRequest(http.MethodGet, "https://mobile.example.test/index.html", nil))
	if staticResponse.Code != http.StatusNoContent || staticCalls != 1 {
		t.Fatalf("static status/calls = %d/%d", staticResponse.Code, staticCalls)
	}
	unknownAPIResponse := httptest.NewRecorder()
	runtime.server.Handler.ServeHTTP(unknownAPIResponse, httptest.NewRequest(http.MethodGet, "https://mobile.example.test/api/v1/unknown", nil))
	if unknownAPIResponse.Code != http.StatusNotFound || staticCalls != 1 {
		t.Fatalf("unknown API status/static calls = %d/%d", unknownAPIResponse.Code, staticCalls)
	}
	if unknownAPIResponse.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		unknownAPIResponse.Header().Get("Cache-Control") != "no-store" ||
		unknownAPIResponse.Header().Get("Content-Security-Policy") == "" ||
		unknownAPIResponse.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unknown API security headers = %#v", unknownAPIResponse.Header())
	}
	var unknownEnvelope map[string]any
	if err := json.Unmarshal(unknownAPIResponse.Body.Bytes(), &unknownEnvelope); err != nil {
		t.Fatalf("unknown API body is not JSON: %v: %s", err, unknownAPIResponse.Body.String())
	}
	unknownError, ok := unknownEnvelope["error"].(map[string]any)
	if unknownEnvelope["schema_version"] != float64(1) || !ok || unknownError["code"] != "not_found" ||
		unknownError["message"] != "request could not be completed" {
		t.Fatalf("unknown API envelope = %#v", unknownEnvelope)
	}
	wrongHostResponse := httptest.NewRecorder()
	wrongHostRequest := httptest.NewRequest(http.MethodGet, "https://attacker.invalid/index.html", nil)
	runtime.server.Handler.ServeHTTP(wrongHostResponse, wrongHostRequest)
	if wrongHostResponse.Code != http.StatusNotFound || staticCalls != 1 ||
		wrongHostResponse.Header().Get("Cache-Control") != "no-store" ||
		wrongHostResponse.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("wrong-host static status/calls/headers = %d/%d/%#v", wrongHostResponse.Code, staticCalls, wrongHostResponse.Header())
	}
	nonCanonicalResponse := httptest.NewRecorder()
	runtime.server.Handler.ServeHTTP(nonCanonicalResponse, httptest.NewRequest(http.MethodGet, "https://mobile.example.test/api//v1/healthz", nil))
	if nonCanonicalResponse.Code != http.StatusNotFound || nonCanonicalResponse.Code == http.StatusMovedPermanently {
		t.Fatalf("noncanonical path status = %d", nonCanonicalResponse.Code)
	}
	if nonCanonicalResponse.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("noncanonical API response is not bounded JSON: %#v", nonCanonicalResponse.Header())
	}
	encodedPathResponse := httptest.NewRecorder()
	encodedPathRequest := httptest.NewRequest(http.MethodGet, "https://mobile.example.test/api/v1/healthz", nil)
	encodedPathRequest.URL.RawPath = "/api/v1/%68ealthz"
	runtime.server.Handler.ServeHTTP(encodedPathResponse, encodedPathRequest)
	if encodedPathResponse.Code != http.StatusNotFound ||
		encodedPathResponse.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("encoded canonical route was dispatched: %d %#v", encodedPathResponse.Code, encodedPathResponse.Header())
	}
	apiRootResponse := httptest.NewRecorder()
	runtime.server.Handler.ServeHTTP(apiRootResponse, httptest.NewRequest(http.MethodGet, "https://mobile.example.test/api", nil))
	if apiRootResponse.Code != http.StatusNotFound || apiRootResponse.Header().Get("Location") != "" ||
		apiRootResponse.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("API root redirected or escaped JSON boundary: %d %#v", apiRootResponse.Code, apiRootResponse.Header())
	}
}

func testDependencies(static http.Handler) Dependencies {
	return Dependencies{
		StaticHandler: static,
		Clock:         mobileauth.ClockFunc(func() time.Time { return fixedTime }),
		IDs:           identity.NewGenerator(),
		Listen: func(string, string) (net.Listener, error) {
			return nil, errors.New("listener not configured for test")
		},
		EffectiveUID: 501,
	}
}

func validLookup() mobilegatewayconfig.LookupEnv {
	values := validLookupValues()
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func validLookupValues() map[string]string {
	return map[string]string{
		mobilegatewayconfig.EnvListenAddress:            "127.0.0.1:18080",
		mobilegatewayconfig.EnvPublicOrigin:             "https://mobile.example.test",
		mobilegatewayconfig.EnvControllerBusinessSocket: "/tmp/fixik-mobile-business-missing.sock",
		mobilegatewayconfig.EnvControllerHealthSocket:   "/tmp/fixik-mobile-health-missing.sock",
		mobilegatewayconfig.EnvControllerControlSocket:  "/tmp/fixik-mobile-control-missing.sock",
		mobilegatewayconfig.EnvControllerRecoverySocket: "/tmp/fixik-mobile-recovery-missing.sock",
		mobilegatewayconfig.EnvTelegramBotID:            "100001",
		mobilegatewayconfig.EnvTelegramOwnerID:          "200002",
		mobilegatewayconfig.EnvTelegramEnvironment:      "test",
		mobilegatewayconfig.EnvShutdownTimeoutMS:        "1000",
	}
}

func controllerIdentityTestLookup(
	t *testing.T,
	principal string,
	capabilities string,
	readyCalls *atomic.Int32,
) (mobilegatewayconfig.LookupEnv, mobilegatewayconfig.Config) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "hl210-gateway-identity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	paths := []string{
		filepath.Join(directory, "business.sock"), filepath.Join(directory, "health.sock"),
		filepath.Join(directory, "status.sock"), filepath.Join(directory, "recovery.sock"),
	}
	for index, socketPath := range paths {
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatal(err)
		}
		endpoint := index
		server := &http.Server{
			ReadHeaderTimeout: time.Second,
			Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requestID := request.Header.Get("X-Request-ID")
				encodedPrincipal, _ := json.Marshal(principal)
				encodedCapabilities, _ := json.Marshal(capabilities)
				if endpoint != 1 {
					http.NotFound(response, request)
					return
				}
				data := ""
				switch request.URL.Path {
				case "/internal/mobile/v2/identity":
					data = `{"principal":` + string(encodedPrincipal) + `,"capabilities_version":` + string(encodedCapabilities) + `}`
				case "/internal/mobile/v2/readyz":
					if readyCalls != nil {
						readyCalls.Add(1)
					}
					data = `{"status":"ready"}`
				default:
					http.NotFound(response, request)
					return
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{"schema_version":2,"request_id":"` + requestID +
					`","server_time":"2026-08-29T12:00:00Z","principal":` + string(encodedPrincipal) +
					`,"capabilities_version":` + string(encodedCapabilities) + `,"data":` + data + `}` + "\n"))
			}),
		}
		done := make(chan struct{})
		go func() {
			_ = server.Serve(listener)
			close(done)
		}()
		t.Cleanup(func() {
			_ = server.Close()
			<-done
		})
	}
	values := validLookupValues()
	values[mobilegatewayconfig.EnvControllerBusinessSocket] = paths[0]
	values[mobilegatewayconfig.EnvControllerHealthSocket] = paths[1]
	values[mobilegatewayconfig.EnvControllerControlSocket] = paths[2]
	values[mobilegatewayconfig.EnvControllerRecoverySocket] = paths[3]
	lookup := func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
	configuration, err := mobilegatewayconfig.Load(lookup)
	if err != nil {
		t.Fatal(err)
	}
	return lookup, configuration
}

func validConfiguration() mobilegatewayconfig.Config {
	return mobilegatewayconfig.Config{
		ListenAddress:            "127.0.0.1:18080",
		PublicOrigin:             "https://mobile.example.test",
		ControllerBusinessSocket: "/tmp/fixik-mobile-business-missing.sock",
		ControllerHealthSocket:   "/tmp/fixik-mobile-health-missing.sock",
		ControllerControlSocket:  "/tmp/fixik-mobile-control-missing.sock",
		ControllerRecoverySocket: "/tmp/fixik-mobile-recovery-missing.sock",
		TelegramBotID:            100001,
		TelegramOwnerID:          200002,
		TelegramEnvironment:      mobileauth.EnvironmentTest,
		ShutdownTimeout:          time.Second,
	}
}

func decodeLoggedEvents(t *testing.T, output *bytes.Buffer) []loggedEvent {
	t.Helper()
	var events []loggedEvent
	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	for scanner.Scan() {
		var event loggedEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid log event %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one log event")
	}
	return events
}

func loggedEventNames(events []loggedEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Name)
	}
	return names
}

type blockingListener struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingListener() *blockingListener {
	return &blockingListener{closed: make(chan struct{})}
}

func (listener *blockingListener) Accept() (net.Conn, error) {
	<-listener.closed
	return nil, net.ErrClosed
}

func (listener *blockingListener) Close() error {
	listener.once.Do(func() { close(listener.closed) })
	return nil
}

func (listener *blockingListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18080}
}
