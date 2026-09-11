package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const origin = "https://panel.example.test"

func setup(t *testing.T, _ http.HandlerFunc) *Server {
	t.Helper()
	cfg := Config{Listen: "127.0.0.1:18080", Origin: origin, OwnerID: "1-1", HarnessCommands: true}
	server, err := New(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("web"))
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server
}

func upstream(http.ResponseWriter, *http.Request) {}

func request(s *Server, method, payload, requestPath, cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, origin+requestPath, strings.NewReader(payload))
	if method == http.MethodPost {
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: cookie})
	}
	if csrf != "" {
		r.Header.Set("X-Panel-CSRF", csrf)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, s *Server) (string, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, origin+s.cfg.BasePath+"/api/v2/bootstrap", strings.NewReader(`{}`))
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(edgeUserHeader, "owner")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != s.cfg.BasePath+"/" || cookie.Domain != "" {
		t.Fatal("weak cookie")
	}
	var value struct {
		User struct {
			ID, Login, Name string
		}
		CSRF string
	}
	if json.Unmarshal(w.Body.Bytes(), &value) != nil || value.User.ID != "1-1" || value.User.Login != "owner" || value.User.Name != "owner" || len(value.CSRF) != 43 {
		t.Fatal("invalid session response", w.Body.String())
	}
	return cookie.Value, value.CSRF
}

func TestHarnessPanelBootstrapRequiresTrustedEdge(t *testing.T) {
	s := setup(t, upstream)
	for requestPath, want := range map[string]int{
		"/":                     http.StatusOK,
		"/api/v2/healthz":       http.StatusOK,
		"/api/v2/session":       http.StatusUnauthorized,
		"/api/v2/harness/nodes": http.StatusUnauthorized,
	} {
		if got := request(s, http.MethodGet, "", requestPath, "", ""); got.Code != want {
			t.Fatalf("%s: got %d, want %d", requestPath, got.Code, want)
		}
	}
	if got := request(s, http.MethodPost, `{}`, "/api/v2/bootstrap", "", ""); got.Code != http.StatusForbidden {
		t.Fatal("bootstrap accepted without trusted edge", got.Code, got.Body.String())
	}
	cookie, csrf := login(t, s)
	if got := request(s, http.MethodGet, "", "/api/v2/session", cookie, ""); got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body.String())
	}
	for _, requestPath := range []string{"/api/v2/issues", "/api/v2/articles", "/api/v2/agent/runs", "/api/v2/login"} {
		if got := request(s, http.MethodGet, "", requestPath, cookie, ""); got.Code != http.StatusNotFound {
			t.Fatalf("legacy route %s remains exposed: %d", requestPath, got.Code)
		}
	}
	if got := request(s, http.MethodPost, `{}`, "/api/v2/logout", cookie, "bad"); got.Code != http.StatusForbidden {
		t.Fatal("missing CSRF accepted")
	}
	if got := request(s, http.MethodPost, `{}`, "/api/v2/logout", cookie, csrf); got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body.String())
	}
	if got := request(s, http.MethodGet, "", "/api/v2/session", cookie, ""); got.Code != http.StatusUnauthorized {
		t.Fatal("revoked session accepted")
	}
}

func TestPathMountScopesOwnerSession(t *testing.T) {
	s := setup(t, upstream)
	s.cfg.BasePath = "/panel"
	for requestPath, want := range map[string]int{
		"/panel":                   http.StatusPermanentRedirect,
		"/panel/":                  http.StatusOK,
		"/panel/api/v2/healthz":    http.StatusOK,
		"/panel/api/v2/session":    http.StatusUnauthorized,
		"/api/v2/healthz":          http.StatusNotFound,
		"/panel-other/":            http.StatusForbidden,
		"/panel//api/v2/healthz":   http.StatusForbidden,
		"/panel/../api/v2/healthz": http.StatusForbidden,
	} {
		got := request(s, http.MethodGet, "", requestPath, "", "")
		if got.Code != want {
			t.Fatalf("%s: got %d, want %d", requestPath, got.Code, want)
		}
	}
	cookie, csrf := login(t, s)
	if cookie == "" || csrf == "" {
		t.Fatal("mounted session unavailable")
	}
	response := request(s, http.MethodPost, `{}`, "/panel/api/v2/logout", cookie, csrf)
	cleared := response.Result().Cookies()[0]
	if response.Code != http.StatusOK || cleared.Name != "__Secure-panel_session" || cleared.Path != "/panel/" || cleared.MaxAge != -1 {
		t.Fatal("incorrect mounted logout cookie")
	}
}

func TestOwnerIdentityMustMatchSignedRegistry(t *testing.T) {
	cfg := Config{Listen: "127.0.0.1:18080", Origin: origin}
	if _, err := New(cfg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
		t.Fatal("ownerless Panel started")
	}
}
