package harnesstunnel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bindingFixture(node, host, address string) EndpointBinding {
	return EndpointBinding{
		NodeID: node, RegistrationRevision: 1, RegistrationEpoch: 1, EndpointRevision: 1,
		HostID: host, HostVersion: 1, Transport: "ssh", TargetRef: "host-one",
		CredentialRef: "ssh-one", DockerContextRef: "default",
		ExpectedHostKey:            "SHA256:abcdefghijklmnopqrstuvwx12345678",
		ExpectedHostIdentitySHA256: strings.Repeat("a", 64), HostPlatform: "linux", HostArchitecture: "amd64",
		ContainerID: strings.Repeat("b", 64), RuntimeGeneration: 1, Address: address,
	}
}

func TestManifestRejectsReusedHostPortAndRequiresPrivateFile(t *testing.T) {
	one := bindingFixture("20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", "127.0.0.1:9443")
	two := bindingFixture("20000000-0000-4000-8000-000000000002", one.HostID, one.Address)
	manifest := BindingManifest{SchemaID: BindingSchemaID, OwnerID: "owner-1", RegistrySHA256: strings.Repeat("c", 64), Nodes: []EndpointBinding{one, two}}
	if manifest.Validate() == nil {
		t.Fatal("reused port on the same host was accepted")
	}
	manifest.Nodes = []EndpointBinding{one}
	raw, _ := json.Marshal(manifest)
	path := filepath.Join(t.TempDir(), "bindings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err == nil {
		t.Fatal("world-readable endpoint bindings were accepted")
	}
}

func TestDialRequestCannotOverrideAdapterAddress(t *testing.T) {
	binding := bindingFixture("20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", "127.0.0.1:9443")
	request := DialRequest{
		SchemaID: RequestSchemaID, OwnerID: "owner-1", NodeID: binding.NodeID,
		RegistrationRevision: 1, RegistrationEpoch: 1, EndpointRevision: 2,
		Purpose: PurposeCommand, DeadlineUnixMilli: 1,
	}
	if request.Matches("owner-1", binding) {
		t.Fatal("stale endpoint revision matched")
	}
	raw, _ := json.Marshal(request)
	if strings.Contains(string(raw), "address") || strings.Contains(string(raw), "credential") || strings.Contains(string(raw), "targetRef") {
		t.Fatalf("dial request exposed adapter-owned routing: %s", raw)
	}
}

func TestStageExternalBindingIsAtomicIdempotentAndRegistryFenced(t *testing.T) {
	current := strings.Repeat("c", 64)
	candidate := strings.Repeat("d", 64)
	existing := bindingFixture("20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", "127.0.0.1:9443")
	manifest := BindingManifest{SchemaID: BindingSchemaID, OwnerID: "owner-1", RegistrySHA256: current, Nodes: []EndpointBinding{existing}}
	raw, _ := json.Marshal(manifest)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "bindings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	external := EndpointBinding{
		Kind: "external", NodeID: "20000000-0000-4000-8000-000000000002",
		RegistrationRevision: 1, RegistrationEpoch: 1, EndpointRevision: 1,
		HostID: "30000000-0000-4000-8000-000000000002", HostVersion: 4,
		Transport: "local", TargetRef: "local-two", Address: "10.20.30.40:9443",
	}
	if err := StageExternalBinding(path, "owner-1", current, candidate, external); err != nil {
		t.Fatal(err)
	}
	if err := StageExternalBinding(path, "owner-1", current, candidate, external); err != nil {
		t.Fatal("idempotent retry failed", err)
	}
	staged, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if staged.RegistrySHA256 != current || !staged.AcceptsRegistry(current) || !staged.AcceptsRegistry(candidate) || len(staged.Nodes) != 2 {
		t.Fatalf("unexpected staged manifest: %+v", staged)
	}
	conflict := external
	conflict.Address = "10.20.30.41:9443"
	if err := StageExternalBinding(path, "owner-1", current, candidate, conflict); err == nil {
		t.Fatal("conflicting node binding was accepted")
	}
	if err := StageExternalBinding(path, "owner-1", strings.Repeat("e", 64), candidate, external); err == nil {
		t.Fatal("stale registry anchor was accepted")
	}
	if err := UnstageExternalBinding(path, "owner-1", current, candidate, conflict); err == nil {
		t.Fatal("conflicting binding was unstaged")
	}
	if err := UnstageExternalBinding(path, "owner-1", current, candidate, external); err != nil {
		t.Fatal(err)
	}
	if err := UnstageExternalBinding(path, "owner-1", current, candidate, external); err != nil {
		t.Fatal("idempotent unstage failed", err)
	}
	unstaged, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(unstaged.Nodes) != 1 || !unstaged.AcceptsRegistry(current) || unstaged.AcceptsRegistry(candidate) {
		t.Fatalf("unexpected unstaged manifest: %+v", unstaged)
	}
}

func TestExternalSSHBindingAcceptsIPv6Loopback(t *testing.T) {
	binding := EndpointBinding{
		Kind: "external", NodeID: "20000000-0000-4000-8000-000000000002",
		RegistrationRevision: 1, RegistrationEpoch: 1, EndpointRevision: 1,
		HostID: "30000000-0000-4000-8000-000000000002", HostVersion: 4,
		Transport: "ssh", TargetRef: "ssh-two", CredentialRef: "credential-two",
		ExpectedHostKey: "SHA256:abcdefghijklmnopqrstuvwx12345678", Address: "[::1]:9443",
	}
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
}
