package migrations

import (
	"strings"
	"testing"
)

func TestR01ThroughR09MigrationCatalogIsAdditive(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[0].Version != 1 || all[1].Version != 2 || all[2].Version != 3 || all[3].Version != 4 {
		t.Fatalf("unexpected migration catalog: %#v", all)
	}
	for _, forbidden := range []string{"operation_steps", "transfer_manifests", "replica_checkpoints", "docker"} {
		if strings.Contains(strings.ToLower(all[0].SQL), forbidden) {
			t.Fatalf("R02+ entity %q leaked into R01 migration", forbidden)
		}
	}
	for _, required := range []string{"hosts", "instances", "logical_dialogs", "dialog_bindings", "retirement_descriptors"} {
		if !strings.Contains(all[0].SQL, required) {
			t.Errorf("missing R01 entity %q", required)
		}
	}
	for _, required := range []string{"registration_revision", "operation_generation", "operations", "operation_steps", "request_hash", "lease_expires_at"} {
		if !strings.Contains(all[1].SQL, required) {
			t.Errorf("missing R02 operation entity %q", required)
		}
	}
	if strings.Contains(strings.ToLower(all[1].SQL), "docker") {
		t.Fatal("R02 metadata migration contains a Docker effect")
	}
	for _, required := range []string{"host_descriptors", "host_version", "probe_revision", "target_ref", "expected_host_key", "identity_sha256", "registry_availability"} {
		if !strings.Contains(all[2].SQL, required) {
			t.Errorf("missing R07 host field %q", required)
		}
	}
	for _, forbidden := range []string{"private_key", "passphrase", "docker.sock", "create container", "pull image"} {
		if strings.Contains(strings.ToLower(all[2].SQL), forbidden) {
			t.Errorf("secret or Docker effect %q leaked into R07 metadata", forbidden)
		}
	}
	for _, required := range []string{"configuration_drafts", "raw_json_text", "raw_dockerfile_text", "build_context_manifest", "draft_version", "validation"} {
		if !strings.Contains(all[3].SQL, required) {
			t.Errorf("missing R09 draft field %q", required)
		}
	}
	for _, forbidden := range []string{"applied_revision", "private_key", "token", "password"} {
		if strings.Contains(strings.ToLower(all[3].SQL), forbidden) {
			t.Errorf("effect or secret %q leaked into R09 draft migration", forbidden)
		}
	}
}
