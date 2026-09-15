package dockeradapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func stateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func requestFixture(operationID string, generation int64) Request {
	return Request{
		SchemaID: RequestSchemaID, OperationID: operationID, StepID: "apply-1", Generation: generation,
		Action: "fixture.apply", ResourceIDs: []string{"container:agent-1", "volume:agent-1"},
	}
}

func proofFixture(request Request) AuthorityProof {
	return AuthorityProof{
		OperationID: request.OperationID, Generation: request.Generation,
		WorkerID: "worker-1", WorkerToken: "lease-token-1", OperationVersion: 1,
	}
}

func executeAuthorized(executor *Executor, backend Backend, request Request) (Result, error) {
	authority := NewFixtureAuthority()
	proof := proofFixture(request)
	if err := authority.Grant(executor.daemonID, executor.instanceID, request, proof); err != nil {
		return Result{}, err
	}
	execution, err := executor.Execute(context.Background(), authority, proof, backend, request)
	return execution.Result, err
}

func TestLostAcknowledgementRestartReconcilesWithoutDoubleEffect(t *testing.T) {
	directory := stateDirectory(t)
	backend := NewFixtureBackend()
	backend.LoseNextAcknowledgement()
	request := requestFixture("op-lost-ack", 1)
	executor, err := Initialize(directory, "daemon-1", "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeAuthorized(executor, backend, request); !errors.Is(err, ErrUnknownResult) {
		t.Fatal("lost acknowledgement was not unknown", err)
	}
	if backend.ApplyCalls() != 1 || len(executor.Snapshot()) != 1 || executor.Snapshot()[0].State != "sent" {
		t.Fatal("sent barrier was not durable", backend.ApplyCalls(), executor.Snapshot())
	}
	executor.Close()

	reopened, err := Open(directory, "daemon-1", "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	backend.SetStatusUnknown(true)
	if _, err := executeAuthorized(reopened, backend, request); !errors.Is(err, ErrUnknownResult) {
		t.Fatal("unknown restart result was repeated", err)
	}
	if _, err := executeAuthorized(reopened, backend, requestFixture("op-next", 2)); !errors.Is(err, ErrUnknownResult) {
		t.Fatal("unresolved call did not block next generation", err)
	}
	if backend.ApplyCalls() != 1 {
		t.Fatal("unknown result repeated the effect", backend.ApplyCalls())
	}
	backend.SetStatusUnknown(false)
	result, err := reopened.Reconcile(context.Background(), backend, request)
	if err != nil || result.Outcome != "applied" || result.ReceiptID != "fixture:op-lost-ack" {
		t.Fatal("reconciliation failed", result, err)
	}
	if repeat, err := executeAuthorized(reopened, backend, request); err != nil || repeat != result || backend.ApplyCalls() != 1 {
		t.Fatal("idempotent receipt changed or effect repeated", repeat, err, backend.ApplyCalls())
	}
	if _, err := executeAuthorized(reopened, backend, requestFixture("op-next", 2)); err != nil || backend.ApplyCalls() != 2 {
		t.Fatal("resolved journal did not admit next generation", err, backend.ApplyCalls())
	}
	if _, err := executeAuthorized(reopened, backend, requestFixture("op-late", 1)); !errors.Is(err, ErrConflict) {
		t.Fatal("late generation was accepted", err)
	}
}

func TestAcknowledgedJournalSurvivesAmbiguousGuardCleanupWithoutDoubleEffect(t *testing.T) {
	directory := stateDirectory(t)
	request := requestFixture("op-ambiguous-guard-cleanup", 1)
	executor, err := Initialize(directory, "daemon-guard-cleanup", "instance-guard-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFixtureBackend()
	executor.syncGuard = func() error {
		if journalHasUnresolvedSent(executor.state) {
			return executor.syncJournalGuard()
		}
		guardPath := journalGuardPath(executor.journalPath)
		if err := os.Remove(guardPath); err != nil {
			return err
		}
		if err := syncStateDirectory(guardPath); err != nil {
			return err
		}
		return ErrReconciliationRequired
	}
	if execution, err := func() (Execution, error) {
		authority := NewFixtureAuthority()
		proof := proofFixture(request)
		if grantErr := authority.Grant(executor.daemonID, executor.instanceID, request, proof); grantErr != nil {
			return Execution{}, grantErr
		}
		return executor.Execute(context.Background(), authority, proof, backend, request)
	}(); !errors.Is(err, ErrReconciliationRequired) || execution.JournalState != "sent" {
		t.Fatalf("execution=%+v err=%v", execution, err)
	}
	if backend.ApplyCalls() != 1 || executor.Snapshot()[0].State != "sent" {
		t.Fatalf("applyCalls=%d memory=%+v", backend.ApplyCalls(), executor.Snapshot())
	}
	executor.Close()

	reopened, err := Open(directory, "daemon-guard-cleanup", "instance-guard-cleanup")
	if err != nil {
		t.Fatal("durable acknowledged journal did not reopen", err)
	}
	defer reopened.Close()
	authority := NewFixtureAuthority()
	proof := proofFixture(request)
	if err := authority.Grant(reopened.daemonID, reopened.instanceID, request, proof); err != nil {
		t.Fatal(err)
	}
	execution, err := reopened.Execute(context.Background(), authority, proof, backend, request)
	if err != nil || execution.JournalState != "acknowledged" || !execution.Replayed ||
		execution.Result.Outcome != "applied" || backend.ApplyCalls() != 1 {
		t.Fatalf("execution=%+v err=%v applyCalls=%d", execution, err, backend.ApplyCalls())
	}
}

func TestRecoveryWithoutExactJournalEntryNeverCallsApply(t *testing.T) {
	executor, err := Initialize(stateDirectory(t), "daemon-recovery-only", "instance-recovery-only")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	request := requestFixture("op-missing-recovery-entry", 1)
	authority := NewFixtureAuthority()
	proof := proofFixture(request)
	if err := authority.Grant(executor.daemonID, executor.instanceID, request, proof); err != nil {
		t.Fatal(err)
	}
	backend := NewFixtureBackend()
	if execution, err := executor.Recover(context.Background(), authority, proof, backend, request); !errors.Is(err, ErrReconciliationRequired) || execution != (Execution{}) {
		t.Fatalf("execution=%+v err=%v", execution, err)
	}
	if backend.ApplyCalls() != 0 || len(executor.Snapshot()) != 0 {
		t.Fatalf("recovery created an effect: applyCalls=%d journal=%+v", backend.ApplyCalls(), executor.Snapshot())
	}
}

func TestPersistentFixtureBackendRestoresStatusAcrossAdapterProcessRestart(t *testing.T) {
	directory := stateDirectory(t)
	request := requestFixture("op-persistent-lost-ack", 1)
	executor, err := Initialize(directory, "daemon-persistent", "instance-persistent")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := OpenFixtureBackend(directory, "daemon-persistent", "instance-persistent", len(executor.Snapshot()) == 0)
	if err != nil {
		executor.Close()
		t.Fatal(err)
	}
	backend.LoseNextAcknowledgement()
	if _, err := executeAuthorized(executor, backend, request); !errors.Is(err, ErrUnknownResult) {
		executor.Close()
		t.Fatal("fixture did not retain a lost acknowledgement", err)
	}
	executor.Close()

	reopened, err := Open(directory, "daemon-persistent", "instance-persistent")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := OpenFixtureBackend(directory, "daemon-persistent", "instance-persistent", len(reopened.Snapshot()) == 0)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := func() (Execution, error) {
		authority := NewFixtureAuthority()
		proof := proofFixture(request)
		if grantErr := authority.Grant(reopened.daemonID, reopened.instanceID, request, proof); grantErr != nil {
			return Execution{}, grantErr
		}
		return reopened.Execute(context.Background(), authority, proof, restored, request)
	}()
	if err != nil || execution.Result.Outcome != "applied" || execution.JournalState != "reconciled" || restored.ApplyCalls() != 0 {
		t.Fatalf("execution=%+v err=%v applyCalls=%d", execution, err, restored.ApplyCalls())
	}

	fixturePath := filepath.Join(directory, fixtureFileKey("daemon-persistent", "instance-persistent")+".fixture.json")
	if err := os.Remove(fixturePath); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFixtureBackend(directory, "daemon-persistent", "instance-persistent", false); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatal("lost external fixture state was treated as not-applied", err)
	}
}

func TestTotalJournalTupleLossCannotBecomeImplicitFirstRun(t *testing.T) {
	directory := stateDirectory(t)
	daemonID, instanceID := "daemon-total-loss", "instance-total-loss"
	executor, err := Initialize(directory, daemonID, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := OpenFixtureBackend(directory, daemonID, instanceID, true)
	if err != nil {
		executor.Close()
		t.Fatal(err)
	}
	request := requestFixture("op-total-loss", 1)
	backend.LoseNextAcknowledgement()
	if _, err := executeAuthorized(executor, backend, request); !errors.Is(err, ErrUnknownResult) {
		executor.Close()
		t.Fatal("fixture effect did not reach unknown", err)
	}
	if backend.ApplyCalls() != 1 {
		executor.Close()
		t.Fatal("fixture effect count changed", backend.ApplyCalls())
	}
	executor.Close()

	lockPath, anchorPath, journalPath := fileNames(directory, daemonID, instanceID)
	for _, path := range []string{journalGuardPath(journalPath), journalPath, anchorPath, lockPath + ".guard", lockPath} {
		if err := os.Remove(path); err != nil {
			t.Fatal("cannot simulate complete journal tuple loss", path, err)
		}
	}
	if reopened, err := Open(directory, daemonID, instanceID); !errors.Is(err, ErrReconciliationRequired) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("ordinary open recreated a lost journal tuple", err)
	}
	if reinitialized, err := Initialize(directory, daemonID, instanceID); !errors.Is(err, ErrReconciliationRequired) {
		if reinitialized != nil {
			reinitialized.Close()
		}
		t.Fatal("fixture state did not block unsafe reinitialization", err)
	}
	if backend.ApplyCalls() != 1 {
		t.Fatal("journal loss admitted another effect", backend.ApplyCalls())
	}
}

func TestPersistentFixtureBackendPoisonsAmbiguousCommitUntilCleanReopen(t *testing.T) {
	directory := stateDirectory(t)
	request := requestFixture("op-fixture-ambiguous", 1)
	executor, err := Initialize(directory, "daemon-fixture-ambiguous", "instance-fixture-ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := OpenFixtureBackend(directory, executor.daemonID, executor.instanceID, true)
	if err != nil {
		executor.Close()
		t.Fatal(err)
	}
	backend.replace = func(path string, value any, replacing bool) error {
		if err := replaceJSON(path, value, replacing); err != nil {
			return err
		}
		return errors.New("synthetic post-rename uncertainty")
	}
	if _, err := executeAuthorized(executor, backend, request); !errors.Is(err, ErrUnknownResult) {
		executor.Close()
		t.Fatal("ambiguous fixture commit was not unknown", err)
	}
	if _, err := backend.Status(context.Background(), request); err == nil {
		executor.Close()
		t.Fatal("poisoned fixture backend reported a known status")
	}
	executor.Close()

	reopened, err := Open(directory, "daemon-fixture-ambiguous", "instance-fixture-ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := OpenFixtureBackend(directory, reopened.daemonID, reopened.instanceID, false)
	if err != nil {
		t.Fatal("clean durable fixture readback failed", err)
	}
	result, err := reopened.Reconcile(context.Background(), restored, request)
	if err != nil || result.Outcome != "applied" || restored.ApplyCalls() != 0 {
		t.Fatalf("result=%+v err=%v applyCalls=%d", result, err, restored.ApplyCalls())
	}
}

func TestExecutorSerializesSameOperationAndReturnsOneReceipt(t *testing.T) {
	executor, err := Initialize(stateDirectory(t), "daemon-2", "instance-2")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	backend := NewFixtureBackend()
	request := requestFixture("op-concurrent", 1)
	start := make(chan struct{})
	results := make(chan Result, 2)
	errorsOut := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, executeErr := executeAuthorized(executor, backend, request)
			results <- result
			errorsOut <- executeErr
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *Result
	for result := range results {
		if first == nil {
			copy := result
			first = &copy
		} else if result != *first {
			t.Fatal("concurrent callers received different receipts", result, *first)
		}
	}
	if backend.ApplyCalls() != 1 {
		t.Fatal("concurrent operation executed more than once", backend.ApplyCalls())
	}
}

func TestFixtureReceiptsRemainContractValidAtMaximumOperationID(t *testing.T) {
	executor, err := Initialize(stateDirectory(t), "daemon-max-id", "instance-max-id")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	backend := NewFixtureBackend()
	request := requestFixture(strings.Repeat("a", 128), 1)
	result, err := executeAuthorized(executor, backend, request)
	if err != nil || !validResult(result) || len(result.ReceiptID) > 128 {
		t.Fatal("maximum operation ID produced an invalid receipt", result, err)
	}
	missing := requestFixture(strings.Repeat("b", 128), 2)
	status, err := backend.Status(context.Background(), missing)
	if err != nil || !validResult(status) || len(status.ReceiptID) > 128 {
		t.Fatal("maximum missing operation ID produced an invalid receipt", status, err)
	}
}

func TestExecutorFileLockJournalLossAndLockLossFailClosed(t *testing.T) {
	directory := stateDirectory(t)
	executor, err := Initialize(directory, "daemon-3", "instance-3")
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(directory, "daemon-3", "instance-3"); err == nil {
		second.Close()
		t.Fatal("second executor acquired the same daemon and instance")
	}
	backend := NewFixtureBackend()
	lockPath, _, journalPath := fileNames(directory, "daemon-3", "instance-3")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if replacement, err := Open(directory, "daemon-3", "instance-3"); !errors.Is(err, ErrReconciliationRequired) {
		if replacement != nil {
			replacement.Close()
		}
		t.Fatal("deleted lock allowed a replacement executor", err)
	}
	if _, err := executeAuthorized(executor, backend, requestFixture("op-lock-lost", 1)); !errors.Is(err, ErrLockLost) {
		t.Fatal("lost lock admitted a mutation", err)
	}
	if backend.ApplyCalls() != 0 {
		t.Fatal("backend called after lock loss")
	}
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(directory, "daemon-3", "instance-3"); !errors.Is(err, ErrReconciliationRequired) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("lost journal did not block mutations", err)
	}
}

func TestValidJournalRollbackCannotRepeatUnknownEffect(t *testing.T) {
	directory := stateDirectory(t)
	executor, err := Initialize(directory, "daemon-journal-rollback", "instance-journal-rollback")
	if err != nil {
		t.Fatal(err)
	}
	empty := executor.state
	backend := NewFixtureBackend()
	backend.LoseNextAcknowledgement()
	request := requestFixture("op-journal-rollback", 1)
	if _, err := executeAuthorized(executor, backend, request); !errors.Is(err, ErrUnknownResult) {
		executor.Close()
		t.Fatal("fixture effect did not become unknown", err)
	}
	executor.Close()
	_, _, journalPath := fileNames(directory, executor.daemonID, executor.instanceID)
	if err := replaceJSON(journalPath, empty, true); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(directory, executor.daemonID, executor.instanceID); !errors.Is(err, ErrReconciliationRequired) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("contract-valid journal rollback was accepted", err)
	}
	if backend.ApplyCalls() != 1 {
		t.Fatal("unknown effect count changed", backend.ApplyCalls())
	}
}

func TestLockReplacementDuringSentPersistenceBlocksEffectAndReconciles(t *testing.T) {
	directory := stateDirectory(t)
	executor, err := Initialize(directory, "daemon-lock-race", "instance-lock-race")
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFixtureBackend()
	request := requestFixture("op-lock-race", 1)
	replaced := false
	executor.replace = func(path string, value any, replacing bool) error {
		if err := replaceJSON(path, value, replacing); err != nil {
			return err
		}
		if !replaced {
			lockRecord, err := os.ReadFile(executor.lock.path)
			if err != nil {
				return err
			}
			if err := os.Remove(executor.lock.path); err != nil {
				return err
			}
			if err := os.WriteFile(executor.lock.path, lockRecord, 0o600); err != nil {
				return err
			}
			replaced = true
		}
		return nil
	}
	if _, err := executeAuthorized(executor, backend, request); !errors.Is(err, ErrLockLost) {
		t.Fatal("lock replacement during sent persistence reached the effect boundary", err)
	}
	if backend.ApplyCalls() != 0 || executor.Snapshot()[0].State != "sent" {
		t.Fatal("lock replacement applied an effect or lost the sent barrier", backend.ApplyCalls(), executor.Snapshot())
	}
	executor.Close()
	if replacement, err := Open(directory, "daemon-lock-race", "instance-lock-race"); !errors.Is(err, ErrReconciliationRequired) {
		if replacement != nil {
			replacement.Close()
		}
		t.Fatal("replacement lock inode admitted another executor", err)
	}
	if err := os.Remove(executor.lock.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(executor.lock.guardPath, executor.lock.path); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(directory, "daemon-lock-race", "instance-lock-race"); !errors.Is(err, ErrReconciliationRequired) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("unattested journal digest was accepted", err)
	}
	var durable journalState
	if err := readExactJSON(executor.journalPath, maximumJournalBytes, &durable); err != nil {
		t.Fatal(err)
	}
	digest, err := journalSHA256(durable)
	if err != nil {
		t.Fatal(err)
	}
	recoveryLock, err := openProcessLock(executor.lock.path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryLock.bindGuard(false); err != nil {
		recoveryLock.close()
		t.Fatal(err)
	}
	record, err := readLockRecord(recoveryLock.file)
	if err != nil {
		recoveryLock.close()
		t.Fatal(err)
	}
	record.JournalSHA256 = digest
	if err := recoveryLock.initialize(record); err != nil {
		recoveryLock.close()
		t.Fatal(err)
	}
	recoveryLock.close()
	reopened, err := Open(directory, "daemon-lock-race", "instance-lock-race")
	if err != nil {
		t.Fatal("replacement executor could not recover the exact journal", err)
	}
	defer reopened.Close()
	result, err := reopened.Reconcile(context.Background(), backend, request)
	if err != nil || result.Outcome != "not_applied" || backend.ApplyCalls() != 0 {
		t.Fatal("replacement executor did not reconcile without Apply", result, err, backend.ApplyCalls())
	}
}

func TestLockReplacementAtEffectBoundaryCannotAdmitApplyOrReplacementExecutor(t *testing.T) {
	directory := stateDirectory(t)
	executor, err := Initialize(directory, "daemon-effect-lock-race", "instance-effect-lock-race")
	if err != nil {
		t.Fatal(err)
	}
	backend := NewFixtureBackend()
	executor.beforeApply = func() {
		record, readErr := os.ReadFile(executor.lock.path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if removeErr := os.Remove(executor.lock.path); removeErr != nil {
			t.Fatal(removeErr)
		}
		if writeErr := os.WriteFile(executor.lock.path, record, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if _, err := executeAuthorized(executor, backend, requestFixture("op-effect-lock-race", 1)); !errors.Is(err, ErrLockLost) {
		t.Fatal("effect-boundary lock replacement was not rejected", err)
	}
	if backend.ApplyCalls() != 0 || executor.Snapshot()[0].State != "sent" {
		t.Fatal("effect ran or sent barrier was lost", backend.ApplyCalls(), executor.Snapshot())
	}
	if replacement, err := Open(directory, "daemon-effect-lock-race", "instance-effect-lock-race"); !errors.Is(err, ErrReconciliationRequired) {
		if replacement != nil {
			replacement.Close()
		}
		t.Fatal("hard-link guard admitted a replacement executor", err)
	}
	executor.Close()
}

func TestJournalLossOrReplacementAtEffectBoundaryCannotAdmitApply(t *testing.T) {
	mutations := map[string]func(*Executor) error{
		"removed": func(executor *Executor) error {
			return os.Remove(executor.journalPath)
		},
		"same content on replacement inode": func(executor *Executor) error {
			return replaceJSON(executor.journalPath, executor.state, true)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			directory := stateDirectory(t)
			executor, err := Initialize(directory, "daemon-journal-boundary", "instance-"+strings.ReplaceAll(name, " ", "-"))
			if err != nil {
				t.Fatal(err)
			}
			backend := NewFixtureBackend()
			request := requestFixture("op-journal-boundary", 1)
			authority := NewFixtureAuthority()
			proof := proofFixture(request)
			if err := authority.Grant(executor.daemonID, executor.instanceID, request, proof); err != nil {
				executor.Close()
				t.Fatal(err)
			}
			executor.beforeApply = func() {
				if err := mutate(executor); err != nil {
					t.Fatal(err)
				}
			}
			execution, err := executor.Execute(context.Background(), authority, proof, backend, request)
			if !errors.Is(err, ErrReconciliationRequired) || execution.JournalState != "sent" {
				executor.Close()
				t.Fatalf("execution=%+v err=%v", execution, err)
			}
			if backend.ApplyCalls() != 0 || !executor.poisoned || len(executor.Snapshot()) != 1 || executor.Snapshot()[0].State != "sent" {
				executor.Close()
				t.Fatal("journal mutation crossed the effect boundary or lost the in-memory sent barrier")
			}
			guardPath := journalGuardPath(executor.journalPath)
			var guarded journalState
			if err := readExactJSON(guardPath, maximumJournalBytes, &guarded); err != nil || !sameJournal(guarded, executor.state) {
				executor.Close()
				t.Fatal("sent journal guard was not retained", err)
			}
			executor.Close()
			if reopened, err := Open(directory, executor.daemonID, executor.instanceID); !errors.Is(err, ErrReconciliationRequired) {
				if reopened != nil {
					reopened.Close()
				}
				t.Fatal("mutated public journal path was accepted", err)
			}
			if info, err := os.Lstat(executor.journalPath); err == nil {
				if !info.Mode().IsRegular() || os.Remove(executor.journalPath) != nil {
					t.Fatal("cannot remove the test replacement journal")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Link(guardPath, executor.journalPath); err != nil {
				t.Fatal(err)
			}
			recovered, err := Open(directory, executor.daemonID, executor.instanceID)
			if err != nil {
				t.Fatal("exact guarded sent journal could not be recovered", err)
			}
			result, err := recovered.Reconcile(context.Background(), backend, request)
			recovered.Close()
			if err != nil || result.Outcome != "not_applied" || backend.ApplyCalls() != 0 {
				t.Fatal("guarded sent journal did not reconcile without Apply", result, err, backend.ApplyCalls())
			}
			if _, err := os.Lstat(guardPath); !os.IsNotExist(err) {
				t.Fatal("resolved journal retained its sent guard", err)
			}
		})
	}
}

type observingBackend struct {
	t          *testing.T
	journal    string
	applyCalls int
}

func (backend *observingBackend) Apply(_ context.Context, request Request) (Result, error) {
	backend.applyCalls++
	var state journalState
	if err := readExactJSON(backend.journal, maximumJournalBytes, &state); err != nil || len(state.Entries) != 1 || state.Entries[0].State != "sent" {
		backend.t.Fatalf("backend was called before durable sent: %+v %v", state, err)
	}
	return Result{Outcome: "applied", ReceiptID: "observed:" + request.OperationID}, nil
}

func (backend *observingBackend) Status(context.Context, Request) (Result, error) {
	return Result{}, errors.New("unexpected status")
}

func TestSentIsFsyncedBeforeBackendCall(t *testing.T) {
	directory := stateDirectory(t)
	executor, err := Initialize(directory, "daemon-4", "instance-4")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	_, _, journalPath := fileNames(directory, "daemon-4", "instance-4")
	backend := &observingBackend{t: t, journal: journalPath}
	if _, err := executeAuthorized(executor, backend, requestFixture("op-observed", 1)); err != nil {
		t.Fatal(err)
	}
	if backend.applyCalls != 1 {
		t.Fatal("fixture backend was not called exactly once")
	}
	info, err := os.Lstat(journalPath)
	if err != nil || info.Mode().Perm() != 0o600 || !filepath.IsAbs(journalPath) {
		t.Fatal("journal is not a private regular file", info, err)
	}
}

func TestJournalRejectsUnsafeRequestShape(t *testing.T) {
	executor, err := Initialize(stateDirectory(t), "daemon-5", "instance-5")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	backend := NewFixtureBackend()
	request := requestFixture("op-invalid", 1)
	request.ResourceIDs = []string{"volume:agent-1", "container:agent-1"}
	if _, err := executeAuthorized(executor, backend, request); err == nil {
		t.Fatal("non-canonical exact resources accepted")
	}
	if backend.ApplyCalls() != 0 {
		t.Fatal("invalid request reached backend")
	}
	executor.now = func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }
}

func TestJournalRejectsAnUnresolvedEntryBeforeAYoungerGeneration(t *testing.T) {
	state := journalState{
		SchemaID: JournalSchemaID, DaemonID: "daemon-5", InstanceID: "instance-5",
		Entries: []Entry{
			{
				OperationID: "op-unresolved", StepID: "apply-1", Generation: 1,
				RequestHash: strings.Repeat("a", 64), ResourceIDs: []string{"container:agent-1"},
				State: "sent", UpdatedAt: "2026-09-14T12:00:00Z",
			},
			{
				OperationID: "op-younger", StepID: "apply-2", Generation: 2,
				RequestHash: strings.Repeat("b", 64), ResourceIDs: []string{"container:agent-1"},
				State: "acknowledged", Outcome: "applied", ReceiptID: "fixture:op-younger",
				UpdatedAt: "2026-09-14T12:00:01Z",
			},
		},
	}
	if err := validateJournal(state, state.DaemonID, state.InstanceID); err == nil {
		t.Fatal("restart accepted a journal that crossed an unresolved effect")
	}
}

func TestSnapshotCannotMutateExecutorJournal(t *testing.T) {
	directory := stateDirectory(t)
	executor, err := Initialize(directory, "daemon-6", "instance-6")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	backend := NewFixtureBackend()
	request := requestFixture("op-snapshot", 1)
	result, err := executeAuthorized(executor, backend, request)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := executor.Snapshot()
	snapshot[0].ResourceIDs[0] = "container:tampered"
	snapshot[0].State = "sent"
	repeated, err := executeAuthorized(executor, backend, request)
	if err != nil || repeated != result || backend.ApplyCalls() != 1 {
		t.Fatal("snapshot mutated executor state", repeated, err, backend.ApplyCalls())
	}
}

func TestJournalReadbackMustExactlyMatchPersistedIntent(t *testing.T) {
	directory := stateDirectory(t)
	executor, err := Initialize(directory, "daemon-7", "instance-7")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	backend := NewFixtureBackend()
	executor.now = func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }
	executor.replace = func(path string, value any, replacing bool) error {
		state, ok := value.(journalState)
		if !ok || len(state.Entries) != 1 {
			t.Fatal("unexpected journal write")
		}
		state.Entries = append([]Entry(nil), state.Entries...)
		state.Entries[0].ResourceIDs = append([]string(nil), state.Entries[0].ResourceIDs...)
		state.Entries[0].UpdatedAt = "2026-09-14T12:00:01Z"
		return replaceJSON(path, state, replacing)
	}
	if _, err := executeAuthorized(executor, backend, requestFixture("op-readback-mismatch", 1)); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatal("valid but different journal readback was accepted", err)
	}
	if backend.ApplyCalls() != 0 || !executor.poisoned {
		t.Fatal("readback mismatch reached the effect backend")
	}
}

func TestActiveAuthorityRejectsStaleWorkerAndAllowsGlobalGenerationGaps(t *testing.T) {
	executor, err := Initialize(stateDirectory(t), "daemon-8", "instance-8")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	backend := NewFixtureBackend()
	authority := NewFixtureAuthority()
	stale := requestFixture("op-stale-worker", 1)
	staleProof := proofFixture(stale)
	current := requestFixture("op-current-worker", 2)
	currentProof := proofFixture(current)
	if err := authority.Grant(executor.daemonID, executor.instanceID, current, currentProof); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), authority, staleProof, backend, stale); !errors.Is(err, ErrStaleAuthority) {
		t.Fatal("stale worker reached a fresh adapter journal", err)
	}
	if backend.ApplyCalls() != 0 || len(executor.Snapshot()) != 0 {
		t.Fatal("stale authority persisted or executed an effect")
	}
	if _, err := executor.Execute(context.Background(), authority, currentProof, backend, current); err != nil {
		t.Fatal("current global generation was rejected", err)
	}

	// Generation 3 can belong to registry.install and therefore never appear in
	// this adapter journal. Active authority makes a forward gap unambiguous.
	afterRegistry := requestFixture("op-after-registry", 4)
	afterRegistryProof := proofFixture(afterRegistry)
	if err := authority.Grant(executor.daemonID, executor.instanceID, afterRegistry, afterRegistryProof); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), authority, afterRegistryProof, backend, afterRegistry); err != nil {
		t.Fatal("adapter rejected generation after a non-adapter operation", err)
	}
	reused := requestFixture(current.OperationID, 5)
	reused.StepID = "apply-reused-operation"
	reusedProof := proofFixture(reused)
	if err := authority.Grant(executor.daemonID, executor.instanceID, reused, reusedProof); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), authority, reusedProof, backend, reused); !errors.Is(err, ErrConflict) {
		t.Fatal("operation ID was reused with a different step", err)
	}
}

type expiringAuthority struct {
	calls int
}

func (authority *expiringAuthority) VerifyActive(context.Context, string, string, Request, AuthorityProof) error {
	authority.calls++
	if authority.calls > 1 {
		return ErrStaleAuthority
	}
	return nil
}

func (authority *expiringAuthority) RecordSent(_ context.Context, _ string, _ string, _ Request, proof AuthorityProof) (AuthorityProof, error) {
	proof.OperationVersion++
	return proof, nil
}

func TestAuthorityLostAfterSentDoesNotReachBackendOrAllowNextEffect(t *testing.T) {
	executor, err := Initialize(stateDirectory(t), "daemon-9", "instance-9")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	backend := NewFixtureBackend()
	request := requestFixture("op-authority-lost", 1)
	proof := proofFixture(request)
	if _, err := executor.Execute(context.Background(), &expiringAuthority{}, proof, backend, request); !errors.Is(err, ErrStaleAuthority) {
		t.Fatal("lost authority was not rejected after sent", err)
	}
	if backend.ApplyCalls() != 0 || len(executor.Snapshot()) != 1 || executor.Snapshot()[0].State != "sent" {
		t.Fatal("authority loss crossed the effect boundary or lost its sent barrier")
	}
	if _, err := executeAuthorized(executor, backend, requestFixture("op-next-after-authority-loss", 2)); !errors.Is(err, ErrUnknownResult) {
		t.Fatal("unresolved authority loss admitted the next effect", err)
	}
	result, err := executor.Reconcile(context.Background(), backend, request)
	if err != nil || result.Outcome != "not_applied" {
		t.Fatal("known not-applied result did not reconcile", result, err)
	}
	if _, err := executeAuthorized(executor, backend, requestFixture("op-next-after-authority-loss", 2)); err != nil || backend.ApplyCalls() != 1 {
		t.Fatal("known reconciliation did not release the next generation", err)
	}
}

func TestAuthorityRevokedAtEffectBoundaryLeavesSentBarrierWithoutApply(t *testing.T) {
	executor, err := Initialize(stateDirectory(t), "daemon-boundary", "instance-boundary")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	backend := NewFixtureBackend()
	authority := NewFixtureAuthority()
	request := requestFixture("op-boundary-revoked", 1)
	proof := proofFixture(request)
	if err := authority.Grant(executor.daemonID, executor.instanceID, request, proof); err != nil {
		t.Fatal(err)
	}
	executor.beforeApply = func() {
		revoked := proof
		revoked.WorkerToken = "lease-token-2"
		revoked.OperationVersion = 3
		if err := authority.Grant(executor.daemonID, executor.instanceID, request, revoked); err != nil {
			t.Fatal(err)
		}
	}
	execution, err := executor.Execute(context.Background(), authority, proof, backend, request)
	if !errors.Is(err, ErrStaleAuthority) || execution.JournalState != "sent" {
		t.Fatalf("execution=%+v err=%v", execution, err)
	}
	if backend.ApplyCalls() != 0 || len(executor.Snapshot()) != 1 || executor.Snapshot()[0].State != "sent" {
		t.Fatal("revoked authority crossed the effect boundary or lost its sent barrier")
	}
}
