// Package harnessrouter is the non-authoritative routing boundary between the
// Panel and Harness nodes. It preserves exact node affinity and wire payloads;
// admission policy and drain state can be added here without coupling the
// browser-facing Panel to the private node transport.
package harnessrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
	"github.com/boxvtk621/homelab-telegram-panel/internal/logicaldelete"
)

// Backend is the private node transport used by Router.
type Backend interface {
	Public(owner string) (harnessclient.PublicRegistry, bool)
	RoutingRegistry() harnessclient.RoutingRegistry
	Read(ctx context.Context, nodeID, owner, route, query string) (harnessclient.Response, error)
	Command(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error)
	CommandFenced(ctx context.Context, nodeID, owner string, body []byte, expected hp.NodeIdentity) (harnessclient.Response, error)
	OpenEvents(ctx context.Context, nodeID, owner string, after int64) (*harnessclient.Stream, error)
	Artifact(ctx context.Context, nodeID, owner, artifactID, byteRange string) (harnessclient.BinaryResponse, error)
	TranscriptChunk(ctx context.Context, nodeID, owner string, request harnessclient.TranscriptChunkRequest) (harnessclient.TranscriptChunkResponse, error)
	Close()
}

// AdministrativeBackend is deliberately separate from the browser-facing
// Backend surface. Only the local operator control plane may obtain and act on
// quiescence proofs.
type AdministrativeBackend interface {
	InstallHold(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error)
	QuiescenceProof(ctx context.Context, nodeID, owner, operationID string) (harnessclient.Response, error)
	ReleaseHold(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error)
}

type LogicalDeleteBackend interface {
	DeleteLogicalDialog(ctx context.Context, nodeID, owner string, request logicaldelete.NodeRequest) (harnessclient.Response, error)
	LogicalDeleteStatus(ctx context.Context, nodeID, owner, operationID string) (harnessclient.Response, error)
}

// HistoryExportBackend is separate from Backend so the replication stream is
// unavailable to browser-facing generic reads and old fixture transports remain
// read-only compatible.
type HistoryExportBackend interface {
	ExportHistory(context.Context, string, string, historyreplica.StreamIdentity, int64, int) (historyreplica.ExportPage, error)
}

type nodeRoute struct {
	mu    sync.RWMutex
	state NodeState
}

type registryFactory func([]byte) (Backend, error)

type Router struct {
	backendMu             sync.RWMutex
	backend               Backend
	registry              harnessclient.RoutingRegistry
	registryFactory       registryFactory
	registryOperationID   string
	registryRequestSHA256 string
	registryEnvelope      json.RawMessage
	statePath             string
	stateMu               sync.Mutex
	nodes                 map[string]*nodeRoute
	lock                  *stateLock
	control               *controlServer
	closeOnce             sync.Once
	poisoned              atomic.Bool
	persist               func(string, State, bool, func() error) (bool, error)
	beforeCommit          func()
}

func Load(paths harnessclient.Paths, statePath, controlSocket string) (*Router, error) {
	if paths.Registry == "" {
		client, err := harnessclient.Load(paths)
		if err != nil {
			return nil, err
		}
		if statePath != "" || controlSocket != "" {
			client.Close()
			return nil, errors.New("Harness Router state requires a configured registry")
		}
		return New(client)
	}
	router, err := loadManaged(paths, statePath, controlSocket)
	if err != nil {
		return nil, err
	}
	return router, nil
}

func New(backend Backend) (*Router, error) {
	if backend == nil {
		return nil, errors.New("missing Harness Router backend")
	}
	return &Router{backend: backend, registry: backend.RoutingRegistry()}, nil
}

func newManaged(backend Backend, statePath, controlSocket string) (*Router, error) {
	if backend == nil || !filepath.IsAbs(statePath) || filepath.Clean(statePath) != statePath ||
		!filepath.IsAbs(controlSocket) || filepath.Clean(controlSocket) != controlSocket ||
		filepath.Dir(statePath) != filepath.Dir(controlSocket) || statePath == controlSocket {
		return nil, errors.New("invalid Harness Router managed paths")
	}
	registry := backend.RoutingRegistry()
	if registry.OwnerID == "" {
		return nil, errors.New("managed Harness Router requires an owner")
	}
	observedState, err := decodeState(statePath)
	if err != nil {
		return nil, errors.New("invalid Harness Router state")
	}
	lock, err := acquireStateLockMode(filepath.Dir(statePath), false, observedState.Schema == LegacyStateSchema)
	if err != nil {
		return nil, err
	}
	state, err := decodeState(statePath)
	if err != nil || !exactState(state, observedState) || validateState(state, registry) != nil {
		lock.close()
		return nil, errors.New("invalid Harness Router state")
	}
	return managedRouter(backend, state, statePath, controlSocket, lock, nil)
}

func loadManaged(paths harnessclient.Paths, statePath, controlSocket string) (*Router, error) {
	if !filepath.IsAbs(statePath) || filepath.Clean(statePath) != statePath ||
		!filepath.IsAbs(controlSocket) || filepath.Clean(controlSocket) != controlSocket ||
		filepath.Dir(statePath) != filepath.Dir(controlSocket) || statePath == controlSocket {
		return nil, errors.New("invalid Harness Router managed paths")
	}
	observedState, err := decodeState(statePath)
	if err != nil {
		return nil, err
	}
	lock, err := acquireStateLockMode(filepath.Dir(statePath), false, observedState.Schema == LegacyStateSchema)
	if err != nil {
		return nil, err
	}
	state, err := decodeState(statePath)
	if err != nil || !exactState(state, observedState) {
		lock.close()
		return nil, errors.New("invalid Harness Router state")
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.LoadRaw(paths, raw)
	})
	var backend Backend
	if state.Schema == ProjectionStateSchema && len(state.RegistryEnvelope) != 0 {
		backend, err = factory(state.RegistryEnvelope)
	} else {
		backend, err = harnessclient.Load(paths)
	}
	if err != nil {
		lock.close()
		return nil, err
	}
	if validateState(state, backend.RoutingRegistry()) != nil {
		backend.Close()
		lock.close()
		return nil, errors.New("invalid Harness Router state")
	}
	router, err := managedRouter(backend, state, statePath, controlSocket, lock, factory)
	if err != nil {
		backend.Close()
	}
	return router, err
}

func managedRouter(backend Backend, state State, statePath, controlSocket string, lock *stateLock, factory registryFactory) (*Router, error) {
	registry := backend.RoutingRegistry()
	router := &Router{
		backend: backend, registry: registry, registryFactory: factory, statePath: statePath,
		registryOperationID: state.RegistryOperationID, registryRequestSHA256: state.RegistryRequestSHA256,
		registryEnvelope: append(json.RawMessage(nil), state.RegistryEnvelope...),
		nodes:            map[string]*nodeRoute{}, lock: lock, persist: writeStateGuarded,
	}
	for nodeID, value := range state.Nodes {
		router.nodes[nodeID] = &nodeRoute{state: value}
	}
	control, err := openControlServer(router, controlSocket)
	if err != nil {
		lock.close()
		return nil, err
	}
	router.control = control
	return router, nil
}

func (r *Router) Public(owner string) (harnessclient.PublicRegistry, bool) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	return r.backend.Public(owner)
}

// OwnerID returns the operator-signed identity used for every private Harness
// request. It is never accepted from the browser or inferred from YouTrack.
func (r *Router) OwnerID() string {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	return r.registry.OwnerID
}

func (r *Router) Read(ctx context.Context, nodeID, owner, route, query string) (harnessclient.Response, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	return r.backend.Read(ctx, nodeID, owner, route, query)
}

func (r *Router) ExportHistory(ctx context.Context, nodeID, owner string, identity historyreplica.StreamIdentity, after int64, limit int) (historyreplica.ExportPage, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	exporter, ok := r.backend.(HistoryExportBackend)
	if !ok {
		return historyreplica.ExportPage{}, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "node_unavailable"}
	}
	return exporter.ExportHistory(ctx, nodeID, owner, identity, after, limit)
}

func (r *Router) InstallHold(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	return r.administrative(ctx, func(backend AdministrativeBackend) (harnessclient.Response, error) {
		return backend.InstallHold(ctx, nodeID, owner, body)
	})
}

func (r *Router) QuiescenceProof(ctx context.Context, nodeID, owner, operationID string) (harnessclient.Response, error) {
	return r.administrative(ctx, func(backend AdministrativeBackend) (harnessclient.Response, error) {
		return backend.QuiescenceProof(ctx, nodeID, owner, operationID)
	})
}

func (r *Router) ReleaseHold(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	return r.administrative(ctx, func(backend AdministrativeBackend) (harnessclient.Response, error) {
		return backend.ReleaseHold(ctx, nodeID, owner, body)
	})
}

func (r *Router) DeleteLogicalDialog(ctx context.Context, nodeID, owner string, request logicaldelete.NodeRequest) (harnessclient.Response, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	if !r.ensureStateLock() || r.poisoned.Load() {
		return harnessclient.Response{}, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "node_unavailable"}
	}
	backend, ok := r.backend.(LogicalDeleteBackend)
	if !ok {
		return harnessclient.Response{}, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "node_unavailable"}
	}
	return backend.DeleteLogicalDialog(ctx, nodeID, owner, request)
}

func (r *Router) LogicalDeleteStatus(ctx context.Context, nodeID, owner, operationID string) (harnessclient.Response, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	if !r.ensureStateLock() || r.poisoned.Load() {
		return harnessclient.Response{}, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "node_unavailable"}
	}
	backend, ok := r.backend.(LogicalDeleteBackend)
	if !ok {
		return harnessclient.Response{}, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "node_unavailable"}
	}
	return backend.LogicalDeleteStatus(ctx, nodeID, owner, operationID)
}

func (r *Router) administrative(ctx context.Context, call func(AdministrativeBackend) (harnessclient.Response, error)) (harnessclient.Response, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	if !r.ensureStateLock() || r.poisoned.Load() {
		return harnessclient.Response{}, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "node_unavailable"}
	}
	backend, ok := r.backend.(AdministrativeBackend)
	if !ok {
		return harnessclient.Response{}, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "node_unavailable"}
	}
	return call(backend)
}

func (r *Router) Command(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	if !r.ensureStateLock() {
		return harnessclient.Response{}, &harnessclient.Fault{Status: 503, Code: "node_unavailable"}
	}
	if r.nodes != nil && owner == r.registry.OwnerID {
		var command hp.CommandEnvelope
		route := r.nodes[nodeID]
		if route != nil && hp.Validate("command", body) == nil && json.Unmarshal(body, &command) == nil {
			route.mu.RLock()
			defer route.mu.RUnlock()
			if r.poisoned.Load() {
				return harnessclient.Response{}, &harnessclient.Fault{Status: 503, Code: "node_unavailable"}
			}
			if commandBlockedByRoute(route.state, command) {
				return harnessclient.Response{}, &harnessclient.Fault{Status: 409, Code: "stale"}
			}
			state := route.state
			if state.Compatibility == "legacy_readonly" {
				return harnessclient.Response{}, &harnessclient.Fault{Status: 409, Code: "stale"}
			}
			registryVersion := r.registry.RegistryVersion
			if state.RegistrationRevision > 0 {
				registryVersion = state.RegistrationRevision
			}
			expected := hp.NodeIdentity{NodeID: nodeID, RegistryVersion: registryVersion,
				IdentityEpoch: state.IdentityEpoch, Adapter: hp.AdapterIdentity{Kind: state.AdapterKind, Version: state.AdapterVersion}}
			return r.backend.CommandFenced(ctx, nodeID, owner, body, expected)
		}
	}
	return r.backend.Command(ctx, nodeID, owner, body)
}

func commandBlockedByRoute(state NodeState, command hp.CommandEnvelope) bool {
	if hp.IsDrainControlCommand(command.Kind) {
		return false
	}
	if state.Mode == ModeDraining {
		return true
	}
	if state.Mode != ModeSealed {
		return false
	}
	if state.SealScope == nil || state.SealScope.Kind == "node" {
		return true
	}
	var target struct {
		DialogID string `json:"dialogId"`
	}
	// Commands without a direct dialog binding are delegated to Harness, which
	// is the authoritative scope barrier and can resolve request/attempt IDs.
	return json.Unmarshal(command.Target, &target) == nil && target.DialogID == state.SealScope.DialogID
}

func (r *Router) ensureStateLock() bool {
	if r.lock == nil {
		return true
	}
	if err := r.lock.ensure(); err != nil {
		r.poisoned.Store(true)
		return false
	}
	return true
}

func (r *Router) commitGuard() error {
	if r.beforeCommit != nil {
		r.beforeCommit()
	}
	if r.lock == nil {
		return nil
	}
	if err := r.lock.ensure(); err != nil {
		r.poisoned.Store(true)
		return err
	}
	return nil
}

func (r *Router) OpenEvents(ctx context.Context, nodeID, owner string, after int64) (*harnessclient.Stream, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	return r.backend.OpenEvents(ctx, nodeID, owner, after)
}

func (r *Router) Artifact(ctx context.Context, nodeID, owner, artifactID, byteRange string) (harnessclient.BinaryResponse, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	return r.backend.Artifact(ctx, nodeID, owner, artifactID, byteRange)
}

func (r *Router) TranscriptChunk(ctx context.Context, nodeID, owner string, request harnessclient.TranscriptChunkRequest) (harnessclient.TranscriptChunkResponse, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	return r.backend.TranscriptChunk(ctx, nodeID, owner, request)
}

func (r *Router) Close() {
	r.closeOnce.Do(func() {
		if r.control != nil {
			r.control.close()
		}
		r.backendMu.Lock()
		r.backend.Close()
		r.backendMu.Unlock()
		if r.lock != nil {
			r.lock.close()
		}
	})
}

func (r *Router) snapshot() State {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.snapshotLocked()
}

func (r *Router) snapshotLocked() State {
	schema := LegacyStateSchema
	if r.registry.SchemaID == harnessclient.RouterRegistrySchemaID {
		schema = ProjectionStateSchema
	}
	state := State{
		Schema: schema, OwnerID: r.registry.OwnerID, RegistryVersion: r.registry.RegistryVersion,
		RegistrySHA256: r.registry.ManifestSHA256, RegistryOperationID: r.registryOperationID,
		RegistryRequestSHA256: r.registryRequestSHA256,
		RegistryEnvelope:      append(json.RawMessage(nil), r.registryEnvelope...),
		Nodes:                 make(map[string]NodeState, len(r.nodes)),
	}
	for nodeID, route := range r.nodes {
		state.Nodes[nodeID] = route.state
	}
	return state
}

// persistNode is called with route.mu held for writing. Every state mutation is
// written and fsynced before the new admission mode becomes visible in memory.
func (r *Router) persistNode(nodeID string, _ *nodeRoute, next NodeState) error {
	return r.persistNodes(map[string]NodeState{nodeID: next})
}

// persistNodes is called with every affected route locked for writing. One
// rename commits the whole transition set, so a multi-node reopen cannot split.
func (r *Router) persistNodes(next map[string]NodeState) error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.poisoned.Load() {
		return &controlFault{status: 503, code: "state_unavailable"}
	}
	if !r.ensureStateLock() {
		return &controlFault{status: 503, code: "state_unavailable"}
	}
	state := r.snapshotLocked()
	for nodeID, value := range next {
		state.Nodes[nodeID] = value
	}
	if err := validateState(state, r.registry); err != nil {
		return err
	}
	committed, err := r.persist(r.statePath, state, true, r.commitGuard)
	if committed {
		for nodeID, value := range next {
			r.nodes[nodeID].state = value
		}
	}
	if err != nil {
		if committed {
			r.poisoned.Store(true)
		}
		return err
	}
	return nil
}
