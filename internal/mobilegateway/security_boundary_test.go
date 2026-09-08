package mobilegateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
)

func TestBootstrapRateLimitProtectsBoundedPool(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	handler := mustAuthHandler(t, clock, &fakeTelegramVerifier{}, &sequenceIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)

	for index := 0; index < bootstrapRateLimit; index++ {
		response := serve(mux, http.MethodGet, "/api/v1/auth/bootstrap", "", map[string]string{
			// Forwarded identity is deliberately ignored: the local Gateway gate
			// is global and the external edge has its own source-IP policy.
			"X-Forwarded-For":    "198.51.100." + strconv.Itoa(index),
			clientInstanceHeader: "rate-" + strconv.Itoa(index),
		})
		if response.Code != http.StatusOK {
			t.Fatalf("bootstrap %d status=%d body=%s", index, response.Code, response.Body.String())
		}
	}
	rejected := serve(mux, http.MethodGet, "/api/v1/auth/bootstrap", "", nil)
	if rejected.Code != http.StatusTooManyRequests || rejected.Header().Get("Retry-After") != "60" || len(rejected.Result().Cookies()) != 0 {
		t.Fatalf("rate rejection status=%d retry=%q cookies=%v body=%s", rejected.Code, rejected.Header().Get("Retry-After"), rejected.Result().Cookies(), rejected.Body.String())
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(authRateWindow)
	clock.mu.Unlock()
	if response := serveBootstrap(mux, "after-window"); response.Code != http.StatusOK {
		t.Fatalf("bootstrap after window status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAuthRateGateIsFixedAndClockRollbackDoesNotRefill(t *testing.T) {
	t.Parallel()
	gate := newAuthGate()
	now := time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)
	for range exchangeRateLimit {
		if !gate.admit(authRouteExchange, now) {
			t.Fatal("auth gate rejected within rate budget")
		}
	}
	if gate.admit(authRouteExchange, now) {
		t.Fatal("auth gate exceeded rate budget")
	}
	if gate.admit(authRouteExchange, now.Add(-time.Second)) {
		t.Fatal("clock rollback refilled auth budget")
	}
	if !gate.admit(authRouteExchange, now.Add(authRateWindow)) {
		t.Fatal("auth gate did not refill after exact window")
	}
}

func TestNonCanonicalAuthRequestsDoNotBurnGlobalAdmission(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	handler := mustAuthHandler(t, clock, &fakeTelegramVerifier{}, &sequenceIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)

	tests := []struct {
		name    string
		request *http.Request
		status  int
	}{
		{
			name: "wrong method",
			request: httptest.NewRequest(
				http.MethodPost, testPublicOrigin+"/api/v1/auth/bootstrap", nil,
			),
			status: http.StatusMethodNotAllowed,
		},
		{
			name: "wrong host",
			request: httptest.NewRequest(
				http.MethodGet, "https://attacker.invalid/api/v1/auth/bootstrap", nil,
			),
			status: http.StatusBadRequest,
		},
		{
			name: "wrong origin",
			request: func() *http.Request {
				request := httptest.NewRequest(
					http.MethodPost, testPublicOrigin+"/api/v1/auth/telegram", strings.NewReader("signed"),
				)
				request.Header.Set("Origin", "https://attacker.invalid")
				return request
			}(),
			status: http.StatusForbidden,
		},
		{
			name: "query ambiguity",
			request: httptest.NewRequest(
				http.MethodGet, testPublicOrigin+"/api/v1/auth/bootstrap?debug=1", nil,
			),
			status: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, test.request)
		if response.Code != test.status {
			t.Errorf("%s status=%d want=%d body=%s", test.name, response.Code, test.status, response.Body.String())
		}
	}
	handler.gate.mu.Lock()
	windows := len(handler.gate.windows)
	handler.gate.mu.Unlock()
	if windows != 0 {
		t.Fatalf("non-canonical requests consumed auth rate windows: %d", windows)
	}
	if snapshot := handler.capacity.Snapshot(); snapshot.GeneralInFlight != 0 || snapshot.ReservedInFlight != 0 {
		t.Fatalf("non-canonical requests consumed capacity: %+v", snapshot)
	}
}

func TestCapacityGateKeepsFourIsolatedTwoSlotReserves(t *testing.T) {
	t.Parallel()
	gate := NewCapacityGate()
	releases := make([]func(), 0, maximumBusinessInFlight)
	for range maximumGeneralInFlight {
		release, ok := gate.Acquire(CapacityGeneral)
		if !ok {
			t.Fatal("general request rejected within its 24-slot budget")
		}
		releases = append(releases, release)
	}
	if _, ok := gate.Acquire(CapacityGeneral); ok {
		t.Fatal("general request consumed reserved capacity")
	}
	for _, class := range []CapacityClass{CapacityHealth, CapacityStatus, CapacityCancel, CapacityRecovery} {
		for range maximumReservedInFlight {
			release, ok := gate.Acquire(class)
			if !ok {
				t.Fatalf("reserved class %d rejected within two-slot budget", class)
			}
			releases = append(releases, release)
		}
		if _, ok := gate.Acquire(class); ok {
			t.Fatalf("reserved class %d borrowed another class capacity", class)
		}
	}
	for _, release := range releases {
		release()
	}
}

func TestCapacityGateSnapshotAggregatesCanonicalPublicCapacity(t *testing.T) {
	t.Parallel()

	gate := NewCapacityGate()
	general := acquireReadCapacity(t, gate, CapacityGeneral, 3)
	health := acquireReadCapacity(t, gate, CapacityHealth, 2)
	status := acquireReadCapacity(t, gate, CapacityStatus, 1)
	cancel := acquireReadCapacity(t, gate, CapacityCancel, 2)
	recovery := acquireReadCapacity(t, gate, CapacityRecovery, 1)

	snapshot := gate.Snapshot()
	if snapshot != (CapacitySnapshot{
		GeneralInFlight: 3, GeneralLimit: maximumGeneralInFlight,
		ReservedInFlight: 6, ReservedLimit: 4 * maximumReservedInFlight,
	}) {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
	for _, releases := range [][]func(){general, health, status, cancel, recovery} {
		releaseReadCapacity(releases)
	}
	if snapshot := gate.Snapshot(); snapshot.GeneralInFlight != 0 || snapshot.ReservedInFlight != 0 ||
		snapshot.GeneralLimit != 24 || snapshot.ReservedLimit != 8 {
		t.Fatalf("released Snapshot() = %+v", snapshot)
	}
}

func TestCapacityGateSnapshotIsRaceSafeDuringConcurrentAdmission(t *testing.T) {
	t.Parallel()

	gate := NewCapacityGate()
	classes := []CapacityClass{CapacityGeneral, CapacityHealth, CapacityStatus, CapacityCancel, CapacityRecovery}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		class := classes[worker%len(classes)]
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 500 {
				release, admitted := gate.Acquire(class)
				snapshot := gate.Snapshot()
				if snapshot.GeneralLimit != 24 || snapshot.ReservedLimit != 8 ||
					snapshot.GeneralInFlight < 0 || snapshot.GeneralInFlight > snapshot.GeneralLimit ||
					snapshot.ReservedInFlight < 0 || snapshot.ReservedInFlight > snapshot.ReservedLimit {
					t.Errorf("unsafe concurrent Snapshot() = %+v", snapshot)
				}
				if admitted {
					release()
				}
			}
		}()
	}
	close(start)
	workers.Wait()
	if snapshot := gate.Snapshot(); snapshot.GeneralInFlight != 0 || snapshot.ReservedInFlight != 0 {
		t.Fatalf("concurrent releases leaked capacity: %+v", snapshot)
	}
}

func TestAuthHandlerUsesSharedGeneralCapacity(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	capacity := NewCapacityGate()
	releases := make([]func(), 0, maximumGeneralInFlight)
	for range maximumGeneralInFlight {
		release, ok := capacity.Acquire(CapacityGeneral)
		if !ok {
			t.Fatal("failed to saturate general capacity")
		}
		releases = append(releases, release)
	}
	sessions, err := mobileauth.NewSessionManager(mobileauth.SessionConfig{CapabilityVersion: 1, Clock: clock})
	if err != nil {
		t.Fatalf("NewSessionManager(): %v", err)
	}
	handler, err := NewAuthHandler(AuthConfig{PublicOrigin: testPublicOrigin, Capacity: capacity, Audit: acceptingAuthAuditor{}}, &fakeTelegramVerifier{}, sessions, &sequenceIDs{}, clock)
	if err != nil {
		t.Fatalf("NewAuthHandler(): %v", err)
	}
	mux := http.NewServeMux()
	_ = handler.Register(mux)
	if response := serveBootstrap(mux, "capacity-client"); response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("saturated auth status=%d retry=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	releases[0]()
	if response := serveBootstrap(mux, "capacity-client"); response.Code != http.StatusOK {
		t.Fatalf("auth after capacity release status=%d body=%s", response.Code, response.Body.String())
	}
	for _, release := range releases[1:] {
		release()
	}
}

func TestAuthHandlerRequiresAuditAndPinnedVerifierEnvironment(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)}
	sessions, err := mobileauth.NewSessionManager(mobileauth.SessionConfig{CapabilityVersion: 1, Clock: clock})
	if err != nil {
		t.Fatalf("NewSessionManager(): %v", err)
	}
	base := AuthConfig{PublicOrigin: testPublicOrigin, Capacity: NewCapacityGate()}
	if _, err := NewAuthHandler(base, &fakeTelegramVerifier{}, sessions, &sequenceIDs{}, clock); err == nil {
		t.Fatal("handler accepted a missing auth audit sink")
	}
	base.Audit = acceptingAuthAuditor{}
	if _, err := NewAuthHandler(base, &fakeTelegramVerifier{environment: "unknown"}, sessions, &sequenceIDs{}, clock); err == nil {
		t.Fatal("handler accepted an unpinned Telegram environment")
	}
}

func TestAuthenticateRejectsHostAndCookieAmbiguity(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	identity := mobileauth.TelegramIdentity{UserID: 900000000001, AuthDate: clock.Now().Add(time.Second)}
	identity.Fingerprint = [32]byte{1}
	handler := mustAuthHandler(t, clock, &fakeTelegramVerifier{identity: identity}, &sequenceIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)
	bootstrap := responseCookie(t, serveBootstrap(mux, "client-1"), BootstrapCookieName)
	exchange := serve(mux, http.MethodPost, "/api/v1/auth/telegram", "x", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin,
		"Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1",
	})
	sessionCookie := responseCookie(t, exchange, SessionCookieName)

	wrongHost := httptest.NewRequest(http.MethodGet, "https://evil.example/api/v1/bootstrap", nil)
	wrongHost.AddCookie(sessionCookie)
	if _, err := handler.Authenticate(wrongHost); err == nil {
		t.Fatal("wrong Host authenticated")
	}
	ambiguous := httptest.NewRequest(http.MethodGet, testPublicOrigin+"/api/v1/bootstrap", nil)
	ambiguous.AddCookie(sessionCookie)
	ambiguous.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionCookie.Value})
	if _, err := handler.Authenticate(ambiguous); err == nil {
		t.Fatal("duplicate session cookie authenticated")
	}
}

func TestAuthExchangeRejectsDuplicateOriginContentTypeAndBootstrapCookie(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	handler := mustAuthHandler(t, clock, &fakeTelegramVerifier{}, &sequenceIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)
	bootstrap := responseCookie(t, serveBootstrap(mux, "client-1"), BootstrapCookieName)

	tests := []struct {
		name       string
		wantStatus int
		mutate     func(*http.Request)
	}{
		{
			name: "duplicate origin", wantStatus: http.StatusForbidden,
			mutate: func(request *http.Request) { request.Header.Add("Origin", testPublicOrigin) },
		},
		{
			name: "duplicate content type", wantStatus: http.StatusUnsupportedMediaType,
			mutate: func(request *http.Request) { request.Header.Add("Content-Type", "application/x-www-form-urlencoded") },
		},
		{
			name: "duplicate bootstrap cookie", wantStatus: http.StatusUnauthorized,
			mutate: func(request *http.Request) {
				request.AddCookie(&http.Cookie{Name: BootstrapCookieName, Value: bootstrap.Value})
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, testPublicOrigin+"/api/v1/auth/telegram", strings.NewReader("x"))
			request.Header.Set("Origin", testPublicOrigin)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set(clientInstanceHeader, "client-1")
			request.AddCookie(bootstrap)
			test.mutate(request)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestHardenedServerTimeoutContract(t *testing.T) {
	t.Parallel()
	server, err := NewHardenedServer("127.0.0.1:8080", http.NewServeMux())
	if err != nil {
		t.Fatalf("NewHardenedServer(): %v", err)
	}
	if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 10*time.Second || server.WriteTimeout != 15*time.Second || server.IdleTimeout != 60*time.Second || server.MaxHeaderBytes != 16*1024 {
		t.Fatalf("unexpected server boundary: %+v", server)
	}
	for _, address := range []string{"", ":8080", "0.0.0.0:8080", "192.0.2.10:8080", "localhost:8080", "127.0.0.1:0", "127.0.0.1"} {
		if _, err := NewHardenedServer(address, http.NewServeMux()); err == nil {
			t.Errorf("unsafe address %q accepted", address)
		}
	}
	if _, err := NewHardenedServer("[::1]:8080", http.NewServeMux()); err != nil {
		t.Errorf("IPv6 loopback rejected: %v", err)
	}
}

func TestAuthRoutesHardenMethodConfusion(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)}
	verifier := &fakeTelegramVerifier{}
	handler := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)

	for _, test := range []struct {
		path  string
		allow string
	}{
		{path: "/api/v1/auth/bootstrap", allow: http.MethodGet},
		{path: "/api/v1/auth/telegram", allow: http.MethodPost},
		{path: "/api/v1/session", allow: http.MethodDelete},
	} {
		request := httptest.NewRequest(http.MethodOptions, testPublicOrigin+test.path, nil)
		request.Host = "mobile.example.test"
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != test.allow {
			t.Fatalf("%s status=%d allow=%q body=%s", test.path, response.Code, response.Header().Get("Allow"), response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s unsafe method response headers: %v", test.path, response.Header())
		}
		var parsed envelope
		if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil || parsed.Error == nil || parsed.Error.Code != "method_not_allowed" {
			t.Fatalf("%s method response is not a bounded envelope: err=%v body=%s", test.path, err, response.Body.String())
		}
		if len(response.Result().Cookies()) != 0 {
			t.Fatalf("%s method confusion set cookies: %v", test.path, response.Result().Cookies())
		}
	}
	if verifier.calls != 0 {
		t.Fatalf("method confusion reached verifier %d times", verifier.calls)
	}
}
