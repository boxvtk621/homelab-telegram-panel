package server_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/harness/fixture"
	"github.com/boxvtk621/homelab-telegram-panel/harness/node"
	"github.com/boxvtk621/homelab-telegram-panel/harness/server"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

const testNodeID = "20000000-0000-4000-8000-000000000001"

type enoughSpace struct{}

func (enoughSpace) Measure(string) (node.SpaceInfo, error) {
	return node.SpaceInfo{FreeBytes: 16 << 30, TotalBytes: 64 << 30}, nil
}

func TestRealMTLSCommandsAndReads(t *testing.T) {
	ca, caKey, caPool := certificateAuthority(t)
	serverCertificate, _ := signedCertificate(t, ca, caKey, "localhost", false)
	clientCertificate, clientLeaf := signedCertificate(t, ca, caKey, "gateway", true)
	digest := sha256.Sum256(clientLeaf.Raw)
	authority, err := node.Open(context.Background(), node.Config{
		DataDir: t.TempDir(), NodeID: testNodeID, OwnerID: "1-1", RegistryVersion: 1,
		Adapter: fixture.NewAdapter(), Space: enoughSpace{}, ManualDispatchForTesting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	handler, err := server.New(server.Config{NodeID: testNodeID, GatewayCertificateSHA256: hex.EncodeToString(digest[:])}, authority)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewUnstartedServer(handler)
	endpoint.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate}, ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs: caPool, MinVersion: tls.VersionTLS13,
	}
	endpoint.StartTLS()
	defer endpoint.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: caPool, Certificates: []tls.Certificate{clientCertificate}, ServerName: "localhost", MinVersion: tls.VersionTLS13,
	}}}

	identity := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/identity", "", "1-1")
	validateResponse(t, identity, http.StatusOK, "nodeIdentity")
	create := `{"protocolVersion":1,"schemaId":"harness-wire-v1","commandId":"10000000-0000-4000-8000-000000000201","kind":"dialog.create","target":{"nodeId":"` + testNodeID + `"},"expected":{"registryVersion":1},"payload":{}}`
	accepted := request(t, client, http.MethodPost, endpoint.URL+"/v1/nodes/"+testNodeID+"/commands", create, "1-1")
	validateResponse(t, accepted, http.StatusAccepted, "receipt")
	status := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/commands/10000000-0000-4000-8000-000000000201", "", "1-1")
	validateResponse(t, status, http.StatusOK, "commandStatus")
	snapshot := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/snapshot", "", "1-1")
	validateResponse(t, snapshot, http.StatusOK, "snapshot")
	ready := request(t, client, http.MethodGet, endpoint.URL+"/health/ready", "", "1-1")
	validateResponse(t, ready, http.StatusOK, "healthReady")
	forbidden := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/snapshot", "", "1-2")
	validateResponse(t, forbidden, http.StatusForbidden, "error")

	withoutCertificate := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caPool, ServerName: "localhost", MinVersion: tls.VersionTLS13}}}
	requestValue, _ := http.NewRequest(http.MethodGet, endpoint.URL+"/health/live", nil)
	requestValue.Header.Set(server.DefaultActorHeader, "1-1")
	if _, err := withoutCertificate.Do(requestValue); err == nil {
		t.Fatal("listener accepted a client without mTLS certificate")
	}
}

func request(t *testing.T, client *http.Client, method, url, body, actor string) []byte {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(server.DefaultActorHeader, actor)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte{byte(response.StatusCode >> 8), byte(response.StatusCode)}, content...)
}

func validateResponse(t *testing.T, encoded []byte, status int, wireType string) {
	t.Helper()
	actualStatus := int(encoded[0])<<8 | int(encoded[1])
	body := encoded[2:]
	if actualStatus != status {
		t.Fatalf("status=%d want=%d body=%s", actualStatus, status, body)
	}
	if err := harnessprotocol.Validate(wireType, body); err != nil {
		t.Fatalf("invalid %s: %v body=%s", wireType, err, body)
	}
}

func certificateAuthority(t *testing.T) (*x509.Certificate, *rsa.PrivateKey, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return certificate, key, pool
}

func signedCertificate(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, name string, client bool) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if client {
		usage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		template.DNSNames = nil
	}
	template.ExtKeyUsage = usage
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key, Leaf: leaf}, leaf
}
