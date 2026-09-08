package mobilegateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
)

func FuzzAuthHTTPBoundary(f *testing.F) {
	seeds := []struct {
		route       uint8
		method      string
		host        string
		origin      string
		contentType string
		client      string
		query       string
		body        []byte
	}{
		{0, http.MethodGet, "mobile.example.test", "", "", "seed-client", "", nil},
		{1, http.MethodPost, "mobile.example.test", testPublicOrigin, "application/x-www-form-urlencoded", "seed-client", "", []byte("auth_date=1&signature=x&user=x")},
		{2, http.MethodDelete, "mobile.example.test", testPublicOrigin, "", "", "", nil},
		{0, http.MethodOptions, "evil.example.test", "https://evil.example.test", "text/plain", "bad client", "initData=forbidden", []byte("hostile")},
	}
	for _, seed := range seeds {
		f.Add(seed.route, seed.method, seed.host, seed.origin, seed.contentType, seed.client, seed.query, seed.body)
	}

	f.Fuzz(func(t *testing.T, route uint8, method, host, origin, contentType, client, query string, body []byte) {
		if len(method) > 32 || len(host) > 256 || len(origin) > 512 || len(contentType) > 256 || len(client) > 128 || len(query) > 2048 || len(body) > maximumAuthBody+1 {
			return
		}
		clock := mobileauth.ClockFunc(func() time.Time {
			return time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC)
		})
		sessions, err := mobileauth.NewSessionManager(mobileauth.SessionConfig{CapabilityVersion: 1, Clock: clock})
		if err != nil {
			t.Fatalf("NewSessionManager(): %v", err)
		}
		verifier := &fakeTelegramVerifier{err: errFuzzAuthentication}
		handler, err := NewAuthHandler(AuthConfig{
			PublicOrigin: testPublicOrigin, Capacity: NewCapacityGate(), Audit: acceptingAuthAuditor{},
		}, verifier, sessions, &sequenceIDs{}, clock)
		if err != nil {
			t.Fatalf("NewAuthHandler(): %v", err)
		}
		mux := http.NewServeMux()
		if err := handler.Register(mux); err != nil {
			t.Fatalf("Register(): %v", err)
		}

		paths := [...]string{"/api/v1/auth/bootstrap", "/api/v1/auth/telegram", "/api/v1/session"}
		path := paths[int(route)%len(paths)]
		request := httptest.NewRequest(http.MethodGet, testPublicOrigin+path, bytes.NewReader(body))
		request.Method = method
		request.Host = host
		request.URL.RawQuery = query
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		if client != "" {
			request.Header.Set(clientInstanceHeader, client)
		}
		if path == "/api/v1/auth/telegram" && mobileauth.ValidClientInstanceID(client) {
			bootstrap, _, issueErr := sessions.IssueBootstrap(client)
			if issueErr == nil {
				request.AddCookie(&http.Cookie{Name: BootstrapCookieName, Value: bootstrap})
			}
		}

		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code < 200 || response.Code > 599 {
			t.Fatalf("unbounded HTTP status=%d method=%q path=%q body=%s", response.Code, method, path, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("unsafe boundary headers: %v", response.Header())
		}
		if !json.Valid(response.Body.Bytes()) {
			t.Fatalf("non-JSON boundary response: %q", response.Body.String())
		}
		if len(body) >= 32 && strings.Contains(response.Body.String(), string(body)) {
			t.Fatalf("request body reflected into response")
		}
	})
}

var errFuzzAuthentication = &fuzzAuthenticationError{}

type fuzzAuthenticationError struct{}

func (*fuzzAuthenticationError) Error() string { return "synthetic authentication rejection" }
