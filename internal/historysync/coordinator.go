// Package historysync copies normalized Harness history into agent-service.
// Harness remains the execution authority and primary history owner.
package historysync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historysearch"
)

const (
	DefaultInterval = 2 * time.Second
	pageLimit       = 200
	inventoryLimit  = 100
	maximumPages    = 10_000
)

type Inventory interface {
	Inventory(context.Context, string, int, string) (agentserviceclient.Page, error)
	DialogBindings(context.Context, string, string, int, string) (agentserviceclient.DialogPage, error)
}

type Exporter interface {
	ExportHistory(context.Context, string, string, historyreplica.StreamIdentity, int64, int) (historyreplica.ExportPage, error)
	DialogMetadataSnapshot(context.Context, string, string) (map[string]historysearch.SourceDialogMetadata, error)
}

type ReplicaStore interface {
	ApplyHistoryReplica(context.Context, string, historyreplica.ExportPage) (agentserviceclient.HistoryImportResult, error)
	UpsertHistoryDialogMetadata(context.Context, string, string, historysearch.DialogMetadata) (historysearch.DialogMetadata, error)
}

type Coordinator struct {
	owner     string
	inventory Inventory
	exporter  Exporter
	store     ReplicaStore
	interval  time.Duration
	mu        sync.Mutex
	cursors   map[string]int64
	metadata  map[string]string
}

func New(owner string, inventory Inventory, exporter Exporter, store ReplicaStore, interval time.Duration) (*Coordinator, error) {
	if owner == "" || inventory == nil || exporter == nil || store == nil {
		return nil, errors.New("history coordinator is incomplete")
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Coordinator{owner: owner, inventory: inventory, exporter: exporter, store: store, interval: interval,
		cursors: map[string]int64{}, metadata: map[string]string{}}, nil
}

// Run performs an immediate backfill, then checks frequently enough for the
// five-second R12 copy objective. Individual offline nodes do not stop other
// bindings from advancing.
func (c *Coordinator) Run(ctx context.Context) {
	_ = c.SyncOnce(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.SyncOnce(ctx)
		}
	}
}

func (c *Coordinator) SyncOnce(ctx context.Context) error {
	var failures []error
	cursor := ""
	for pageNumber := 0; pageNumber < maximumPages; pageNumber++ {
		page, err := c.inventory.Inventory(ctx, c.owner, inventoryLimit, cursor)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			if err := c.syncNode(ctx, item.NodeID); err != nil {
				failures = append(failures, fmt.Errorf("node %s: %w", item.NodeID, err))
			}
		}
		if page.NextCursor == nil {
			return errors.Join(failures...)
		}
		if *page.NextCursor == cursor {
			return errors.Join(append(failures, errors.New("inventory cursor did not advance"))...)
		}
		cursor = *page.NextCursor
	}
	return errors.Join(append(failures, errors.New("inventory page bound exceeded"))...)
}

func (c *Coordinator) syncNode(ctx context.Context, nodeID string) error {
	var failures []error
	metadata, metadataErr := c.exporter.DialogMetadataSnapshot(ctx, nodeID, c.owner)
	if metadataErr != nil {
		failures = append(failures, fmt.Errorf("dialog metadata snapshot: %w", metadataErr))
	}
	cursor := ""
	for pageNumber := 0; pageNumber < maximumPages; pageNumber++ {
		page, err := c.inventory.DialogBindings(ctx, c.owner, nodeID, inventoryLimit, cursor)
		if err != nil {
			return err
		}
		for _, binding := range page.Items {
			identity := historyreplica.StreamIdentity{
				OwnerID: c.owner, LogicalDialogID: binding.LogicalDialogID, NodeID: nodeID,
				NodeDialogID: binding.NodeDialogID, BindingGeneration: binding.BindingVersion,
			}
			if err := c.syncStream(ctx, identity); err != nil {
				failures = append(failures, fmt.Errorf("dialog %s: %w", binding.NodeDialogID, err))
			}
			if metadataErr == nil {
				source, found := metadata[binding.NodeDialogID]
				if !found {
					failures = append(failures, fmt.Errorf("dialog %s metadata: missing from source snapshot", binding.NodeDialogID))
				} else if err := c.syncMetadata(ctx, identity, source); err != nil {
					failures = append(failures, fmt.Errorf("dialog %s metadata: %w", binding.NodeDialogID, err))
				}
			}
		}
		if page.NextCursor == nil {
			return errors.Join(failures...)
		}
		if *page.NextCursor == cursor {
			return errors.Join(append(failures, errors.New("binding cursor did not advance"))...)
		}
		cursor = *page.NextCursor
	}
	return errors.Join(append(failures, errors.New("binding page bound exceeded"))...)
}

func (c *Coordinator) syncMetadata(ctx context.Context, identity historyreplica.StreamIdentity, source historysearch.SourceDialogMetadata) error {
	fingerprint := fmt.Sprintf("%s\x00%s\x00%d\x00%t\x00%s", identity.NodeID, identity.NodeDialogID,
		identity.BindingGeneration, source.Archived, source.Title)
	c.mu.Lock()
	unchanged := c.metadata[identity.LogicalDialogID] == fingerprint
	c.mu.Unlock()
	if unchanged {
		return nil
	}
	_, err := c.store.UpsertHistoryDialogMetadata(ctx, c.owner, identity.LogicalDialogID, historysearch.DialogMetadata{
		SchemaID: historysearch.MetadataSchemaID, NodeID: identity.NodeID, NodeDialogID: identity.NodeDialogID,
		BindingGeneration: identity.BindingGeneration, Title: source.Title, Archived: source.Archived,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err == nil {
		c.mu.Lock()
		c.metadata[identity.LogicalDialogID] = fingerprint
		c.mu.Unlock()
	}
	return err
}

func (c *Coordinator) syncStream(ctx context.Context, identity historyreplica.StreamIdentity) error {
	streamID := historyreplica.StreamID(identity)
	c.mu.Lock()
	after := c.cursors[streamID]
	c.mu.Unlock()
	err := c.syncStreamFrom(ctx, identity, after)
	var fault *agentserviceclient.Fault
	if after > 0 && errors.As(err, &fault) && fault.Code == "history_gap" {
		// A restored/replaced PostgreSQL copy can be behind this process-local
		// hint. Exact replay from zero is the recovery path, never a skipped gap.
		// Metadata may have rolled back with the same database, so invalidate its
		// process-local no-op hint before the caller performs the metadata sync.
		c.mu.Lock()
		delete(c.cursors, streamID)
		delete(c.metadata, identity.LogicalDialogID)
		c.mu.Unlock()
		return c.syncStreamFrom(ctx, identity, 0)
	}
	return err
}

func (c *Coordinator) syncStreamFrom(ctx context.Context, identity historyreplica.StreamIdentity, after int64) error {
	streamID := historyreplica.StreamID(identity)
	for pageNumber := 0; pageNumber < maximumPages; pageNumber++ {
		page, err := c.exporter.ExportHistory(ctx, identity.NodeID, c.owner, identity, after, pageLimit)
		if err != nil {
			return err
		}
		result, err := c.store.ApplyHistoryReplica(ctx, c.owner, page)
		if err != nil {
			return err
		}
		if page.NextAfter == nil {
			c.mu.Lock()
			c.cursors[streamID] = result.ImportedThrough
			c.mu.Unlock()
			return nil
		}
		if *page.NextAfter <= after {
			return errors.New("history cursor did not advance")
		}
		after = *page.NextAfter
	}
	return errors.New("history page bound exceeded")
}
