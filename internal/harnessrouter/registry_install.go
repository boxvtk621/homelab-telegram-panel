package harnessrouter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

type registryExpected struct {
	RegistryVersion int64  `json:"registryVersion"`
	RegistrySHA256  string `json:"registrySHA256"`
}

type registryInstallRequest struct {
	OperationID string           `json:"operationId"`
	Expected    registryExpected `json:"expected"`
	Registry    json.RawMessage  `json:"registry"`
}

func registryInstallShape(raw []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 3 || envelope["operationId"] == nil ||
		envelope["expected"] == nil || envelope["registry"] == nil {
		return false
	}
	var expected map[string]json.RawMessage
	return json.Unmarshal(envelope["expected"], &expected) == nil && len(expected) == 2 &&
		expected["registryVersion"] != nil && expected["registrySHA256"] != nil
}

func registryRequestHash(operationID string, expected registryExpected, manifestSHA256 string) string {
	canonical, _ := json.Marshal(struct {
		OperationID    string           `json:"operationId"`
		Expected       registryExpected `json:"expected"`
		ManifestSHA256 string           `json:"manifestSHA256"`
	}{operationID, expected, manifestSHA256})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func (r *Router) installRegistry(input registryInstallRequest) (State, error) {
	if !operationID.MatchString(input.OperationID) || input.Expected.RegistryVersion < 1 ||
		input.Expected.RegistryVersion > hp.MaximumSafeInteger || !sha256Hex.MatchString(input.Expected.RegistrySHA256) ||
		len(input.Registry) == 0 || len(input.Registry) > 256<<10 || r.registryFactory == nil || r.statePath == "" {
		return State{}, &controlFault{status: 400, code: "invalid"}
	}
	candidate, err := r.registryFactory(input.Registry)
	if err != nil {
		return State{}, &controlFault{status: 400, code: "registry_rejected"}
	}
	keepCandidate := false
	defer func() {
		if !keepCandidate {
			candidate.Close()
		}
	}()
	candidateRegistry := candidate.RoutingRegistry()
	if candidateRegistry.SchemaID != harnessclient.RouterRegistrySchemaID || candidateRegistry.WireSchemaSHA256 != hp.SchemaSHA256 {
		return State{}, &controlFault{status: 409, code: "incompatible"}
	}
	requestHash := registryRequestHash(input.OperationID, input.Expected, candidateRegistry.ManifestSHA256)

	r.backendMu.Lock()
	defer r.backendMu.Unlock()
	if r.poisoned.Load() {
		return State{}, &controlFault{status: 503, code: "state_unavailable"}
	}
	if !r.ensureStateLock() {
		return State{}, &controlFault{status: 503, code: "state_unavailable"}
	}
	current := r.registry
	currentState := r.snapshotLockedWithStateMutex()
	if currentState.RegistryOperationID == input.OperationID {
		if currentState.RegistryRequestSHA256 == requestHash && current.ManifestSHA256 == candidateRegistry.ManifestSHA256 {
			return operatorState(currentState), nil
		}
		return State{}, &controlFault{status: 409, code: "id_conflict"}
	}
	if input.Expected.RegistryVersion != current.RegistryVersion || input.Expected.RegistrySHA256 != current.ManifestSHA256 {
		return State{}, &controlFault{status: 409, code: "stale"}
	}
	if current.RegistryVersion >= hp.MaximumSafeInteger || candidateRegistry.RegistryVersion != current.RegistryVersion+1 ||
		candidateRegistry.OwnerID != current.OwnerID || candidateRegistry.Mode != current.Mode {
		return State{}, &controlFault{status: 409, code: "stale"}
	}

	ids := make([]string, 0, len(r.nodes))
	for nodeID := range r.nodes {
		ids = append(ids, nodeID)
	}
	sort.Strings(ids)
	lockedRoutes := make([]*nodeRoute, 0, len(ids))
	for _, nodeID := range ids {
		route := r.nodes[nodeID]
		route.mu.Lock()
		lockedRoutes = append(lockedRoutes, route)
	}
	defer func() {
		for index := len(lockedRoutes) - 1; index >= 0; index-- {
			lockedRoutes[index].mu.Unlock()
		}
	}()

	nextNodes, changedNode, changes, err := r.projectNodes(current, candidateRegistry, input.OperationID)
	if err != nil || changes > 1 || (changes == 0 && current.SchemaID != "") {
		return State{}, &controlFault{status: 409, code: "stale"}
	}
	if changedNode != "" {
		route := r.nodes[changedNode]
		if route != nil && (route.state.Mode != ModeSealed || (route.state.OperationID != input.OperationID && route.state.OperationID != bootstrapOpID)) {
			return State{}, &controlFault{status: 409, code: "node_not_sealed"}
		}
	}
	next := State{
		Schema: ProjectionStateSchema, OwnerID: candidateRegistry.OwnerID,
		RegistryVersion: candidateRegistry.RegistryVersion, RegistrySHA256: candidateRegistry.ManifestSHA256,
		RegistryOperationID: input.OperationID, RegistryRequestSHA256: requestHash,
		RegistryEnvelope: append(json.RawMessage(nil), input.Registry...), Nodes: nextNodes,
	}
	if err := validateState(next, candidateRegistry); err != nil {
		return State{}, &controlFault{status: 409, code: "incompatible"}
	}
	committed, persistErr := r.persist(r.statePath, next, true, r.commitGuard)
	if !committed {
		if persistErr == nil {
			persistErr = errors.New("registry state was not committed")
		}
		return State{}, persistErr
	}
	readback, readbackErr := decodeState(r.statePath)
	if readbackErr == nil {
		readbackErr = validateState(readback, candidateRegistry)
	}
	if readbackErr == nil && !exactState(readback, next) {
		readbackErr = errors.New("registry readback mismatch")
	}

	previous := r.backend
	r.backend = candidate
	r.registry = candidateRegistry
	r.registryOperationID = input.OperationID
	r.registryRequestSHA256 = requestHash
	r.registryEnvelope = append(json.RawMessage(nil), input.Registry...)
	r.nodes = make(map[string]*nodeRoute, len(nextNodes))
	for nodeID, state := range nextNodes {
		r.nodes[nodeID] = &nodeRoute{state: state}
	}
	// backendMu drains synchronous calls. harnessclient.Close only releases idle
	// pools, so an established event stream keeps its own connection and context
	// while every obsolete idle transport is bounded to this swap.
	previous.Close()
	keepCandidate = true
	if persistErr != nil || readbackErr != nil {
		r.poisoned.Store(true)
		return State{}, &controlFault{status: 503, code: "state_unavailable"}
	}
	return operatorState(readback), nil
}

func (r *Router) snapshotLockedWithStateMutex() State {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.snapshotLocked()
}

func (r *Router) projectNodes(current, candidate harnessclient.RoutingRegistry, operationID string) (map[string]NodeState, string, int, error) {
	currentNodes := make(map[string]harnessclient.RoutingNode, len(current.Nodes))
	for _, node := range current.Nodes {
		currentNodes[node.NodeID] = node
	}
	candidateNodes := make(map[string]harnessclient.RoutingNode, len(candidate.Nodes))
	for _, node := range candidate.Nodes {
		candidateNodes[node.NodeID] = node
	}
	if len(candidateNodes) < len(currentNodes) || len(candidateNodes) > len(currentNodes)+1 {
		return nil, "", 0, errors.New("registry node count changed unsafely")
	}
	for nodeID := range currentNodes {
		if _, present := candidateNodes[nodeID]; !present {
			return nil, "", 0, errors.New("registry removal is outside R02")
		}
	}
	next := make(map[string]NodeState, len(candidateNodes))
	changedNode, changes := "", 0
	legacyUpgrade := current.SchemaID == ""
	for _, candidateNode := range candidate.Nodes {
		currentNode, exists := currentNodes[candidateNode.NodeID]
		if !exists {
			if candidateNode.RegistrationRevision != 1 || candidateNode.RegistrationEpoch != 1 {
				return nil, "", 0, errors.New("invalid new node registration")
			}
			changes++
			changedNode = candidateNode.NodeID
			next[candidateNode.NodeID] = NodeState{
				Mode: ModeSealed, StateVersion: 1, OperationID: operationID,
				RegistrationRevision: candidateNode.RegistrationRevision, IdentityEpoch: candidateNode.RegistrationEpoch,
				Compatibility: candidateNode.Compatibility, AdapterKind: candidateNode.Adapter,
			}
			continue
		}
		currentState := r.nodes[candidateNode.NodeID].state
		currentRevision := currentNode.RegistrationRevision
		if currentRevision == 0 {
			currentRevision = current.RegistryVersion
		}
		bindingChanged := currentNode.BindingSHA256 != candidateNode.BindingSHA256 ||
			(!legacyUpgrade && currentNode.Compatibility != candidateNode.Compatibility)
		if !bindingChanged {
			if candidateNode.RegistrationRevision != currentRevision ||
				(currentState.IdentityEpoch > 0 && candidateNode.RegistrationEpoch != currentState.IdentityEpoch) ||
				(!legacyUpgrade && candidateNode.RegistrationEpoch != currentNode.RegistrationEpoch) {
				return nil, "", 0, errors.New("untouched node registration changed")
			}
			preserved := currentState
			preserved.RegistrationRevision = candidateNode.RegistrationRevision
			preserved.Compatibility = candidateNode.Compatibility
			if preserved.IdentityEpoch == 0 {
				if preserved.Mode != ModeSealed || preserved.OperationID != bootstrapOpID {
					return nil, "", 0, errors.New("unknown identity cannot be promoted")
				}
				preserved.IdentityEpoch = candidateNode.RegistrationEpoch
			}
			next[candidateNode.NodeID] = preserved
			continue
		}
		if currentRevision >= hp.MaximumSafeInteger || candidateNode.RegistrationRevision != currentRevision+1 ||
			candidateNode.RegistrationEpoch != currentState.IdentityEpoch+1 || currentState.IdentityEpoch >= hp.MaximumSafeInteger ||
			currentState.StateVersion >= hp.MaximumSafeInteger {
			return nil, "", 0, errors.New("changed node registration is stale")
		}
		changes++
		changedNode = candidateNode.NodeID
		updated := currentState
		updated.Mode, updated.OperationID = ModeSealed, operationID
		updated.StateVersion++
		updated.RegistrationRevision = candidateNode.RegistrationRevision
		updated.IdentityEpoch = candidateNode.RegistrationEpoch
		updated.Compatibility = candidateNode.Compatibility
		updated.AdapterKind, updated.AdapterVersion = candidateNode.Adapter, ""
		next[candidateNode.NodeID] = updated
	}
	return next, changedNode, changes, nil
}

func operatorState(state State) State {
	state.RegistryEnvelope = nil
	return state
}
