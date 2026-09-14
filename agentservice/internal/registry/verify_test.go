package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

func signedRegistry(t *testing.T, count int) ([]byte, []byte, model.RegistryManifest) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.RegistryManifest{RegistryVersion: 7, OwnerID: "owner-1", Mode: "fixture", Nodes: make([]model.RegistryNode, count)}
	for index := range manifest.Nodes {
		digest := sha256.Sum256([]byte(fmt.Sprintf("certificate-%d", index)))
		manifest.Nodes[index] = model.RegistryNode{
			NodeID: fmt.Sprintf("10000000-0000-4000-8000-%012d", index+1),
			Name:   fmt.Sprintf("Agent %03d", index+1), Adapter: []string{"cursor", "codex"}[index%2],
			URL: fmt.Sprintf("https://agent-%03d.example", index+1), CertificateSHA256: hex.EncodeToString(digest[:]),
		}
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(signedManifest{Manifest: manifest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, canonical))})
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return raw, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), manifest
}

func TestVerifySupportsBeyondHundredAgentTestScaleWithoutTransport(t *testing.T) {
	raw, signer, manifest := signedRegistry(t, 101)
	verified, err := Verify(raw, signer)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified.Manifest.Nodes) != 101 || verified.ManifestSHA256 == "" {
		t.Fatalf("unexpected verified registry: nodes=%d sha=%q", len(verified.Manifest.Nodes), verified.ManifestSHA256)
	}
	for index, node := range verified.Manifest.Nodes {
		if node.NodeID != manifest.Nodes[index].NodeID {
			t.Fatal("node identity changed")
		}
	}
}

func TestVerifyRejectsTamperDuplicateAndCaseAliases(t *testing.T) {
	raw, signer, _ := signedRegistry(t, 1)
	tampered := strings.Replace(string(raw), "Agent 001", "Agent changed", 1)
	if _, err := Verify([]byte(tampered), signer); err == nil {
		t.Fatal("tampered registry accepted")
	}
	if UniqueJSON([]byte(`{"a":1,"a":2}`)) {
		t.Fatal("duplicate key accepted")
	}
	aliases := []struct {
		name string
		from string
		to   string
	}{
		{"envelope", `"manifest"`, `"Manifest"`},
		{"signature", `"signature"`, `"Signature"`},
		{"manifest", `"registryVersion"`, `"RegistryVersion"`},
		{"owner", `"ownerId"`, `"OwnerID"`},
		{"nodes", `"nodes"`, `"Nodes"`},
		{"node", `"nodeId"`, `"NodeID"`},
		{"certificate", `"certificateSHA256"`, `"CertificateSHA256"`},
	}
	for _, alias := range aliases {
		t.Run("case alias "+alias.name, func(t *testing.T) {
			mutated := strings.Replace(string(raw), alias.from, alias.to, 1)
			if _, err := Verify([]byte(mutated), signer); err == nil {
				t.Fatalf("case alias %s accepted", alias.to)
			}
		})
	}
}
