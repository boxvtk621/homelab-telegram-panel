package harnessclient

import (
	"bufio"
	"bytes"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/transcriptview"
)

const testNode = "20000000-0000-4000-8000-000000000001"
const testOwner = "1-1"

type abruptSSEReader struct{ payload []byte }

func (r *abruptSSEReader) Read(target []byte) (int, error) {
	if len(r.payload) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	written := copy(target, r.payload)
	r.payload = r.payload[written:]
	if len(r.payload) == 0 {
		return written, io.ErrUnexpectedEOF
	}
	return written, nil
}

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
	client     *Client
	server     *httptest.Server
	manifest   Manifest
	pub        ed25519.PublicKey
	priv       ed25519.PrivateKey
	ca         *x509.Certificate
	caKey      ed25519.PrivateKey
	roots      *x509.CertPool
	cert       tls.Certificate
	serverCert tls.Certificate
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
	return &testRig{client: c, server: s, manifest: m, pub: pub, priv: key, ca: parsed, caKey: key, roots: roots, cert: clientCert, serverCert: serverCert}
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

func TestLegacyRegistryRemainsByteBoundedWithoutDynamicNodeCap(t *testing.T) {
	rig := newRig(t, func(http.ResponseWriter, *http.Request) {})
	manifest := rig.manifest
	manifest.Nodes = make([]Node, 1001)
	for index := range manifest.Nodes {
		pin := sha256.Sum256([]byte(fmt.Sprintf("legacy-pin-%d", index)))
		manifest.Nodes[index] = Node{
			NodeID: fmt.Sprintf("10000000-0000-4000-8000-%012d", index+1), Name: "A", Adapter: "cursor",
			URL: rig.server.URL, CertificateSHA256: hex.EncodeToString(pin[:]),
		}
	}
	raw := signedBytes(t, manifest, rig.priv)
	if len(raw) > 256<<10 {
		t.Fatalf("legacy compatibility fixture exceeds byte contract: %d", len(raw))
	}
	client, err := New(raw, rig.pub, rig.roots, rig.cert)
	if err != nil {
		t.Fatal("byte-bounded legacy registry was rejected", err)
	}
	client.Close()
}

func TestLegacyEmptyRegistryRemainsValidForFirstNodeProjection(t *testing.T) {
	rig := newRig(t, func(http.ResponseWriter, *http.Request) {})
	manifest := rig.manifest
	manifest.Nodes = []Node{}
	client, err := New(signedBytes(t, manifest, rig.priv), rig.pub, rig.roots, rig.cert)
	if err != nil {
		t.Fatal("R01-compatible empty legacy registry was rejected", err)
	}
	defer client.Close()
	public, ok := client.Public(testOwner)
	if !ok || len(public.Nodes) != 0 || len(client.RoutingRegistry().Nodes) != 0 {
		t.Fatalf("empty registry changed: public=%+v routing=%+v", public, client.RoutingRegistry())
	}
}

func TestSignedRegistrySupportsBeyondHundredNodeTestScale(t *testing.T) {
	rig := newRig(t, func(http.ResponseWriter, *http.Request) {})
	manifest := rig.manifest
	manifest.SchemaID = RouterRegistrySchemaID
	manifest.RegistryVersion = 2
	manifest.WireSchemaSHA256 = hp.SchemaSHA256
	manifest.Nodes = append([]Node(nil), manifest.Nodes...)
	const nodeCount = 101
	for index := 2; index <= nodeCount; index++ {
		pin := sha256.Sum256([]byte(fmt.Sprintf("node-%03d", index)))
		manifest.Nodes = append(manifest.Nodes, Node{
			NodeID: fmt.Sprintf("20000000-0000-4000-8000-%012d", index),
			Name:   fmt.Sprintf("Agent %03d", index), Adapter: "codex",
			URL: fmt.Sprintf("https://node-%03d.invalid:9443", index), CertificateSHA256: hex.EncodeToString(pin[:]),
		})
	}
	for index := range manifest.Nodes {
		manifest.Nodes[index].RegistrationRevision = 1
		manifest.Nodes[index].RegistrationEpoch = 1
		manifest.Nodes[index].Compatibility = "compatible"
	}
	client, err := New(signedBytes(t, manifest, rig.priv), rig.pub, rig.roots, rig.cert)
	if err != nil {
		t.Fatal(err)
	}
	if public, ok := client.Public(testOwner); !ok || len(public.Nodes) != nodeCount {
		client.Close()
		t.Fatalf("registry nodes=%d ok=%v", len(public.Nodes), ok)
	}
	client.Close()
}

func TestDynamicRegistryUsesPerNodeRevisionAndExplicitCompatibility(t *testing.T) {
	identity := fixture(t, "read.identity")
	var projectedIdentity map[string]any
	if json.Unmarshal(identity, &projectedIdentity) != nil {
		t.Fatal("invalid identity fixture")
	}
	projectedIdentity["identityEpoch"] = float64(7)
	identity, _ = json.Marshal(projectedIdentity)
	rig := newRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(identity)
	})
	manifest := rig.manifest
	manifest.SchemaID = RouterRegistrySchemaID
	manifest.RegistryVersion = 2
	manifest.WireSchemaSHA256 = hp.SchemaSHA256
	manifest.Nodes = append([]Node(nil), manifest.Nodes...)
	manifest.Nodes[0].RegistrationRevision = 1
	manifest.Nodes[0].RegistrationEpoch = 7
	manifest.Nodes[0].Compatibility = "compatible"
	client, err := New(signedBytes(t, manifest, rig.priv), rig.pub, rig.roots, rig.cert)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Read(context.Background(), testNode, testOwner, "identity", "")
	if err != nil || response.Status != http.StatusOK {
		t.Fatal("global inventory generation leaked into node identity fence", response, err)
	}
	routing := client.RoutingRegistry()
	if routing.SchemaID != RouterRegistrySchemaID || routing.WireSchemaSHA256 != hp.SchemaSHA256 ||
		len(routing.Nodes) != 1 || routing.Nodes[0].RegistrationRevision != 1 ||
		routing.Nodes[0].RegistrationEpoch != 7 || routing.Nodes[0].Compatibility != "compatible" ||
		len(routing.Nodes[0].BindingSHA256) != 64 {
		t.Fatalf("dynamic routing metadata missing: %+v", routing)
	}
	if string(client.Envelope()) != string(signedBytes(t, manifest, rig.priv)) {
		t.Fatal("verified envelope was not retained for durable Router state")
	}

	for _, test := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"stale wire schema", func(value *Manifest) { value.WireSchemaSHA256 = strings.Repeat("0", 64) }},
		{"missing node revision", func(value *Manifest) { value.Nodes[0].RegistrationRevision = 0 }},
		{"missing node epoch", func(value *Manifest) { value.Nodes[0].RegistrationEpoch = 0 }},
		{"implicit compatibility", func(value *Manifest) { value.Nodes[0].Compatibility = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := manifest
			invalid.Nodes = append([]Node(nil), manifest.Nodes...)
			test.mutate(&invalid)
			if candidate, err := New(signedBytes(t, invalid, rig.priv), rig.pub, rig.roots, rig.cert); err == nil {
				candidate.Close()
				t.Fatal("invalid dynamic registry accepted")
			}
		})
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

func TestLostCommandAcknowledgementReconcilesWithoutSecondPost(t *testing.T) {
	command := fixture(t, "command.2.message.enqueue")
	receipt := fixture(t, "receipt.2.message.enqueue")
	_, canonicalHash, err := hp.CanonicalCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	var accepted hp.Receipt
	if json.Unmarshal(receipt, &accepted) != nil {
		t.Fatal("invalid receipt fixture")
	}
	status, err := json.Marshal(hp.CommandStatus{
		ProtocolVersion: hp.ProtocolVersion, SchemaID: hp.SchemaID, NodeID: testNode,
		CommandID: accepted.CommandID, CanonicalPayloadHash: canonicalHash, Status: "accepted", Receipt: accepted,
	})
	if err != nil || hp.Validate("commandStatus", status) != nil {
		t.Fatal("invalid status fixture", err)
	}
	var posts, readbacks atomic.Int32
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/identity"):
			_, _ = w.Write(fixture(t, "read.identity"))
		case r.Method == http.MethodPost:
			posts.Add(1)
			panic(http.ErrAbortHandler)
		case strings.HasSuffix(r.URL.Path, "/commands/"+accepted.CommandID):
			readbacks.Add(1)
			_, _ = w.Write(status)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	response, err := rig.client.Command(context.Background(), testNode, testOwner, command)
	var recovered hp.Receipt
	if err != nil || response.Status != http.StatusAccepted || json.Unmarshal(response.Body, &recovered) != nil || recovered.CommandID != accepted.CommandID || recovered.ReceiptID != accepted.ReceiptID {
		t.Fatalf("lost ACK was not reconciled: status=%d err=%v body=%s", response.Status, err, response.Body)
	}
	if posts.Load() != 1 || readbacks.Load() != 1 {
		t.Fatalf("unsafe retry shape: posts=%d readbacks=%d", posts.Load(), readbacks.Load())
	}
}

func TestEventStreamReconnectsFromLastAcceptedCursorAfterPartialFrame(t *testing.T) {
	eventOne, eventTwo := fixture(t, "event.1.node.state_changed"), fixture(t, "event.2.queue.changed")
	for _, event := range []*[]byte{&eventOne, &eventTwo} {
		var value map[string]any
		if json.Unmarshal(*event, &value) != nil {
			t.Fatal("invalid event fixture")
		}
		*event, _ = json.Marshal(value)
	}
	var streamCalls atomic.Int32
	var cursors []string
	var cursorMu sync.Mutex
	dropFirst := make(chan struct{})
	var dropOnce sync.Once
	defer dropOnce.Do(func() { close(dropFirst) })
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/identity") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "read.identity"))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/events") {
			t.Errorf("unexpected route %s", r.URL.Path)
			return
		}
		cursorMu.Lock()
		cursors = append(cursors, r.URL.Query().Get("after"))
		cursorMu.Unlock()
		call := streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if call == 1 {
			_, _ = fmt.Fprintf(w, "id: 1\ndata: %s\n\n", eventOne)
			w.(http.Flusher).Flush()
			<-dropFirst
			_, _ = io.WriteString(w, "id: 2\ndata: {\"protocolVersion\":")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		} else {
			_, _ = fmt.Fprintf(w, "id: 2\ndata: %s\n\n", eventTwo)
		}
	})
	stream, err := rig.client.OpenEvents(context.Background(), testNode, testOwner, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next()
	if err != nil || !bytes.Equal(first, eventOne) {
		t.Fatalf("first event: err=%v body=%s", err, first)
	}
	dropOnce.Do(func() { close(dropFirst) })
	second, err := stream.Next()
	if err != nil || !bytes.Equal(second, eventTwo) {
		t.Fatalf("reconnected event: err=%v body=%s", err, second)
	}
	cursorMu.Lock()
	defer cursorMu.Unlock()
	if len(cursors) != 2 || cursors[0] != "0" || cursors[1] != "1" {
		t.Fatalf("cursor recovery changed: %v", cursors)
	}
}

func TestEventStreamTreatsTornFieldLineAsTransportLoss(t *testing.T) {
	for _, partial := range []string{"i", "id: ", "event: mess"} {
		t.Run(partial, func(t *testing.T) {
			stream := &Stream{ctx: context.Background(), nodeID: testNode, identity: hp.NodeIdentity{IdentityEpoch: 1}}
			scanner := bufio.NewScanner(&abruptSSEReader{payload: []byte(partial)})
			scanner.Buffer(make([]byte, 16<<10), hp.MaximumWireBytes+1024)
			scanner.Split(boundedSSELines(&stream.incomplete))
			stream.scanner = scanner
			body, ended, err := stream.nextFrame()
			if err != nil || !ended || body != nil || stream.last != 0 {
				t.Fatalf("partial %q was not recoverable: ended=%v err=%v body=%q last=%d", partial, ended, err, body, stream.last)
			}
		})
	}

	stream := &Stream{ctx: context.Background(), nodeID: testNode, identity: hp.NodeIdentity{IdentityEpoch: 1}}
	scanner := bufio.NewScanner(strings.NewReader("i"))
	scanner.Buffer(make([]byte, 16<<10), hp.MaximumWireBytes+1024)
	scanner.Split(boundedSSELines(&stream.incomplete))
	stream.scanner = scanner
	_, ended, err := stream.nextFrame()
	if ended {
		t.Fatal("clean EOF was treated as retryable transport loss")
	}
	requireFault(t, err, http.StatusConflict, "schema_mismatch")
}

func TestEventStreamReconnectRejectsChangedIdentity(t *testing.T) {
	event := fixture(t, "event.1.node.state_changed")
	var compact map[string]any
	if json.Unmarshal(event, &compact) != nil {
		t.Fatal("invalid event fixture")
	}
	event, _ = json.Marshal(compact)
	identity := fixture(t, "read.identity")
	var changed hp.NodeIdentity
	if json.Unmarshal(identity, &changed) != nil {
		t.Fatal("invalid identity fixture")
	}
	changed.IdentityEpoch++
	changedIdentity, _ := json.Marshal(changed)
	var identityCalls, streamCalls atomic.Int32
	drop := make(chan struct{})
	var dropOnce sync.Once
	defer dropOnce.Do(func() { close(drop) })
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/identity") {
			w.Header().Set("Content-Type", "application/json")
			if identityCalls.Add(1) == 1 {
				_, _ = w.Write(identity)
			} else {
				_, _ = w.Write(changedIdentity)
			}
			return
		}
		streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "id: 1\ndata: %s\n\n", event)
		w.(http.Flusher).Flush()
		<-drop
		panic(http.ErrAbortHandler)
	})
	stream, err := rig.client.OpenEvents(context.Background(), testNode, testOwner, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	dropOnce.Do(func() { close(drop) })
	_, err = stream.Next()
	requireFault(t, err, http.StatusConflict, "stale")
	if identityCalls.Load() != 2 || streamCalls.Load() != 1 {
		t.Fatalf("identity drift retried: identities=%d streams=%d", identityCalls.Load(), streamCalls.Load())
	}
}

func TestEventStreamReconnectSurfacesExpiredCursor(t *testing.T) {
	event := fixture(t, "event.1.node.state_changed")
	var compact map[string]any
	if json.Unmarshal(event, &compact) != nil {
		t.Fatal("invalid event fixture")
	}
	event, _ = json.Marshal(compact)
	staleBody := fixture(t, "error.stale")
	var streamCalls atomic.Int32
	drop := make(chan struct{})
	var dropOnce sync.Once
	defer dropOnce.Do(func() { close(drop) })
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/identity") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "read.identity"))
			return
		}
		call := streamCalls.Add(1)
		if call == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "id: 1\ndata: %s\n\n", event)
			w.(http.Flusher).Flush()
			<-drop
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(staleBody)
	})
	stream, err := rig.client.OpenEvents(context.Background(), testNode, testOwner, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(); err != nil {
		t.Fatal(err)
	}
	stream.waitForRetry = func(context.Context, time.Duration) error {
		t.Fatal("permanent stale cursor entered reconnect retry")
		return nil
	}
	dropOnce.Do(func() { close(drop) })
	_, err = stream.Next()
	requireFault(t, err, http.StatusConflict, "stale")
	if streamCalls.Load() != 2 {
		t.Fatalf("expired cursor retried %d stream requests", streamCalls.Load())
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

func TestSafeTextReadUsesExactSourceScope(t *testing.T) {
	id := fixture(t, "read.identity")
	dialogID := "30000000-0000-4000-8000-000000000001"
	attemptID := "60000000-0000-4000-8000-000000000001"
	messageID := "40000000-0000-4000-8000-000000000001"
	source := transcriptview.Source{Kind: "assistant_message", ID: messageID, Stream: "none"}
	hash := sha256.Sum256([]byte("answer"))
	manifest, err := transcriptview.Encode(transcriptview.Manifest{
		SchemaID: transcriptview.SchemaID, NodeID: testNode, DialogID: dialogID, AttemptID: attemptID,
		Generation: 1, TextID: "50000000-0000-4000-8000-000000000001", Source: source,
		Preview: "answer", Redaction: "none", Complete: true, SizeBytes: 6, SHA256: hex.EncodeToString(hash[:]),
		Chunks: []transcriptview.Chunk{{ArtifactID: "70000000-0000-4000-8000-000000000001", SizeBytes: 6, SHA256: hex.EncodeToString(hash[:])}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/identity") {
			_, _ = w.Write(id)
			return
		}
		if r.URL.Path != "/v1/nodes/"+testNode+"/texts/resolve" {
			t.Errorf("unexpected safe-text route %s", r.URL.Path)
		}
		_, _ = w.Write(manifest)
	})
	query := "dialogId=" + dialogID + "&attemptId=" + attemptID + "&sourceKind=assistant_message&sourceId=" + messageID + "&sourceIndex=0&sourceStream=none"
	response, err := rig.client.Read(context.Background(), testNode, testOwner, "texts/resolve", query)
	if err != nil || response.Status != http.StatusOK || !bytes.Equal(response.Body, manifest) {
		t.Fatalf("safe text read failed: status=%d err=%v body=%s", response.Status, err, response.Body)
	}

	wrongQuery := strings.Replace(query, messageID, "40000000-0000-4000-8000-000000000009", 1)
	_, err = rig.client.Read(context.Background(), testNode, testOwner, "texts/resolve", wrongQuery)
	requireFault(t, err, 409, "schema_mismatch")

	before := calls.Load()
	for _, invalidQuery := range []string{
		strings.Replace(query, "&sourceStream=none", "", 1),
		query + "&extra=x",
		strings.Replace(query, "sourceIndex=0", "sourceIndex=00", 1),
		strings.Replace(query, "sourceStream=none", "sourceStream=stdout", 1),
		query + "&sourceId=" + messageID,
	} {
		_, err := rig.client.Read(context.Background(), testNode, testOwner, "texts/resolve", invalidQuery)
		requireFault(t, err, 400, "invalid")
	}
	if calls.Load() != before {
		t.Fatal("invalid safe-text query reached node")
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
