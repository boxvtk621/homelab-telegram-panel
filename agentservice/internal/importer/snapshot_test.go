package importer

import (
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

func TestDecodeSnapshotRejectsPartialOrDuplicateInput(t *testing.T) {
	manifest := model.RegistryManifest{Nodes: []model.RegistryNode{{NodeID: "10000000-0000-4000-8000-000000000001"}}}
	valid := []byte(`{
      "schemaId":"agent-registry-import-v1",
      "complete":true,
      "hosts":[{"hostId":"20000000-0000-4000-8000-000000000001","name":"Mac"}],
      "nodes":[{"nodeId":"10000000-0000-4000-8000-000000000001","hostId":"20000000-0000-4000-8000-000000000001","registrationMode":"legacy_readonly","observation":null,"dialogs":[]}]
    }`)
	if _, err := DecodeSnapshot(valid, manifest); err != nil {
		t.Fatal(err)
	}
	partial := []byte(`{"schemaId":"agent-registry-import-v1","complete":false,"hosts":[],"nodes":[]}`)
	if _, err := DecodeSnapshot(partial, manifest); err == nil {
		t.Fatal("partial snapshot accepted")
	}
	duplicate := []byte(`{"schemaId":"agent-registry-import-v1","schemaId":"agent-registry-import-v1","complete":true,"hosts":[],"nodes":[]}`)
	if _, err := DecodeSnapshot(duplicate, model.RegistryManifest{}); err == nil {
		t.Fatal("duplicate key accepted")
	}

	rich := `{
      "schemaId":"agent-registry-import-v1",
      "complete":true,
      "hosts":[{"hostId":"20000000-0000-4000-8000-000000000001","name":"Mac"}],
      "nodes":[{"nodeId":"10000000-0000-4000-8000-000000000001","hostId":"20000000-0000-4000-8000-000000000001","registrationMode":"compatible","observation":{"process":"running","connection":"online","readiness":"ready","occupancy":"idle","observedAt":"2026-09-14T10:00:00Z","source":"registry-import","pendingCount":0},"dialogs":[{"nodeDialogId":"30000000-0000-4000-8000-000000000001"}]}]
    }`
	if _, err := DecodeSnapshot([]byte(rich), manifest); err != nil {
		t.Fatal(err)
	}
	missingObservation := strings.Replace(string(valid), `"observation":null,`, "", 1)
	if _, err := DecodeSnapshot([]byte(missingObservation), manifest); err == nil {
		t.Fatal("missing required nullable observation accepted")
	}
	missingPendingCount := strings.Replace(rich, `,"pendingCount":0`, "", 1)
	if _, err := DecodeSnapshot([]byte(missingPendingCount), manifest); err == nil {
		t.Fatal("missing required nullable pendingCount accepted")
	}
	aliases := []struct {
		name string
		from string
		to   string
	}{
		{"snapshot", `"schemaId"`, `"SchemaID"`},
		{"hosts", `"hosts"`, `"Hosts"`},
		{"host", `"hostId"`, `"HostID"`},
		{"nodes", `"nodes"`, `"Nodes"`},
		{"node", `"nodeId"`, `"NodeID"`},
		{"registration", `"registrationMode"`, `"RegistrationMode"`},
		{"observation", `"observation"`, `"Observation"`},
		{"observation field", `"observedAt"`, `"ObservedAt"`},
		{"pending count", `"pendingCount"`, `"PendingCount"`},
		{"dialogs", `"dialogs"`, `"Dialogs"`},
		{"dialog", `"nodeDialogId"`, `"NodeDialogID"`},
	}
	for _, alias := range aliases {
		t.Run("case alias "+alias.name, func(t *testing.T) {
			mutated := strings.Replace(rich, alias.from, alias.to, 1)
			if _, err := DecodeSnapshot([]byte(mutated), manifest); err == nil {
				t.Fatalf("case alias %s accepted", alias.to)
			}
		})
	}
}
