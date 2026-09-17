package harnessrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type EnrollmentRegistryInput struct {
	OperationID             string
	NodeID                  string
	ExpectedRegistryVersion int64
	ExpectedRegistrySHA256  string
	Registry                json.RawMessage
}

type EnrollmentFault struct {
	Status int
	Code   string
}

func (fault *EnrollmentFault) Error() string { return fault.Code }

func enrollmentError(err error) error {
	var fault *controlFault
	if errors.As(err, &fault) {
		return &EnrollmentFault{Status: fault.status, Code: fault.code}
	}
	return &EnrollmentFault{Status: http.StatusServiceUnavailable, Code: "enrollment_unavailable"}
}

// InstallEnrollmentRegistry performs the existing durable registry CAS. A
// duplicate request remains successful even after a later registry revision if
// the exact enrollment operation is already projected on the requested node.
func (r *Router) InstallEnrollmentRegistry(input EnrollmentRegistryInput) (State, error) {
	if !operationID.MatchString(input.OperationID) || !entityID.MatchString(input.NodeID) {
		return State{}, &EnrollmentFault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	state, err := r.installRegistry(registryInstallRequest{
		OperationID: input.OperationID,
		Expected:    registryExpected{RegistryVersion: input.ExpectedRegistryVersion, RegistrySHA256: input.ExpectedRegistrySHA256},
		Registry:    append(json.RawMessage(nil), input.Registry...),
	})
	if err != nil {
		return State{}, enrollmentError(err)
	}
	return state, nil
}

// ActivateEnrollment re-observes the exact candidate transport, identity,
// owner and mandatory admission profile before changing only this node from
// sealed to eligible. No process lifecycle action is available on this path.
func (r *Router) ActivateEnrollment(ctx context.Context, enrollmentOperationID, nodeID string) (NodeState, error) {
	if !operationID.MatchString(enrollmentOperationID) || !entityID.MatchString(nodeID) {
		return NodeState{}, &EnrollmentFault{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	r.backendMu.RLock()
	if r.poisoned.Load() || !r.ensureStateLock() {
		r.backendMu.RUnlock()
		return NodeState{}, &EnrollmentFault{Status: http.StatusServiceUnavailable, Code: "state_unavailable"}
	}
	route := r.nodes[nodeID]
	if route == nil {
		r.backendMu.RUnlock()
		return NodeState{}, &EnrollmentFault{Status: http.StatusNotFound, Code: "not_found"}
	}
	route.mu.RLock()
	current := route.state
	route.mu.RUnlock()
	if current.EnrollmentOperationID != enrollmentOperationID || !current.AdmissionRequired {
		r.backendMu.RUnlock()
		return NodeState{}, &EnrollmentFault{Status: http.StatusConflict, Code: "enrollment_conflict"}
	}
	if current.Mode == ModeEligible {
		r.backendMu.RUnlock()
		return current, nil
	}
	if current.Mode != ModeSealed || current.OperationID != enrollmentOperationID {
		r.backendMu.RUnlock()
		return NodeState{}, &EnrollmentFault{Status: http.StatusConflict, Code: "enrollment_conflict"}
	}
	identity, err := observeNode(ctx, r.backend, r.registry, nodeID, current.AdapterKind, "", current.IdentityEpoch, false, true)
	r.backendMu.RUnlock()
	if err != nil {
		var incompatible *admissionFault
		if errors.As(err, &incompatible) {
			return NodeState{}, &EnrollmentFault{Status: http.StatusUnprocessableEntity, Code: incompatible.code}
		}
		return NodeState{}, &EnrollmentFault{Status: http.StatusServiceUnavailable, Code: "node_not_ready"}
	}
	state, err := r.transition(ctx, nodeID, "activate", transitionRequest{
		OperationID:   enrollmentOperationID,
		Expected:      expectedState{Mode: current.Mode, StateVersion: current.StateVersion, Generation: current.Generation},
		IdentityEpoch: identity.IdentityEpoch, AdapterVersion: identity.Adapter.Version,
	})
	if err != nil {
		return NodeState{}, enrollmentError(err)
	}
	return state, nil
}
