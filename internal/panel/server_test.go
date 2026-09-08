package panel

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/boxvtk621/homelab-telegram-panel/internal/youtrack"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const origin = "https://panel.example.test"
const testToken = "perm:synthetic-test-token-not-a-credential"

// Production has no skip-TLS option and never reads the bot's credentials.
func setup(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	yt := httptest.NewTLSServer(upstream)
	t.Cleanup(yt.Close)
	// No mutation of the shared/default HTTP transport or TLS verification.
	cfg := Config{Listen: "127.0.0.1:18080", Origin: origin, YouTrackURL: yt.URL, ProjectID: "0-1", ProjectKey: "HL", OwnerLogin: "owner", Writes: true}
	s, err := New(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("web")) }))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(yt.Certificate())
	client, err := youtrack.NewWithRoots(yt.URL, "0-1", "HL", roots)
	if err != nil {
		t.Fatal(err)
	}
	s.client.Close()
	s.client = client
	t.Cleanup(s.Close)
	return s
}

func upstream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/users/me":
		_, _ = fmt.Fprint(w, `{"id":"1-1","login":"owner","name":"Owner"}`)
	case r.URL.Path == "/api/issues/HL-210":
		_, _ = fmt.Fprint(w, `{"id":"2-1","idReadable":"HL-210","project":{"id":"0-1","shortName":"HL"},"summary":"Task","customFields":[]}`)
	case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "POST":
		var in struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "7-1", "text": in.Text, "created": 1000})
	default:
		_, _ = fmt.Fprint(w, `[]`)
	}
}
func request(s *Server, method, payload, path, cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, origin+path, strings.NewReader(payload))
	if method == "POST" {
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
	w := request(s, "POST", `{"token":"`+testToken+`"}`, "/api/v2/login", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	c := w.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatal("weak cookie")
	}
	var v struct{ CSRF string }
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("token leaked")
	}
	return c.Value, v.CSRF
}

func TestIndependentRuntimeAndAuth(t *testing.T) {
	s := setup(t, upstream)
	for path, want := range map[string]int{"/": 200, "/api/v2/healthz": 200, "/api/v2/issues": 401, "/api/v1/dialogs": 401} {
		if w := request(s, "GET", "", path, "", ""); w.Code != want {
			t.Fatal(path, w.Code)
		}
	}
	id, csrf := login(t, s)
	if w := request(s, "GET", "", "/api/v2/issues", id, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(s, "POST", `{}`, "/api/v2/logout", id, "bad"); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if w := request(s, "POST", `{}`, "/api/v2/logout", id, csrf); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "", "/api/v2/session", id, ""); w.Code != 401 {
		t.Fatal("revoked session accepted")
	}
}

func TestPathMountKeepsAuthAndScopesCookies(t *testing.T) {
	s := setup(t, upstream)
	s.cfg.BasePath = "/panel"
	for p, want := range map[string]int{"/panel": 308, "/panel/": 200, "/panel/api/v2/healthz": 200, "/panel/api/v2/issues": 401, "/api/v2/healthz": 404, "/panel-other/": 403, "/panel//api/v2/healthz": 403, "/panel/../api/v2/healthz": 403} {
		w := request(s, "GET", "", p, "", "")
		if w.Code != want {
			t.Fatalf("%s: got %d, want %d", p, w.Code, want)
		}
		if w.Code == 308 && w.Header().Get("Location") != "/panel/" {
			t.Fatal("incorrect mount redirect")
		}
	}
	w := request(s, "POST", `{"token":"`+testToken+`"}`, "/panel/api/v2/login", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	c := w.Result().Cookies()[0]
	if c.Name != "__Secure-panel_session" || c.Path != "/panel/" || !c.Secure || !c.HttpOnly || c.Domain != "" || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("incorrect scoped cookie")
	}
	var v struct{ CSRF string }
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if got := request(s, "GET", "", "/panel/api/v2/session", c.Value, ""); got.Code != 200 {
		t.Fatal("mounted session unavailable")
	}
	if got := request(s, "POST", `{}`, "/panel/api/v2/logout", c.Value, "bad"); got.Code != 403 {
		t.Fatal("mounted CSRF bypass")
	}
	out := request(s, "POST", `{}`, "/panel/api/v2/logout", c.Value, v.CSRF)
	if out.Code != 200 || out.Result().Cookies()[0].Path != "/panel/" || out.Result().Cookies()[0].Name != c.Name || out.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("incorrect mounted logout cookie")
	}
}

func TestCommentSingleUseAndLossless(t *testing.T) {
	var posts atomic.Int32
	s := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		upstream(w, r)
	})
	id, csrf := login(t, s)
	w := request(s, "POST", `{"target":"HL-210/comments"}`, "/api/v2/permits", id, csrf)
	var p struct{ Permit string }
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	for _, bad := range []string{`{"permit":"` + p.Permit + `","text":"\ud800"}`, "{\"permit\":\"" + p.Permit + "\",\"text\":\"\xff\"}", `{"permit":"` + p.Permit + `","text":"one","text":"two"}`} {
		if w := request(s, "POST", bad, "/api/v2/issues/HL-210/comments", id, csrf); w.Code != 400 {
			t.Fatal("malformed text accepted", w.Code, w.Body.String())
		}
	}
	if posts.Load() != 0 {
		t.Fatal("bad text posted")
	}
	body := `{"permit":"` + p.Permit + `","text":"Привет 🌿"}`
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := request(s, "POST", body, "/api/v2/issues/HL-210/comments", id, csrf); w.Code == 201 {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 || posts.Load() != 1 {
		t.Fatal("duplicate comment", accepted.Load(), posts.Load())
	}
	s.sessions.close()
	if w := request(s, "POST", body, "/api/v2/issues/HL-210/comments", id, csrf); w.Code != 401 {
		t.Fatal("restart replay")
	}
}

func TestOutageDoesNotBecomeBadCredentials(t *testing.T) {
	s := setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	w := request(s, "POST", `{"token":"`+testToken+`"}`, "/api/v2/login", "", "")
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestOriginHostScopeAndExpiry(t *testing.T) {
	s := setup(t, upstream)
	id, csrf := login(t, s)
	for _, kind := range []string{"host", "origin", "duplicate_cookie", "foreign_target"} {
		target := "HL-210/comments"
		if kind == "foreign_target" {
			target = "OTHER-1/comments"
		}
		r := httptest.NewRequest("POST", origin+"/api/v2/permits", bytes.NewBufferString(`{"target":"`+target+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Panel-CSRF", csrf)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: id})
		switch kind {
		case "host":
			r.Host = "evil.test"
		case "origin":
			r.Header.Set("Origin", "https://evil.test")
		case "duplicate_cookie":
			r.AddCookie(&http.Cookie{Name: cookieName, Value: id})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal(kind, w.Code)
		}
	}
	now := time.Now()
	s.sessions.now = func() time.Time { return now.Add(31 * time.Minute) }
	if w := request(s, "GET", "", "/api/v2/session", id, ""); w.Code != 401 {
		t.Fatal("idle session survived")
	}
}
