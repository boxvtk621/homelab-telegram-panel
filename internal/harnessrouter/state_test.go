package harnessrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

const routerNodeID = "20000000-0000-4000-8000-000000000001"
const secondRouterNodeID = "20000000-0000-4000-8000-000000000002"

func routingRegistry() harnessclient.RoutingRegistry {
	return harnessclient.RoutingRegistry{RegistryVersion: 1, OwnerID: "1-1", ManifestSHA256: "a8f9da106644f67ec9d26a36d20ad15aa90ad499c22f1156d8ec037f9a42cf38", Nodes: []harnessclient.PublicNode{{NodeID: routerNodeID, Name: "Node", Adapter: "cursor"}}}
}

func eligibleState() State {
	registry := routingRegistry()
	return State{Schema: StateSchema, OwnerID: registry.OwnerID, RegistryVersion: registry.RegistryVersion, RegistrySHA256: registry.ManifestSHA256, Nodes: map[string]NodeState{
		routerNodeID: {Mode: ModeEligible, StateVersion: 1, Generation: 1, IdentityEpoch: 7, AdapterKind: "cursor", AdapterVersion: "1.0.31"},
	}}
}

func managedFixture(t *testing.T, backend Backend, state State) (*Router, string, string) {
	t.Helper()
	directory, err := os.MkdirTemp("", "harness-router-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath, socketPath := filepath.Join(directory, "state.json"), filepath.Join(directory, "control.sock")
	if _, err := writeState(statePath, state, false); err != nil {
		t.Fatal(err)
	}
	router, err := newManaged(backend, statePath, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.Close)
	return router, statePath, socketPath
}

func commandFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../api/harness-v1.fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Fixtures []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"fixtures"`
	}
	if json.Unmarshal(raw, &fixtures) != nil {
		t.Fatal("invalid fixture corpus")
	}
	for _, fixture := range fixtures.Fixtures {
		if fixture.Name == name {
			return fixture.Value
		}
	}
	t.Fatal("missing fixture", name)
	return nil
}

func TestManagedRouterCommandModes(t *testing.T) {
	backend := &readyBackend{registry: routingRegistry()}
	router, statePath, _ := managedFixture(t, backend, eligibleState())
	input := transitionRequest{OperationID: "deploy-17", Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}}
	drained, err := router.transition(context.Background(), routerNodeID, "drain", input)
	if err != nil || drained.Mode != ModeDraining || drained.StateVersion != 2 {
		t.Fatal("drain failed", drained, err)
	}
	names := []string{"command.1.dialog.create", "command.2.message.enqueue", "command.3.message.steer", "command.4.request.cancel", "command.5.attempt.stop", "command.6.queue.resume", "command.7.attempt.retry", "command.8.approval.respond", "command.9.input.respond"}
	blocked := map[string]bool{names[0]: true, names[1]: true, names[5]: true, names[6]: true}
	forwarded := 0
	for _, name := range names {
		_, err := router.Command(context.Background(), routerNodeID, "1-1", commandFixture(t, name))
		var fault *harnessclient.Fault
		if blocked[name] {
			if !errorsAs(err, &fault) || fault.Status != 409 || fault.Code != "stale" {
				t.Fatalf("%s was not rejected as stale: %v", name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s control rejected: %v", name, err)
		}
		forwarded++
	}
	if got := backend.commandCalls; got != forwarded {
		t.Fatalf("backend calls=%d want=%d", got, forwarded)
	}
	sealed, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{OperationID: "deploy-17", Expected: expectedState{Mode: ModeDraining, StateVersion: 2, Generation: 1}})
	if err != nil || sealed.Mode != ModeSealed || sealed.StateVersion != 3 {
		t.Fatal("seal failed", sealed, err)
	}
	if _, err := router.Command(context.Background(), routerNodeID, "1-1", commandFixture(t, "command.5.attempt.stop")); !errorsAs(err, new(*harnessclient.Fault)) {
		t.Fatal("sealed Router forwarded a control", err)
	}
	persisted, err := decodeState(statePath)
	if err != nil || persisted.Nodes[routerNodeID].Mode != ModeSealed {
		t.Fatal("sealed state was not durable", persisted, err)
	}
}

// errorsAs keeps the assertions compact while retaining the exact typed fault.
func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}

type barrierBackend struct {
	fakeBackend
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (backend *barrierBackend) Command(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	backend.once.Do(func() { close(backend.started) })
	select {
	case <-backend.release:
		return backend.fakeBackend.Command(ctx, nodeID, owner, body)
	case <-ctx.Done():
		return harnessclient.Response{}, ctx.Err()
	}
}

func (backend *barrierBackend) CommandFenced(ctx context.Context, nodeID, owner string, body []byte, _ hp.NodeIdentity) (harnessclient.Response, error) {
	return backend.Command(ctx, nodeID, owner, body)
}

func TestDrainWaitsForInFlightCommandBarrier(t *testing.T) {
	backend := &barrierBackend{fakeBackend: fakeBackend{routing: routingRegistry(), response: harnessclient.Response{Status: 202}}, started: make(chan struct{}), release: make(chan struct{})}
	router, _, _ := managedFixture(t, backend, eligibleState())
	body := commandFixture(t, "command.2.message.enqueue")
	commandDone := make(chan error, 1)
	go func() {
		_, err := router.Command(context.Background(), routerNodeID, "1-1", body)
		commandDone <- err
	}()
	<-backend.started
	drainDone := make(chan error, 1)
	go func() {
		_, err := router.transition(context.Background(), routerNodeID, "drain", transitionRequest{OperationID: "deploy-18", Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}})
		drainDone <- err
	}()
	select {
	case err := <-drainDone:
		t.Fatal("drain crossed in-flight command", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.release)
	if err := <-commandDone; err != nil {
		t.Fatal(err)
	}
	if err := <-drainDone; err != nil {
		t.Fatal(err)
	}
}

type readyBackend struct {
	registry     harnessclient.RoutingRegistry
	pending      int64
	adapters     map[string]hp.AdapterIdentity
	epochs       map[string]int64
	commandCalls int
}

func (backend *readyBackend) Public(string) (harnessclient.PublicRegistry, bool) {
	return harnessclient.PublicRegistry{}, true
}
func (backend *readyBackend) RoutingRegistry() harnessclient.RoutingRegistry { return backend.registry }
func (backend *readyBackend) Read(_ context.Context, nodeID, _ string, route, _ string) (harnessclient.Response, error) {
	adapter := hp.AdapterIdentity{Kind: "cursor", Version: "1.0.31"}
	if value, ok := backend.adapters[nodeID]; ok {
		adapter = value
	}
	epoch := int64(7)
	if value, ok := backend.epochs[nodeID]; ok {
		epoch = value
	}
	identity := hp.NodeIdentity{ProtocolVersion: hp.ProtocolVersion, SchemaID: hp.SchemaID, SchemaSHA256: hp.SchemaSHA256, NodeID: nodeID, RegistryVersion: 1, IdentityEpoch: epoch, Adapter: adapter}
	var value any
	switch route {
	case "identity":
		value = identity
	case "health/ready":
		value = hp.HealthReady{ProtocolVersion: hp.ProtocolVersion, SchemaID: hp.SchemaID, Identity: identity, Readiness: "ready", BlockedReasons: []string{}}
	case "snapshot":
		value = hp.Snapshot{ProtocolVersion: hp.ProtocolVersion, SchemaID: hp.SchemaID, NodeID: nodeID, Epoch: epoch, Completeness: "complete", Node: hp.NodeState{TransportAvailability: "online", EngineReadiness: "ready", Occupancy: "idle", PendingCount: backend.pending, BlockedReasons: []string{}}, PendingQueue: []hp.Request{}}
	default:
		return harnessclient.Response{}, &harnessclient.Fault{Status: 404, Code: "not_found"}
	}
	body, _ := json.Marshal(value)
	return harnessclient.Response{Status: 200, Body: body}, nil
}
func (backend *readyBackend) Command(context.Context, string, string, []byte) (harnessclient.Response, error) {
	backend.commandCalls++
	return harnessclient.Response{Status: 202}, nil
}
func (backend *readyBackend) CommandFenced(ctx context.Context, nodeID, owner string, body []byte, _ hp.NodeIdentity) (harnessclient.Response, error) {
	return backend.Command(ctx, nodeID, owner, body)
}
func (backend *readyBackend) OpenEvents(context.Context, string, string, int64) (*harnessclient.Stream, error) {
	return nil, nil
}
func (backend *readyBackend) Artifact(context.Context, string, string, string, string) (harnessclient.BinaryResponse, error) {
	return harnessclient.BinaryResponse{}, nil
}
func (backend *readyBackend) Close() {}

func TestSealRechecksQuiescenceInsideCommandBarrier(t *testing.T) {
	backend := &readyBackend{registry: routingRegistry(), pending: 1}
	router, statePath, _ := managedFixture(t, backend, eligibleState())
	drained, err := router.transition(context.Background(), routerNodeID, "drain", transitionRequest{OperationID: "deploy-20", Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}})
	if err != nil {
		t.Fatal(err)
	}
	seal := transitionRequest{OperationID: "deploy-20", Expected: expectedState{Mode: ModeDraining, StateVersion: drained.StateVersion, Generation: drained.Generation}}
	if _, err := router.transition(context.Background(), routerNodeID, "seal", seal); err == nil {
		t.Fatal("non-empty queue was sealed")
	}
	persisted, err := decodeState(statePath)
	if err != nil || persisted.Nodes[routerNodeID].Mode != ModeDraining || persisted.Nodes[routerNodeID].StateVersion != drained.StateVersion {
		t.Fatal("failed seal changed durable fence", persisted, err)
	}
	backend.pending = 0
	sealed, err := router.transition(context.Background(), routerNodeID, "seal", seal)
	if err != nil || sealed.Mode != ModeSealed {
		t.Fatal("quiescent node was not sealed", sealed, err)
	}
}

func TestActivationRequiresExactQuiescentIdentity(t *testing.T) {
	registry := routingRegistry()
	initial := bootstrapState(registry)
	backend := &readyBackend{registry: registry, pending: 1}
	router, _, _ := managedFixture(t, backend, initial)
	input := transitionRequest{OperationID: bootstrapOpID, Expected: expectedState{Mode: ModeSealed, StateVersion: 1, Generation: 0}, IdentityEpoch: 7, AdapterVersion: "1.0.31"}
	if _, err := router.transition(context.Background(), routerNodeID, "activate", input); err == nil {
		t.Fatal("non-empty queue was activated")
	}
	backend.pending = 0
	activated, err := router.transition(context.Background(), routerNodeID, "activate", input)
	if err != nil || activated.Mode != ModeEligible || activated.Generation != 1 || activated.StateVersion != 2 || activated.OperationID != "" {
		t.Fatal("exact ready node was not activated", activated, err)
	}
}

func TestBatchActivationIsAllOrNothing(t *testing.T) {
	registry := routingRegistry()
	registry.Nodes = append(registry.Nodes, harnessclient.PublicNode{NodeID: secondRouterNodeID, Name: "Codex", Adapter: "codex"})
	state := eligibleState()
	state.Nodes[secondRouterNodeID] = NodeState{Mode: ModeEligible, StateVersion: 1, Generation: 1, IdentityEpoch: 9, AdapterKind: "codex", AdapterVersion: "0.153.4"}
	backend := &readyBackend{
		registry: registry,
		adapters: map[string]hp.AdapterIdentity{
			routerNodeID:       {Kind: "cursor", Version: "1.0.31"},
			secondRouterNodeID: {Kind: "codex", Version: "0.153.4"},
		},
		epochs: map[string]int64{routerNodeID: 7, secondRouterNodeID: 9},
	}
	router, statePath, _ := managedFixture(t, backend, state)
	drained, err := router.transitionBatch(context.Background(), "drain", "deploy-21", map[string]nodeTransitionRequest{
		routerNodeID:       {Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}},
		secondRouterNodeID: {Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := router.transitionBatch(context.Background(), "seal", "deploy-21", map[string]nodeTransitionRequest{
		routerNodeID:       {Expected: expectedState{Mode: ModeDraining, StateVersion: drained[routerNodeID].StateVersion, Generation: 1}},
		secondRouterNodeID: {Expected: expectedState{Mode: ModeDraining, StateVersion: drained[secondRouterNodeID].StateVersion, Generation: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]nodeTransitionRequest{
		routerNodeID: {Expected: expectedState{Mode: ModeSealed, StateVersion: sealed[routerNodeID].StateVersion, Generation: 1},
			IdentityEpoch: 7, AdapterVersion: "1.0.31"},
		secondRouterNodeID: {Expected: expectedState{Mode: ModeSealed, StateVersion: sealed[secondRouterNodeID].StateVersion, Generation: 1},
			IdentityEpoch: 9, AdapterVersion: "0.153.5"},
	}
	if _, err := router.transitionBatch(context.Background(), "activate", "deploy-21", bad); err == nil {
		t.Fatal("batch activation accepted one mismatched node")
	}
	persisted, err := decodeState(statePath)
	if err != nil || persisted.Nodes[routerNodeID].Mode != ModeSealed || persisted.Nodes[secondRouterNodeID].Mode != ModeSealed {
		t.Fatal("failed batch activation partially reopened nodes", persisted, err)
	}
	bad[secondRouterNodeID] = nodeTransitionRequest{
		Expected:       expectedState{Mode: ModeSealed, StateVersion: sealed[secondRouterNodeID].StateVersion, Generation: 1},
		IdentityEpoch:  9,
		AdapterVersion: "0.153.4",
	}
	activated, err := router.transitionBatch(context.Background(), "activate", "deploy-21", bad)
	if err != nil || activated[routerNodeID].Mode != ModeEligible || activated[secondRouterNodeID].Mode != ModeEligible {
		t.Fatal("exact batch activation failed", activated, err)
	}
}

func TestPostRenamePersistenceErrorPoisonsAdmission(t *testing.T) {
	backend := &readyBackend{registry: routingRegistry()}
	router, _, _ := managedFixture(t, backend, eligibleState())
	router.persist = func(string, State, bool) (bool, error) {
		return true, errors.New("synthetic directory sync failure")
	}
	_, err := router.transition(context.Background(), routerNodeID, "drain", transitionRequest{
		OperationID: "deploy-22",
		Expected:    expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1},
	})
	if err == nil || !router.poisoned.Load() || router.nodes[routerNodeID].state.Mode != ModeDraining {
		t.Fatal("post-rename failure did not poison Router", err)
	}
	_, err = router.Command(context.Background(), routerNodeID, "1-1", commandFixture(t, "command.2.message.enqueue"))
	var fault *harnessclient.Fault
	if !errors.As(err, &fault) || fault.Status != 503 || backend.commandCalls != 0 {
		t.Fatal("poisoned Router admitted work", err, backend.commandCalls)
	}
}

func TestPostRenamePersistenceFailurePoisonsAdmission(t *testing.T) {
	backend := &readyBackend{registry: routingRegistry()}
	router, _, _ := managedFixture(t, backend, eligibleState())
	router.persist = func(string, State, bool) (bool, error) {
		return true, errors.New("synthetic directory fsync failure")
	}
	_, err := router.transition(context.Background(), routerNodeID, "drain", transitionRequest{
		OperationID: "deploy-22",
		Expected:    expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1},
	})
	if err == nil || !router.poisoned.Load() || router.snapshot().Nodes[routerNodeID].Mode != ModeDraining {
		t.Fatal("post-rename failure did not poison the committed fence", err)
	}
	_, err = router.Command(context.Background(), routerNodeID, "1-1", commandFixture(t, "command.2.message.enqueue"))
	var fault *harnessclient.Fault
	if !errors.As(err, &fault) || fault.Status != 503 || fault.Code != "node_unavailable" || backend.commandCalls != 0 {
		t.Fatal("poisoned Router admitted a command", err, backend.commandCalls)
	}
}

type poisonBarrierBackend struct {
	*readyBackend
	nodeID  string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (backend *poisonBarrierBackend) Read(ctx context.Context, nodeID, owner, route, query string) (harnessclient.Response, error) {
	if nodeID == backend.nodeID && route == "identity" {
		backend.once.Do(func() { close(backend.started) })
		select {
		case <-backend.release:
		case <-ctx.Done():
			return harnessclient.Response{}, ctx.Err()
		}
	}
	return backend.readyBackend.Read(ctx, nodeID, owner, route, query)
}

func TestConcurrentTransitionCannotPersistAfterRouterIsPoisoned(t *testing.T) {
	registry := routingRegistry()
	registry.Nodes = append(registry.Nodes, harnessclient.PublicNode{NodeID: secondRouterNodeID, Name: "Codex", Adapter: "codex"})
	state := eligibleState()
	state.Nodes[secondRouterNodeID] = NodeState{Mode: ModeEligible, StateVersion: 1, Generation: 1, IdentityEpoch: 9, AdapterKind: "codex", AdapterVersion: "0.153.4"}
	ready := &readyBackend{
		registry: registry,
		adapters: map[string]hp.AdapterIdentity{
			routerNodeID:       {Kind: "cursor", Version: "1.0.31"},
			secondRouterNodeID: {Kind: "codex", Version: "0.153.4"},
		},
		epochs: map[string]int64{routerNodeID: 7, secondRouterNodeID: 9},
	}
	backend := &poisonBarrierBackend{readyBackend: ready, nodeID: secondRouterNodeID, started: make(chan struct{}), release: make(chan struct{})}
	router, _, _ := managedFixture(t, backend, state)
	drainedSecond, err := router.transition(context.Background(), secondRouterNodeID, "drain", transitionRequest{
		OperationID: "deploy-24", Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	router.persist = func(string, State, bool) (bool, error) {
		writes.Add(1)
		return true, errors.New("synthetic directory fsync failure")
	}
	sealDone := make(chan error, 1)
	go func() {
		_, transitionErr := router.transition(context.Background(), secondRouterNodeID, "seal", transitionRequest{
			OperationID: "deploy-24", Expected: expectedState{Mode: ModeDraining, StateVersion: drainedSecond.StateVersion, Generation: 1},
		})
		sealDone <- transitionErr
	}()
	<-backend.started
	_, poisonErr := router.transition(context.Background(), routerNodeID, "drain", transitionRequest{
		OperationID: "deploy-25", Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1},
	})
	if poisonErr == nil || !router.poisoned.Load() {
		t.Fatal("first transition did not poison Router", poisonErr)
	}
	close(backend.release)
	if err := <-sealDone; err == nil {
		t.Fatal("concurrent transition persisted after poison")
	}
	if writes.Load() != 1 || router.nodes[secondRouterNodeID].state.Mode != ModeDraining {
		t.Fatal("poisoned Router performed a second state write", writes.Load(), router.nodes[secondRouterNodeID].state)
	}
}

func unixClient(path string) *http.Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	return &http.Client{Transport: transport, Timeout: 2 * time.Second}
}

func TestControlSocketBatchCAS(t *testing.T) {
	backend := &readyBackend{registry: routingRegistry()}
	_, _, socketPath := managedFixture(t, backend, eligibleState())
	client := unixClient(socketPath)
	body := []byte(`{"operationId":"deploy-23","nodes":{"` + routerNodeID +
		`":{"expected":{"mode":"eligible","stateVersion":1,"generation":1}}}}`)
	request, _ := http.NewRequest(http.MethodPost, "http://router/v1/nodes/drain", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result batchTransitionResponse
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&result) != nil ||
		result.Nodes[routerNodeID].Mode != ModeDraining {
		t.Fatal("batch control transition failed", response.StatusCode, result)
	}
}

func TestControlSocketCASAndStateLock(t *testing.T) {
	backend := &fakeBackend{routing: routingRegistry()}
	router, statePath, socketPath := managedFixture(t, backend, eligibleState())
	info, err := os.Lstat(socketPath)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("control socket is not private", info, err)
	}
	client := unixClient(socketPath)
	response, err := client.Get("http://router/v1/state")
	if err != nil || response.StatusCode != 200 {
		t.Fatal("state read failed", err)
	}
	response.Body.Close()
	body := []byte(`{"operationId":"deploy-19","expected":{"mode":"eligible","stateVersion":1,"generation":1}}`)
	post := func() int {
		request, _ := http.NewRequest(http.MethodPost, "http://router/v1/nodes/"+routerNodeID+"/drain", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	if got := post(); got != 200 {
		t.Fatal("drain status", got)
	}
	if got := post(); got != 409 {
		t.Fatal("stale CAS status", got)
	}
	if second, err := newManaged(&fakeBackend{routing: routingRegistry()}, statePath, filepath.Join(filepath.Dir(statePath), "other.sock")); err == nil {
		second.Close()
		t.Fatal("second Router acquired the same state")
	}
	router.Close()
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatal("control socket survived close", err)
	}
	reloaded, err := newManaged(&fakeBackend{routing: routingRegistry()}, statePath, socketPath)
	if err != nil {
		t.Fatal("durable state did not reload", err)
	}
	defer reloaded.Close()
	if reloaded.snapshot().Nodes[routerNodeID].Mode != ModeDraining {
		t.Fatal("reload lost drain state")
	}
}

func TestManagedRouterRejectsCorruptOrRegistryDriftedState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string, *fakeBackend)
	}{
		{"registry drift", func(_ string, backend *fakeBackend) { backend.routing.ManifestSHA256 = strings.Repeat("b", 64) }},
		{"corrupt state", func(path string, _ *fakeBackend) { _ = os.WriteFile(path, []byte(`{"schema":1}`), 0o600) }},
		{"permissive state", func(path string, _ *fakeBackend) { _ = os.Chmod(path, 0o644) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory, err := os.MkdirTemp("", "harness-router-invalid-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(directory)
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(directory, "state.json")
			if _, err := writeState(statePath, eligibleState(), false); err != nil {
				t.Fatal(err)
			}
			backend := &fakeBackend{routing: routingRegistry()}
			test.mutate(statePath, backend)
			router, err := newManaged(backend, statePath, filepath.Join(directory, "control.sock"))
			if err == nil {
				router.Close()
				t.Fatal("invalid state started a managed Router")
			}
		})
	}
}
