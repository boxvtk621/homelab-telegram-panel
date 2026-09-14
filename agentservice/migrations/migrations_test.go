package migrations

import (
	"strings"
	"testing"
)

func TestR01MigrationCatalogIsBounded(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Version != 1 {
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
}
