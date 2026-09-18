package harnesstunnel

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

const poolNode = "20000000-0000-4000-8000-000000000001"

func TestDefaultPoolConfigReservesFourControlSlotsAcrossNodes(t *testing.T) {
	config := DefaultPoolConfig()
	wantConfig := PoolConfig{Total: 16, Streams: 12, PerNode: 4, PerNodeStreams: 3, Pending: 64}
	if config != wantConfig {
		t.Fatalf("default pool config = %+v; want %+v", config, wantConfig)
	}
	pool, err := NewPool(config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	acquire := func(nodeID string, purpose Purpose) (*Lease, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return pool.Acquire(ctx, nodeID, purpose)
	}

	var streams []*Lease
	for nodeIndex := 1; nodeIndex <= 4; nodeIndex++ {
		nodeID := fmt.Sprintf("20000000-0000-4000-8000-%012d", nodeIndex)
		for streamIndex := 0; streamIndex < 3; streamIndex++ {
			lease, acquireErr := acquire(nodeID, PurposeEvents)
			if acquireErr != nil {
				t.Fatalf("node %d stream %d: %v", nodeIndex, streamIndex, acquireErr)
			}
			streams = append(streams, lease)
		}
	}
	defer func() {
		for _, lease := range streams {
			lease.Release()
		}
	}()

	pool.mu.Lock()
	if pool.total != 12 || pool.streams != 12 {
		pool.mu.Unlock()
		t.Fatalf("default pool stream saturation = total %d, streams %d; want 12, 12", pool.total, pool.streams)
	}
	pool.mu.Unlock()
	blockedContext, cancelBlocked := context.WithCancel(context.Background())
	cancelBlocked()
	blocked, blockedErr := pool.Acquire(blockedContext, "20000000-0000-4000-8000-000000000005", PurposeEvents)
	if blocked != nil {
		blocked.Release()
		t.Fatal("thirteenth stream consumed reserved control capacity")
	}
	if !errors.Is(blockedErr, context.Canceled) {
		t.Fatalf("thirteenth stream error = %v; want context.Canceled", blockedErr)
	}

	var controls []*Lease
	for nodeIndex := 1; nodeIndex <= 4; nodeIndex++ {
		nodeID := fmt.Sprintf("20000000-0000-4000-8000-%012d", nodeIndex)
		lease, acquireErr := acquire(nodeID, PurposeCommand)
		if acquireErr != nil {
			t.Fatalf("node %d control starved at default capacity: %v", nodeIndex, acquireErr)
		}
		controls = append(controls, lease)
	}
	defer func() {
		for _, lease := range controls {
			lease.Release()
		}
	}()

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.total != 16 || pool.streams != 12 {
		t.Fatalf("default pool capacity = total %d, streams %d; want 16, 12", pool.total, pool.streams)
	}
	for nodeIndex := 1; nodeIndex <= 4; nodeIndex++ {
		nodeID := fmt.Sprintf("20000000-0000-4000-8000-%012d", nodeIndex)
		usage := pool.nodes[nodeID]
		if usage.total != 4 || usage.streams != 3 {
			t.Fatalf("node %d usage = total %d, streams %d; want 4, 3", nodeIndex, usage.total, usage.streams)
		}
	}
}

func TestLongStreamCannotConsumeReservedControlCapacity(t *testing.T) {
	for iteration := 0; iteration < 10; iteration++ {
		pool, err := NewPool(PoolConfig{Total: 2, Streams: 1, PerNode: 2, PerNodeStreams: 1, Pending: 4})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := pool.Acquire(context.Background(), poolNode, PurposeEvents)
		if err != nil {
			t.Fatal(err)
		}
		blocked := make(chan *Lease, 1)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			lease, _ := pool.Acquire(ctx, poolNode, PurposeReplicaExport)
			blocked <- lease
		}()
		control, err := pool.Acquire(context.Background(), poolNode, PurposeCommand)
		if err != nil {
			t.Fatalf("iteration %d: control starved: %v", iteration, err)
		}
		select {
		case lease := <-blocked:
			if lease != nil {
				lease.Release()
				t.Fatalf("iteration %d: second stream consumed reserved slot", iteration)
			}
		default:
		}
		control.Release()
		stream.Release()
		select {
		case lease := <-blocked:
			if lease == nil {
				t.Fatalf("iteration %d: queued stream was not admitted", iteration)
			}
			lease.Release()
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: pool did not release queued stream", iteration)
		}
		cancel()
		pool.Close()
	}
}

func TestFullStreamQueueCannotBlockFreeControlReserve(t *testing.T) {
	pool, err := NewPool(PoolConfig{Total: 2, Streams: 1, PerNode: 2, PerNodeStreams: 1, Pending: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	stream, err := pool.Acquire(context.Background(), poolNode, PurposeEvents)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Release()

	// Model a saturated pending queue of streams. Neither waiter may consume
	// the one free slot because it is reserved for control traffic.
	pool.mu.Lock()
	pool.waiters = []*poolWaiter{
		{node: poolNode, purpose: string(PurposeReplicaExport), stream: true, ready: make(chan struct{})},
		{node: poolNode, purpose: string(PurposeReplicaImport), stream: true, ready: make(chan struct{})},
	}
	pool.mu.Unlock()

	control, err := pool.Acquire(context.Background(), poolNode, PurposeCommand)
	if err != nil {
		t.Fatalf("free control reserve rejected behind full stream queue: %v", err)
	}
	control.Release()
}
