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
	"reflect"
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
	raw, err := json.Marshal(struct {
		Manifest  model.RegistryManifest `json:"manifest"`
		Signature string                 `json:"signature"`
	}{Manifest: manifest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, canonical))})
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return raw, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), manifest
}

func TestVerifyAcceptsExactProjectedRegistry(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("projected-certificate"))
	manifest := model.RegistryManifest{
		SchemaID: routerRegistrySchema, RegistryVersion: 9, OwnerID: "owner-1", Mode: "fixture",
		WireSchemaSHA256: harnessWireSHA256,
		Nodes: []model.RegistryNode{{
			NodeID: "10000000-0000-4000-8000-000000000001", Name: "Projected", Adapter: "codex",
			URL: "https://projected.example", CertificateSHA256: hex.EncodeToString(digest[:]),
			RegistrationRevision: 4, RegistrationEpoch: 8, Compatibility: "compatible",
		}},
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Manifest  model.RegistryManifest `json:"manifest"`
		Signature string                 `json:"signature"`
	}{Manifest: manifest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, canonical))})
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(raw, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	expectedDigest := sha256.Sum256(canonical)
	if err != nil || !reflect.DeepEqual(verified.Manifest, manifest) || verified.ManifestSHA256 != hex.EncodeToString(expectedDigest[:]) {
		t.Fatal("projected registry was not preserved exactly", verified, err)
	}
}

func TestVerifyPreservesByteBoundedLegacyRegistryBeyondDynamicNodeCap(t *testing.T) {
	raw, signer, manifest := signedRegistry(t, 1001)
	if len(raw) > maximumRegistryBytes {
		t.Fatalf("legacy compatibility fixture exceeds byte contract: %d", len(raw))
	}
	verified, err := Verify(raw, signer)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified.Manifest.Nodes) != 1001 || verified.ManifestSHA256 == "" {
		t.Fatalf("unexpected verified registry: nodes=%d sha=%q", len(verified.Manifest.Nodes), verified.ManifestSHA256)
	}
	for index, node := range verified.Manifest.Nodes {
		if node.NodeID != manifest.Nodes[index].NodeID {
			t.Fatal("node identity changed")
		}
	}
}

func TestVerifyAcceptsLegacyEmptyRegistryForFirstProjection(t *testing.T) {
	raw, signer, manifest := signedRegistry(t, 0)
	verified, err := Verify(raw, signer)
	if err != nil {
		t.Fatal("R01-compatible empty legacy registry was rejected", err)
	}
	if len(verified.Manifest.Nodes) != 0 || !reflect.DeepEqual(verified.Manifest, manifest) || len(verified.Envelope) == 0 {
		t.Fatalf("empty registry changed: %+v", verified)
	}
}

func TestProjectionDeltaRejectsRevisionOnlyBumpAndNonInitialNodeEpoch(t *testing.T) {
	current := model.RegistryManifest{
		SchemaID: routerRegistrySchema, RegistryVersion: 2, OwnerID: "owner-1", Mode: "fixture",
		WireSchemaSHA256: harnessWireSHA256,
		Nodes: []model.RegistryNode{{
			NodeID: "10000000-0000-4000-8000-000000000001", Name: "Agent", Adapter: "codex",
			URL: "https://agent.example", CertificateSHA256: strings.Repeat("a", 64),
			RegistrationRevision: 1, RegistrationEpoch: 1, Compatibility: "compatible",
		}},
	}
	revisionOnly := current
	revisionOnly.RegistryVersion = 3
	revisionOnly.Nodes = append([]model.RegistryNode(nil), current.Nodes...)
	revisionOnly.Nodes[0].RegistrationRevision = 2
	revisionOnly.Nodes[0].RegistrationEpoch = 2
	if _, _, err := ProjectionDelta(current, revisionOnly); err == nil {
		t.Fatal("revision-only node bump was accepted")
	}
	badAddition := current
	badAddition.RegistryVersion = 3
	badAddition.Nodes = append([]model.RegistryNode(nil), current.Nodes...)
	badAddition.Nodes = append(badAddition.Nodes, model.RegistryNode{
		NodeID: "10000000-0000-4000-8000-000000000002", Name: "New", Adapter: "cursor",
		URL: "https://new.example", CertificateSHA256: strings.Repeat("b", 64),
		RegistrationRevision: 1, RegistrationEpoch: 2, Compatibility: "compatible",
	})
	if _, _, err := ProjectionDelta(current, badAddition); err == nil {
		t.Fatal("new node with a non-initial epoch was accepted")
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
