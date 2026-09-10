package cursor

import (
	"context"
	"testing"
	"time"
)

func TestBridgeCloseWaitsForEventHandler(t *testing.T) {
	config := fakeConfig(t)
	started := make(chan struct{})
	release := make(chan struct{})
	worker, err := startBridge(config, func(frame bridgeFrame) {
		if frame.Event != "terminal" {
			return
		}
		close(started)
		<-release
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.OperationTimeout)
	defer cancel()
	var initialized struct {
		Version string `json:"version"`
	}
	if err := worker.call(ctx, "init", map[string]any{}, &initialized); err != nil {
		close(release)
		_ = worker.Close()
		t.Fatal(err)
	}
	var dispatched struct {
		AgentID string `json:"agentId"`
		RunID   string `json:"runId"`
	}
	if err := worker.call(ctx, "dispatch", map[string]any{
		"attemptKey": "close-waits", "prompt": "complete", "policyContent": "deny all tools",
	}, &dispatched); err != nil {
		close(release)
		_ = worker.Close()
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-ctx.Done():
		close(release)
		_ = worker.Close()
		t.Fatal("terminal event handler did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- worker.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("close returned before event handler completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-closed // The worker is deliberately killed, so only completion matters here.
}

func TestBridgeEventHandlerCanStopWithoutDeadlock(t *testing.T) {
	config := fakeConfig(t)
	stopped := make(chan struct{})
	var worker *bridge
	worker, err := startBridge(config, func(frame bridgeFrame) {
		if frame.Event == "terminal" {
			worker.stop()
			close(stopped)
		}
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.OperationTimeout)
	defer cancel()
	var initialized struct {
		Version string `json:"version"`
	}
	if err := worker.call(ctx, "init", map[string]any{}, &initialized); err != nil {
		_ = worker.Close()
		t.Fatal(err)
	}
	var dispatched struct {
		AgentID string `json:"agentId"`
		RunID   string `json:"runId"`
	}
	if err := worker.call(ctx, "dispatch", map[string]any{
		"attemptKey": "handler-stops", "prompt": "complete", "policyContent": "deny all tools",
	}, &dispatched); err != nil {
		_ = worker.Close()
		t.Fatal(err)
	}

	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("event handler stop deadlocked")
	}
	_ = worker.Close() // Reap the deliberately stopped worker.
}
