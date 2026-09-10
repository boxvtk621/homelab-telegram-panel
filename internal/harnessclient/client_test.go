package harnessclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

const testNode = "20000000-0000-4000-8000-000000000001"
const testOwner = "1-1"

func fixture(t *testing.T, name string) []byte {
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
				t.Fatal(name, err)
			}
			return f.Value
		}
	}
	t.Fatal("missing fixture", name)
	return nil
}

func signedBytes(t *testing.T, m Manifest, key ed25519.PrivateKey) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(SignedManifest{Manifest: m, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, b))})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Each fake node has a separate CA, server leaf and client leaf. The server
// requires a verified client certificate; tests never disable TLS verification.
type testRig struct {
	client   *Client
	server   *httptest.Server
	manifest Manifest
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	roots    *x509.CertPool
	cert     tls.Certificate
}

func newRig(t *testing.T, handler http.HandlerFunc) *testRig {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	leaf := func(serial int64, usage x509.ExtKeyUsage) tls.Certificate {
		p, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		x := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		d, err := x509.CreateCertificate(rand.Reader, x, parsed, p, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{d}, PrivateKey: k}
	}
	serverCert, clientCert := leaf(2, x509.ExtKeyUsageServerAuth), leaf(3, x509.ExtKeyUsageClientAuth)
	s := httptest.NewUnstartedServer(handler)
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	s.StartTLS()
	t.Cleanup(s.Close)
	pin := sha256.Sum256(serverCert.Certificate[0])
	m := Manifest{RegistryVersion: 1, OwnerID: testOwner, Mode: "fixture", Nodes: []Node{{NodeID: testNode, Name: "Synthetic Cursor", Adapter: "cursor", URL: s.URL, CertificateSHA256: hex.EncodeToString(pin[:])}}}
	c, err := New(signedBytes(t, m, key), pub, roots, clientCert)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return &testRig{client: c, server: s, manifest: m, pub: pub, priv: key, roots: roots, cert: clientCert}
}

func requireFault(t *testing.T, err error, status int, code string) {
	t.Helper()
	var f *Fault
	if !errors.As(err, &f) || f.Status != status || f.Code != code {
		t.Fatalf("fault: %v, want %d %s", err, status, code)
	}
}

func TestSignedRegistryAndMutualTLS(t *testing.T) {
	id := fixture(t, "read.identity")
	var calls atomic.Int32
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || r.Header.Get("X-Harness-Actor-ID") != testOwner {
			t.Error("missing authenticated caller")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(id)
	})
	got, err := rig.client.Read(context.Background(), testNode, testOwner, "identity", "")
	if err != nil || got.Status != 200 {
		t.Fatal(got.Status, err)
	}
	pub, ok := rig.client.Public(testOwner)
	b, _ := json.Marshal(pub)
	if !ok || pub.Mode != "fixture" || strings.Contains(string(b), rig.server.URL) || strings.Contains(string(b), "certificate") {
		t.Fatal("public registry exposes private routing")
	}
	if _, ok := rig.client.Public("foreign"); ok {
		t.Fatal("foreign owner saw registry")
	}
	routing := rig.client.RoutingRegistry()
	canonical, _ := json.Marshal(rig.manifest)
	digest := sha256.Sum256(canonical)
	routingJSON, _ := json.Marshal(routing)
	if routing.ManifestSHA256 != hex.EncodeToString(digest[:]) || routing.OwnerID != testOwner || len(routing.Nodes) != 1 ||
		strings.Contains(string(routingJSON), rig.server.URL) || strings.Contains(string(routingJSON), rig.manifest.Nodes[0].CertificateSHA256) {
		t.Fatal("private routing identity is incomplete or exposes transport trust")
	}
	_, err = rig.client.Read(context.Background(), testNode, "foreign", "identity", "")
	requireFault(t, err, 404, "not_found")
	if calls.Load() != 1 {
		t.Fatal("foreign owner reached node")
	}
	original := string(signedBytes(t, rig.manifest, rig.priv))
	for _, field := range []string{"manifest", "signature", "ownerId", "nodeId", "certificateSHA256"} {
		t.Run("exact key "+field, func(t *testing.T) {
			raw := []byte(strings.Replace(original, `"`+field+`":`, `"`+strings.ToUpper(field)+`":`, 1))
			if c, err := New(raw, rig.pub, rig.roots, rig.cert); err == nil {
				c.Close()
				t.Error("case-insensitive registry key accepted")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*Manifest)
		resign bool
	}{
		{"unsigned URL mutation", func(m *Manifest) { m.Nodes[0].URL = "https://other.invalid" }, false},
		{"duplicate node", func(m *Manifest) { m.Nodes = append(m.Nodes, m.Nodes[0]) }, true},
		{"query URL", func(m *Manifest) { m.Nodes[0].URL += "?actor=foreign" }, true},
		{"missing owner", func(m *Manifest) { m.OwnerID = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := rig.manifest
			m.Nodes = append([]Node(nil), m.Nodes...)
			original := signedBytes(t, m, rig.priv)
			tc.change(&m)
			var raw []byte
			if tc.resign {
				raw = signedBytes(t, m, rig.priv)
			} else {
				var s SignedManifest
				_ = json.Unmarshal(original, &s)
				s.Manifest = m
				raw, _ = json.Marshal(s)
			}
			if c, e := New(raw, rig.pub, rig.roots, rig.cert); e == nil {
				c.Close()
				t.Fatal("invalid registry accepted")
			}
		})
	}
	for _, tc := range []struct {
		name  string
		roots *x509.CertPool
		pin   string
	}{
		{"wrong leaf", rig.roots, strings.Repeat("0", 64)},
		{"untrusted CA", x509.NewCertPool(), rig.manifest.Nodes[0].CertificateSHA256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := rig.manifest
			m.Nodes = append([]Node(nil), m.Nodes...)
			m.Nodes[0].CertificateSHA256 = tc.pin
			c, e := New(signedBytes(t, m, rig.priv), rig.pub, tc.roots, rig.cert)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			_, e = c.Read(context.Background(), testNode, testOwner, "identity", "")
			requireFault(t, e, 503, "node_unavailable")
		})
	}
	if calls.Load() != 1 {
		t.Fatal("untrusted TLS reached HTTP handler")
	}
}

func TestIdentityMismatchPreventsCommand(t *testing.T) {
	command := fixture(t, "command.2.message.enqueue")
	for _, field := range []string{"nodeId", "registryVersion", "schemaSHA256", "adapter"} {
		t.Run(field, func(t *testing.T) {
			var id map[string]any
			_ = json.Unmarshal(fixture(t, "read.identity"), &id)
			switch field {
			case "nodeId":
				id[field] = "20000000-0000-4000-8000-000000000002"
			case "registryVersion":
				id[field] = 2
			case "schemaSHA256":
				id[field] = strings.Repeat("0", 64)
			case "adapter":
				id[field] = map[string]string{"kind": "codex", "version": "0.153.4"}
			}
			var posts atomic.Int32
			rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(id)
			})
			_, err := rig.client.Command(context.Background(), testNode, testOwner, command)
			requireFault(t, err, 409, "schema_mismatch")
			if posts.Load() != 0 {
				t.Fatal("POST before identity verified")
			}
		})
	}
}

func TestDurableIdentityFencePreventsCommandAfterNodeRestart(t *testing.T) {
	identityBody := fixture(t, "read.identity")
	command := fixture(t, "command.2.message.enqueue")
	var live hp.NodeIdentity
	if err := json.Unmarshal(identityBody, &live); err != nil {
		t.Fatal(err)
	}
	expected := live
	expected.IdentityEpoch--
	var posts atomic.Int32
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = w.Write(identityBody)
	})
	_, err := rig.client.CommandFenced(context.Background(), testNode, testOwner, command, expected)
	requireFault(t, err, 409, "stale")
	if posts.Load() != 0 {
		t.Fatal("identity drift reached command POST")
	}
}

func TestDurableIdentityFenceTravelsWithPostAcrossNodeSwap(t *testing.T) {
	identityBody := fixture(t, "read.identity")
	command := fixture(t, "command.2.message.enqueue")
	staleBody := fixture(t, "error.stale")
	var expected hp.NodeIdentity
	if err := json.Unmarshal(identityBody, &expected); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write(identityBody)
			return
		}
		posts.Add(1)
		if r.Header.Get(hp.ExpectedNodeIDHeader) != expected.NodeID ||
			r.Header.Get(hp.ExpectedRegistryHeader) != fmt.Sprint(expected.RegistryVersion) ||
			r.Header.Get(hp.ExpectedEpochHeader) != fmt.Sprint(expected.IdentityEpoch) ||
			r.Header.Get(hp.ExpectedAdapterKindHeader) != expected.Adapter.Kind ||
			r.Header.Get(hp.ExpectedAdapterVersionHeader) != expected.Adapter.Version {
			t.Error("POST did not carry the exact Router identity fence")
		}
		// Model a replacement after GET: the receiving node atomically rejects
		// the old expected identity instead of admitting the command.
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(staleBody)
	})
	result, err := rig.client.CommandFenced(context.Background(), testNode, testOwner, command, expected)
	if err != nil || result.Status != http.StatusConflict || hp.Validate("error", result.Body) != nil {
		t.Fatal("swapped node did not reject the fenced POST", result.Status, err)
	}
	if posts.Load() != 1 {
		t.Fatal("fenced POST retried or skipped", posts.Load())
	}
}

func TestReceiptMustMatchExactIntentAndLostACKNeverRetries(t *testing.T) {
	id, command, receipt := fixture(t, "read.identity"), fixture(t, "command.2.message.enqueue"), fixture(t, "receipt.2.message.enqueue")
	for _, field := range []string{"ok", "commandId", "nodeId", "dialogId", "lost ACK"} {
		t.Run(field, func(t *testing.T) {
			var out map[string]any
			_ = json.Unmarshal(receipt, &out)
			switch field {
			case "commandId":
				out[field] = "10000000-0000-4000-8000-000000000009"
			case "nodeId":
				out[field] = "20000000-0000-4000-8000-000000000009"
			case "dialogId":
				out["references"].(map[string]any)[field] = "30000000-0000-4000-8000-000000000009"
			}
			var posts atomic.Int32
			rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					_, _ = w.Write(id)
					return
				}
				posts.Add(1)
				b, _ := io.ReadAll(r.Body)
				if string(b) != string(command) {
					t.Error("intent changed")
				}
				if field == "lost ACK" {
					conn, _, e := w.(http.Hijacker).Hijack()
					if e == nil {
						_ = conn.Close()
					}
					return
				}
				w.WriteHeader(202)
				_ = json.NewEncoder(w).Encode(out)
			})
			result, err := rig.client.Command(context.Background(), testNode, testOwner, command)
			if field == "ok" {
				if err != nil || result.Status != 202 {
					t.Fatal(result.Status, err)
				}
			} else if field == "lost ACK" {
				requireFault(t, err, 503, "node_unavailable")
				if len(result.Body) != 0 {
					t.Fatal("fake receipt")
				}
			} else {
				requireFault(t, err, 503, "node_unavailable")
			}
			if posts.Load() != 1 {
				t.Fatal("POST retried or skipped", posts.Load())
			}
		})
	}
}

func TestReadScopesAndRouteAllowlist(t *testing.T) {
	id, history := fixture(t, "read.identity"), fixture(t, "page.history.user")
	var calls atomic.Int32
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "identity") {
			_, _ = w.Write(id)
		} else {
			_, _ = w.Write(history)
		}
	})
	path := "dialogs/30000000-0000-4000-8000-000000000001/messages"
	if _, err := rig.client.Read(context.Background(), testNode, testOwner, path, "limit=10"); err != nil {
		t.Fatal(err)
	}
	_, err := rig.client.Read(context.Background(), testNode, testOwner, strings.Replace(path, "000000000001", "000000000002", 1), "")
	requireFault(t, err, 409, "schema_mismatch")
	before := calls.Load()
	for _, tc := range []struct{ path, query string }{{"../identity", ""}, {"identity", "url=https://foreign"}, {"dialogs", "limit=01"}, {"dialogs", "limit=1&limit=2"}, {"dialogs", "limit=101"}, {"requests", "state=queued|active"}, {"snapshot", "after=1"}} {
		_, err := rig.client.Read(context.Background(), testNode, testOwner, tc.path, tc.query)
		requireFault(t, err, 400, "invalid")
	}
	if calls.Load() != before {
		t.Fatal("untrusted route/query reached node")
	}
}

func TestSSEReplayScopeAndFraming(t *testing.T) {
	id, first := fixture(t, "read.identity"), fixture(t, "event.1.node.state_changed")
	var event map[string]any
	_ = json.Unmarshal(first, &event)
	frame := func(seq int, node string, epoch int) string {
		v := map[string]any{}
		for k, x := range event {
			v[k] = x
		}
		v["seq"], v["nodeId"], v["epoch"] = seq, node, epoch
		b, _ := json.Marshal(v)
		return fmt.Sprintf("id: %d\ndata: %s\n\n", seq, b)
	}
	for _, tc := range []struct {
		name, wire string
		seq        int64
		code       string
	}{
		{"duplicate then contiguous", frame(1, testNode, 1) + ": heartbeat\n\n" + frame(2, testNode, 1), 2, ""},
		{"gap", frame(3, testNode, 1), 0, "stale"},
		{"foreign node", frame(2, "20000000-0000-4000-8000-000000000002", 1), 0, "schema_mismatch"},
		{"new epoch", frame(2, testNode, 2), 0, "schema_mismatch"},
		{"incomplete frame", strings.TrimSuffix(frame(2, testNode, 1), "\n\n"), 0, "schema_mismatch"},
		{"duplicate id", "id: 2\n" + frame(2, testNode, 1), 0, "schema_mismatch"},
		{"raw diagnostic", "data: private diagnostic\n\n", 0, "schema_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "identity") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(id)
					return
				}
				if r.URL.RawQuery != "after=1" {
					t.Error("wrong replay cursor")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			})
			s, err := rig.client.OpenEvents(context.Background(), testNode, testOwner, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			b, err := s.Next()
			if tc.code != "" {
				requireFault(t, err, 409, tc.code)
				return
			}
			var got hp.EventEnvelope
			_ = json.Unmarshal(b, &got)
			if err != nil || got.Seq != tc.seq {
				t.Fatal(got.Seq, err)
			}
			if _, err = s.Next(); !errors.Is(err, io.EOF) {
				t.Fatal("expected EOF", err)
			}
		})
	}
}

func TestSSEErrorBodyCannotHoldStreamSetupForever(t *testing.T) {
	id := fixture(t, "read.identity")
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "identity") {
			_, _ = w.Write(id)
			return
		}
		w.WriteHeader(503)
		w.(http.Flusher).Flush()
		// A broken node sends headers but never completes its error body.
		<-r.Context().Done()
	})
	start := time.Now()
	_, err := rig.client.OpenEvents(context.Background(), testNode, testOwner, 0)
	requireFault(t, err, 503, "node_unavailable")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatal("unbounded stream error body", elapsed)
	}
}
