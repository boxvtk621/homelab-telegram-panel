// Package harnessclient is the non-authoritative, private mTLS client for nodes.
// It owns no queue, provider credential, execution, or durable receipt.
package harnessclient

import (
	"bytes"
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
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var actor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)

type Node struct {
	NodeID            string `json:"nodeId"`
	Name              string `json:"name"`
	Adapter           string `json:"adapter"`
	URL               string `json:"url"`
	CertificateSHA256 string `json:"certificateSHA256"`
}

// Manifest is operator-owned. The signature covers json.Marshal(Manifest),
// including its version, owner, mode, ordered nodes and leaf-certificate pins.
// Node URLs and certificate identities must never be sent to the browser.
type Manifest struct {
	RegistryVersion int64  `json:"registryVersion"`
	OwnerID         string `json:"ownerId"`
	Mode            string `json:"mode"`
	Nodes           []Node `json:"nodes"`
}

type SignedManifest struct {
	Manifest  Manifest `json:"manifest"`
	Signature string   `json:"signature"`
}

type Paths struct {
	Registry, SignerPublicKey, CA, ClientCertificate, ClientKey string
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

type entry struct {
	node      Node
	transport *http.Transport
	http      *http.Client
}

type Client struct {
	manifest Manifest
	nodes    map[string]*entry
}

func Empty() *Client {
	return &Client{manifest: Manifest{Mode: "live", Nodes: []Node{}}, nodes: map[string]*entry{}}
}

func Load(paths Paths) (*Client, error) {
	if paths.Registry == "" && paths.SignerPublicKey == "" && paths.CA == "" && paths.ClientCertificate == "" && paths.ClientKey == "" {
		return Empty(), nil
	}
	raw, err := readBounded(paths.Registry, 256<<10)
	if err != nil {
		return nil, errors.New("invalid Harness registry file")
	}
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
	return New(raw, signer, roots, cert)
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
	if len(raw) > 256<<10 || !strictjson.Valid(raw) || !registryShape(raw) || len(signer) != ed25519.PublicKeySize || roots == nil || len(cert.Certificate) == 0 || cert.PrivateKey == nil {
		return nil, errors.New("invalid Harness trust configuration")
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
	if m.RegistryVersion < 1 || m.RegistryVersion > hp.MaximumSafeInteger || !actor.MatchString(m.OwnerID) || (m.Mode != "live" && m.Mode != "fixture") || m.Nodes == nil || len(m.Nodes) > 16 {
		return nil, errors.New("invalid Harness registry identity")
	}
	c := &Client{manifest: m, nodes: make(map[string]*entry)}
	seenCerts := map[string]bool{}
	for _, n := range m.Nodes {
		u, err := url.Parse(n.URL)
		pin, pinErr := hex.DecodeString(n.CertificateSHA256)
		if !uuid.MatchString(n.NodeID) || n.Name == "" || !utf8.ValidString(n.Name) || len(n.Name) > 200 || strings.ContainsAny(n.Name, "\x00\r\n") || (n.Adapter != "cursor" && n.Adapter != "codex") || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path != "" || pinErr != nil || len(pin) != sha256.Size || strings.ToLower(n.CertificateSHA256) != n.CertificateSHA256 || seenCerts[n.CertificateSHA256] || c.nodes[n.NodeID] != nil {
			c.Close()
			return nil, errors.New("invalid Harness node binding")
		}
		seenCerts[n.CertificateSHA256] = true
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots.Clone(), Certificates: []tls.Certificate{cert}, ServerName: u.Hostname()}
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
				return errors.New("unverified Harness certificate")
			}
			sum := sha256.Sum256(state.PeerCertificates[0].Raw)
			if !bytes.Equal(sum[:], pin) {
				return errors.New("Harness certificate identity mismatch")
			}
			return nil
		}
		transport := &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 2 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 16, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 16, MaxResponseHeaderBytes: 16 << 10, DisableCompression: true}
		c.nodes[n.NodeID] = &entry{node: n, transport: transport, http: &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	}
	return c, nil
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
	manifest, ok := registryObject(envelope["manifest"], "registryVersion", "ownerId", "mode", "nodes")
	if !ok {
		return false
	}
	var nodes []json.RawMessage
	if json.Unmarshal(manifest["nodes"], &nodes) != nil || nodes == nil {
		return false
	}
	for _, node := range nodes {
		if _, ok := registryObject(node, "nodeId", "name", "adapter", "url", "certificateSHA256"); !ok {
			return false
		}
	}
	return true
}

func (c *Client) Close() {
	for _, e := range c.nodes {
		e.transport.CloseIdleConnections()
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
