package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/harness/node"
	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historysearch"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historysync"
)

type harnessReplicaIDs struct {
	mu           sync.Mutex
	next         int
	nodeDialogID string
}

func (ids *harnessReplicaIDs) NewID() (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	if ids.next == 1 {
		return ids.nodeDialogID, nil
	}
	return fmt.Sprintf("90000000-0000-4000-8000-%012d", ids.next), nil
}

type directHarnessHistoryExporter struct {
	authority    *node.Node
	owner        string
	nodeID       string
	nodeDialogID string
}

func (exporter *directHarnessHistoryExporter) ExportHistory(ctx context.Context, nodeID, owner string,
	identity historyreplica.StreamIdentity, after int64, limit int) (historyreplica.ExportPage, error) {
	if nodeID != exporter.nodeID || owner != exporter.owner {
		return historyreplica.ExportPage{}, errors.New("unexpected direct Harness history scope")
	}
	result := exporter.authority.ExportHistory(ctx, node.TrustContext{
		ActorID: owner, TransportNodeID: nodeID, PeerVerified: true,
	}, identity, after, limit)
	if result.HTTPStatus != 200 {
		return historyreplica.ExportPage{}, fmt.Errorf("direct Harness history export status %d", result.HTTPStatus)
	}
	var page historyreplica.ExportPage
	if json.Unmarshal(result.Body, &page) != nil || historyreplica.ValidatePage(page) != nil {
		return historyreplica.ExportPage{}, errors.New("direct Harness history export is invalid")
	}
	return page, nil
}

func (exporter *directHarnessHistoryExporter) DialogMetadataSnapshot(_ context.Context, nodeID, owner string) (map[string]historysearch.SourceDialogMetadata, error) {
	if nodeID != exporter.nodeID || owner != exporter.owner {
		return nil, errors.New("unexpected direct Harness metadata scope")
	}
	return map[string]historysearch.SourceDialogMetadata{
		exporter.nodeDialogID: {Title: "Harness component R11-R12 evidence"},
	}, nil
}

func TestHarnessHistoryCoordinatorCopiesCommittedEntryAndBackfillsAfterRecreation(t *testing.T) {
	socket := os.Getenv("HL294_HARNESS_COMPONENT_REPLICA_SOCKET")
	owner := os.Getenv("HL294_HARNESS_COMPONENT_REPLICA_OWNER")
	workerToken := os.Getenv("HL294_HARNESS_COMPONENT_REPLICA_WORKER_TOKEN")
	if socket == "" || owner == "" || workerToken == "" {
		t.Skip("HL294 Harness component replica environment is not configured")
	}
	client, err := agentserviceclient.NewWithWorkerToken(socket, workerToken)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inventory, err := client.Inventory(ctx, owner, 10, "")
	if err != nil || len(inventory.Items) != 1 {
		t.Fatalf("Harness component replica inventory=%+v err=%v", inventory, err)
	}
	nodeID := inventory.Items[0].NodeID
	bindings, err := client.DialogBindings(ctx, owner, nodeID, 10, "")
	if err != nil || len(bindings.Items) != 1 {
		t.Fatalf("Harness component replica bindings=%+v err=%v", bindings, err)
	}
	binding := bindings.Items[0]
	if existing, readErr := client.ReadHistoryReplica(ctx, owner, binding.LogicalDialogID, 200, ""); readErr == nil {
		t.Fatalf("Harness component replica target is not clean: %+v", existing.SyncedThrough)
	} else {
		var fault *agentserviceclient.Fault
		if !errors.As(readErr, &fault) || fault.Status != 404 || fault.Code != "not_found" {
			t.Fatalf("Harness component replica clean-target check failed: %v", readErr)
		}
	}

	config := testConfig(t.TempDir())
	config.NodeID = nodeID
	config.OwnerID = owner
	config.IDs = &harnessReplicaIDs{nodeDialogID: binding.NodeDialogID}
	authority, err := node.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	trust := node.TrustContext{ActorID: owner, TransportNodeID: nodeID, PeerVerified: true}
	created := decodeReceipt(t, authority.SubmitCommand(ctx, trust, command(t,
		"a0000000-0000-4000-8000-000000000001", "dialog.create",
		map[string]any{"nodeId": nodeID}, map[string]any{"registryVersion": 1}, map[string]any{})), 202)
	var dialog harnessprotocol.DialogCreateReferences
	if json.Unmarshal(created.References, &dialog) != nil || dialog.DialogID != binding.NodeDialogID {
		t.Fatalf("Harness component dialog=%+v binding=%+v", dialog, binding)
	}
	exporter := &directHarnessHistoryExporter{authority: authority, owner: owner, nodeID: nodeID, nodeDialogID: dialog.DialogID}

	coordinator, err := historysync.New(owner, client, exporter, client, historysync.DefaultInterval)
	if err != nil {
		t.Fatal(err)
	}
	runContext, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		coordinator.Run(runContext)
	}()
	waitHarnessComponentReplicaEntry(t, ctx, client, owner, binding.LogicalDialogID, "", time.Now())

	firstStart := time.Now()
	first := decodeReceipt(t, authority.SubmitCommand(ctx, trust, command(t,
		"a0000000-0000-4000-8000-000000000002", "message.enqueue",
		map[string]any{"nodeId": nodeID, "dialogId": dialog.DialogID}, map[string]any{"dialogVersion": 1},
		map[string]any{"text": "Harness component committed entry"})), 202)
	var firstMessage harnessprotocol.MessageEnqueueReferences
	if json.Unmarshal(first.References, &firstMessage) != nil {
		t.Fatal("Harness component first message references are invalid")
	}
	firstElapsed := waitHarnessComponentReplicaEntry(t, ctx, client, owner, binding.LogicalDialogID, firstMessage.MessageID, firstStart)
	if firstElapsed > 5*time.Second {
		t.Fatalf("healthy committed-to-replica latency=%s", firstElapsed)
	}
	stop()
	<-done

	second := decodeReceipt(t, authority.SubmitCommand(ctx, trust, command(t,
		"a0000000-0000-4000-8000-000000000003", "message.enqueue",
		map[string]any{"nodeId": nodeID, "dialogId": dialog.DialogID}, map[string]any{"dialogVersion": 2},
		map[string]any{"text": "offline tail for backfill"})), 202)
	var secondMessage harnessprotocol.MessageEnqueueReferences
	if json.Unmarshal(second.References, &secondMessage) != nil {
		t.Fatal("Harness component second message references are invalid")
	}
	recreated, err := historysync.New(owner, client, exporter, client, historysync.DefaultInterval)
	if err != nil {
		t.Fatal(err)
	}
	recreationContext, recreationStop := context.WithCancel(ctx)
	recreationDone := make(chan struct{})
	recreationStart := time.Now()
	go func() {
		defer close(recreationDone)
		recreated.Run(recreationContext)
	}()
	backfillElapsed := waitHarnessComponentReplicaEntry(t, ctx, client, owner, binding.LogicalDialogID, secondMessage.MessageID, recreationStart)
	recreationStop()
	<-recreationDone
	if backfillElapsed > 5*time.Second {
		t.Fatalf("coordinator recreation backfill latency=%s", backfillElapsed)
	}
	t.Logf("harness_component_profile committed_to_replica=%s coordinator_recreation_backfill=%s", firstElapsed, backfillElapsed)
}

func waitHarnessComponentReplicaEntry(t *testing.T, ctx context.Context, client *agentserviceclient.Client, owner, logicalDialogID, entryID string, started time.Time) time.Duration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, err := client.ReadHistoryReplica(ctx, owner, logicalDialogID, 200, "")
		if err == nil {
			if entryID == "" {
				return time.Since(started)
			}
			for _, entry := range page.Entries {
				if entry.EntryID == entryID {
					return time.Since(started)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica entry %q did not appear within five seconds: %v", entryID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
