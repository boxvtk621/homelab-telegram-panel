package dockeradapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"sync"
)

// FixtureBackend is the controlled R02 effect implementation. It records a
// deterministic effect and can lose one acknowledgement while retaining the
// result for reconciliation. It performs no Docker or host mutation.
type FixtureBackend struct {
	mu            sync.Mutex
	effects       map[string]fixtureEffect
	path          string
	daemonID      string
	instanceID    string
	replace       func(string, any, bool) error
	applyCalls    int
	loseNextAck   bool
	statusUnknown bool
	poisoned      bool
}

const fixtureStateSchemaID = "docker-adapter-fixture-v1"

type fixtureEffect struct {
	OperationID string `json:"operationId"`
	StepID      string `json:"stepId"`
	RequestHash string `json:"requestHash"`
	Outcome     string `json:"outcome"`
	ReceiptID   string `json:"receiptId"`
}

type fixtureState struct {
	SchemaID   string          `json:"schemaId"`
	DaemonID   string          `json:"daemonId"`
	InstanceID string          `json:"instanceId"`
	Effects    []fixtureEffect `json:"effects"`
}

type fixtureGrant struct {
	proof       AuthorityProof
	requestHash string
}

// FixtureAuthority is the controlled R02 active-generation source. Grant
// replaces the sole active lease for one daemon/instance, making older workers
// fail before the fixture effect boundary.
type FixtureAuthority struct {
	mu     sync.Mutex
	grants map[string]fixtureGrant
}

func fixtureReceipt(prefix string, request Request) string {
	candidate := prefix + request.OperationID
	if identifier.MatchString(candidate) {
		return candidate
	}
	digest, _ := requestHash(request)
	return prefix + digest
}

func NewFixtureAuthority() *FixtureAuthority {
	return &FixtureAuthority{grants: map[string]fixtureGrant{}}
}

func (authority *FixtureAuthority) Grant(daemonID, instanceID string, request Request, proof AuthorityProof) error {
	if authority == nil || !validIdentity(daemonID) || !validIdentity(instanceID) || validateAuthorityProof(request, proof) != nil {
		return ErrStaleAuthority
	}
	hash, err := requestHash(request)
	if err != nil {
		return err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.grants[daemonID+"\x00"+instanceID] = fixtureGrant{proof: proof, requestHash: hash}
	return nil
}

func (authority *FixtureAuthority) VerifyActive(ctx context.Context, daemonID, instanceID string, request Request, proof AuthorityProof) error {
	if authority == nil || validateAuthorityProof(request, proof) != nil {
		return ErrStaleAuthority
	}
	if err := ctx.Err(); err != nil {
		return ErrStaleAuthority
	}
	hash, err := requestHash(request)
	if err != nil {
		return ErrStaleAuthority
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	grant, ok := authority.grants[daemonID+"\x00"+instanceID]
	if !ok || grant.proof != proof || grant.requestHash != hash {
		return ErrStaleAuthority
	}
	return nil
}

func (authority *FixtureAuthority) RecordSent(ctx context.Context, daemonID, instanceID string, request Request, proof AuthorityProof) (AuthorityProof, error) {
	if err := authority.VerifyActive(ctx, daemonID, instanceID, request, proof); err != nil {
		return AuthorityProof{}, err
	}
	if proof.OperationVersion >= maximumSafeInt {
		return AuthorityProof{}, ErrStaleAuthority
	}
	hash, err := requestHash(request)
	if err != nil {
		return AuthorityProof{}, ErrStaleAuthority
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	key := daemonID + "\x00" + instanceID
	grant, ok := authority.grants[key]
	if !ok || grant.proof != proof || grant.requestHash != hash {
		return AuthorityProof{}, ErrStaleAuthority
	}
	proof.OperationVersion++
	authority.grants[key] = fixtureGrant{proof: proof, requestHash: hash}
	return proof, nil
}

func NewFixtureBackend() *FixtureBackend {
	return &FixtureBackend{effects: map[string]fixtureEffect{}}
}

// OpenFixtureBackend gives the R02 test backend durable observable state, like
// a real external engine would. A missing state file is safe only after the
// adapter journal was explicitly initialized and still has no entries; callers
// pass allowCreate from that journal.
func OpenFixtureBackend(directory, daemonID, instanceID string, allowCreate bool) (*FixtureBackend, error) {
	if !validIdentity(daemonID) || !validIdentity(instanceID) || privateDirectory(directory) != nil {
		return nil, ErrReconciliationRequired
	}
	path := filepath.Join(directory, fixtureFileKey(daemonID, instanceID)+".fixture.json")
	backend := &FixtureBackend{
		effects: map[string]fixtureEffect{}, path: path, daemonID: daemonID, instanceID: instanceID, replace: replaceJSON,
	}
	if !exists(path) {
		if !allowCreate {
			return nil, ErrReconciliationRequired
		}
		state := fixtureState{SchemaID: fixtureStateSchemaID, DaemonID: daemonID, InstanceID: instanceID, Effects: []fixtureEffect{}}
		if err := replaceJSON(path, state, false); err != nil {
			return nil, ErrReconciliationRequired
		}
		return backend, nil
	}
	var state fixtureState
	if err := readExactJSON(path, maximumJournalBytes, &state); err != nil || validateFixtureState(state, daemonID, instanceID) != nil {
		return nil, ErrReconciliationRequired
	}
	for _, effect := range state.Effects {
		backend.effects[effect.OperationID+"\x00"+effect.StepID] = effect
	}
	return backend, nil
}

func (backend *FixtureBackend) LoseNextAcknowledgement() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.loseNextAck = true
}

func (backend *FixtureBackend) SetStatusUnknown(value bool) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.statusUnknown = value
}

func (backend *FixtureBackend) ApplyCalls() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.applyCalls
}

func (backend *FixtureBackend) Apply(_ context.Context, request Request) (Result, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.poisoned {
		return Result{}, &EffectError{Known: false, Code: "fixture_state_unknown"}
	}
	backend.applyCalls++
	key := request.OperationID + "\x00" + request.StepID
	if _, exists := backend.effects[key]; exists {
		return Result{}, &EffectError{Known: true, Code: "duplicate_effect"}
	}
	result := Result{Outcome: "applied", ReceiptID: fixtureReceipt("fixture:", request)}
	hash, err := requestHash(request)
	if err != nil {
		return Result{}, &EffectError{Known: true, Code: "invalid_request"}
	}
	effect := fixtureEffect{
		OperationID: request.OperationID, StepID: request.StepID, RequestHash: hash,
		Outcome: result.Outcome, ReceiptID: result.ReceiptID,
	}
	backend.effects[key] = effect
	if backend.path != "" && backend.persistLocked() != nil {
		// replaceJSON can fail after rename, so the commit point is ambiguous.
		// Poison this process: only a clean reopen may use durable readback to
		// decide whether the fixture effect exists.
		backend.poisoned = true
		return Result{}, &EffectError{Known: false, Code: "fixture_persist_unknown"}
	}
	if backend.loseNextAck {
		backend.loseNextAck = false
		return Result{}, &EffectError{Known: false, Code: "lost_ack"}
	}
	return result, nil
}

func (backend *FixtureBackend) Status(_ context.Context, request Request) (Result, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.statusUnknown || backend.poisoned {
		return Result{}, errors.New("fixture status unavailable")
	}
	key := request.OperationID + "\x00" + request.StepID
	if effect, exists := backend.effects[key]; exists {
		hash, err := requestHash(request)
		if err != nil || effect.RequestHash != hash {
			return Result{}, errors.New("fixture request mismatch")
		}
		return Result{Outcome: effect.Outcome, ReceiptID: effect.ReceiptID}, nil
	}
	return Result{Outcome: "not_applied", ReceiptID: fixtureReceipt("fixture:not-applied:", request)}, nil
}

func (backend *FixtureBackend) persistLocked() error {
	state := fixtureState{
		SchemaID: fixtureStateSchemaID, DaemonID: backend.daemonID, InstanceID: backend.instanceID,
		Effects: make([]fixtureEffect, 0, len(backend.effects)),
	}
	for _, effect := range backend.effects {
		state.Effects = append(state.Effects, effect)
	}
	sort.Slice(state.Effects, func(left, right int) bool {
		return state.Effects[left].OperationID+"\x00"+state.Effects[left].StepID < state.Effects[right].OperationID+"\x00"+state.Effects[right].StepID
	})
	if validateFixtureState(state, backend.daemonID, backend.instanceID) != nil || backend.replace == nil || backend.replace(backend.path, state, true) != nil {
		return ErrReconciliationRequired
	}
	var readback fixtureState
	if readExactJSON(backend.path, maximumJournalBytes, &readback) != nil || validateFixtureState(readback, backend.daemonID, backend.instanceID) != nil {
		return ErrReconciliationRequired
	}
	want, wantErr := json.Marshal(state)
	got, gotErr := json.Marshal(readback)
	if wantErr != nil || gotErr != nil || !bytes.Equal(want, got) {
		return ErrReconciliationRequired
	}
	return nil
}

func validateFixtureState(state fixtureState, daemonID, instanceID string) error {
	if state.SchemaID != fixtureStateSchemaID || state.DaemonID != daemonID || state.InstanceID != instanceID ||
		state.Effects == nil || len(state.Effects) > 10_000 {
		return errors.New("invalid fixture state")
	}
	previous := ""
	for _, effect := range state.Effects {
		key := effect.OperationID + "\x00" + effect.StepID
		if !identifier.MatchString(effect.OperationID) || !identifier.MatchString(effect.StepID) || !sha256Hex.MatchString(effect.RequestHash) ||
			!validResult(Result{Outcome: effect.Outcome, ReceiptID: effect.ReceiptID}) || previous != "" && key <= previous {
			return errors.New("invalid fixture effect")
		}
		previous = key
	}
	return nil
}
