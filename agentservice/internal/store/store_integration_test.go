package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/registry"
)

func TestPostgresMigrationImportIdentityAndInventory(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	owner := fmt.Sprintf("hl282-%d", time.Now().UnixNano())

	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		t.Fatal("migration is not idempotent", err)
	}
	database.now = func() time.Time { return now }
	verified, snapshot := inventoryFixture(owner, 100, now)
	for index := 1; index <= 256; index++ {
		snapshot.Nodes[0].Dialogs = append(snapshot.Nodes[0].Dialogs, model.DialogSeed{NodeDialogID: fixtureUUID("31000000", index)})
	}
	result, err := database.Import(ctx, verified, snapshot)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if result.NodesSeen != 100 || result.DialogsSeen != 356 || result.MappingsCreated != 356 {
		database.Close()
		t.Fatalf("unexpected first import result: %+v", result)
	}
	items := listAll(t, ctx, database, owner, 37)
	if len(items) != 100 {
		database.Close()
		t.Fatalf("inventory size=%d", len(items))
	}
	hosts, statuses := map[string]bool{}, map[string]bool{}
	mappings := map[string]string{}
	for _, item := range items {
		hosts[item.Host.HostID] = true
		statuses[item.Status] = true
		bindings := listAllBindings(t, ctx, database, owner, item.NodeID, 37)
		expectedCount := int64(1)
		if item.NodeID == snapshot.Nodes[0].NodeID {
			expectedCount = 257
		}
		if item.DialogCount != expectedCount || int64(len(bindings)) != expectedCount || bindings[0].BindingVersion != 1 {
			database.Close()
			t.Fatalf("invalid initial mapping for %s: count=%d bindings=%d", item.NodeID, item.DialogCount, len(bindings))
		}
		mappings[item.NodeID] = bindings[0].LogicalDialogID
	}
	if len(hosts) != 10 {
		database.Close()
		t.Fatalf("host count=%d", len(hosts))
	}
	for _, status := range []string{"online", "unready", "busy", "stale", "stopped", "unknown", "readonly"} {
		if !statuses[status] {
			database.Close()
			t.Fatal("missing status", status)
		}
	}
	if items[0].PendingCount != nil || items[0].ObservedAt == nil || items[0].Source == nil {
		database.Close()
		t.Fatalf("nullable metric or provenance collapsed: %+v", items[0])
	}
	if items[2].PendingCount == nil || items[2].PendingCount.Value != 2 ||
		items[2].PendingCount.ObservedAt != *items[2].ObservedAt || items[2].PendingCount.Source != *items[2].Source {
		database.Close()
		t.Fatalf("metric provenance mismatch: %+v", items[2])
	}

	slices.Reverse(snapshot.Nodes)
	result, err = database.Import(ctx, verified, snapshot)
	if err != nil || result.MappingsCreated != 0 {
		database.Close()
		t.Fatalf("reimport changed identities: result=%+v err=%v", result, err)
	}
	conflict := verified
	conflict.ManifestSHA256 = strings.Repeat("b", 64)
	if _, err := database.Import(ctx, conflict, snapshot); err == nil {
		database.Close()
		t.Fatal("same registry version accepted with a different signed manifest")
	}
	upgrade := conflict
	upgrade.Manifest.RegistryVersion = 2
	if result, err := database.Import(ctx, upgrade, snapshot); err != nil || result.MappingsCreated != 0 {
		database.Close()
		t.Fatalf("registry upgrade changed identities: result=%+v err=%v", result, err)
	}
	if _, err := database.Import(ctx, verified, snapshot); err == nil {
		database.Close()
		t.Fatal("registry version regression was accepted")
	}
	database.Close()

	reopened, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.now = func() time.Time { return now }
	for _, item := range listAll(t, ctx, reopened, owner, 100) {
		bindings := listAllBindings(t, ctx, reopened, owner, item.NodeID, 100)
		if len(bindings) == 0 || mappings[item.NodeID] != bindings[0].LogicalDialogID {
			t.Fatalf("mapping changed after reopen for %s", item.NodeID)
		}
	}

	secondOwner := owner + "-other"
	secondVerified, secondSnapshot := inventoryFixture(secondOwner, 100, now)
	if _, err := reopened.Import(ctx, secondVerified, secondSnapshot); err != nil {
		t.Fatal(err)
	}
	other := listAll(t, ctx, reopened, secondOwner, 100)
	otherBindings := listAllBindings(t, ctx, reopened, secondOwner, other[0].NodeID, 100)
	if len(other) != 100 || len(otherBindings) != 1 || otherBindings[0].LogicalDialogID == mappings[other[0].NodeID] {
		t.Fatal("owner isolation did not create an independent identity namespace")
	}
	foreign, err := reopened.ListInventory(ctx, owner+"-absent", "", 100)
	if err != nil || len(foreign.Items) != 0 {
		t.Fatalf("foreign owner observed inventory: %+v %v", foreign, err)
	}

	rollbackOwner := owner + "-rollback"
	rollbackVerified, rollbackSnapshot := inventoryFixture(rollbackOwner, 2, now)
	failing := NewForPool(reopened.pool)
	calls := 0
	failing.newUUID = func() (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("synthetic identity failure")
		}
		return "90000000-0000-4000-8000-000000000001", nil
	}
	if _, err := failing.Import(ctx, rollbackVerified, rollbackSnapshot); err == nil {
		t.Fatal("partial import unexpectedly committed")
	}
	rolledBack, err := reopened.ListInventory(ctx, rollbackOwner, "", 100)
	if err != nil || len(rolledBack.Items) != 0 {
		t.Fatalf("failed import left partial rows: %+v %v", rolledBack, err)
	}

	var r02Columns int
	if err := reopened.pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema='agent_service' AND table_name='dialog_bindings'
		AND column_name IN ('generation','checkpoint_id')`).Scan(&r02Columns); err != nil || r02Columns != 0 {
		t.Fatalf("R02 columns present: count=%d err=%v", r02Columns, err)
	}
	var r02Tables int
	if err := reopened.pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema='agent_service'
		AND table_name IN ('operations','action_journal','desired_states','router_projections')`).Scan(&r02Tables); err != nil || r02Tables != 0 {
		t.Fatalf("R02 tables present: count=%d err=%v", r02Tables, err)
	}
}

func inventoryFixture(owner string, count int, now time.Time) (registry.Verified, model.ImportSnapshot) {
	manifest := model.RegistryManifest{RegistryVersion: 1, OwnerID: owner, Mode: "fixture", Nodes: make([]model.RegistryNode, 0, count)}
	snapshot := model.ImportSnapshot{SchemaID: model.ImportSchemaID, Complete: true, Hosts: make([]model.HostSeed, 10), Nodes: make([]model.NodeSeed, 0, count)}
	for index := range snapshot.Hosts {
		snapshot.Hosts[index] = model.HostSeed{HostID: fixtureUUID("10000000", index+1), Name: fmt.Sprintf("Mac %02d", index+1)}
	}
	for index := 1; index <= count; index++ {
		nodeID := fixtureUUID("20000000", index)
		manifest.Nodes = append(manifest.Nodes, model.RegistryNode{NodeID: nodeID, Name: fmt.Sprintf("Agent %03d", index), Adapter: []string{"cursor", "codex"}[index%2]})
		seed := model.NodeSeed{
			NodeID: nodeID, HostID: snapshot.Hosts[(index-1)%10].HostID, RegistrationMode: "compatible",
			Dialogs: []model.DialogSeed{{NodeDialogID: fixtureUUID("30000000", index)}},
		}
		recent := now.Add(-5 * time.Second).Format(time.RFC3339Nano)
		observation := &model.ObservationSeed{Process: "running", Connection: "online", Readiness: "ready", Occupancy: "idle", ObservedAt: recent, Source: "registry-import"}
		switch index {
		case 2:
			observation.Readiness = "unready"
		case 3:
			pending := int64(2)
			observation.Occupancy, observation.PendingCount = "busy", &pending
		case 4:
			observation.ObservedAt = now.Add(-16 * time.Second).Format(time.RFC3339Nano)
		case 5:
			observation.Process, observation.Connection, observation.Readiness, observation.Occupancy = "stopped", "offline", "unknown", "unknown"
		case 6:
			observation = nil
		case 7:
			seed.RegistrationMode, observation = "legacy_readonly", nil
		}
		seed.Observation = observation
		snapshot.Nodes = append(snapshot.Nodes, seed)
	}
	return registry.Verified{Manifest: manifest, ManifestSHA256: strings.Repeat("a", 64)}, snapshot
}

func listAll(t *testing.T, ctx context.Context, database *Store, owner string, limit int) []model.InventoryItem {
	t.Helper()
	var items []model.InventoryItem
	after := ""
	for {
		page, err := database.ListInventory(ctx, owner, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		if !page.HasMore {
			return items
		}
		if page.After == "" || page.After == after {
			t.Fatal("pagination did not advance")
		}
		after = page.After
	}
}

func listAllBindings(t *testing.T, ctx context.Context, database *Store, owner, nodeID string, limit int) []model.DialogMapping {
	t.Helper()
	var items []model.DialogMapping
	after := ""
	for {
		page, err := database.ListDialogBindings(ctx, owner, nodeID, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		if !page.HasMore {
			return items
		}
		if page.After == "" || page.After == after {
			t.Fatal("binding pagination did not advance")
		}
		after = page.After
	}
}

func fixtureUUID(prefix string, index int) string {
	return fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, index)
}
