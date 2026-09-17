package historysync

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
)

const (
	testOwner   = "1-1"
	testNode    = "10000000-0000-4000-8000-000000000001"
	testDialog  = "10000000-0000-4000-8000-000000000002"
	testLogical = "10000000-0000-4000-8000-000000000003"
)

type fixture struct {
	exportCalls []int64
	imports     []int64
	failNode    string
	gapAfter    int64
	gapReturned bool
}

func (f *fixture) Inventory(context.Context, string, int, string) (agentserviceclient.Page, error) {
	return agentserviceclient.Page{SchemaID: agentserviceclient.InventorySchema, Items: []agentserviceclient.Item{
		{NodeID: testNode}, {NodeID: "10000000-0000-4000-8000-000000000004"},
	}}, nil
}

func (f *fixture) DialogBindings(_ context.Context, _ string, nodeID string, _ int, _ string) (agentserviceclient.DialogPage, error) {
	if nodeID == f.failNode {
		return agentserviceclient.DialogPage{}, errors.New("offline")
	}
	return agentserviceclient.DialogPage{SchemaID: agentserviceclient.BindingsSchema, NodeID: nodeID, Items: []agentserviceclient.Dialog{{
		NodeDialogID: testDialog, LogicalDialogID: testLogical, BindingVersion: 2,
	}}}, nil
}

func (f *fixture) ExportHistory(_ context.Context, _ string, _ string, identity historyreplica.StreamIdentity, after int64, _ int) (historyreplica.ExportPage, error) {
	f.exportCalls = append(f.exportCalls, after)
	next := int64(1)
	page := historyreplica.ExportPage{
		SchemaID: historyreplica.SchemaID, SchemaSHA256: historyreplica.SchemaSHA256,
		StreamID: historyreplica.StreamID(identity), Identity: identity, AfterSeq: after,
		Checkpoint: historyreplica.Checkpoint{ThroughSeq: 2, ChainHash: historyreplica.ChainGenesis(historyreplica.StreamID(identity)), DialogVersion: 1, QueueRevision: 1,
			EntriesHash: historyreplica.GenesisHash, FactsHash: historyreplica.GenesisHash, ReceiptsHash: historyreplica.GenesisHash,
			TextHash: historyreplica.GenesisHash, AssetManifestHash: historyreplica.GenesisHash,
			EffectsSummary: historyreplica.EffectsSummary{AssetManifestCount: 2}, Ready: true, CapturedAt: "2026-09-17T00:00:00Z"},
	}
	if after == 0 {
		page.NextAfter = &next
	}
	return page, nil
}

func (f *fixture) ApplyHistoryReplica(_ context.Context, _ string, page historyreplica.ExportPage) (agentserviceclient.HistoryImportResult, error) {
	f.imports = append(f.imports, page.AfterSeq)
	if page.AfterSeq == f.gapAfter && f.gapAfter > 0 && !f.gapReturned {
		f.gapReturned = true
		return agentserviceclient.HistoryImportResult{}, &agentserviceclient.Fault{Status: 409, Code: "history_gap"}
	}
	through := page.Checkpoint.ThroughSeq
	if page.NextAfter != nil {
		through = *page.NextAfter
	}
	return agentserviceclient.HistoryImportResult{StreamID: page.StreamID, ImportedThrough: through}, nil
}

func TestSyncBackfillsAndContinuesPastOfflineNode(t *testing.T) {
	backend := &fixture{failNode: "10000000-0000-4000-8000-000000000004"}
	coordinator, err := New(testOwner, backend, backend, backend, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.SyncOnce(context.Background()); err == nil {
		t.Fatal("offline node was not reported")
	}
	if len(backend.exportCalls) != 2 || backend.exportCalls[0] != 0 || backend.exportCalls[1] != 1 ||
		len(backend.imports) != 2 {
		t.Fatalf("contiguous backfill missing: exports=%v imports=%v", backend.exportCalls, backend.imports)
	}
	backend.gapAfter = 2
	if err := coordinator.SyncOnce(context.Background()); err == nil {
		t.Fatal("offline node was not reported on incremental sync")
	}
	if !slices.Equal(backend.exportCalls, []int64{0, 1, 2, 0, 1}) {
		t.Fatalf("incremental checkpoint and full fallback failed: exports=%v", backend.exportCalls)
	}
}
