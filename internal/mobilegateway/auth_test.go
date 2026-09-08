package mobilegateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
)

const testPublicOrigin = "https://mobile.example.test"

func TestTelegramExchangeCreatesProtectedSession(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	verifier := &fakeTelegramVerifier{identity: mobileauth.TelegramIdentity{
		UserID: 900000000001, AuthDate: clock.Now().Add(time.Second), Fingerprint: sha256.Sum256([]byte("signed-init-data")),
	}}
	handler := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
	mux := http.NewServeMux()
	if err := handler.Register(mux); err != nil {
		t.Fatalf("Register(): %v", err)
	}

	bootstrapResponse := serveBootstrap(mux, "client-1")
	if bootstrapResponse.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d body=%s", bootstrapResponse.Code, bootstrapResponse.Body.String())
	}
	bootstrap := responseCookie(t, bootstrapResponse, BootstrapCookieName)
	assertProtectedCookie(t, bootstrap)
	if got := bootstrapResponse.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	body := "signed-init-data"
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin, "Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1"}
	exchangeResponse := serve(mux, http.MethodPost, "/api/v1/auth/telegram", body, headers)
	if exchangeResponse.Code != http.StatusOK {
		t.Fatalf("exchange status = %d body=%s", exchangeResponse.Code, exchangeResponse.Body.String())
	}
	if verifier.calls != 1 || verifier.lastRaw != "signed-init-data" {
		t.Fatalf("verifier calls=%d raw=%q", verifier.calls, verifier.lastRaw)
	}
	sessionCookie := responseCookie(t, exchangeResponse, SessionCookieName)
	csrfToken := responseCSRF(t, exchangeResponse)
	assertProtectedCookie(t, sessionCookie)
	if strings.Contains(exchangeResponse.Body.String(), "signed-init-data") || strings.Contains(exchangeResponse.Body.String(), "900000000001") || strings.Contains(exchangeResponse.Body.String(), sessionCookie.Value) || csrfToken == "" {
		t.Fatalf("auth response leaked protected identity or token: %s", exchangeResponse.Body.String())
	}

	authenticatedRequest := httptest.NewRequest(http.MethodGet, testPublicOrigin+"/api/v1/bootstrap", nil)
	authenticatedRequest.AddCookie(sessionCookie)
	session, err := handler.Authenticate(authenticatedRequest)
	if err != nil {
		t.Fatalf("Authenticate(): %v", err)
	}
	if session.Role != "owner" || session.ClientInstanceID != "client-1" {
		t.Fatalf("session = %+v", session)
	}
	authenticatedRequest.Header.Set("Origin", testPublicOrigin)
	authenticatedRequest.Header.Set(CSRFHeaderName, csrfToken)
	if _, err := handler.AuthorizeMutation(authenticatedRequest); err != nil {
		t.Fatalf("AuthorizeMutation(): %v", err)
	}

	replayResponse := serve(mux, http.MethodPost, "/api/v1/auth/telegram", body, headers)
	if replayResponse.Code != http.StatusUnauthorized || verifier.calls != 1 {
		t.Fatalf("bootstrap replay status=%d verifier calls=%d", replayResponse.Code, verifier.calls)
	}
}

func TestAuthenticationFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	verifier := &fakeTelegramVerifier{err: errors.New("signature detail must not escape")}
	handler := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)

	failureBodies := make([]string, 0, 2)
	for index := 0; index < 2; index++ {
		bootstrapResponse := serveBootstrap(mux, "client-1")
		bootstrap := responseCookie(t, bootstrapResponse, BootstrapCookieName)
		headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin, "Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1"}
		response := serve(mux, http.MethodPost, "/api/v1/auth/telegram", "hostile", headers)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("failure status = %d body=%s", response.Code, response.Body.String())
		}
		var parsed envelope
		if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if parsed.Error == nil || parsed.Error.Code != "authentication_failed" || strings.Contains(response.Body.String(), "signature detail") {
			t.Fatalf("unsafe error response: %s", response.Body.String())
		}
		parsed.RequestID = "normalized"
		encoded, _ := json.Marshal(parsed)
		failureBodies = append(failureBodies, string(encoded))
	}
	if failureBodies[0] != failureBodies[1] {
		t.Fatalf("failure envelopes differ:\n%s\n%s", failureBodies[0], failureBodies[1])
	}
}

func TestAuthRequestAndOriginAreStrict(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	verifier := &fakeTelegramVerifier{identity: mobileauth.TelegramIdentity{
		UserID: 900000000001, AuthDate: clock.Now().Add(time.Second), Fingerprint: sha256.Sum256([]byte("strict")),
	}}
	handler := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)
	bootstrapResponse := serveBootstrap(mux, "client-1")
	bootstrap := responseCookie(t, bootstrapResponse, BootstrapCookieName)

	tests := []struct {
		name                  string
		body                  string
		headers               map[string]string
		duplicateClientHeader bool
		wantStatus            int
	}{
		{
			name: "wrong origin", body: "x",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "https://evil.example", "Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1"}, wantStatus: http.StatusForbidden,
		},
		{
			name: "wrong content type", body: "x",
			headers: map[string]string{"Content-Type": "text/plain", "Origin": testPublicOrigin, "Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1"}, wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "missing client header", body: "x",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin, "Cookie": cookieHeader(bootstrap)}, wantStatus: http.StatusBadRequest,
		},
		{
			name: "duplicate client header", body: "x", duplicateClientHeader: true,
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin, "Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1"}, wantStatus: http.StatusBadRequest,
		},
		{
			name: "empty body", body: "",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin, "Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1"}, wantStatus: http.StatusBadRequest,
		},
		{
			name: "oversize body", body: strings.Repeat("x", maximumAuthBody+1),
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin, "Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1"}, wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := serveWith(t, mux, http.MethodPost, "/api/v1/auth/telegram", test.body, test.headers, test.duplicateClientHeader)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestSessionRevokeRequiresOriginAndSessionBoundCSRF(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	verifier := &fakeTelegramVerifier{identity: mobileauth.TelegramIdentity{
		UserID: 900000000001, AuthDate: clock.Now().Add(time.Second), Fingerprint: sha256.Sum256([]byte("revoke")),
	}}
	handler := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)
	bootstrapResponse := serveBootstrap(mux, "client-1")
	bootstrap := responseCookie(t, bootstrapResponse, BootstrapCookieName)
	exchangeHeaders := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin, "Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1"}
	exchange := serve(mux, http.MethodPost, "/api/v1/auth/telegram", "x", exchangeHeaders)
	sessionCookie := responseCookie(t, exchange, SessionCookieName)
	csrfToken := responseCSRF(t, exchange)

	denied := serve(mux, http.MethodDelete, "/api/v1/session", "", map[string]string{"Origin": "https://evil.example", "Cookie": cookieHeader(sessionCookie), CSRFHeaderName: csrfToken})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("wrong-origin revoke status=%d", denied.Code)
	}
	request := httptest.NewRequest(http.MethodGet, testPublicOrigin+"/", nil)
	request.AddCookie(sessionCookie)
	if _, err := handler.Authenticate(request); err != nil {
		t.Fatalf("wrong-origin request revoked session: %v", err)
	}

	missingCSRF := serve(mux, http.MethodDelete, "/api/v1/session", "", map[string]string{"Origin": testPublicOrigin, "Cookie": cookieHeader(sessionCookie)})
	if missingCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("missing-CSRF revoke status=%d", missingCSRF.Code)
	}
	response := serve(mux, http.MethodDelete, "/api/v1/session", "", map[string]string{"Origin": testPublicOrigin, "Cookie": cookieHeader(sessionCookie), CSRFHeaderName: csrfToken})
	if response.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := handler.Authenticate(request); err == nil {
		t.Fatal("revoked session authenticated")
	}
}

func TestRequestIdentityFailureStopsBeforeBootstrap(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
	handler := mustAuthHandler(t, clock, &fakeTelegramVerifier{}, failingIDs{})
	mux := http.NewServeMux()
	_ = handler.Register(mux)
	response := serveBootstrap(mux, "client-1")
	if response.Code != http.StatusServiceUnavailable || len(response.Result().Cookies()) != 0 {
		t.Fatalf("status=%d cookies=%v body=%s", response.Code, response.Result().Cookies(), response.Body.String())
	}
}

func TestBootstrapRejectsQueryAndBodyAmbiguityWithoutReplacingState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{
			name: "query",
			mutate: func(request *http.Request) {
				request.URL.RawQuery = "debug=1"
			},
		},
		{
			name: "empty query marker",
			mutate: func(request *http.Request) {
				request.URL.ForceQuery = true
			},
		},
		{
			name: "body",
			mutate: func(request *http.Request) {
				request.Body = io.NopCloser(strings.NewReader("hostile"))
				request.ContentLength = int64(len("hostile"))
			},
		},
		{
			name: "transfer encoding",
			mutate: func(request *http.Request) {
				request.TransferEncoding = []string{"chunked"}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
			verifier := &fakeTelegramVerifier{identity: mobileauth.TelegramIdentity{
				UserID: 900000000001, AuthDate: clock.Now().Add(time.Second), Fingerprint: sha256.Sum256([]byte(test.name)),
			}}
			handler := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
			mux := http.NewServeMux()
			_ = handler.Register(mux)
			original := responseCookie(t, serveBootstrap(mux, "client-1"), BootstrapCookieName)

			request := httptest.NewRequest(http.MethodGet, testPublicOrigin+"/api/v1/auth/bootstrap", nil)
			request.Header.Set(clientInstanceHeader, "client-1")
			test.mutate(request)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			assertGenericInvalidRequest(t, response)
			if len(response.Result().Cookies()) != 0 || verifier.calls != 0 {
				t.Fatalf("ambiguous bootstrap cookies/verifier calls = %v/%d", response.Result().Cookies(), verifier.calls)
			}

			exchange := serve(mux, http.MethodPost, "/api/v1/auth/telegram", "signed", map[string]string{
				"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin,
				"Cookie": cookieHeader(original), clientInstanceHeader: "client-1",
			})
			if exchange.Code != http.StatusOK || verifier.calls != 1 {
				t.Fatalf("original bootstrap was replaced: status=%d verifier=%d body=%s", exchange.Code, verifier.calls, exchange.Body.String())
			}
		})
	}
}

func TestExchangeRejectsQueryAmbiguityWithoutConsumingBootstrapOrDispatching(t *testing.T) {
	t.Parallel()

	for _, mutate := range []func(*http.Request){
		func(request *http.Request) { request.URL.RawQuery = "debug=1" },
		func(request *http.Request) { request.URL.ForceQuery = true },
	} {
		clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
		verifier := &fakeTelegramVerifier{identity: mobileauth.TelegramIdentity{
			UserID: 900000000001, AuthDate: clock.Now().Add(time.Second), Fingerprint: sha256.Sum256([]byte("query ambiguity")),
		}}
		handler := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
		mux := http.NewServeMux()
		_ = handler.Register(mux)
		bootstrap := responseCookie(t, serveBootstrap(mux, "client-1"), BootstrapCookieName)

		request := httptest.NewRequest(http.MethodPost, testPublicOrigin+"/api/v1/auth/telegram", strings.NewReader("signed"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", testPublicOrigin)
		request.Header.Set(clientInstanceHeader, "client-1")
		request.AddCookie(bootstrap)
		mutate(request)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		assertGenericInvalidRequest(t, response)
		if verifier.calls != 0 || handler.sessions.ActiveCount() != 0 || len(response.Result().Cookies()) != 0 {
			t.Fatalf("ambiguous exchange verifier/sessions/cookies = %d/%d/%v", verifier.calls, handler.sessions.ActiveCount(), response.Result().Cookies())
		}

		retry := serve(mux, http.MethodPost, "/api/v1/auth/telegram", "signed", map[string]string{
			"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin,
			"Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1",
		})
		if retry.Code != http.StatusOK || verifier.calls != 1 || handler.sessions.ActiveCount() != 1 {
			t.Fatalf("bootstrap was consumed before dispatch: status=%d verifier=%d sessions=%d body=%s", retry.Code, verifier.calls, handler.sessions.ActiveCount(), retry.Body.String())
		}
	}
}

func TestRevokeRejectsQueryAndBodyAmbiguityWithoutRevokingSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "query", mutate: func(request *http.Request) { request.URL.RawQuery = "debug=1" }},
		{name: "empty query marker", mutate: func(request *http.Request) { request.URL.ForceQuery = true }},
		{name: "body", mutate: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader("hostile"))
			request.ContentLength = int64(len("hostile"))
		}},
		{name: "transfer encoding", mutate: func(request *http.Request) { request.TransferEncoding = []string{"chunked"} }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := &gatewayClock{now: time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)}
			verifier := &fakeTelegramVerifier{identity: mobileauth.TelegramIdentity{
				UserID: 900000000001, AuthDate: clock.Now().Add(time.Second), Fingerprint: sha256.Sum256([]byte("revoke " + test.name)),
			}}
			handler := mustAuthHandler(t, clock, verifier, &sequenceIDs{})
			mux := http.NewServeMux()
			_ = handler.Register(mux)
			bootstrap := responseCookie(t, serveBootstrap(mux, "client-1"), BootstrapCookieName)
			exchange := serve(mux, http.MethodPost, "/api/v1/auth/telegram", "signed", map[string]string{
				"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin,
				"Cookie": cookieHeader(bootstrap), clientInstanceHeader: "client-1",
			})
			sessionCookie := responseCookie(t, exchange, SessionCookieName)
			csrfToken := responseCSRF(t, exchange)

			request := httptest.NewRequest(http.MethodDelete, testPublicOrigin+"/api/v1/session", nil)
			request.Header.Set("Origin", testPublicOrigin)
			request.Header.Set(CSRFHeaderName, csrfToken)
			request.AddCookie(sessionCookie)
			test.mutate(request)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			assertGenericInvalidRequest(t, response)
			if len(response.Result().Cookies()) != 0 || handler.sessions.ActiveCount() != 1 {
				t.Fatalf("ambiguous revoke cookies/session count = %v/%d", response.Result().Cookies(), handler.sessions.ActiveCount())
			}
			authenticate := httptest.NewRequest(http.MethodGet, testPublicOrigin+"/", nil)
			authenticate.AddCookie(sessionCookie)
			if _, err := handler.Authenticate(authenticate); err != nil {
				t.Fatalf("ambiguous revoke mutated session: %v", err)
			}
		})
	}
}

func assertGenericInvalidRequest(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	var parsed envelope
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil || parsed.Error == nil ||
		parsed.Error.Code != "invalid_request" || parsed.Error.Message != "request could not be completed" {
		t.Fatalf("response is not generic invalid_request: err=%v body=%s", err, response.Body.String())
	}
}

type fakeTelegramVerifier struct {
	mu          sync.Mutex
	identity    mobileauth.TelegramIdentity
	err         error
	environment mobileauth.Environment
	calls       int
	lastRaw     string
}

func (verifier *fakeTelegramVerifier) SignatureEnvironment() mobileauth.Environment {
	if verifier.environment == "" {
		return mobileauth.EnvironmentProduction
	}
	return verifier.environment
}

func (verifier *fakeTelegramVerifier) Verify(raw string) (mobileauth.TelegramIdentity, error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.calls++
	verifier.lastRaw = raw
	return verifier.identity, verifier.err
}

type sequenceIDs struct {
	mu   sync.Mutex
	next int
}

func (ids *sequenceIDs) New(prefix string) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return prefix + "-test-" + strconv.Itoa(ids.next), nil
}

type failingIDs struct{}

func (failingIDs) New(string) (string, error) { return "", io.ErrUnexpectedEOF }

type acceptingAuthAuditor struct{}

func (acceptingAuthAuditor) Record(context.Context, AuthAuditRecord) error { return nil }

type gatewayClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *gatewayClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func mustAuthHandler(t *testing.T, clock mobileauth.Clock, verifier TelegramVerifier, ids IDGenerator) *AuthHandler {
	t.Helper()
	sessions, err := mobileauth.NewSessionManager(mobileauth.SessionConfig{
		CapabilityVersion: 1, Clock: clock,
	})
	if err != nil {
		t.Fatalf("NewSessionManager(): %v", err)
	}
	handler, err := NewAuthHandler(AuthConfig{PublicOrigin: testPublicOrigin, Capacity: NewCapacityGate(), Audit: acceptingAuthAuditor{}}, verifier, sessions, ids, clock)
	if err != nil {
		t.Fatalf("NewAuthHandler(): %v", err)
	}
	return handler
}

func serve(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	return serveWith(nil, handler, method, path, body, headers, false)
}

func serveBootstrap(handler http.Handler, clientInstanceID string) *httptest.ResponseRecorder {
	return serve(handler, http.MethodGet, "/api/v1/auth/bootstrap", "", map[string]string{clientInstanceHeader: clientInstanceID})
}

func serveWith(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string, duplicateClientHeader bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, testPublicOrigin+path, bytes.NewBufferString(body))
	request.Host = "mobile.example.test"
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if duplicateClientHeader {
		request.Header.Add(clientInstanceHeader, "client-2")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func responseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %s not found in %v", name, response.Result().Cookies())
	return nil
}

func assertProtectedCookie(t *testing.T, cookie *http.Cookie) {
	t.Helper()
	if cookie.Value == "" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
}

func responseCSRF(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var parsed envelope
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode auth envelope: %v", err)
	}
	value, ok := parsed.Data["csrf_token"].(string)
	if !ok || value == "" {
		t.Fatalf("CSRF token missing from auth envelope: %s", response.Body.String())
	}
	return value
}

func cookieHeader(cookie *http.Cookie) string {
	return cookie.Name + "=" + cookie.Value
}
