package dockeradapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type Backend interface {
	Apply(context.Context, Request) (Result, error)
	Status(context.Context, Request) (Result, error)
}

// EffectError says whether a failed backend call has a known outcome. Unknown
// results deliberately leave the durable journal at sent.
type EffectError struct {
	Known bool
	Code  string
}

func (err *EffectError) Error() string { return err.Code }

type Executor struct {
	mu          sync.Mutex
	daemonID    string
	instanceID  string
	anchorPath  string
	journalPath string
	lock        *processLock
	state       journalState
	now         func() time.Time
	replace     func(string, any, bool) error
	syncGuard   func() error
	beforeApply func()
	poisoned    bool
}

// Initialize creates the one-time local journal identity. Ordinary execution
// must use Open so complete state loss can never be mistaken for a first run.
func Initialize(directory, daemonID, instanceID string) (*Executor, error) {
	return openExecutor(directory, daemonID, instanceID, true)
}

func Open(directory, daemonID, instanceID string) (*Executor, error) {
	return openExecutor(directory, daemonID, instanceID, false)
}

func openExecutor(directory, daemonID, instanceID string, initialize bool) (*Executor, error) {
	if !validIdentity(daemonID) || !validIdentity(instanceID) {
		return nil, errors.New("invalid adapter executor identity")
	}
	if err := privateDirectory(directory); err != nil {
		return nil, err
	}
	lockPath, anchorPath, journalPath := fileNames(directory, daemonID, instanceID)
	if initialize {
		statePaths := []string{
			lockPath, lockPath + ".guard", anchorPath, journalPath, journalGuardPath(journalPath),
			filepath.Join(directory, fixtureFileKey(daemonID, instanceID)+".fixture.json"),
		}
		for _, path := range statePaths {
			if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
				return nil, ErrReconciliationRequired
			}
		}
	}
	if !initialize && !exists(lockPath) {
		return nil, ErrReconciliationRequired
	}
	lock, err := openProcessLock(lockPath, initialize)
	if err != nil {
		if initialize || os.IsNotExist(err) {
			return nil, ErrReconciliationRequired
		}
		return nil, err
	}
	executor := &Executor{
		daemonID: daemonID, instanceID: instanceID, anchorPath: anchorPath, journalPath: journalPath,
		lock: lock, now: time.Now, replace: replaceJSON,
	}
	executor.syncGuard = executor.syncJournalGuard
	if err := lock.bindGuard(initialize); err != nil {
		lock.close()
		return nil, ErrReconciliationRequired
	}
	anchorExists := exists(anchorPath)
	journalExists := exists(journalPath)
	if initialize && (anchorExists || journalExists) {
		lock.close()
		return nil, ErrReconciliationRequired
	}
	if anchorExists != journalExists {
		lock.close()
		return nil, ErrReconciliationRequired
	}
	if !anchorExists {
		if !initialize {
			lock.close()
			return nil, ErrReconciliationRequired
		}
		lockID, err := newLockID()
		if err != nil {
			lock.close()
			return nil, ErrReconciliationRequired
		}
		executor.state = journalState{SchemaID: JournalSchemaID, DaemonID: daemonID, InstanceID: instanceID, Entries: []Entry{}}
		journalDigest, err := journalSHA256(executor.state)
		if err != nil {
			lock.close()
			return nil, ErrReconciliationRequired
		}
		record := lockRecord{
			SchemaID: lockSchemaID, DaemonID: daemonID, InstanceID: instanceID, LockID: lockID,
			JournalSHA256: journalDigest,
		}
		if err := lock.initialize(record); err != nil {
			lock.close()
			return nil, ErrReconciliationRequired
		}
		identity := anchor{SchemaID: JournalSchemaID, DaemonID: daemonID, InstanceID: instanceID, LockID: lockID}
		if err := replaceJSON(anchorPath, identity, false); err != nil {
			lock.close()
			return nil, err
		}
		if err := replaceJSON(journalPath, executor.state, false); err != nil {
			lock.close()
			return nil, ErrReconciliationRequired
		}
		if err := executor.syncJournalGuard(); err != nil {
			lock.close()
			return nil, ErrReconciliationRequired
		}
		return executor, nil
	}
	var identity anchor
	if err := readExactJSON(anchorPath, 16<<10, &identity); err != nil || identity.SchemaID != JournalSchemaID ||
		identity.DaemonID != daemonID || identity.InstanceID != instanceID || !sha256Hex.MatchString(identity.LockID) {
		lock.close()
		return nil, ErrReconciliationRequired
	}
	record, recordErr := readLockRecord(lock.file)
	if recordErr != nil || record.DaemonID != daemonID || record.InstanceID != instanceID ||
		record.LockID != identity.LockID || lock.bind(record) != nil {
		lock.close()
		return nil, ErrReconciliationRequired
	}
	if err := readExactJSON(journalPath, maximumJournalBytes, &executor.state); err != nil || validateJournal(executor.state, daemonID, instanceID) != nil {
		lock.close()
		return nil, ErrReconciliationRequired
	}
	journalDigest, err := journalSHA256(executor.state)
	if err != nil || journalDigest != record.JournalSHA256 {
		lock.close()
		return nil, ErrReconciliationRequired
	}
	if err := executor.syncJournalGuard(); err != nil {
		lock.close()
		return nil, ErrReconciliationRequired
	}
	return executor, nil
}

func newLockID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (executor *Executor) Close() {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.lock.close()
}

func (executor *Executor) Snapshot() []Entry {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	result := append([]Entry(nil), executor.state.Entries...)
	for index := range result {
		result[index].ResourceIDs = append([]string(nil), result[index].ResourceIDs...)
	}
	return result
}

func (executor *Executor) Execute(ctx context.Context, authority Authority, proof AuthorityProof, backend Backend, request Request) (Execution, error) {
	return executor.execute(ctx, authority, proof, backend, request, false)
}

// Recover admits only an exact journal replay or backend status readback. It
// never creates a sent entry and therefore can never repeat an uncertain
// effect whose Agent Service state is already sent or unknown.
func (executor *Executor) Recover(ctx context.Context, authority Authority, proof AuthorityProof, backend Backend, request Request) (Execution, error) {
	return executor.execute(ctx, authority, proof, backend, request, true)
}

func (executor *Executor) execute(ctx context.Context, authority Authority, proof AuthorityProof, backend Backend, request Request, recoveryOnly bool) (Execution, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if backend == nil {
		return Execution{}, errors.New("adapter backend is required")
	}
	if err := executor.lock.ensure(); err != nil {
		return Execution{}, err
	}
	if executor.poisoned {
		return Execution{}, ErrReconciliationRequired
	}
	hash, err := requestHash(request)
	if err != nil {
		return Execution{}, err
	}
	if index := executor.find(request.OperationID, request.StepID); index >= 0 {
		entry := executor.state.Entries[index]
		if entry.Generation != request.Generation || entry.RequestHash != hash || !slices.Equal(entry.ResourceIDs, request.ResourceIDs) {
			return Execution{}, ErrConflict
		}
		if entry.State == "sent" {
			if authority == nil || validateAuthorityProof(request, proof) != nil ||
				authority.VerifyActive(ctx, executor.daemonID, executor.instanceID, request, proof) != nil {
				return Execution{Proof: proof, JournalState: "sent", Replayed: true}, ErrStaleAuthority
			}
			recordedProof, recordErr := authority.RecordSent(ctx, executor.daemonID, executor.instanceID, request, proof)
			if recordErr != nil || validateAuthorityProof(request, recordedProof) != nil {
				return Execution{Proof: proof, JournalState: "sent", Replayed: true}, ErrStaleAuthority
			}
			proof = recordedProof
			if authority.VerifyActive(ctx, executor.daemonID, executor.instanceID, request, proof) != nil {
				return Execution{Proof: proof, JournalState: "sent", Replayed: true}, ErrStaleAuthority
			}
			result, reconcileErr := executor.reconcileLocked(ctx, backend, request, index)
			return Execution{Result: result, Proof: proof, JournalState: executor.state.Entries[index].State, Replayed: true}, reconcileErr
		}
		if authority == nil || validateAuthorityProof(request, proof) != nil ||
			authority.VerifyActive(ctx, executor.daemonID, executor.instanceID, request, proof) != nil {
			return Execution{}, ErrStaleAuthority
		}
		return Execution{Result: Result{Outcome: entry.Outcome, ReceiptID: entry.ReceiptID}, Proof: proof, JournalState: entry.State, Replayed: true}, nil
	}
	if recoveryOnly {
		return Execution{}, ErrReconciliationRequired
	}
	if len(executor.state.Entries) > 0 {
		last := executor.state.Entries[len(executor.state.Entries)-1]
		if last.State == "sent" {
			return Execution{}, ErrUnknownResult
		}
		if request.Generation <= last.Generation {
			return Execution{}, ErrConflict
		}
	}
	for _, prior := range executor.state.Entries {
		if prior.OperationID == request.OperationID {
			return Execution{}, ErrConflict
		}
	}
	if authority == nil || validateAuthorityProof(request, proof) != nil ||
		authority.VerifyActive(ctx, executor.daemonID, executor.instanceID, request, proof) != nil {
		return Execution{}, ErrStaleAuthority
	}
	entry := Entry{
		OperationID: request.OperationID, StepID: request.StepID, Generation: request.Generation,
		RequestHash: hash, ResourceIDs: append([]string(nil), request.ResourceIDs...), State: "sent",
		UpdatedAt: executor.timestamp(),
	}
	executor.state.Entries = append(executor.state.Entries, entry)
	if err := executor.persist(); err != nil {
		executor.poisoned = true
		return Execution{Proof: proof, JournalState: "sent"}, err
	}
	if err := executor.lock.ensure(); err != nil {
		return Execution{Proof: proof, JournalState: "sent"}, err
	}
	recordedProof, recordErr := authority.RecordSent(ctx, executor.daemonID, executor.instanceID, request, proof)
	if recordErr != nil || validateAuthorityProof(request, recordedProof) != nil {
		return Execution{Proof: proof, JournalState: "sent"}, ErrStaleAuthority
	}
	proof = recordedProof
	if err := executor.lock.ensure(); err != nil {
		return Execution{Proof: proof, JournalState: "sent"}, err
	}
	if authority.VerifyActive(ctx, executor.daemonID, executor.instanceID, request, proof) != nil {
		return Execution{Proof: proof, JournalState: "sent"}, ErrStaleAuthority
	}
	if executor.beforeApply != nil {
		executor.beforeApply()
	}
	if authority.VerifyActive(ctx, executor.daemonID, executor.instanceID, request, proof) != nil {
		return Execution{Proof: proof, JournalState: "sent"}, ErrStaleAuthority
	}
	if err := executor.attestSentJournal(); err != nil {
		executor.poisoned = true
		return Execution{Proof: proof, JournalState: "sent"}, err
	}
	result, callErr := backend.Apply(ctx, request)
	if callErr != nil {
		var effect *EffectError
		if !errors.As(callErr, &effect) || !effect.Known {
			return Execution{Proof: proof, JournalState: "sent"}, ErrUnknownResult
		}
		result = Result{Outcome: "failed", ReceiptID: effect.Code}
		if !validResult(result) {
			return Execution{Proof: proof, JournalState: "sent"}, ErrUnknownResult
		}
		if err := executor.lock.ensure(); err != nil {
			return Execution{Proof: proof, JournalState: "sent"}, err
		}
		if err := executor.resolve(len(executor.state.Entries)-1, "failed", result); err != nil {
			return Execution{Proof: proof, JournalState: "sent"}, ErrReconciliationRequired
		}
		return Execution{Result: result, Proof: proof, JournalState: "failed"}, callErr
	}
	if !validResult(result) {
		return Execution{Proof: proof, JournalState: "sent"}, ErrUnknownResult
	}
	if err := executor.lock.ensure(); err != nil {
		return Execution{Proof: proof, JournalState: "sent"}, err
	}
	resolvedState := "acknowledged"
	if result.Outcome == "failed" {
		resolvedState = "failed"
	}
	if err := executor.resolve(len(executor.state.Entries)-1, resolvedState, result); err != nil {
		return Execution{Proof: proof, JournalState: "sent"}, ErrReconciliationRequired
	}
	return Execution{Result: result, Proof: proof, JournalState: resolvedState}, nil
}

// Reconcile performs status/readback only. It never repeats Apply.
func (executor *Executor) Reconcile(ctx context.Context, backend Backend, request Request) (Result, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if backend == nil {
		return Result{}, errors.New("adapter backend is required")
	}
	if err := executor.lock.ensure(); err != nil {
		return Result{}, err
	}
	if executor.poisoned {
		return Result{}, ErrReconciliationRequired
	}
	hash, err := requestHash(request)
	if err != nil {
		return Result{}, err
	}
	index := executor.find(request.OperationID, request.StepID)
	if index < 0 {
		return Result{}, ErrConflict
	}
	entry := executor.state.Entries[index]
	if entry.Generation != request.Generation || entry.RequestHash != hash || !slices.Equal(entry.ResourceIDs, request.ResourceIDs) {
		return Result{}, ErrConflict
	}
	if entry.State != "sent" {
		return Result{Outcome: entry.Outcome, ReceiptID: entry.ReceiptID}, nil
	}
	return executor.reconcileLocked(ctx, backend, request, index)
}

func (executor *Executor) reconcileLocked(ctx context.Context, backend Backend, request Request, index int) (Result, error) {
	if err := executor.lock.ensure(); err != nil {
		return Result{}, err
	}
	result, err := backend.Status(ctx, request)
	if err != nil || !validResult(result) {
		return Result{}, ErrUnknownResult
	}
	if err := executor.lock.ensure(); err != nil {
		return Result{}, err
	}
	if err := executor.resolve(index, "reconciled", result); err != nil {
		return Result{}, ErrReconciliationRequired
	}
	return result, nil
}

func (executor *Executor) find(operationID, stepID string) int {
	for index := range executor.state.Entries {
		entry := executor.state.Entries[index]
		if entry.OperationID == operationID && entry.StepID == stepID {
			return index
		}
	}
	return -1
}

func (executor *Executor) resolve(index int, state string, result Result) error {
	original := executor.state.Entries[index]
	executor.state.Entries[index].State = state
	executor.state.Entries[index].Outcome = result.Outcome
	executor.state.Entries[index].ReceiptID = result.ReceiptID
	executor.state.Entries[index].UpdatedAt = executor.timestamp()
	if err := executor.persist(); err != nil {
		executor.state.Entries[index] = original
		executor.poisoned = true
		return err
	}
	return nil
}

func (executor *Executor) persist() error {
	if err := validateJournal(executor.state, executor.daemonID, executor.instanceID); err != nil {
		return err
	}
	if err := executor.replace(executor.journalPath, executor.state, true); err != nil {
		return err
	}
	var readback journalState
	if err := readExactJSON(executor.journalPath, maximumJournalBytes, &readback); err != nil || validateJournal(readback, executor.daemonID, executor.instanceID) != nil {
		return ErrReconciliationRequired
	}
	expected, expectedErr := json.Marshal(executor.state)
	actual, actualErr := json.Marshal(readback)
	if expectedErr != nil || actualErr != nil || !bytes.Equal(expected, actual) {
		return ErrReconciliationRequired
	}
	executor.state = readback
	digest, err := journalSHA256(readback)
	if err != nil {
		return ErrReconciliationRequired
	}
	if err := executor.lock.updateJournalDigest(digest); err != nil {
		return err
	}
	return executor.syncGuard()
}

func (executor *Executor) timestamp() string {
	return executor.now().UTC().Format(time.RFC3339Nano)
}
