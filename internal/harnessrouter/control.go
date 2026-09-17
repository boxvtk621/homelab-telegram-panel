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

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
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
	OperationID    string                          `json:"operationId"`
	Expected       expectedState                   `json:"expected"`
	IdentityEpoch  int64                           `json:"identityEpoch,omitempty"`
	AdapterVersion string                          `json:"adapterVersion,omitempty"`
	Proof          *harnessbarrier.QuiescenceProof `json:"proof,omitempty"`
	Release        *harnessbarrier.ReleaseRequest  `json:"release,omitempty"`
}

type nodeTransitionRequest struct {
	Expected       expectedState                   `json:"expected"`
	IdentityEpoch  int64                           `json:"identityEpoch,omitempty"`
	AdapterVersion string                          `json:"adapterVersion,omitempty"`
	Proof          *harnessbarrier.QuiescenceProof `json:"proof,omitempty"`
	Release        *harnessbarrier.ReleaseRequest  `json:"release,omitempty"`
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

type admissionFault struct{ code string }

func (fault *admissionFault) Error() string { return fault.code }

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
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		return nil, errors.New("cannot protect Harness Router control socket")
	}
	unixListener.SetUnlinkOnClose(false)
	createdInfo, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, errors.New("cannot inspect Harness Router control socket")
	}
	createdUID, createdOwnerOK := ownerUID(createdInfo)
	if !createdOwnerOK || createdInfo.Mode()&os.ModeSocket == 0 || createdUID != os.Geteuid() {
		_ = listener.Close()
		return nil, errors.New("cannot protect Harness Router control socket")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		removeControlSocket(path, createdInfo)
		return nil, errors.New("cannot protect Harness Router control socket")
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		removeControlSocket(path, createdInfo)
		return nil, errors.New("cannot inspect Harness Router control socket")
	}
	uid, ownerOK := ownerUID(info)
	if !os.SameFile(createdInfo, info) || !ownerOK || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 || uid != os.Geteuid() {
		_ = listener.Close()
		removeControlSocket(path, createdInfo)
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
		removeControlSocket(control.path, control.info)
	})
}

func removeControlSocket(path string, expected os.FileInfo) {
	if expected == nil {
		return
	}
	if current, err := os.Lstat(path); err == nil && os.SameFile(current, expected) {
		_ = os.Remove(path)
	}
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
		if handler.router.poisoned.Load() || !handler.router.ensureStateLock() {
			controlReply(w, http.StatusServiceUnavailable, map[string]string{"error": "state_unavailable"})
			return
		}
		controlReply(w, http.StatusOK, operatorState(handler.router.snapshot()))
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/registry/install" {
		handler.installRegistry(w, request)
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
	if action != "drain" && action != "legacy-seal" && action != "seal" && action != "activate" && action != "release" && action != "abort" {
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

func (handler controlHandler) installRegistry(w http.ResponseWriter, request *http.Request) {
	media, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || request.ContentLength <= 0 || request.ContentLength > 320<<10 || len(request.TransferEncoding) != 0 {
		controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, (320<<10)+1))
	if err != nil || len(raw) > 320<<10 || !strictjson.Valid(raw) || !registryInstallShape(raw) {
		controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input registryInstallRequest
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		controlReply(w, http.StatusBadRequest, map[string]string{"error": "invalid"})
		return
	}
	result, err := handler.router.installRegistry(input)
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
		nodeID: {Expected: input.Expected, IdentityEpoch: input.IdentityEpoch, AdapterVersion: input.AdapterVersion, Proof: input.Proof, Release: input.Release},
	})
	return states[nodeID], err
}

func (r *Router) transitionBatch(ctx context.Context, action, operation string, inputs map[string]nodeTransitionRequest) (map[string]NodeState, error) {
	r.backendMu.RLock()
	defer r.backendMu.RUnlock()
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
		if current.Mode != ModeEligible || current.OperationID != "" || input.IdentityEpoch != 0 || input.AdapterVersion != "" || input.Proof != nil || input.Release != nil {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		next.Mode, next.OperationID = ModeDraining, operation
	case "legacy-seal":
		if current.Mode != ModeDraining || current.OperationID != operation || input.IdentityEpoch != 0 || input.AdapterVersion != "" ||
			input.Proof != nil || input.Release != nil || hasSealProjection(current) {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if err := r.verifyReady(ctx, nodeID, current.AdapterKind, current.AdapterVersion, current.IdentityEpoch); err != nil {
			return NodeState{}, &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
		}
		next.Mode = ModeSealed
	case "seal":
		if input.IdentityEpoch != 0 || input.AdapterVersion != "" || input.Proof == nil || input.Release != nil ||
			(current.Mode != ModeEligible && current.Mode != ModeDraining) ||
			(current.Mode == ModeEligible && current.OperationID != "") ||
			(current.Mode == ModeDraining && current.OperationID != operation) || hasSealProjection(current) {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if err := r.verifyQuiescenceProof(ctx, nodeID, operation, current, *input.Proof); err != nil {
			return NodeState{}, err
		}
		next.Mode, next.OperationID = ModeSealed, operation
		scope := input.Proof.Scope
		next.SealScope = &scope
		next.SealHoldVersion = input.Proof.HoldVersion
		next.SealScopeRevision = input.Proof.ScopeRevision
		next.SealedProofHash = input.Proof.ProofHash
	case "activate":
		if current.Mode != ModeSealed || current.OperationID != operation || input.IdentityEpoch < 1 || !adapterVersion.MatchString(input.AdapterVersion) {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if input.Proof != nil || input.Release != nil || hasSealProjection(current) {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if current.IdentityEpoch != 0 && current.IdentityEpoch != input.IdentityEpoch {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "identity_changed"}
		}
		if err := r.verifyReady(ctx, nodeID, current.AdapterKind, input.AdapterVersion, input.IdentityEpoch); err != nil {
			var incompatible *admissionFault
			if errors.As(err, &incompatible) {
				return NodeState{}, &controlFault{status: http.StatusUnprocessableEntity, code: incompatible.code}
			}
			return NodeState{}, &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
		}
		next.Mode, next.OperationID = ModeEligible, ""
		next.Generation++
		next.IdentityEpoch, next.AdapterVersion = input.IdentityEpoch, input.AdapterVersion
		clearSealProjection(&next)
	case "release":
		if current.Mode != ModeSealed || current.OperationID != operation || !hasSealProjection(current) ||
			input.IdentityEpoch != current.IdentityEpoch || input.AdapterVersion != current.AdapterVersion ||
			input.IdentityEpoch < 1 || !adapterVersion.MatchString(input.AdapterVersion) || input.Proof != nil {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if err := r.releaseProjectedHold(ctx, nodeID, current, input.Release, "release"); err != nil {
			return NodeState{}, err
		}
		next.Mode, next.OperationID = ModeEligible, ""
		next.Generation++
		clearSealProjection(&next)
	case "abort":
		if (current.Mode != ModeDraining && current.Mode != ModeSealed) || current.OperationID != operation ||
			input.IdentityEpoch != current.IdentityEpoch || input.AdapterVersion != current.AdapterVersion ||
			input.IdentityEpoch < 1 || !adapterVersion.MatchString(input.AdapterVersion) || input.Proof != nil {
			return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
		}
		if hasSealProjection(current) {
			if err := r.releaseProjectedHold(ctx, nodeID, current, input.Release, "cancel"); err != nil {
				return NodeState{}, err
			}
		} else {
			if input.Release != nil {
				return NodeState{}, &controlFault{status: http.StatusConflict, code: "stale"}
			}
			if err := r.verifyReady(ctx, nodeID, current.AdapterKind, current.AdapterVersion, current.IdentityEpoch); err != nil {
				return NodeState{}, &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
			}
		}
		next.Mode, next.OperationID = ModeEligible, ""
		next.Generation++
		clearSealProjection(&next)
	default:
		return NodeState{}, &controlFault{status: http.StatusNotFound, code: "not_found"}
	}
	if next.StateVersion == hp.MaximumSafeInteger || next.Generation > hp.MaximumSafeInteger {
		return NodeState{}, &controlFault{status: http.StatusConflict, code: "version_exhausted"}
	}
	next.StateVersion++
	return next, nil
}

func hasSealProjection(state NodeState) bool {
	return state.SealScope != nil || state.SealHoldVersion != 0 || state.SealScopeRevision != 0 || state.SealedProofHash != ""
}

func clearSealProjection(state *NodeState) {
	state.SealScope = nil
	state.SealHoldVersion = 0
	state.SealScopeRevision = 0
	state.SealedProofHash = ""
}

func (r *Router) verifyQuiescenceProof(ctx context.Context, nodeID, operation string, current NodeState, proof harnessbarrier.QuiescenceProof) error {
	if r.registry.SchemaID != harnessclient.RouterRegistrySchemaID {
		return &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
	}
	raw, err := json.Marshal(proof)
	registration := r.registry.RegistryVersion
	if current.RegistrationRevision > 0 {
		registration = current.RegistrationRevision
	}
	if err != nil || harnessbarrier.Validate("quiescenceProof", raw) != nil || proof.OperationID != operation || proof.NodeID != nodeID ||
		proof.Epoch != current.IdentityEpoch || proof.BindingGeneration != registration {
		return &controlFault{status: http.StatusConflict, code: "stale"}
	}
	backend, ok := r.backend.(AdministrativeBackend)
	if !ok {
		return &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
	}
	response, err := backend.QuiescenceProof(ctx, nodeID, r.registry.OwnerID, operation)
	if err != nil {
		return &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
	}
	if response.Status == http.StatusConflict || response.Status == http.StatusNotFound {
		return &controlFault{status: http.StatusConflict, code: "stale"}
	}
	if response.Status != http.StatusOK || harnessbarrier.Validate("quiescenceProof", response.Body) != nil {
		return &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
	}
	var currentProof harnessbarrier.QuiescenceProof
	if json.Unmarshal(response.Body, &currentProof) != nil || currentProof != proof {
		return &controlFault{status: http.StatusConflict, code: "stale"}
	}
	return nil
}

func (r *Router) releaseProjectedHold(ctx context.Context, nodeID string, current NodeState, request *harnessbarrier.ReleaseRequest, action string) error {
	registration := r.registry.RegistryVersion
	if current.RegistrationRevision > 0 {
		registration = current.RegistrationRevision
	}
	if current.SealScope == nil || request == nil || request.OperationID != current.OperationID || request.NodeID != nodeID || request.ExpectedEpoch != current.IdentityEpoch ||
		request.BindingGeneration != registration || request.Scope != *current.SealScope || request.HoldVersion != current.SealHoldVersion ||
		request.ExpectedScopeRevision != current.SealScopeRevision || request.Action != action {
		return &controlFault{status: http.StatusConflict, code: "stale"}
	}
	raw, err := json.Marshal(request)
	if err != nil || harnessbarrier.Validate("releaseRequest", raw) != nil {
		return &controlFault{status: http.StatusBadRequest, code: "invalid"}
	}
	backend, ok := r.backend.(AdministrativeBackend)
	if !ok {
		return &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
	}
	response, err := backend.ReleaseHold(ctx, nodeID, r.registry.OwnerID, raw)
	if err != nil {
		return &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
	}
	if response.Status == http.StatusConflict || response.Status == http.StatusNotFound {
		return &controlFault{status: http.StatusConflict, code: "stale"}
	}
	if (response.Status != http.StatusOK && response.Status != http.StatusCreated) || harnessbarrier.Validate("releaseReceipt", response.Body) != nil {
		return &controlFault{status: http.StatusServiceUnavailable, code: "node_not_ready"}
	}
	var receipt harnessbarrier.ReleaseReceipt
	expectedKind := "hold.released"
	if action == "cancel" {
		expectedKind = "hold.cancelled"
	}
	if json.Unmarshal(response.Body, &receipt) != nil || receipt.OperationID != request.OperationID || receipt.NodeID != nodeID ||
		receipt.Epoch != request.ExpectedEpoch || receipt.BindingGeneration != request.BindingGeneration || receipt.Scope != request.Scope ||
		receipt.HoldVersion != request.HoldVersion || receipt.ScopeRevision != request.ExpectedScopeRevision+1 || receipt.Kind != expectedKind {
		return &controlFault{status: http.StatusConflict, code: "stale"}
	}
	return nil
}

func (r *Router) verifyReady(ctx context.Context, nodeID, adapterKind, version string, epoch int64) error {
	required := false
	if route := r.nodes[nodeID]; route != nil {
		required = route.state.AdmissionRequired
	}
	_, err := observeNode(ctx, r.backend, r.registry, nodeID, adapterKind, version, epoch, false, required)
	return err
}

func observeReady(ctx context.Context, backend Backend, registry harnessclient.RoutingRegistry, nodeID, adapterKind, version string, epoch int64) (hp.NodeIdentity, error) {
	return observeNode(ctx, backend, registry, nodeID, adapterKind, version, epoch, false, false)
}

func observePreflight(ctx context.Context, backend Backend, registry harnessclient.RoutingRegistry, nodeID, adapterKind, version string, epoch int64) (hp.NodeIdentity, error) {
	return observeNode(ctx, backend, registry, nodeID, adapterKind, version, epoch, true, false)
}

func observeNode(ctx context.Context, backend Backend, registry harnessclient.RoutingRegistry, nodeID, adapterKind, version string, epoch int64, allowPristinePolicySentinel, admissionRequired bool) (hp.NodeIdentity, error) {
	registrationRevision := registry.RegistryVersion
	compatibility := "compatible"
	if registry.SchemaID == harnessclient.RouterRegistrySchemaID {
		found := false
		for _, node := range registry.Nodes {
			if node.NodeID == nodeID {
				registrationRevision, compatibility, found = node.RegistrationRevision, node.Compatibility, true
				break
			}
		}
		if !found || compatibility != "compatible" {
			return hp.NodeIdentity{}, errors.New("node registration is read-only")
		}
	}
	read := func(route string, target any) error {
		response, err := backend.Read(ctx, nodeID, registry.OwnerID, route, "")
		if err != nil || response.Status != http.StatusOK || json.Unmarshal(response.Body, target) != nil {
			return errors.New("node read failed")
		}
		return nil
	}
	var identity hp.NodeIdentity
	if err := read("identity", &identity); err != nil || identity.NodeID != nodeID || identity.RegistryVersion != registrationRevision ||
		identity.Adapter.Kind != adapterKind || (epoch > 0 && identity.IdentityEpoch != epoch) || (version != "" && identity.Adapter.Version != version) {
		return hp.NodeIdentity{}, errors.New("node identity is not ready")
	}
	if admissionRequired {
		response, admissionErr := backend.Read(ctx, nodeID, registry.OwnerID, "admission", "")
		if admissionErr != nil {
			var fault *harnessclient.Fault
			if errors.As(admissionErr, &fault) && fault.Code == "schema_mismatch" {
				return hp.NodeIdentity{}, &admissionFault{code: "admission_schema_mismatch"}
			}
			return hp.NodeIdentity{}, admissionErr
		}
		if response.Status == http.StatusNotFound {
			return hp.NodeIdentity{}, &admissionFault{code: "admission_profile_missing"}
		}
		var profile hp.AdmissionProfile
		if response.Status != http.StatusOK || hp.Validate("admissionProfile", response.Body) != nil || json.Unmarshal(response.Body, &profile) != nil {
			return hp.NodeIdentity{}, &admissionFault{code: "admission_schema_mismatch"}
		}
		if profile.OwnerID != registry.OwnerID {
			return hp.NodeIdentity{}, &admissionFault{code: "admission_owner_mismatch"}
		}
		if profile.NodeID != identity.NodeID || profile.RegistrationRevision != registrationRevision ||
			profile.IdentityEpoch != identity.IdentityEpoch || profile.Adapter != identity.Adapter ||
			profile.WireSchemaSHA256 != hp.SchemaSHA256 {
			return hp.NodeIdentity{}, &admissionFault{code: "admission_identity_mismatch"}
		}
		if len(hp.MissingAdmissionCapabilities(profile.Capabilities)) != 0 {
			return hp.NodeIdentity{}, &admissionFault{code: "admission_capability_missing"}
		}
		if profile.Readiness != "ready" {
			return hp.NodeIdentity{}, &admissionFault{code: "admission_unready"}
		}
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
	if registry.OwnerID == "" {
		return errors.New("managed Harness Router requires an owner")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, node := range registry.Nodes {
		if registry.SchemaID == harnessclient.RouterRegistrySchemaID && node.Compatibility != "compatible" {
			continue
		}
		if _, err := observePreflight(ctx, client, registry, node.NodeID, node.Adapter, "", 0); err != nil {
			return err
		}
	}
	return nil
}

// Compile-time check that the concrete private transport still satisfies the
// Router backend after registry identity was added to the boundary.
var _ Backend = (*harnessclient.Client)(nil)
