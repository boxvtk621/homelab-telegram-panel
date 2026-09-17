// Package harnessclient is the non-authoritative, private mTLS client for nodes.
// It owns no queue, provider credential, execution, or durable receipt.
package harnessclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnesstunnel"
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var actor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)

const RouterRegistrySchemaID = "harness-router-registry-v1"

type Node struct {
	NodeID                string `json:"nodeId"`
	Name                  string `json:"name"`
	Adapter               string `json:"adapter"`
	URL                   string `json:"url"`
	CertificateSHA256     string `json:"certificateSHA256"`
	RegistrationRevision  int64  `json:"registrationRevision,omitempty"`
	RegistrationEpoch     int64  `json:"registrationEpoch,omitempty"`
	Compatibility         string `json:"compatibility,omitempty"`
	EndpointBindingSHA256 string `json:"endpointBindingSHA256,omitempty"`
}

// Manifest is operator-owned. The signature covers json.Marshal(Manifest),
// including its version, owner, mode, ordered nodes and leaf-certificate pins.
// Node URLs and certificate identities must never be sent to the browser.
type Manifest struct {
	SchemaID         string `json:"schemaId,omitempty"`
	RegistryVersion  int64  `json:"registryVersion"`
	OwnerID          string `json:"ownerId"`
	Mode             string `json:"mode"`
	WireSchemaSHA256 string `json:"wireSchemaSHA256,omitempty"`
	Nodes            []Node `json:"nodes"`
}

type SignedManifest struct {
	Manifest  Manifest `json:"manifest"`
	Signature string   `json:"signature"`
}

type Paths struct {
	Registry, SignerPublicKey, CA, ClientCertificate, ClientKey string
	TunnelBindings, TunnelSocket                                string
	OperatorCertificate, OperatorKey                            string
}

type PublicNode struct {
	NodeID  string `json:"nodeId"`
	Name    string `json:"name"`
	Adapter string `json:"adapter"`
}
type PublicRegistry struct {
	RegistryVersion int64        `json:"registryVersion"`
	Mode            string       `json:"mode"`
	Nodes           []PublicNode `json:"nodes"`
}

// RoutingRegistry is the private, non-secret identity used to bind durable
// Router state to one exact signed registry. It intentionally omits node URLs,
// certificate pins and trust material.
type RoutingRegistry struct {
	RegistryVersion  int64
	OwnerID          string
	Mode             string
	ManifestSHA256   string
	SchemaID         string
	WireSchemaSHA256 string
	Nodes            []RoutingNode
}

type RoutingNode struct {
	NodeID               string
	Name                 string
	Adapter              string
	RegistrationRevision int64
	RegistrationEpoch    int64
	Compatibility        string
	BindingSHA256        string
}

type entry struct {
	node       Node
	transports map[harnesstunnel.Purpose]*http.Transport
	clients    map[harnesstunnel.Purpose]*http.Client
}

type Client struct {
	manifest       Manifest
	manifestSHA256 string
	envelope       []byte
	nodes          map[string]*entry
}

func Empty() *Client {
	return &Client{manifest: Manifest{Mode: "live", Nodes: []Node{}}, nodes: map[string]*entry{}}
}

func Load(paths Paths) (*Client, error) {
	if paths.Registry == "" && paths.SignerPublicKey == "" && paths.CA == "" && paths.ClientCertificate == "" && paths.ClientKey == "" &&
		paths.TunnelBindings == "" && paths.TunnelSocket == "" && paths.OperatorCertificate == "" && paths.OperatorKey == "" {
		return Empty(), nil
	}
	raw, err := readBounded(paths.Registry, 256<<10)
	if err != nil {
		return nil, errors.New("invalid Harness registry file")
	}
	return LoadRaw(paths, raw)
}

// LoadRaw verifies an operator-supplied registry projection with the trust
// material from paths. It never writes the configured registry file.
func LoadRaw(paths Paths, raw []byte) (*Client, error) {
	pub, err := readBounded(paths.SignerPublicKey, 16<<10)
	if err != nil {
		return nil, errors.New("invalid Harness registry signer")
	}
	block, rest := pem.Decode(pub)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || block.Type != "PUBLIC KEY" {
		return nil, errors.New("invalid Harness registry signer")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid Harness registry signer")
	}
	signer, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("Harness registry requires Ed25519 signer")
	}
	ca, err := readBounded(paths.CA, 256<<10)
	if err != nil {
		return nil, errors.New("invalid Harness CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid Harness CA")
	}
	cert, err := tls.LoadX509KeyPair(paths.ClientCertificate, paths.ClientKey)
	if err != nil {
		return nil, errors.New("invalid Harness mTLS credential")
	}
	tunnelConfigured := paths.TunnelBindings != "" || paths.TunnelSocket != "" || paths.OperatorCertificate != "" || paths.OperatorKey != ""
	if !tunnelConfigured {
		return New(raw, signer, roots, cert)
	}
	if paths.TunnelBindings == "" || paths.TunnelSocket == "" || paths.OperatorCertificate == "" || paths.OperatorKey == "" {
		return nil, errors.New("incomplete private Harness tunnel configuration")
	}
	bindings, err := harnesstunnel.LoadManifest(paths.TunnelBindings)
	if err != nil {
		return nil, errors.New("invalid Harness tunnel bindings")
	}
	tunnel, err := harnesstunnel.NewClient(paths.TunnelSocket)
	if err != nil {
		return nil, errors.New("invalid Harness tunnel socket")
	}
	operator, err := tls.LoadX509KeyPair(paths.OperatorCertificate, paths.OperatorKey)
	if err != nil || len(operator.Certificate) == 0 || len(cert.Certificate) == 0 || bytes.Equal(operator.Certificate[0], cert.Certificate[0]) {
		return nil, errors.New("invalid distinct Harness operator credential")
	}
	return newClient(raw, signer, roots, cert, operator, &bindings, tunnel)
}

func readBounded(path string, maximum int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("configuration size exceeded")
	}
	return data, nil
}

func New(raw []byte, signer ed25519.PublicKey, roots *x509.CertPool, cert tls.Certificate) (*Client, error) {
	return newClient(raw, signer, roots, cert, cert, nil, nil)
}

func newClient(raw []byte, signer ed25519.PublicKey, roots *x509.CertPool, cert, operator tls.Certificate, bindings *harnesstunnel.BindingManifest, tunnel *harnesstunnel.Client) (*Client, error) {
	if len(raw) > 256<<10 || !strictjson.Valid(raw) || !registryShape(raw) || len(signer) != ed25519.PublicKeySize || roots == nil || len(cert.Certificate) == 0 || cert.PrivateKey == nil {
		return nil, errors.New("invalid Harness trust configuration")
	}
	if bindings != nil && (len(operator.Certificate) == 0 || operator.PrivateKey == nil || bytes.Equal(cert.Certificate[0], operator.Certificate[0])) {
		return nil, errors.New("distinct Harness operator credential required")
	}
	var signed SignedManifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&signed); err != nil {
		return nil, errors.New("invalid Harness registry")
	}
	canonical, err := json.Marshal(signed.Manifest)
	signature, sigErr := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil || sigErr != nil || !ed25519.Verify(signer, canonical, signature) {
		return nil, errors.New("Harness registry signature rejected")
	}
	m := signed.Manifest
	dynamic := m.SchemaID != ""
	if m.RegistryVersion < 1 || m.RegistryVersion > hp.MaximumSafeInteger || !actor.MatchString(m.OwnerID) ||
		(m.Mode != "live" && m.Mode != "fixture") || m.Nodes == nil || dynamic && (len(m.Nodes) == 0 || len(m.Nodes) > 1000) {
		return nil, errors.New("invalid Harness registry identity")
	}
	if dynamic {
		if m.SchemaID != RouterRegistrySchemaID || m.WireSchemaSHA256 != hp.SchemaSHA256 {
			return nil, errors.New("unsupported Harness registry projection")
		}
	} else if m.WireSchemaSHA256 != "" {
		return nil, errors.New("invalid legacy Harness registry")
	}
	sum := sha256.Sum256(canonical)
	c := &Client{manifest: m, manifestSHA256: hex.EncodeToString(sum[:]), envelope: append([]byte(nil), raw...), nodes: make(map[string]*entry)}
	if bindings != nil && (tunnel == nil || bindings.OwnerID != m.OwnerID || !bindings.AcceptsRegistry(c.manifestSHA256)) {
		return nil, errors.New("Harness tunnel binding does not match signed registry")
	}
	seenCerts := map[string]bool{}
	for _, n := range m.Nodes {
		u, err := url.Parse(n.URL)
		pin, pinErr := hex.DecodeString(n.CertificateSHA256)
		bindingDigest, bindingDigestErr := hex.DecodeString(n.EndpointBindingSHA256)
		registrationValid := n.RegistrationRevision == 0 && n.RegistrationEpoch == 0 && n.Compatibility == ""
		if dynamic {
			registrationValid = n.RegistrationRevision >= 1 && n.RegistrationRevision <= hp.MaximumSafeInteger &&
				n.RegistrationEpoch >= 1 && n.RegistrationEpoch <= hp.MaximumSafeInteger &&
				(n.Compatibility == "compatible" || n.Compatibility == "legacy_readonly")
		}
		if !uuid.MatchString(n.NodeID) || n.Name == "" || !utf8.ValidString(n.Name) || len(n.Name) > 200 || strings.ContainsAny(n.Name, "\x00\r\n") || (n.Adapter != "cursor" && n.Adapter != "codex") || !registrationValid || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path != "" || pinErr != nil || len(pin) != sha256.Size || strings.ToLower(n.CertificateSHA256) != n.CertificateSHA256 || (n.EndpointBindingSHA256 != "" && (bindingDigestErr != nil || len(bindingDigest) != sha256.Size || strings.ToLower(n.EndpointBindingSHA256) != n.EndpointBindingSHA256)) || seenCerts[n.CertificateSHA256] || c.nodes[n.NodeID] != nil {
			c.Close()
			return nil, errors.New("invalid Harness node binding")
		}
		seenCerts[n.CertificateSHA256] = true
		entry := &entry{node: n, transports: make(map[harnesstunnel.Purpose]*http.Transport), clients: make(map[harnesstunnel.Purpose]*http.Client)}
		purposes := []harnesstunnel.Purpose{harnesstunnel.PurposeResponse, harnesstunnel.PurposeCommand, harnesstunnel.PurposeEvents, harnesstunnel.PurposeHealth, harnesstunnel.PurposeAdmin, harnesstunnel.PurposeReplicaExport, harnesstunnel.PurposeReplicaImport}
		var binding harnesstunnel.EndpointBinding
		if bindings != nil {
			var present bool
			binding, present = bindings.Binding(n.NodeID)
			bindingSHA256, bindingErr := harnesstunnel.BindingSHA256(binding)
			if !present || binding.RegistrationRevision != n.RegistrationRevision || binding.RegistrationEpoch != n.RegistrationEpoch ||
				(n.EndpointBindingSHA256 != "" && (bindingErr != nil || bindingSHA256 != n.EndpointBindingSHA256)) {
				c.Close()
				return nil, errors.New("Harness tunnel endpoint revision mismatch")
			}
		}
		for _, purpose := range purposes {
			credential := cert
			if purpose == harnesstunnel.PurposeAdmin {
				credential = operator
			}
			dial := (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
			private := bindings != nil
			if private {
				selected := purpose
				dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
					return tunnel.DialContext(ctx, m.OwnerID, binding, selected)
				}
			}
			transport := newHarnessTransport(u.Hostname(), pin, roots, credential, dial, private)
			entry.transports[purpose] = transport
			entry.clients[purpose] = &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
		}
		c.nodes[n.NodeID] = entry
	}
	return c, nil
}

func newHarnessTransport(serverName string, pin []byte, roots *x509.CertPool, certificate tls.Certificate,
	dial func(context.Context, string, string) (net.Conn, error), private bool) *http.Transport {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots.Clone(), Certificates: []tls.Certificate{certificate}, ServerName: serverName}
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return errors.New("unverified Harness certificate")
		}
		digest := sha256.Sum256(state.PeerCertificates[0].Raw)
		if !bytes.Equal(digest[:], pin) {
			return errors.New("Harness certificate identity mismatch")
		}
		return nil
	}
	return &http.Transport{
		TLSClientConfig: tlsConfig, Proxy: nil, DialContext: dial, TLSHandshakeTimeout: 2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second, IdleConnTimeout: 30 * time.Second,
		MaxIdleConns: 16, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 4,
		MaxResponseHeaderBytes: 16 << 10, DisableCompression: true, DisableKeepAlives: private,
	}
}

// encoding/json's struct decoder otherwise accepts case-insensitive aliases.
// Check exact decoded keys before normalizing the signed manifest to Go types.
func registryObject(raw json.RawMessage, keys ...string) (map[string]json.RawMessage, bool) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || len(value) != len(keys) {
		return nil, false
	}
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			return nil, false
		}
	}
	return value, true
}

func registryShape(raw []byte) bool {
	envelope, ok := registryObject(raw, "manifest", "signature")
	if !ok {
		return false
	}
	manifest, legacy := registryObject(envelope["manifest"], "registryVersion", "ownerId", "mode", "nodes")
	if !legacy {
		manifest, ok = registryObject(envelope["manifest"], "schemaId", "registryVersion", "ownerId", "mode", "wireSchemaSHA256", "nodes")
		if !ok {
			return false
		}
	}
	var nodes []json.RawMessage
	if json.Unmarshal(manifest["nodes"], &nodes) != nil || nodes == nil {
		return false
	}
	for _, node := range nodes {
		if legacy {
			if _, ok := registryObject(node, "nodeId", "name", "adapter", "url", "certificateSHA256"); !ok {
				return false
			}
		} else if _, ok := registryObject(node, "nodeId", "name", "adapter", "url", "certificateSHA256", "registrationRevision", "registrationEpoch", "compatibility"); !ok {
			if _, extended := registryObject(node, "nodeId", "name", "adapter", "url", "certificateSHA256", "registrationRevision", "registrationEpoch", "compatibility", "endpointBindingSHA256"); !extended {
				return false
			}
		}
	}
	return true
}

func (c *Client) Close() {
	for _, e := range c.nodes {
		for _, transport := range e.transports {
			transport.CloseIdleConnections()
		}
	}
}

func (c *Client) Public(owner string) (PublicRegistry, bool) {
	if c.manifest.OwnerID != "" && owner != c.manifest.OwnerID {
		return PublicRegistry{}, false
	}
	r := PublicRegistry{RegistryVersion: c.manifest.RegistryVersion, Mode: c.manifest.Mode, Nodes: []PublicNode{}}
	for _, n := range c.manifest.Nodes {
		r.Nodes = append(r.Nodes, PublicNode{NodeID: n.NodeID, Name: n.Name, Adapter: n.Adapter})
	}
	return r, true
}

func (c *Client) RoutingRegistry() RoutingRegistry {
	r := RoutingRegistry{
		RegistryVersion: c.manifest.RegistryVersion, OwnerID: c.manifest.OwnerID, Mode: c.manifest.Mode,
		ManifestSHA256: c.manifestSHA256, SchemaID: c.manifest.SchemaID,
		WireSchemaSHA256: c.manifest.WireSchemaSHA256, Nodes: []RoutingNode{},
	}
	for _, n := range c.manifest.Nodes {
		binding := struct {
			NodeID                string `json:"nodeId"`
			Name                  string `json:"name"`
			Adapter               string `json:"adapter"`
			URL                   string `json:"url"`
			CertificateSHA256     string `json:"certificateSHA256"`
			EndpointBindingSHA256 string `json:"endpointBindingSHA256,omitempty"`
		}{n.NodeID, n.Name, n.Adapter, n.URL, n.CertificateSHA256, n.EndpointBindingSHA256}
		canonical, _ := json.Marshal(binding)
		digest := sha256.Sum256(canonical)
		r.Nodes = append(r.Nodes, RoutingNode{
			NodeID: n.NodeID, Name: n.Name, Adapter: n.Adapter,
			RegistrationRevision: n.RegistrationRevision, RegistrationEpoch: n.RegistrationEpoch,
			Compatibility: n.Compatibility, BindingSHA256: hex.EncodeToString(digest[:]),
		})
	}
	return r
}

func (c *Client) Envelope() []byte {
	return append([]byte(nil), c.envelope...)
}

func (c *Client) registryVersion(node Node) int64 {
	if node.RegistrationRevision > 0 {
		return node.RegistrationRevision
	}
	return c.manifest.RegistryVersion
}

func (c *Client) node(id, owner string) (*entry, error) {
	if !actor.MatchString(owner) || owner != c.manifest.OwnerID {
		return nil, &Fault{Status: 404, Code: "not_found"}
	}
	e := c.nodes[id]
	if !uuid.MatchString(id) || e == nil {
		return nil, &Fault{Status: 404, Code: "not_found"}
	}
	return e, nil
}
