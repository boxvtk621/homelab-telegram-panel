package harnessrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

type controlServer struct {
	server   *http.Server
	listener net.Listener
	path     string
	info     os.FileInfo
	once     sync.Once
}

type expectedState struct {
	Mode         string `json:"mode"`
	StateVersion int64  `json:"stateVersion"`
	Generation   int64  `json:"generation"`
}

type transitionRequest struct {
	OperationID    string        `json:"operationId"`
	Expected       expectedState `json:"expected"`
	IdentityEpoch  int64         `json:"identityEpoch,omitempty"`
	AdapterVersion string        `json:"adapterVersion,omitempty"`
}

type nodeTransitionRequest struct {
	Expected       expectedState `json:"expected"`
	IdentityEpoch  int64         `json:"identityEpoch,omitempty"`
	AdapterVersion string        `json:"adapterVersion,omitempty"`
}

type batchTransitionRequest struct {
	OperationID string                           `json:"operationId"`
	Nodes       map[string]nodeTransitionRequest `json:"nodes"`
}

type batchTransitionResponse struct {
	Nodes map[string]NodeState `json:"nodes"`
}

type controlFault struct {
	status int
	code   string
}

func (fault *controlFault) Error() string { return fault.code }

func openControlServer(router *Router, path string) (*controlServer, error) {
	if current, err := os.Lstat(path); err == nil {
		uid, uidOK := ownerUID(current)
		if !uidOK || current.Mode()&os.ModeSocket == 0 || current.Mode()&os.ModeSymlink != 0 || current.Mode().Perm()&0o077 != 0 || uid != os.Geteuid() {
			return nil, errors.New("unsafe Harness Router control socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, errors.New("cannot remove stale Harness Router control socket")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("cannot inspect Harness Router control socket")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("cannot listen on Harness Router control socket")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("cannot protect Harness Router control socket")
	}
	info, err := os.Lstat(path)
	if err != nil {
		listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("cannot inspect Harness Router control socket")
	}
	control := &controlServer{listener: listener, path: path, info: info}
	control.server = &http.Server{
		Handler:           controlHandler{router: router},
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() { _ = control.server.Serve(listener) }()
	return control, nil
}

func (control *controlServer) close() {
	control.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if control.server.Shutdown(ctx) != nil {
			_ = control.server.Close()
		}
		cancel()
		_ = control.listener.Close()
		if current, err := os.Lstat(control.path); err == nil && os.SameFile(current, control.info) {
			_ = os.Remove(control.path)
		}
	})
}

type controlHandler struct {
	router *Router
}

func controlReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (handler controlHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.RawPath != "" || request.URL.RawQuery != "" || request.URL.ForceQuery {
		controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/state" {
		if request.ContentLength > 0 || len(request.TransferEncoding) != 0 {
			controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
			return
		}
		if handler.router.poisoned.Load() {
			controlReply(w, http.StatusServiceUnavailable, map[string]string{"error": "state_unavailable"})
			return
		}
		controlReply(w, http.StatusOK, handler.router.snapshot())
		return
	}
	if request.Method != http.MethodPost {
		controlReply(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	single := len(parts) == 4 && parts[0] == "v1" && parts[1] == "nodes"
	batch := len(parts) == 3 && parts[0] == "v1" && parts[1] == "nodes"
	action := ""
	if single {
		action = parts[3]
	} else if batch {
		action = parts[2]
	}
	if action != "drain" && action != "seal" && action != "activate" && action != "abort" {
		controlReply(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	media, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || request.ContentLength > 64<<10 {
		controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || !strictjson.Valid(raw) {
		controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result any
	if single {
		var input transitionRequest
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
			controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
			return
		}
		result, err = handler.router.transition(request.Context(), parts[2], action, input)
	} else {
		var input batchTransitionRequest
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
			controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
			return
		}
		states, transitionErr := handler.router.transitionBatch(request.Context(), action, input.OperationID, input.Nodes)
		result, err = batchTransitionResponse{Nodes: states}, transitionErr
	}
	if err != nil {
		fault := &controlFault{status: http.StatusServiceUnavailable, code: "state_unavailable"}
		if errors.As(err, &fault) {
			controlReply(w, fault.status, map[string]string{"error": fault.code})
		} else {
			controlReply(w, http.StatusServiceUnavailable, map[string]string{"error": "state_unavailable"})
		}
		return
	}
	controlReply(w, http.StatusOK, result)
}

func (r *Router) transition(ctx context.Context, nodeID, action string, input transitionRequest) (NodeState, error) {
	states, err := r.transitionBatch(ctx, action, input.OperationID, map[string]nodeTransitionRequest{
		nodeID: {Expected: input.Expected, IdentityEpoch: input.IdentityEpoch, AdapterVersion: input.AdapterVersion},
	})
	return states[nodeID], err
}

func (r *Router) transitionBatch(ctx context.Context, action, operation string, inputs map[string]nodeTransitionRequest) (map[string]NodeState, error) {
	if r.poisoned.Load() {
		return nil, &controlFault{status: http.StatusServiceUnavailable, code: "state_unavailable"}
	}
	if !operationID.MatchString(operation) || len(inputs) == 0 || len(inputs) > len(r.nodes) {
		return nil, &controlFault{status: http.StatusBadRequest, code: "invalid"}
	}
	ids := make([]string, 0, len(inputs))
	for nodeID, input := range inputs {
		if input.Expected.StateVersion < 1 || input.Expected.StateVersion > hp.MaximumSafeInteger ||
			input.Expected.Generation < 0 || input.Expected.Generation > hp.MaximumSafeInteger ||
			input.IdentityEpoch < 0 || input.IdentityEpoch > hp.MaximumSafeInteger {
			return nil, &controlFault{status: http.StatusBadRequest, code: "invalid"}
		}
		if r.nodes[nodeID] == nil {
			return nil, &controlFault{status: http.StatusNotFound, code: "not_found"}
		}
		ids = append(ids, nodeID)
	}
	sort.Strings(ids)
	for _, nodeID := range ids {
		r.nodes[nodeID].mu.Lock()
	}
	defer func() {
		for index := len(ids) - 1; index >= 0; index-- {
			r.nodes[ids[index]].mu.Unlock()
		}
	}()
	if r.poisoned.Load() {
		return nil, &controlFault{status: http.StatusServiceUnavailable, code: "state_unavailable"}
	}
	next := make(map[string]NodeState, len(inputs))
	for _, nodeID := range ids {
		current := r.nodes[nodeID].state
		candidate, err := r.nextState(ctx, nodeID, action, operation, current, inputs[nodeID])
		if err != nil {
			return nil, err
		}
		next[nodeID] = candidate
	}
	if err := r.persistNodes(next); err != nil {
		return nil, err
	}
	return next, nil
}

func (r *Router) nextState(ctx context.Context, nodeID, action, operation string, current NodeState, input nodeTransitionRequest) (NodeState, error) {
	if current.Mode != input.Expected.Mode || current.StateVersion != input.Expected.StateVersion || current.Generation != input.Expected.Generation {
		return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
	}
	next := current
	switch action {
	case "drain":
		if current.Mode != ModeEligible || current.OperationID != "" || input.IdentityEpoch != 0 || input.AdapterVersion != "" {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		next.Mode, next.OperationID = ModeDraining, operation
	case "seal":
		if current.Mode != ModeDraining || current.OperationID != operation || input.IdentityEpoch != 0 || input.AdapterVersion != "" {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if err := r.verifyReady(ctx, nodeID, current.AdapterKind, current.AdapterVersion, current.IdentityEpoch); err != nil {
			return NodeState{}, &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
		}
		next.Mode = ModeSealed
	case "activate":
		if current.Mode != ModeSealed || current.OperationID != operation || input.IdentityEpoch < 1 || !adapterVersion.MatchString(input.AdapterVersion) {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if current.IdentityEpoch != 0 && current.IdentityEpoch != input.IdentityEpoch {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "identity_changed"}
		}
		if err := r.verifyReady(ctx, nodeID, current.AdapterKind, input.AdapterVersion, input.IdentityEpoch); err != nil {
			return NodeState{}, &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
		}
		next.Mode, next.OperationID = ModeEligible, ""
		next.Generation++
		next.IdentityEpoch, next.AdapterVersion = input.IdentityEpoch, input.AdapterVersion
	case "abort":
		if (current.Mode != ModeDraining && current.Mode != ModeSealed) || current.OperationID != operation ||
			input.IdentityEpoch != current.IdentityEpoch || input.AdapterVersion != current.AdapterVersion ||
			input.IdentityEpoch < 1 || !adapterVersion.MatchString(input.AdapterVersion) {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if err := r.verifyReady(ctx, nodeID, current.AdapterKind, current.AdapterVersion, current.IdentityEpoch); err != nil {
			return NodeState{}, &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
		}
		next.Mode, next.OperationID = ModeEligible, ""
		next.Generation++
	default:
		return NodeState{}, &controlFault{status: http.StatusNotFound, code: "not_found"}
	}
	if next.StateVersion == hp.MaximumSafeInteger || next.Generation > hp.MaximumSafeInteger {
		return NodeState{}, &controlFault{status: http.StatusConflict, code: "version_exhausted"}
	}
	next.StateVersion++
	return next, nil
}

func (r *Router) verifyReady(ctx context.Context, nodeID, adapterKind, version string, epoch int64) error {
	_, err := observeReady(ctx, r.backend, r.registry, nodeID, adapterKind, version, epoch)
	return err
}

func observeReady(ctx context.Context, backend Backend, registry harnessclient.RoutingRegistry, nodeID, adapterKind, version string, epoch int64) (hp.NodeIdentity, error) {
	return observeNode(ctx, backend, registry, nodeID, adapterKind, version, epoch, false)
}

func observePreflight(ctx context.Context, backend Backend, registry harnessclient.RoutingRegistry, nodeID, adapterKind, version string, epoch int64) (hp.NodeIdentity, error) {
	return observeNode(ctx, backend, registry, nodeID, adapterKind, version, epoch, true)
}

func observeNode(ctx context.Context, backend Backend, registry harnessclient.RoutingRegistry, nodeID, adapterKind, version string, epoch int64, allowPristinePolicySentinel bool) (hp.NodeIdentity, error) {
	read := func(route string, target any) error {
		response, err := backend.Read(ctx, nodeID, registry.OwnerID, route, "")
		if err != nil || response.Status != http.StatusOK || json.Unmarshal(response.Body, target) != nil {
			return errors.New("node read failed")
		}
		return nil
	}
	var identity hp.NodeIdentity
	if err := read("identity", &identity); err != nil || identity.NodeID != nodeID || identity.RegistryVersion != registry.RegistryVersion ||
		identity.Adapter.Kind != adapterKind || (epoch > 0 && identity.IdentityEpoch != epoch) || (version != "" && identity.Adapter.Version != version) {
		return hp.NodeIdentity{}, errors.New("node identity is not ready")
	}
	var health hp.HealthReady
	if err := read("health/ready", &health); err != nil || health.Identity.NodeID != identity.NodeID ||
		health.Identity.RegistryVersion != identity.RegistryVersion || health.Identity.IdentityEpoch != identity.IdentityEpoch || health.Identity.Adapter != identity.Adapter {
		return hp.NodeIdentity{}, errors.New("node health is not ready")
	}
	var snapshot hp.Snapshot
	if err := read("snapshot", &snapshot); err != nil || snapshot.NodeID != nodeID || snapshot.Epoch != identity.IdentityEpoch || snapshot.Completeness != "complete" ||
		snapshot.Node.TransportAvailability != "online" || snapshot.Node.Occupancy != "idle" || snapshot.Node.QueuePaused ||
		snapshot.Node.ActiveAttemptID != nil || snapshot.ActiveAttempt != nil || snapshot.Node.PendingCount != 0 || len(snapshot.PendingQueue) != 0 {
		return hp.NodeIdentity{}, errors.New("node is not quiescent")
	}
	if health.Readiness == "ready" && len(health.BlockedReasons) == 0 && snapshot.Node.EngineReadiness == "ready" && len(snapshot.Node.BlockedReasons) == 0 {
		return identity, nil
	}
	legacyReason := []string{"policy_unavailable"}
	if allowPristinePolicySentinel && health.Readiness == "blocked" && slices.Equal(health.BlockedReasons, legacyReason) &&
		snapshot.StateVersion == 0 && snapshot.LastEventSeq == 0 && snapshot.Node.QueueVersion == 0 &&
		snapshot.Node.EngineReadiness == "blocked" && slices.Equal(snapshot.Node.BlockedReasons, legacyReason) {
		return identity, nil
	}
	return hp.NodeIdentity{}, errors.New("node health is not ready")
}

// Preflight proves every signed-registry node is ready and quiescent without
// changing Router state or sending a command to a provider.
func Preflight(ctx context.Context, paths harnessclient.Paths) error {
	if paths.Registry == "" {
		return errors.New("managed Harness Router configuration is required")
	}
	client, err := harnessclient.Load(paths)
	if err != nil {
		return err
	}
	defer client.Close()
	registry := client.RoutingRegistry()
	if registry.OwnerID == "" || len(registry.Nodes) == 0 {
		return errors.New("managed Harness Router requires at least one node")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, node := range registry.Nodes {
		if _, err := observePreflight(ctx, client, registry, node.NodeID, node.Adapter, "", 0); err != nil {
			return err
		}
	}
	return nil
}

// Compile-time check that the concrete private transport still satisfies the
// Router backend after registry identity was added to the boundary.
var _ Backend = (*harnessclient.Client)(nil)
