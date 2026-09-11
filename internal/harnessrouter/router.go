// Package harnessrouter is the non-authoritative routing boundary between the
// Panel and Harness nodes. It preserves exact node affinity and wire payloads;
// admission policy and drain state can be added here without coupling the
// browser-facing Panel to the private node transport.
package harnessrouter

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
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
	Close()
}

type nodeRoute struct {
	mu    sync.RWMutex
	state NodeState
}

type Router struct {
	backend   Backend
	registry  harnessclient.RoutingRegistry
	statePath string
	stateMu   sync.Mutex
	nodes     map[string]*nodeRoute
	lock      *stateLock
	control   *controlServer
	closeOnce sync.Once
	poisoned  atomic.Bool
	persist   func(string, State, bool) (bool, error)
}

func Load(paths harnessclient.Paths, statePath, controlSocket string) (*Router, error) {
	client, err := harnessclient.Load(paths)
	if err != nil {
		return nil, err
	}
	if paths.Registry == "" {
		if statePath != "" || controlSocket != "" {
			client.Close()
			return nil, errors.New("Harness Router state requires a configured registry")
		}
		return New(client)
	}
	router, err := newManaged(client, statePath, controlSocket)
	if err != nil {
		client.Close()
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
	if registry.OwnerID == "" || len(registry.Nodes) == 0 {
		return nil, errors.New("managed Harness Router requires at least one node")
	}
	lock, err := acquireStateLock(filepath.Dir(statePath))
	if err != nil {
		return nil, err
	}
	state, err := decodeState(statePath)
	if err == nil {
		err = validateState(state, registry)
	}
	if err != nil {
		lock.close()
		return nil, err
	}
	router := &Router{backend: backend, registry: registry, statePath: statePath, nodes: map[string]*nodeRoute{}, lock: lock, persist: writeState}
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
	return r.backend.Public(owner)
}

// OwnerID returns the operator-signed identity used for every private Harness
// request. It is never accepted from the browser or inferred from YouTrack.
func (r *Router) OwnerID() string {
	return r.registry.OwnerID
}

func (r *Router) Read(ctx context.Context, nodeID, owner, route, query string) (harnessclient.Response, error) {
	return r.backend.Read(ctx, nodeID, owner, route, query)
}

func (r *Router) Command(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	if r.nodes != nil && owner == r.registry.OwnerID {
		var command hp.CommandEnvelope
		route := r.nodes[nodeID]
		if route != nil && hp.Validate("command", body) == nil && json.Unmarshal(body, &command) == nil {
			route.mu.RLock()
			defer route.mu.RUnlock()
			if r.poisoned.Load() {
				return harnessclient.Response{}, &harnessclient.Fault{Status: 503, Code: "node_unavailable"}
			}
			if route.state.Mode == ModeSealed || (route.state.Mode == ModeDraining && !hp.IsDrainControlCommand(command.Kind)) {
				return harnessclient.Response{}, &harnessclient.Fault{Status: 409, Code: "stale"}
			}
			state := route.state
			expected := hp.NodeIdentity{NodeID: nodeID, RegistryVersion: r.registry.RegistryVersion,
				IdentityEpoch: state.IdentityEpoch, Adapter: hp.AdapterIdentity{Kind: state.AdapterKind, Version: state.AdapterVersion}}
			return r.backend.CommandFenced(ctx, nodeID, owner, body, expected)
		}
	}
	return r.backend.Command(ctx, nodeID, owner, body)
}

func (r *Router) OpenEvents(ctx context.Context, nodeID, owner string, after int64) (*harnessclient.Stream, error) {
	return r.backend.OpenEvents(ctx, nodeID, owner, after)
}

func (r *Router) Artifact(ctx context.Context, nodeID, owner, artifactID, byteRange string) (harnessclient.BinaryResponse, error) {
	return r.backend.Artifact(ctx, nodeID, owner, artifactID, byteRange)
}

func (r *Router) Close() {
	r.closeOnce.Do(func() {
		if r.control != nil {
			r.control.close()
		}
		r.backend.Close()
		if r.lock != nil {
			r.lock.close()
		}
	})
}

func (r *Router) snapshot() State {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.snapshotLocked()
}

func (r *Router) snapshotLocked() State {
	state := State{Schema: StateSchema, OwnerID: r.registry.OwnerID, RegistryVersion: r.registry.RegistryVersion, RegistrySHA256: r.registry.ManifestSHA256, Nodes: make(map[string]NodeState, len(r.nodes))}
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
	state := r.snapshotLocked()
	for nodeID, value := range next {
		state.Nodes[nodeID] = value
	}
	if err := validateState(state, r.registry); err != nil {
		return err
	}
	committed, err := r.persist(r.statePath, state, true)
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
