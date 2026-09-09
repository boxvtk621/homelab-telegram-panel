package panel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

const harnessNode = "20000000-0000-4000-8000-000000000001"
const harnessPath = "/api/v2/harness/nodes/" + harnessNode

func harnessFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../api/harness-v1.fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var all struct {
		Fixtures []struct {
			Name, WireType string
			Value          json.RawMessage
		}
	}
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	for _, f := range all.Fixtures {
		if f.Name == name {
			if err := hp.Validate(f.WireType, f.Value); err != nil {
				t.Fatal(err)
			}
			return f.Value
		}
	}
	t.Fatal("missing fixture", name)
	return nil
}

// Panel tests exercise browser auth and HTTP lifetimes against a private TLS
// fixture. harnessclient tests separately require and verify mutual TLS.
func setupHarness(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	node := httptest.NewTLSServer(handler)
	t.Cleanup(node.Close)
	s := setup(t, upstream)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(node.Certificate().Raw)
	m := harnessclient.Manifest{RegistryVersion: 1, OwnerID: "1-1", Mode: "fixture", Nodes: []harnessclient.Node{{NodeID: harnessNode, Name: "Synthetic node", Adapter: "cursor", URL: node.URL, CertificateSHA256: hex.EncodeToString(pin[:])}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(harnessclient.SignedManifest{Manifest: m, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, b))})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(node.Certificate())
	c, err := harnessclient.New(b, pub, roots, node.TLS.Certificates[0])
	if err != nil {
		t.Fatal(err)
	}
	s.harness.Close()
	s.harness = c
	return s
}

func TestHarnessUsesExistingAuthAndTrustedActor(t *testing.T) {
	id, receipt, command := harnessFixture(t, "read.identity"), harnessFixture(t, "receipt.2.message.enqueue"), harnessFixture(t, "command.2.message.enqueue")
	var calls, posts atomic.Int32
	s := setupHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Harness-Actor-ID") != "1-1" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Panel-CSRF") != "" {
			t.Error("untrusted actor or browser credential forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			posts.Add(1)
			w.WriteHeader(202)
			_, _ = w.Write(receipt)
		} else {
			_, _ = w.Write(id)
		}
	})
	cookie, csrf := login(t, s)
	for _, tc := range []struct {
		name, cookie, csrf string
		want               int
	}{{"no session", "", "", 401}, {"no CSRF", cookie, "", 403}, {"bad CSRF", cookie, "bad", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			if w := request(s, "POST", string(command), harnessPath+"/commands", tc.cookie, tc.csrf); w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	r := httptest.NewRequest("POST", origin+harnessPath+"/commands", strings.NewReader(string(command)))
	r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: cookie})
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Panel-CSRF", csrf)
	r.Header.Set("Origin", "https://foreign.invalid")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	s.cfg.Writes = false
	if w := request(s, "POST", string(command), harnessPath+"/commands", cookie, csrf); w.Code != 403 {
		t.Fatal("read-only admitted command")
	}
	s.cfg.Writes = true
	if calls.Load() != 0 {
		t.Fatal("rejected browser request reached node")
	}
	r = httptest.NewRequest("POST", origin+harnessPath+"/commands", strings.NewReader(string(command)))
	r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: cookie})
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	r.Header.Set("X-Panel-CSRF", csrf)
	r.Header.Set("X-Harness-Actor-ID", "foreign")
	r.Header.Set("Authorization", "Bearer synthetic-browser-token")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 202 || posts.Load() != 1 {
		t.Fatal("valid exact command", w.Code, posts.Load(), w.Body.String())
	}
	registry := request(s, "GET", "", "/api/v2/harness/nodes", cookie, "")
	if registry.Code != 200 || !strings.Contains(registry.Body.String(), `"mode":"fixture"`) || strings.Contains(registry.Body.String(), `"url"`) {
		t.Fatal("public registry", registry.Body.String())
	}
	// A previously authenticated identity cannot acquire another owner's nodes.
	s.sessions.mu.Lock()
	entry := s.sessions.entries[digest(cookie)]
	entry.user.ID = "foreign"
	s.sessions.entries[digest(cookie)] = entry
	s.sessions.mu.Unlock()
	before := calls.Load()
	if w := request(s, "GET", "", harnessPath+"/identity", cookie, ""); w.Code != 404 {
		t.Fatal("foreign object visible", w.Code)
	}
	if calls.Load() != before {
		t.Fatal("foreign owner reached node")
	}
}

func TestHarnessPollingDoesNotRenewIdleSession(t *testing.T) {
	s := setup(t, upstream)
	now := time.Now()
	s.sessions.now = func() time.Time { return now }
	cookie, _ := login(t, s)
	for i := 1; i <= 29; i++ {
		now = now.Add(time.Minute)
		if w := request(s, "GET", "", "/api/v2/harness/nodes", cookie, ""); w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	now = now.Add(time.Minute)
	if w := request(s, "GET", "", "/api/v2/harness/nodes", cookie, ""); w.Code != 401 {
		t.Fatal("polling extended idle TTL", w.Code)
	}
}

func TestHarnessStreamsLeaveControlCapacityAndLogoutClosesTransportOnly(t *testing.T) {
	id, receipt, resume := harnessFixture(t, "read.identity"), harnessFixture(t, "receipt.6.queue.resume"), harnessFixture(t, "command.6.queue.resume")
	closed := make(chan struct{}, 4)
	var posts atomic.Int32
	executing := atomic.Bool{}
	executing.Store(true)
	s := setupHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": online\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			closed <- struct{}{}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			posts.Add(1)
			b, _ := io.ReadAll(r.Body)
			if string(b) != string(resume) {
				executing.Store(false)
				t.Error("unexpected execution command")
			}
			w.WriteHeader(202)
			_, _ = w.Write(receipt)
		} else {
			_, _ = w.Write(id)
		}
	})
	cookie, csrf := login(t, s)
	web := httptest.NewServer(s)
	t.Cleanup(web.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var bodies []io.ReadCloser
	for i := 0; i < 4; i++ {
		r, err := http.NewRequestWithContext(ctx, "GET", web.URL+harnessPath+"/events?after=0", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = "panel.example.test"
		r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: cookie})
		resp, err := web.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, resp.Body)
		defer resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal(resp.StatusCode)
		}
	}
	if w := request(s, "GET", "", harnessPath+"/events?after=0", cookie, ""); w.Code != 503 {
		t.Fatal("stream budget not enforced", w.Code)
	}
	for i := 0; i < cap(s.general); i++ {
		s.general <- struct{}{}
	}
	defer func() {
		for len(s.general) > 0 {
			<-s.general
		}
	}()
	if w := request(s, "GET", "", harnessPath+"/snapshot", cookie, ""); w.Code != 503 {
		t.Fatal("read budget not enforced", w.Code)
	}
	if w := request(s, "POST", string(resume), harnessPath+"/commands", cookie, csrf); w.Code != 202 {
		t.Fatal("control starved", w.Code, w.Body.String())
	}
	for i := 0; i < cap(s.control); i++ {
		s.control <- struct{}{}
	}
	defer func() {
		for len(s.control) > 0 {
			<-s.control
		}
	}()
	if w := request(s, "POST", `{}`, "/api/v2/logout", cookie, csrf); w.Code != 200 {
		t.Fatal("logout starved", w.Code)
	}
	for _, body := range bodies {
		done := make(chan error, 1)
		go func(body io.Reader) { _, err := io.Copy(io.Discard, body); done <- err }(body)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("revoked browser stream remained open")
		}
	}
	for i := 0; i < 4; i++ {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("upstream transport not cancelled")
		}
	}
	if !executing.Load() || posts.Load() != 1 {
		t.Fatal("logout issued an execution command", posts.Load())
	}
	if w := request(s, "GET", "", harnessPath+"/identity", cookie, ""); w.Code != 401 {
		t.Fatal("revoked session accepted")
	}
}

type stalledHarnessBody struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (b stalledHarnessBody) Read(_ []byte) (int, error) {
	b.entered <- struct{}{}
	<-b.release
	return 0, io.EOF
}

func TestHarnessBodyAdmissionBoundedBeforeRead(t *testing.T) {
	s := setup(t, upstream)
	cookie, csrf := login(t, s)
	entered := make(chan struct{}, cap(s.commandBodies)+1)
	release := make(chan struct{})
	done := make(chan int, cap(s.commandBodies))
	defer func() {
		close(release)
		for i := 0; i < cap(s.commandBodies); i++ {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("body handler did not exit")
			}
		}
	}()
	makeRequest := func(body io.Reader) *http.Request {
		r := httptest.NewRequest("POST", origin+harnessPath+"/commands", body)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Panel-CSRF", csrf)
		r.AddCookie(&http.Cookie{Name: s.sessionCookieName(), Value: cookie})
		return r
	}
	for i := 0; i < cap(s.commandBodies); i++ {
		go func() {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, makeRequest(stalledHarnessBody{entered, release}))
			done <- w.Code
		}()
	}
	for i := 0; i < cap(s.commandBodies); i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("body read not started")
		}
	}
	// If this request touches its body, it blocks and the test's bounded wait fails.
	extra := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, makeRequest(stalledHarnessBody{entered, release}))
		extra <- w.Code
	}()
	select {
	case status := <-extra:
		if status != 503 {
			t.Fatal("unbounded body admission", status)
		}
	case <-time.After(time.Second):
		t.Fatal("read happened before capacity gate")
	}
	if w := request(s, "POST", `{}`, "/api/v2/logout", cookie, csrf); w.Code != 200 {
		t.Fatal("slow bodies starved revocation")
	}
}
