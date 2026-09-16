package harnesstunnel

import (
	"context"
	"testing"
	"time"
)

const poolNode = "20000000-0000-4000-8000-000000000001"

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
