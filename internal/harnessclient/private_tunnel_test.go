package harnessclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnesstunnel"
)

type tunnelProxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	purposes []harnesstunnel.Purpose
}

func startTunnelProxy(t *testing.T, target string) (*harnesstunnel.Client, *tunnelProxy) {
	t.Helper()
	directory, err := os.MkdirTemp("", "r08-mtls-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "tunnel.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &tunnelProxy{listener: listener, target: target}
	t.Cleanup(func() { _ = listener.Close() })
	go proxy.serve()
	client, err := harnesstunnel.NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	return client, proxy
}

func (p *tunnelProxy) serve() {
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(connection)
	}
}

func (p *tunnelProxy) handle(connection net.Conn) {
	defer connection.Close()
	request, err := harnesstunnel.ReadDialRequest(connection)
	if err != nil || request.Validate(time.Now()) != nil {
		return
	}
	p.mu.Lock()
	p.purposes = append(p.purposes, request.Purpose)
	p.mu.Unlock()
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = harnesstunnel.WriteDialReply(connection, harnesstunnel.DialReply{SchemaID: harnesstunnel.ReplySchemaID, Status: "rejected", Code: "endpoint_unavailable"})
		return
	}
	defer upstream.Close()
	if harnesstunnel.WriteDialReply(connection, harnesstunnel.DialReply{SchemaID: harnesstunnel.ReplySchemaID, Status: "ready"}) != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(connection, upstream); done <- struct{}{} }()
	go func() { _, _ = io.Copy(upstream, connection); done <- struct{}{} }()
	<-done
}

func operatorCertificate(t *testing.T, rig *testRig) tls.Certificate {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(4), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, rig.ca, public, rig.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
}

func TestPrivateTunnelPreservesEndToEndMTLSAndCredentialRoles(t *testing.T) {
	var mu sync.Mutex
	var executionSerials, operatorSerials []int64
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		serial := r.TLS.PeerCertificates[0].SerialNumber.Int64()
		mu.Lock()
		if strings.Contains(r.URL.Path, "/administration/") {
			operatorSerials = append(operatorSerials, serial)
		} else {
			executionSerials = append(executionSerials, serial)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/identity") {
			_, _ = w.Write(fixture(t, "read.identity"))
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})
	operator := operatorCertificate(t, rig)
	manifest := rig.manifest
	manifest.SchemaID = RouterRegistrySchemaID
	manifest.WireSchemaSHA256 = "5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9"
	manifest.Nodes = append([]Node(nil), manifest.Nodes...)
	manifest.Nodes[0].RegistrationRevision = 1
	manifest.Nodes[0].RegistrationEpoch = 1
	manifest.Nodes[0].Compatibility = "compatible"
	canonical, _ := json.Marshal(manifest)
	registryDigest := sha256.Sum256(canonical)
	binding := harnesstunnel.EndpointBinding{
		NodeID: testNode, RegistrationRevision: 1, RegistrationEpoch: 1, EndpointRevision: 1,
		HostID: "30000000-0000-4000-8000-000000000001", HostVersion: 1, Transport: "local",
		TargetRef: "local-r08", DockerContextRef: "desktop-linux", ExpectedHostIdentitySHA256: strings.Repeat("a", 64),
		HostPlatform: "darwin", HostArchitecture: "arm64", ContainerID: strings.Repeat("b", 64), RuntimeGeneration: 1,
		Address: rig.server.Listener.Addr().String(),
	}
	bindings := harnesstunnel.BindingManifest{
		SchemaID: harnesstunnel.BindingSchemaID, OwnerID: testOwner,
		RegistrySHA256: hex.EncodeToString(registryDigest[:]), Nodes: []harnesstunnel.EndpointBinding{binding},
	}
	tunnel, proxy := startTunnelProxy(t, rig.server.Listener.Addr().String())
	client, err := newClient(signedBytes(t, manifest, rig.priv), rig.pub, rig.roots, rig.cert, operator, &bindings, tunnel)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Read(context.Background(), testNode, testOwner, "identity", ""); err != nil {
		t.Fatal(err)
	}
	entry, err := client.node(testNode, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	response, err := entry.request(context.Background(), testOwner, http.MethodPost,
		"/v1/nodes/"+testNode+"/administration/holds", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	mu.Lock()
	if len(executionSerials) != 1 || executionSerials[0] != 3 || len(operatorSerials) != 1 || operatorSerials[0] != 4 {
		t.Fatalf("mTLS roles crossed: execution=%v operator=%v", executionSerials, operatorSerials)
	}
	mu.Unlock()
	if err := proxy.listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(context.Background(), testNode, testOwner, "identity", ""); err == nil {
		t.Fatal("private node fell back to its registry URL after tunnel loss")
	} else {
		requireFault(t, err, http.StatusServiceUnavailable, "node_unavailable")
	}
	mu.Lock()
	if len(executionSerials) != 1 || len(operatorSerials) != 1 {
		t.Fatalf("tunnel loss reached public/direct endpoint: execution=%v operator=%v", executionSerials, operatorSerials)
	}
	mu.Unlock()
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if len(proxy.purposes) != 2 || proxy.purposes[0] != harnesstunnel.PurposeResponse || proxy.purposes[1] != harnesstunnel.PurposeAdmin {
		t.Fatalf("tunnel purpose routing changed: %v", proxy.purposes)
	}
	if same, err := newClient(signedBytes(t, manifest, rig.priv), rig.pub, rig.roots, rig.cert, rig.cert, &bindings, tunnel); err == nil {
		same.Close()
		t.Fatal("shared execution/operator credential was accepted for private tunnel")
	}
}
