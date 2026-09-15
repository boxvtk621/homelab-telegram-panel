package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
	if _, err := database.pool.Exec(ctx, `UPDATE agent_service.registry_state SET registry_envelope=NULL WHERE owner_id=$1`, owner); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		database.Close()
		t.Fatal("an exact R01 reimport did not initialize the R02 envelope", err)
	}
	if envelope, version, digest, err := database.GetRegistryEnvelope(ctx, owner); err != nil || version != 1 || digest != verified.ManifestSHA256 || !json.Valid(envelope) {
		database.Close()
		t.Fatalf("R01 envelope upgrade unavailable: version=%d hash=%q envelope=%s err=%v", version, digest, envelope, err)
	}
	upgrade := conflict
	upgrade.Manifest.RegistryVersion = 2
	if _, err := database.Import(ctx, upgrade, snapshot); err == nil {
		database.Close()
		t.Fatal("registry upgrade bypassed the coordinated operation path")
	}
	if result, err := database.Import(ctx, verified, snapshot); err != nil || result.MappingsCreated != 0 {
		database.Close()
		t.Fatalf("exact registry observation refresh failed after rejected bypass: result=%+v err=%v", result, err)
	}
	changed := upgrade
	changed.Manifest.RegistryVersion = 3
	changed.ManifestSHA256 = strings.Repeat("c", 64)
	changed.Manifest.Nodes = append([]model.RegistryNode(nil), upgrade.Manifest.Nodes...)
	changed.Manifest.Nodes[0].Name = "Changed Agent"
	if _, err := database.Import(ctx, changed, snapshot); err == nil {
		database.Close()
		t.Fatal("single-node registry mutation bypassed the coordinated operation path")
	}
	var changedRevision, untouchedRevision int64
	if err := database.pool.QueryRow(ctx, `SELECT registration_revision FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2`, owner, changed.Manifest.Nodes[0].NodeID).Scan(&changedRevision); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.pool.QueryRow(ctx, `SELECT registration_revision FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2`, owner, changed.Manifest.Nodes[1].NodeID).Scan(&untouchedRevision); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if changedRevision != 1 || untouchedRevision != 1 {
		database.Close()
		t.Fatalf("rejected registry mutation changed revisions: changed=%d untouched=%d", changedRevision, untouchedRevision)
	}
	compatibilityNodeID := changed.Manifest.Nodes[1].NodeID
	for index := range snapshot.Nodes {
		if snapshot.Nodes[index].NodeID == compatibilityNodeID {
			snapshot.Nodes[index].RegistrationMode = "legacy_readonly"
		}
	}
	if _, err := database.Import(ctx, verified, snapshot); err == nil {
		database.Close()
		t.Fatal("registration mode mutation bypassed the coordinated operation path")
	}
	if err := database.pool.QueryRow(ctx, `SELECT registration_revision FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2`, owner, compatibilityNodeID).Scan(&untouchedRevision); err != nil || untouchedRevision != 1 {
		database.Close()
		t.Fatalf("rejected compatibility mutation changed revision=%d err=%v", untouchedRevision, err)
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

	var protectedBindingColumns int
	if err := reopened.pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema='agent_service' AND table_name='dialog_bindings'
			AND column_name IN ('generation','checkpoint_id')`).Scan(&protectedBindingColumns); err != nil || protectedBindingColumns != 0 {
		t.Fatalf("R02 mutated permanent binding identity: count=%d err=%v", protectedBindingColumns, err)
	}
	var r02Tables int
	if err := reopened.pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			WHERE table_schema='agent_service'
			AND table_name IN ('operations','operation_steps')`).Scan(&r02Tables); err != nil || r02Tables != 2 {
		t.Fatalf("R02 operation tables missing: count=%d err=%v", r02Tables, err)
	}
}

func TestPostgresOperationCASReceiptLeaseAndRestart(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 123456789, time.UTC)
	owner := fmt.Sprintf("hl283-%d", time.Now().UnixNano())
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		t.Fatal(err)
	}
	database.now = func() time.Time { return now }
	verified, snapshot := inventoryFixture(owner, 1, now)
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		database.Close()
		t.Fatal(err)
	}
	targetStatus, err := database.GetOperationTarget(ctx, owner, snapshot.Nodes[0].NodeID)
	if err != nil || targetStatus.Target.HostID != snapshot.Nodes[0].HostID || targetStatus.Target.Generation != 0 ||
		targetStatus.Target.RegistrationRevision != 1 || targetStatus.Target.RegistrationEpoch != 1 {
		database.Close()
		t.Fatalf("exact operation target=%+v err=%v", targetStatus, err)
	}
	readonlyOwner := owner + "-readonly"
	readonlyVerified, readonlySnapshot := inventoryFixture(readonlyOwner, 1, now)
	readonlySnapshot.Nodes[0].RegistrationMode = "legacy_readonly"
	if _, err := database.Import(ctx, readonlyVerified, readonlySnapshot); err != nil {
		database.Close()
		t.Fatal(err)
	}
	readonlyIntent := model.OperationIntent{
		SchemaID: model.OperationSchemaID, OperationID: "op-readonly", Kind: "adapter.fixture",
		Target: model.OperationTarget{
			NodeID: readonlySnapshot.Nodes[0].NodeID, HostID: readonlySnapshot.Nodes[0].HostID,
			RegistrationRevision: 1, RegistrationEpoch: 1, Generation: 0,
		},
		Step: model.OperationStepIntent{StepID: "apply-readonly", Action: "adapter.fixture.apply", ResourceIDs: []string{"container:readonly"}},
	}
	if _, err := database.AcceptOperation(ctx, readonlyOwner, readonlyIntent); !errors.Is(err, ErrOperationConflict) {
		database.Close()
		t.Fatal("fixture effect was accepted for a legacy-readonly node", err)
	}
	liveOwner := owner + "-live"
	liveVerified, liveSnapshot := inventoryFixture(liveOwner, 1, now)
	liveVerified.Manifest.Mode = "live"
	if _, err := database.Import(ctx, liveVerified, liveSnapshot); err != nil {
		database.Close()
		t.Fatal(err)
	}
	liveIntent := readonlyIntent
	liveIntent.OperationID = "op-live-fixture"
	liveIntent.Target = model.OperationTarget{
		NodeID: liveSnapshot.Nodes[0].NodeID, HostID: liveSnapshot.Nodes[0].HostID,
		RegistrationRevision: 1, RegistrationEpoch: 1, Generation: 0,
	}
	liveIntent.Kind = "adapter.fixture"
	liveIntent.Step.StepID = "apply-live"
	liveIntent.Step.Action = "adapter.fixture.apply"
	if _, err := database.AcceptOperation(ctx, liveOwner, liveIntent); !errors.Is(err, ErrOperationConflict) {
		database.Close()
		t.Fatal("fixture effect was accepted for a live registry", err)
	}
	intent := model.OperationIntent{
		SchemaID: model.OperationSchemaID, OperationID: "op-cas-a", Kind: "adapter.fixture",
		Target: model.OperationTarget{
			NodeID: snapshot.Nodes[0].NodeID, HostID: snapshot.Nodes[0].HostID,
			RegistrationRevision: 1, RegistrationEpoch: 1, Generation: 0,
		},
		Step: model.OperationStepIntent{StepID: "apply-1", Action: "adapter.fixture.apply", ResourceIDs: []string{"container:agent-1"}},
	}
	for name, mutate := range map[string]func(*model.OperationIntent){
		"host":  func(value *model.OperationIntent) { value.Target.HostID = "90000000-0000-4000-8000-000000000009" },
		"epoch": func(value *model.OperationIntent) { value.Target.RegistrationEpoch++ },
	} {
		t.Run("stale target "+name, func(t *testing.T) {
			stale := intent
			stale.OperationID = "op-stale-" + name
			mutate(&stale)
			if _, err := database.AcceptOperation(ctx, owner, stale); !errors.Is(err, ErrOperationConflict) {
				t.Fatal("stale exact target was accepted", err)
			}
		})
	}
	competitor := intent
	competitor.OperationID = "op-cas-b"
	competitor.Step.StepID = "apply-2"
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, candidate := range []model.OperationIntent{intent, competitor} {
		candidate := candidate
		go func() {
			<-start
			_, acceptErr := database.AcceptOperation(ctx, owner, candidate)
			results <- acceptErr
		}()
	}
	close(start)
	winner := intent
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ErrOperationConflict) {
			conflicts++
		} else {
			database.Close()
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		database.Close()
		t.Fatalf("CAS winners=%d conflicts=%d", successes, conflicts)
	}
	if _, err := database.GetOperation(ctx, owner, intent.OperationID); errors.Is(err, ErrOperationNotFound) {
		winner = competitor
	} else if err != nil {
		database.Close()
		t.Fatal(err)
	}
	first, err := database.AcceptOperation(ctx, owner, winner)
	if err != nil || !first.Existing {
		database.Close()
		t.Fatalf("idempotent accept result=%+v err=%v", first, err)
	}
	conflictingPayload := winner
	conflictingPayload.Step.ResourceIDs = []string{"container:agent-2"}
	if _, err := database.AcceptOperation(ctx, owner, conflictingPayload); !errors.Is(err, ErrOperationConflict) {
		database.Close()
		t.Fatal("same operation ID with different payload was not rejected", err)
	}
	database.Close()

	reopened, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.now = func() time.Time { return now }
	status, err := reopened.GetOperation(ctx, owner, winner.OperationID)
	if err != nil || status.Receipt != first.Receipt || status.Phase != "accepted" || status.EffectState != "not_sent" {
		t.Fatalf("restart receipt/status=%+v err=%v", status, err)
	}
	claimed, err := reopened.ClaimNextOperation(ctx, owner, winner.Target.NodeID, "worker-a", 2*time.Second)
	if err != nil || claimed.EffectState != "not_sent" || claimed.Intent.OperationID != winner.OperationID || !reflect.DeepEqual(claimed.Intent, winner) {
		t.Fatalf("durable claimed work=%+v err=%v", claimed, err)
	}
	claimA := claimed.Claim
	wrongIntent := claimA
	wrongIntent.RequestHash = strings.Repeat("f", 64)
	if err := reopened.CheckOperationAuthority(ctx, owner, winner.Target.NodeID, wrongIntent); !errors.Is(err, ErrStaleWorker) {
		t.Fatal("different durable intent hash retained effect authority", err)
	}
	if _, err := reopened.MarkOperationSent(ctx, owner, wrongIntent); !errors.Is(err, ErrStaleWorker) {
		t.Fatal("different durable intent hash crossed the sent boundary", err)
	}
	if err := reopened.CheckOperationAuthority(ctx, owner, winner.Target.NodeID, claimA); err != nil {
		t.Fatal("current operation authority was rejected", err)
	}
	claimA, err = reopened.MarkOperationSent(ctx, owner, claimA)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Second)
	claimB, err := reopened.ClaimOperation(ctx, owner, winner.OperationID, "worker-b", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.CheckOperationAuthority(ctx, owner, winner.Target.NodeID, claimA); !errors.Is(err, ErrStaleWorker) {
		t.Fatal("expired worker retained effect authority", err)
	}
	if err := reopened.CheckOperationAuthority(ctx, owner, winner.Target.NodeID, claimB); err != nil {
		t.Fatal("replacement worker did not receive effect authority", err)
	}
	if _, err := reopened.AdvanceOperation(ctx, owner, claimA, OperationUpdate{Phase: "succeeded", EffectState: "acknowledged"}); !errors.Is(err, ErrStaleWorker) {
		t.Fatal("late stale worker committed", err)
	}
	if _, err := reopened.AdvanceOperation(ctx, owner, claimB, OperationUpdate{Phase: "reconciling", EffectState: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CheckOperationAuthority(ctx, owner, winner.Target.NodeID, claimB); !errors.Is(err, ErrStaleWorker) {
		t.Fatal("released unknown worker retained effect authority", err)
	}
	next := winner
	next.OperationID, next.Step.StepID, next.Target.Generation = "op-next", "apply-next", 1
	if _, err := reopened.AcceptOperation(ctx, owner, next); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("unknown operation did not block next generation", err)
	}
	changedRegistry := verified
	changedRegistry.Manifest.RegistryVersion = 2
	changedRegistry.ManifestSHA256 = strings.Repeat("b", 64)
	changedRegistry.Manifest.Nodes = append([]model.RegistryNode(nil), verified.Manifest.Nodes...)
	changedRegistry.Manifest.Nodes[0].Name = "Unsafe concurrent registration"
	if _, err := reopened.Import(ctx, changedRegistry, snapshot); err == nil {
		t.Fatal("active unknown operation allowed target registration to change")
	}
	hostMove := snapshot
	hostMove.Hosts = append([]model.HostSeed(nil), snapshot.Hosts...)
	hostMove.Nodes = append([]model.NodeSeed(nil), snapshot.Nodes...)
	newHost := model.HostSeed{HostID: "90000000-0000-4000-8000-000000000001", Name: "Moved Host"}
	hostMove.Hosts = append(hostMove.Hosts, newHost)
	hostMove.Nodes[0].HostID = newHost.HostID
	if _, err := reopened.Import(ctx, verified, hostMove); err == nil {
		t.Fatal("active unknown operation allowed target host binding to change")
	}
	liveRegistry := verified
	liveRegistry.Manifest.Mode = "live"
	liveRegistry.Manifest.RegistryVersion = 2
	liveRegistry.ManifestSHA256 = strings.Repeat("d", 64)
	if _, err := reopened.Import(ctx, liveRegistry, snapshot); err == nil {
		t.Fatal("active unknown operation allowed target registry mode to change")
	}
	var revision int64
	if err := reopened.pool.QueryRow(ctx, `SELECT registration_revision FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2`, owner, snapshot.Nodes[0].NodeID).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("rejected registration changed target revision=%d err=%v", revision, err)
	}
	recovery, err := reopened.ClaimNextOperation(ctx, owner, winner.Target.NodeID, "worker-c", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.EffectState != "unknown" || !reflect.DeepEqual(recovery.Intent, winner) {
		t.Fatal("recovery claim did not retain exact unknown work", recovery)
	}
	claimC := recovery.Claim
	if err := reopened.CheckOperationAuthority(ctx, owner, winner.Target.NodeID, claimC); err != nil {
		t.Fatal("reconciliation worker did not receive authority", err)
	}
	for _, effectState := range []string{"not_sent", "sent", "acknowledged"} {
		if _, err := reopened.AdvanceOperation(ctx, owner, claimC, OperationUpdate{Phase: "reconciling", EffectState: effectState}); !errors.Is(err, ErrOperationConflict) {
			t.Fatalf("unknown operation regressed to %s: %v", effectState, err)
		}
	}
	if _, err := reopened.AdvanceOperation(ctx, owner, claimC, OperationUpdate{Phase: "succeeded", EffectState: "reconciled"}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CheckOperationAuthority(ctx, owner, winner.Target.NodeID, claimC); !errors.Is(err, ErrStaleWorker) {
		t.Fatal("terminal worker retained effect authority", err)
	}
	if accepted, err := reopened.AcceptOperation(ctx, owner, next); err != nil || accepted.Receipt.AcceptedGeneration != 2 {
		t.Fatalf("resolved operation did not release next generation: %+v %v", accepted, err)
	}
}

func TestPostgresLegacyRegistryAdoptsSignedProjectionOnceAndNeverDowngrades(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	owner := fmt.Sprintf("hl283-projection-%d", time.Now().UnixNano())
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	database.now = func() time.Time { return now }

	legacy, snapshot := inventoryFixture(owner, 1, now)
	legacy.Manifest.RegistryVersion = 7
	legacy.ManifestSHA256 = strings.Repeat("7", 64)
	legacy = withFixtureEnvelope(legacy)
	if _, err := database.Import(ctx, legacy, snapshot); err != nil {
		t.Fatal("legacy seed failed", err)
	}
	bindings := listAllBindings(t, ctx, database, owner, snapshot.Nodes[0].NodeID, 10)
	if len(bindings) != 1 {
		t.Fatal("legacy dialog binding missing")
	}

	projected := legacy
	projected.Manifest.SchemaID = "harness-router-registry-v1"
	projected.Manifest.RegistryVersion = 8
	projected.Manifest.WireSchemaSHA256 = "5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9"
	projected.Manifest.Nodes = append([]model.RegistryNode(nil), legacy.Manifest.Nodes...)
	projected.Manifest.Nodes[0].RegistrationRevision = 7
	projected.Manifest.Nodes[0].RegistrationEpoch = 9
	projected.Manifest.Nodes[0].Compatibility = "compatible"
	projected.ManifestSHA256 = strings.Repeat("8", 64)
	projected = withFixtureEnvelope(projected)
	affected, newNodeID, err := registry.ProjectionDelta(legacy.Manifest, projected.Manifest)
	if err != nil || newNodeID != "" {
		t.Fatal("legacy projection delta failed", affected, newNodeID, err)
	}
	slices.Sort(affected)
	intent := registryIntent("registry-adopt", legacy, projected, nil)
	reserved, err := database.ReserveRegistryOperation(ctx, owner, intent, projected, affected, newNodeID)
	if err != nil || reserved.Existing {
		t.Fatalf("one-time projection reservation failed: result=%+v err=%v", reserved, err)
	}
	sent, err := database.MarkRegistryOperationSent(ctx, owner, registryCommand(reserved.Status))
	if err != nil {
		t.Fatal(err)
	}
	finished, err := database.FinishRegistryOperation(ctx, owner, registryFinish(sent, projected, "acknowledged"), projected)
	if err != nil || finished.Phase != "succeeded" || finished.EffectState != "acknowledged" {
		t.Fatalf("one-time projection finish failed: status=%+v err=%v", finished, err)
	}
	repeated, err := database.ReserveRegistryOperation(ctx, owner, intent, projected, affected, newNodeID)
	if err != nil || !repeated.Existing || !reflect.DeepEqual(repeated.Status, finished) {
		t.Fatalf("terminal registry receipt changed: result=%+v err=%v", repeated, err)
	}
	if result, err := database.Import(ctx, projected, snapshot); err != nil || result.MappingsCreated != 0 {
		t.Fatalf("identical projected reimport failed: result=%+v err=%v", result, err)
	}

	changed := projected
	changed.Manifest.RegistryVersion = 9
	changed.Manifest.Nodes = append([]model.RegistryNode(nil), projected.Manifest.Nodes...)
	changed.Manifest.Nodes[0].Name = "Projected Agent"
	changed.Manifest.Nodes[0].RegistrationRevision = 8
	changed.Manifest.Nodes[0].RegistrationEpoch = 10
	changed.ManifestSHA256 = strings.Repeat("9", 64)
	changed = withFixtureEnvelope(changed)
	affected, newNodeID, err = registry.ProjectionDelta(projected.Manifest, changed.Manifest)
	if err != nil || newNodeID != "" {
		t.Fatal("changed projection delta failed", affected, newNodeID, err)
	}
	slices.Sort(affected)
	changeIntent := registryIntent("registry-change", projected, changed, nil)
	changeReserved, err := database.ReserveRegistryOperation(ctx, owner, changeIntent, changed, affected, newNodeID)
	if err != nil {
		t.Fatal("next signed per-node reservation failed", err)
	}
	changeSent, err := database.MarkRegistryOperationSent(ctx, owner, registryCommand(changeReserved.Status))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinishRegistryOperation(ctx, owner, registryFinish(changeSent, changed, "acknowledged"), changed); err != nil {
		t.Fatal("next signed per-node revision failed", err)
	}

	var revision, epoch int64
	var projectedFlag bool
	if err := database.pool.QueryRow(ctx, `
		SELECT registration_revision,registration_epoch,registration_projected
		FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2`,
		owner, snapshot.Nodes[0].NodeID,
	).Scan(&revision, &epoch, &projectedFlag); err != nil || revision != 8 || epoch != 10 || !projectedFlag {
		t.Fatalf("projected fence revision=%d epoch=%d projected=%t err=%v", revision, epoch, projectedFlag, err)
	}
	after := listAllBindings(t, ctx, database, owner, snapshot.Nodes[0].NodeID, 10)
	if len(after) != 1 || after[0] != bindings[0] {
		t.Fatalf("projection changed permanent dialog identity: before=%+v after=%+v", bindings, after)
	}

	downgrade := legacy
	downgrade.Manifest.RegistryVersion = 10
	downgrade.ManifestSHA256 = strings.Repeat("a", 64)
	if _, err := database.Import(ctx, downgrade, snapshot); err == nil {
		t.Fatal("projected registry downgraded to legacy")
	}
}

func TestPostgresRegistryOperationRecoveryIsolationAndGlobalIdempotency(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	now := time.Date(2026, 9, 14, 14, 0, 0, 123456789, time.UTC)
	owner := fmt.Sprintf("hl283-registry-%d", time.Now().UnixNano())
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		t.Fatal(err)
	}
	database.now = func() time.Time { return now }

	legacy, snapshot := inventoryFixture(owner, 2, now)
	pendingBeforeIdentityChange := int64(3)
	snapshot.Nodes[0].Observation.Occupancy = "busy"
	snapshot.Nodes[0].Observation.PendingCount = &pendingBeforeIdentityChange
	if _, err := database.Import(ctx, legacy, snapshot); err != nil {
		database.Close()
		t.Fatal(err)
	}
	beforeAdoption := listAll(t, ctx, database, owner, 10)
	projected := legacy
	projected.Manifest.SchemaID = "harness-router-registry-v1"
	projected.Manifest.RegistryVersion = 2
	projected.Manifest.WireSchemaSHA256 = "5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9"
	projected.Manifest.Nodes = append([]model.RegistryNode(nil), legacy.Manifest.Nodes...)
	for index := range projected.Manifest.Nodes {
		projected.Manifest.Nodes[index].RegistrationRevision = 1
		projected.Manifest.Nodes[index].RegistrationEpoch = 1
		projected.Manifest.Nodes[index].Compatibility = "compatible"
	}
	projected.ManifestSHA256 = strings.Repeat("b", 64)
	projected = withFixtureEnvelope(projected)
	affected, newNodeID, err := registry.ProjectionDelta(legacy.Manifest, projected.Manifest)
	if err != nil || newNodeID != "" {
		database.Close()
		t.Fatal("projection delta failed", affected, newNodeID, err)
	}
	slices.Sort(affected)
	adoption, err := database.ReserveRegistryOperation(ctx, owner, registryIntent("registry-adopt-all", legacy, projected, nil), projected, affected, "")
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	adoptionSent, err := database.MarkRegistryOperationSent(ctx, owner, registryCommand(adoption.Status))
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	adoptionDone, err := database.FinishRegistryOperation(ctx, owner, registryFinish(adoptionSent, projected, "acknowledged"), projected)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	afterAdoption := listAll(t, ctx, database, owner, 10)
	if !reflect.DeepEqual(afterAdoption, beforeAdoption) {
		database.Close()
		t.Fatalf("legacy projection reset unchanged observations: before=%+v after=%+v", beforeAdoption, afterAdoption)
	}

	neighborNodeID := snapshot.Nodes[1].NodeID
	neighborBindings := listAllBindings(t, ctx, database, owner, neighborNodeID, 10)
	neighborTarget, err := database.GetOperationTarget(ctx, owner, neighborNodeID)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	neighborIntent := model.OperationIntent{
		SchemaID: model.OperationSchemaID, OperationID: "adapter-neighbor", Kind: "adapter.fixture",
		Target: neighborTarget.Target,
		Step:   model.OperationStepIntent{StepID: "apply-neighbor", Action: "adapter.fixture.apply", ResourceIDs: []string{"container:neighbor"}},
	}
	neighborAccepted, err := database.AcceptOperation(ctx, owner, neighborIntent)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}

	changed := projected
	changed.Manifest.RegistryVersion = 3
	changed.Manifest.Nodes = append([]model.RegistryNode(nil), projected.Manifest.Nodes...)
	changed.Manifest.Nodes[0].Name = "Changed Agent"
	changed.Manifest.Nodes[0].RegistrationRevision++
	changed.Manifest.Nodes[0].RegistrationEpoch++
	changed.ManifestSHA256 = strings.Repeat("c", 64)
	changed = withFixtureEnvelope(changed)
	affected, newNodeID, err = registry.ProjectionDelta(projected.Manifest, changed.Manifest)
	if err != nil || newNodeID != "" || len(affected) != 1 || affected[0] != snapshot.Nodes[0].NodeID {
		database.Close()
		t.Fatal("single-node delta changed", affected, newNodeID, err)
	}
	changeIntent := registryIntent("registry-change-one", projected, changed, nil)
	change, err := database.ReserveRegistryOperation(ctx, owner, changeIntent, changed, affected, "")
	if err != nil {
		database.Close()
		t.Fatal("unrelated active adapter operation blocked registry change", err)
	}
	if _, err := database.AcceptOperation(ctx, owner, model.OperationIntent{
		SchemaID: model.OperationSchemaID, OperationID: "adapter-affected", Kind: "adapter.fixture",
		Target: model.OperationTarget{
			NodeID: snapshot.Nodes[0].NodeID, HostID: snapshot.Nodes[0].HostID,
			RegistrationRevision: 1, RegistrationEpoch: 1, Generation: 0,
		},
		Step: model.OperationStepIntent{StepID: "apply-affected", Action: "adapter.fixture.apply", ResourceIDs: []string{"container:affected"}},
	}); !errors.Is(err, ErrOperationConflict) {
		database.Close()
		t.Fatal("registry reservation did not fence its affected node", err)
	}
	storedIntent, err := database.GetRegistryOperationIntent(ctx, owner, changeIntent.OperationID)
	if err != nil || model.ValidateRegistryOperationIntent(storedIntent) != nil {
		database.Close()
		t.Fatal("JSONB-reordered durable intent failed semantic hash readback", err)
	}
	changeSent, err := database.MarkRegistryOperationSent(ctx, owner, registryCommand(change.Status))
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	changeUnknown, err := database.MarkRegistryOperationUnknown(ctx, owner, registryCommand(changeSent))
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.FinishRegistryOperation(ctx, owner, registryFinish(changeUnknown, changed, "acknowledged"), changed); !errors.Is(err, ErrOperationConflict) {
		database.Close()
		t.Fatal("unknown Router result was acknowledged without reconciliation", err)
	}
	failure := model.RegistryOperationFailure{
		SchemaID: model.RegistryOperationFailureSchemaID, OperationID: changeUnknown.Receipt.OperationID,
		RequestHash: changeUnknown.Receipt.RequestHash, OperationVersion: changeUnknown.OperationVersion,
		ResultCode: "router.node_not_sealed",
	}
	if _, err := database.FailRegistryOperation(ctx, owner, failure); !errors.Is(err, ErrOperationConflict) {
		database.Close()
		t.Fatal("unknown Router result was terminalized as a known failure", err)
	}
	database.Close()

	database, err = Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.now = func() time.Time { return now }
	restored, err := database.GetRegistryOperation(ctx, owner, changeIntent.OperationID)
	if err != nil || !reflect.DeepEqual(restored, changeUnknown) {
		t.Fatalf("unknown registry operation was not restored: status=%+v err=%v", restored, err)
	}
	changeDone, err := database.FinishRegistryOperation(ctx, owner, registryFinish(restored, changed, "reconciled"), changed)
	if err != nil || changeDone.Phase != "succeeded" || changeDone.EffectState != "reconciled" {
		t.Fatalf("exact Router reconciliation failed: status=%+v err=%v", changeDone, err)
	}
	afterIdentityChange := listAll(t, ctx, database, owner, 10)
	itemsByNode := make(map[string]model.InventoryItem, len(afterIdentityChange))
	for _, item := range afterIdentityChange {
		itemsByNode[item.NodeID] = item
	}
	changedItem := itemsByNode[snapshot.Nodes[0].NodeID]
	if changedItem.Status != "unknown" || changedItem.State != (model.StateAxes{Process: "unknown", Connection: "unknown", Readiness: "unknown", Occupancy: "unknown"}) ||
		changedItem.ObservedAt != nil || changedItem.Source != nil || changedItem.PendingCount != nil || changedItem.Actions.SendMessage.Allowed {
		t.Fatalf("changed identity retained stale observation or action: %+v", changedItem)
	}
	var neighborBefore model.InventoryItem
	for _, item := range afterAdoption {
		if item.NodeID == neighborNodeID {
			neighborBefore = item
		}
	}
	if neighborAfterObservation := itemsByNode[neighborNodeID]; !reflect.DeepEqual(neighborAfterObservation, neighborBefore) {
		t.Fatalf("untouched node observation changed: before=%+v after=%+v", neighborBefore, neighborAfterObservation)
	}
	replayed, err := database.ReserveRegistryOperation(ctx, owner, changeIntent, changed, affected, "")
	if err != nil || !replayed.Existing || !reflect.DeepEqual(replayed.Status, changeDone) {
		t.Fatalf("terminal exact retry did not return the saved result: result=%+v err=%v", replayed, err)
	}
	alternate := changed
	alternate.Manifest.Nodes = append([]model.RegistryNode(nil), changed.Manifest.Nodes...)
	alternate.Manifest.Nodes[0].Name = "Different payload"
	alternate.ManifestSHA256 = strings.Repeat("d", 64)
	alternate = withFixtureEnvelope(alternate)
	if _, err := database.ReserveRegistryOperation(ctx, owner, registryIntent(changeIntent.OperationID, projected, alternate, nil), alternate, affected, ""); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("same registry operation ID accepted a different payload", err)
	}

	neighborStatus, err := database.GetOperation(ctx, owner, neighborIntent.OperationID)
	if err != nil || neighborStatus.Receipt != neighborAccepted.Receipt || neighborStatus.Phase != "accepted" {
		t.Fatalf("unrelated executing assignment changed: status=%+v err=%v", neighborStatus, err)
	}
	neighborAfter, err := database.GetOperationTarget(ctx, owner, neighborNodeID)
	if err != nil || neighborAfter.Target.HostID != neighborTarget.Target.HostID ||
		neighborAfter.Target.RegistrationRevision != neighborTarget.Target.RegistrationRevision ||
		neighborAfter.Target.RegistrationEpoch != neighborTarget.Target.RegistrationEpoch ||
		neighborAfter.Target.Generation != neighborAccepted.Receipt.AcceptedGeneration {
		t.Fatalf("untouched node fence changed: before=%+v after=%+v err=%v", neighborTarget, neighborAfter, err)
	}
	if after := listAllBindings(t, ctx, database, owner, neighborNodeID, 10); !reflect.DeepEqual(after, neighborBindings) {
		t.Fatalf("untouched dialog identities changed: before=%+v after=%+v", neighborBindings, after)
	}
	if old, err := database.GetRegistryOperation(ctx, owner, adoption.Status.Receipt.OperationID); err != nil || !reflect.DeepEqual(old, adoptionDone) {
		t.Fatalf("older registry result changed after a later operation: status=%+v err=%v", old, err)
	}
	crossKind := neighborIntent
	crossKind.OperationID = changeIntent.OperationID
	crossKind.Step.StepID = "cross-kind-adapter"
	if _, err := database.AcceptOperation(ctx, owner, crossKind); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("registry operation ID was reused by an adapter operation", err)
	}

	next := changed
	next.Manifest.RegistryVersion = 4
	next.Manifest.Nodes = append([]model.RegistryNode(nil), changed.Manifest.Nodes...)
	next.Manifest.Nodes[0].Name = "Next Agent"
	next.Manifest.Nodes[0].RegistrationRevision++
	next.Manifest.Nodes[0].RegistrationEpoch++
	next.ManifestSHA256 = strings.Repeat("e", 64)
	next = withFixtureEnvelope(next)
	nextAffected, _, err := registry.ProjectionDelta(changed.Manifest, next.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ReserveRegistryOperation(ctx, owner, registryIntent(neighborIntent.OperationID, changed, next, nil), next, nextAffected, ""); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("adapter operation ID was reused by a registry operation", err)
	}

	failIntent := registryIntent("registry-known-failure", changed, next, nil)
	failReserved, err := database.ReserveRegistryOperation(ctx, owner, failIntent, next, nextAffected, "")
	if err != nil {
		t.Fatal(err)
	}
	failSent, err := database.MarkRegistryOperationSent(ctx, owner, registryCommand(failReserved.Status))
	if err != nil {
		t.Fatal(err)
	}
	knownFailure := model.RegistryOperationFailure{
		SchemaID: model.RegistryOperationFailureSchemaID, OperationID: failSent.Receipt.OperationID,
		RequestHash: failSent.Receipt.RequestHash, OperationVersion: failSent.OperationVersion,
		ResultCode: "router.node_not_sealed",
	}
	failed, err := database.FailRegistryOperation(ctx, owner, knownFailure)
	if err != nil || failed.Phase != "failed" || failed.EffectState != "failed" {
		t.Fatalf("deterministic pre-commit failure was not saved: status=%+v err=%v", failed, err)
	}
	if repeated, err := database.FailRegistryOperation(ctx, owner, knownFailure); err != nil || !reflect.DeepEqual(repeated, failed) {
		t.Fatalf("deterministic failure retry changed result: status=%+v err=%v", repeated, err)
	}
	afterFailure := registryIntent("registry-after-failure", changed, next, nil)
	afterReserved, err := database.ReserveRegistryOperation(ctx, owner, afterFailure, next, nextAffected, "")
	if err != nil {
		t.Fatal("terminal failure did not release the owner", err)
	}
	afterSent, err := database.MarkRegistryOperationSent(ctx, owner, registryCommand(afterReserved.Status))
	if err != nil {
		t.Fatal(err)
	}
	afterFailureCommand := model.RegistryOperationFailure{
		SchemaID: model.RegistryOperationFailureSchemaID, OperationID: afterSent.Receipt.OperationID,
		RequestHash: afterSent.Receipt.RequestHash, OperationVersion: afterSent.OperationVersion,
		ResultCode: "router.node_not_sealed",
	}
	if _, err := database.FailRegistryOperation(ctx, owner, afterFailureCommand); err != nil {
		t.Fatal(err)
	}

	winnerTx, err := database.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = winnerTx.Rollback(context.Background()) }()
	if _, err := winnerTx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-service-import:"+owner); err != nil {
		t.Fatal(err)
	}
	if _, err := winnerTx.Exec(ctx, `UPDATE agent_service.registry_state SET registry_version=4,manifest_sha256=$2 WHERE owner_id=$1`, owner, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	raceResult := make(chan error, 1)
	go func() {
		_, reserveErr := database.ReserveRegistryOperation(ctx, owner, registryIntent("registry-lock-loser", changed, next, nil), next, nextAffected, "")
		raceResult <- reserveErr
	}()
	waitDeadline := time.Now().Add(2 * time.Second)
	for {
		var waiters int
		if err := database.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND wait_event_type='Lock' AND wait_event='advisory'
				AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			break
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("registry reservation did not wait on the owner serialization point")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := winnerTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-raceResult; !errors.Is(err, ErrOperationConflict) {
		t.Fatal("waiting registry reservation did not observe the committed winner", err)
	}
}

func TestPostgresNearLimitRegistryEnvelopeSurvivesOperationAndRestart(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	owner := fmt.Sprintf("hl283-near-limit-%d", time.Now().UnixNano())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	signer := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	certificate := sha256.Sum256([]byte("near-limit-registry-certificate"))
	currentManifest := model.RegistryManifest{
		SchemaID: "harness-router-registry-v1", RegistryVersion: 2, OwnerID: owner, Mode: "fixture",
		WireSchemaSHA256: "5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9",
		Nodes: []model.RegistryNode{{
			NodeID: fixtureUUID("21000000", 1), Name: "Agent A", Adapter: "codex",
			URL: "https://a.invalid", CertificateSHA256: hex.EncodeToString(certificate[:]),
			RegistrationRevision: 1, RegistrationEpoch: 1, Compatibility: "compatible",
		}},
	}
	currentManifest, currentRaw := paddedSignedRegistry(t, private, currentManifest, 256<<10)
	current, err := registry.Verify(currentRaw, signer)
	if err != nil {
		t.Fatal("near-limit current registry did not verify", err)
	}
	if len(current.Envelope) != 256<<10 {
		t.Fatalf("near-limit envelope bytes=%d", len(current.Envelope))
	}

	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		t.Fatal(err)
	}
	snapshot := model.ImportSnapshot{
		SchemaID: model.ImportSchemaID, Complete: true,
		Hosts: []model.HostSeed{{HostID: fixtureUUID("11000000", 1), Name: "Near-limit host"}},
		Nodes: []model.NodeSeed{{
			NodeID: currentManifest.Nodes[0].NodeID, HostID: fixtureUUID("11000000", 1),
			RegistrationMode: "compatible", Dialogs: []model.DialogSeed{{NodeDialogID: fixtureUUID("31000000", 1)}},
		}},
	}
	if _, err := database.Import(ctx, current, snapshot); err != nil {
		database.Close()
		t.Fatal(err)
	}
	var jsonBytes, jsonbBytes int
	if err := database.pool.QueryRow(ctx, `
		SELECT octet_length(($1::json)::text),octet_length(($1::jsonb)::text)`, string(current.Envelope),
	).Scan(&jsonBytes, &jsonbBytes); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if jsonBytes != len(current.Envelope) || jsonbBytes <= 256<<10 {
		database.Close()
		t.Fatalf("test did not exercise jsonb expansion: input=%d json=%d jsonb=%d", len(current.Envelope), jsonBytes, jsonbBytes)
	}

	candidateManifest := current.Manifest
	candidateManifest.RegistryVersion = 3
	candidateManifest.Nodes = append([]model.RegistryNode(nil), current.Manifest.Nodes...)
	candidateManifest.Nodes[0].Name = "Agent B"
	candidateManifest.Nodes[0].RegistrationRevision++
	candidateManifest.Nodes[0].RegistrationEpoch++
	candidateRaw := signedStoreRegistry(t, private, candidateManifest)
	if len(candidateRaw) != len(current.Envelope) {
		database.Close()
		t.Fatalf("candidate size changed: current=%d candidate=%d", len(current.Envelope), len(candidateRaw))
	}
	candidate, err := registry.Verify(candidateRaw, signer)
	if err != nil {
		database.Close()
		t.Fatal("near-limit candidate registry did not verify", err)
	}
	affected, newNodeID, err := registry.ProjectionDelta(current.Manifest, candidate.Manifest)
	if err != nil || newNodeID != "" || len(affected) != 1 {
		database.Close()
		t.Fatal("near-limit projection delta failed", affected, newNodeID, err)
	}
	intent := registryIntent("registry-near-limit", current, candidate, nil)
	reserved, err := database.ReserveRegistryOperation(ctx, owner, intent, candidate, affected, "")
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	storedIntent, err := database.GetRegistryOperationIntent(ctx, owner, intent.OperationID)
	if err != nil || string(storedIntent.Registry) != string(candidate.Envelope) {
		database.Close()
		t.Fatalf("near-limit operation intent changed: bytes=%d err=%v", len(storedIntent.Registry), err)
	}
	sent, err := database.MarkRegistryOperationSent(ctx, owner, registryCommand(reserved.Status))
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	finished, err := database.FinishRegistryOperation(ctx, owner, registryFinish(sent, candidate, "acknowledged"), candidate)
	if err != nil || finished.Phase != "succeeded" {
		database.Close()
		t.Fatalf("near-limit operation did not finish: status=%+v err=%v", finished, err)
	}
	stored, version, digest, err := database.GetRegistryEnvelope(ctx, owner)
	if err != nil || version != candidate.Manifest.RegistryVersion || digest != candidate.ManifestSHA256 || string(stored) != string(candidate.Envelope) {
		database.Close()
		t.Fatalf("near-limit registry changed before restart: bytes=%d version=%d digest=%s err=%v", len(stored), version, digest, err)
	}
	database.Close()

	reopened, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restoredIntent, err := reopened.GetRegistryOperationIntent(ctx, owner, intent.OperationID)
	if err != nil || model.ValidateRegistryOperationIntent(restoredIntent) != nil || string(restoredIntent.Registry) != string(candidate.Envelope) {
		t.Fatalf("near-limit intent did not survive restart: bytes=%d err=%v", len(restoredIntent.Registry), err)
	}
	restored, version, digest, err := reopened.GetRegistryEnvelope(ctx, owner)
	if err != nil || version != candidate.Manifest.RegistryVersion || digest != candidate.ManifestSHA256 || string(restored) != string(candidate.Envelope) {
		t.Fatalf("near-limit registry did not survive restart: bytes=%d version=%d digest=%s err=%v", len(restored), version, digest, err)
	}
}

func paddedSignedRegistry(t *testing.T, private ed25519.PrivateKey, manifest model.RegistryManifest, targetBytes int) (model.RegistryManifest, []byte) {
	t.Helper()
	manifest.Nodes = append([]model.RegistryNode(nil), manifest.Nodes...)
	raw := signedStoreRegistry(t, private, manifest)
	padding := targetBytes - len(raw)
	if padding < 0 {
		t.Fatalf("registry already exceeds target: bytes=%d target=%d", len(raw), targetBytes)
	}
	manifest.Nodes[0].URL = "https://" + strings.Repeat("a", padding+1) + ".invalid"
	raw = signedStoreRegistry(t, private, manifest)
	if len(raw) != targetBytes {
		t.Fatalf("registry padding is not exact: bytes=%d target=%d", len(raw), targetBytes)
	}
	return manifest, raw
}

func signedStoreRegistry(t *testing.T, private ed25519.PrivateKey, manifest model.RegistryManifest) []byte {
	t.Helper()
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
	return raw
}

func registryIntent(operationID string, current, candidate registry.Verified, newNodeHostID *string) model.RegistryOperationIntent {
	return model.RegistryOperationIntent{
		SchemaID: model.RegistryOperationSchemaID, OperationID: operationID,
		Expected: model.RegistryExpected{RegistryVersion: current.Manifest.RegistryVersion, RegistrySHA256: current.ManifestSHA256},
		Registry: append(json.RawMessage(nil), candidate.Envelope...), NewNodeHostID: newNodeHostID,
	}
}

func registryCommand(status model.RegistryOperationStatus) model.RegistryOperationCommand {
	return model.RegistryOperationCommand{
		SchemaID: model.RegistryOperationCommandSchemaID, OperationID: status.Receipt.OperationID,
		RequestHash: status.Receipt.RequestHash, OperationVersion: status.OperationVersion,
	}
}

func registryFinish(status model.RegistryOperationStatus, candidate registry.Verified, effectState string) model.RegistryOperationFinish {
	return model.RegistryOperationFinish{
		SchemaID: model.RegistryOperationFinishSchemaID, OperationID: status.Receipt.OperationID,
		RequestHash: status.Receipt.RequestHash, OperationVersion: status.OperationVersion,
		RegistryVersion: candidate.Manifest.RegistryVersion, RegistrySHA256: candidate.ManifestSHA256,
		EffectState: effectState,
	}
}

func withFixtureEnvelope(value registry.Verified) registry.Verified {
	envelope, _ := json.Marshal(map[string]any{"manifest": value.Manifest, "signature": "fixture"})
	value.Envelope = envelope
	return value
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
	return withFixtureEnvelope(registry.Verified{Manifest: manifest, ManifestSHA256: strings.Repeat("a", 64)}), snapshot
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
