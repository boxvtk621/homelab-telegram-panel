package dockeradapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

// Ten thousand contract-valid worst-case entries fit below this bound. The
// same limit applies before rename and on every readback/restart.
const maximumJournalBytes = 64 << 20

type processLock struct {
	file      *os.File
	path      string
	guardPath string
	info      os.FileInfo
	record    lockRecord
}

func privateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("invalid adapter state directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("adapter state directory unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || int(stat.Uid) != os.Geteuid() {
		return errors.New("adapter state directory is not private")
	}
	return nil
}

func privateRegular(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("adapter state file unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("adapter state file is not private")
	}
	return info, nil
}

func openProcessLock(path string, create bool) (*processLock, error) {
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, errors.New("adapter executor lock unavailable")
	}
	info, err := privateRegular(path)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, info) {
		_ = file.Close()
		return nil, errors.New("adapter executor lock changed while opening")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("adapter executor is already running")
	}
	return &processLock{file: file, path: path, guardPath: path + ".guard", info: info}, nil
}

// bindGuard anchors the locked inode under a second, never-replaced directory
// entry. Replacing the public lock path therefore cannot let a conforming
// replacement process acquire a different inode while this process is active.
// The guard may be created only with the first brand-new lock.
func (lock *processLock) bindGuard(create bool) error {
	if lock == nil || lock.file == nil {
		return ErrReconciliationRequired
	}
	_, statErr := os.Lstat(lock.guardPath)
	missing := os.IsNotExist(statErr)
	if statErr != nil && !missing {
		return ErrReconciliationRequired
	}
	var guard os.FileInfo
	var err error
	if !missing {
		guard, err = privateRegular(lock.guardPath)
	}
	if guard == nil {
		if !missing || !create {
			return ErrReconciliationRequired
		}
		if err := os.Link(lock.path, lock.guardPath); err != nil {
			return ErrReconciliationRequired
		}
		directory, err := os.Open(filepath.Dir(lock.path))
		if err != nil {
			return ErrReconciliationRequired
		}
		err = directory.Sync()
		_ = directory.Close()
		if err != nil {
			return ErrReconciliationRequired
		}
		guard, err = privateRegular(lock.guardPath)
	}
	opened, openedErr := lock.file.Stat()
	if err != nil || openedErr != nil || !os.SameFile(guard, lock.info) || !os.SameFile(opened, guard) {
		return ErrReconciliationRequired
	}
	return nil
}

func (lock *processLock) initialize(record lockRecord) error {
	if lock == nil || lock.file == nil || !validLockRecord(record) {
		return ErrReconciliationRequired
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return ErrReconciliationRequired
	}
	raw = append(raw, '\n')
	if err := lock.file.Truncate(0); err != nil {
		return ErrReconciliationRequired
	}
	if written, err := lock.file.WriteAt(raw, 0); err != nil || written != len(raw) {
		return ErrReconciliationRequired
	}
	if err := lock.file.Sync(); err != nil {
		return ErrReconciliationRequired
	}
	directory, err := os.Open(filepath.Dir(lock.path))
	if err != nil {
		return ErrReconciliationRequired
	}
	err = directory.Sync()
	_ = directory.Close()
	if err != nil {
		return ErrReconciliationRequired
	}
	lock.record = record
	return nil
}

func (lock *processLock) bind(record lockRecord) error {
	if lock == nil || lock.file == nil || !validLockRecord(record) {
		return ErrReconciliationRequired
	}
	observed, err := readLockRecord(lock.file)
	if err != nil || observed != record {
		return ErrReconciliationRequired
	}
	lock.record = record
	return nil
}

func (lock *processLock) updateJournalDigest(digest string) error {
	if !sha256Hex.MatchString(digest) || lock.ensure() != nil {
		return ErrLockLost
	}
	record := lock.record
	record.JournalSHA256 = digest
	return lock.initialize(record)
}

func validLockRecord(record lockRecord) bool {
	return record.SchemaID == lockSchemaID && validIdentity(record.DaemonID) &&
		validIdentity(record.InstanceID) && sha256Hex.MatchString(record.LockID) &&
		sha256Hex.MatchString(record.JournalSHA256)
}

func journalSHA256(value journalState) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func sameJournal(left, right journalState) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func journalHasUnresolvedSent(value journalState) bool {
	return len(value.Entries) > 0 && value.Entries[len(value.Entries)-1].State == "sent"
}

func sameEntry(left, right Entry) bool {
	return left.OperationID == right.OperationID && left.StepID == right.StepID && left.Generation == right.Generation &&
		left.RequestHash == right.RequestHash && slices.Equal(left.ResourceIDs, right.ResourceIDs) && left.State == right.State &&
		left.Outcome == right.Outcome && left.ReceiptID == right.ReceiptID && left.UpdatedAt == right.UpdatedAt
}

func sentPrecedesResolution(sent, resolved journalState) bool {
	if sent.SchemaID != resolved.SchemaID || sent.DaemonID != resolved.DaemonID || sent.InstanceID != resolved.InstanceID ||
		len(sent.Entries) == 0 || len(sent.Entries) != len(resolved.Entries) || !journalHasUnresolvedSent(sent) || journalHasUnresolvedSent(resolved) {
		return false
	}
	last := len(sent.Entries) - 1
	for index := 0; index < last; index++ {
		if !sameEntry(sent.Entries[index], resolved.Entries[index]) {
			return false
		}
	}
	before, after := sent.Entries[last], resolved.Entries[last]
	return before.OperationID == after.OperationID && before.StepID == after.StepID && before.Generation == after.Generation &&
		before.RequestHash == after.RequestHash && slices.Equal(before.ResourceIDs, after.ResourceIDs) &&
		before.Outcome == "" && before.ReceiptID == "" && after.State != "sent"
}

func readLockRecord(file *os.File) (lockRecord, error) {
	if file == nil {
		return lockRecord{}, ErrLockLost
	}
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 16<<10 {
		return lockRecord{}, ErrLockLost
	}
	raw := make([]byte, info.Size())
	if read, err := file.ReadAt(raw, 0); err != nil && err != io.EOF || int64(read) != info.Size() || !strictjson.Valid(raw) {
		return lockRecord{}, ErrLockLost
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record lockRecord
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF {
		return lockRecord{}, ErrLockLost
	}
	return record, nil
}

func (lock *processLock) ensure() error {
	if lock == nil || lock.file == nil {
		return ErrLockLost
	}
	current, err := os.Lstat(lock.path)
	if err != nil || !os.SameFile(current, lock.info) {
		return ErrLockLost
	}
	guard, err := os.Lstat(lock.guardPath)
	if err != nil || !os.SameFile(guard, lock.info) {
		return ErrLockLost
	}
	opened, err := lock.file.Stat()
	if err != nil || !os.SameFile(opened, lock.info) {
		return ErrLockLost
	}
	if err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrLockLost
	}
	observed, err := readLockRecord(lock.file)
	if err != nil || observed != lock.record {
		return ErrLockLost
	}
	return nil
}

func (lock *processLock) close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
	lock.file = nil
}

func fileNames(directory, daemonID, instanceID string) (string, string, string) {
	key := fixtureFileKey(daemonID, instanceID)
	return filepath.Join(directory, key+".lock"),
		filepath.Join(directory, key+".anchor.json"),
		filepath.Join(directory, key+".journal.json")
}

func journalGuardPath(journalPath string) string { return journalPath + ".guard" }

func syncStateDirectory(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return ErrReconciliationRequired
	}
	err = directory.Sync()
	_ = directory.Close()
	if err != nil {
		return ErrReconciliationRequired
	}
	return nil
}

func (executor *Executor) syncJournalGuard() error {
	if executor == nil || executor.lock == nil || executor.lock.ensure() != nil {
		return ErrReconciliationRequired
	}
	journalInfo, err := privateRegular(executor.journalPath)
	if err != nil {
		return ErrReconciliationRequired
	}
	guardPath := journalGuardPath(executor.journalPath)
	guardInfo, guardErr := privateRegular(guardPath)
	guardMissing := guardErr != nil && !exists(guardPath)
	if guardErr != nil && !guardMissing {
		return ErrReconciliationRequired
	}
	if journalHasUnresolvedSent(executor.state) {
		if guardMissing {
			if err := os.Link(executor.journalPath, guardPath); err != nil || syncStateDirectory(guardPath) != nil {
				return ErrReconciliationRequired
			}
			guardInfo, guardErr = privateRegular(guardPath)
		}
		if guardErr != nil || !os.SameFile(journalInfo, guardInfo) {
			return ErrReconciliationRequired
		}
		return executor.lock.ensure()
	}
	if guardMissing {
		return executor.lock.ensure()
	}
	var sent journalState
	if _, err := readExactJSONInfo(guardPath, maximumJournalBytes, &sent); err != nil ||
		validateJournal(sent, executor.daemonID, executor.instanceID) != nil || !sentPrecedesResolution(sent, executor.state) {
		return ErrReconciliationRequired
	}
	currentGuard, err := os.Lstat(guardPath)
	if err != nil || !os.SameFile(currentGuard, guardInfo) || os.Remove(guardPath) != nil || syncStateDirectory(guardPath) != nil {
		return ErrReconciliationRequired
	}
	return executor.lock.ensure()
}

func (executor *Executor) attestSentJournal() error {
	if executor == nil || executor.lock == nil || !journalHasUnresolvedSent(executor.state) {
		return ErrReconciliationRequired
	}
	if err := executor.lock.ensure(); err != nil {
		return err
	}
	var observed journalState
	journalInfo, err := readExactJSONInfo(executor.journalPath, maximumJournalBytes, &observed)
	if err != nil || validateJournal(observed, executor.daemonID, executor.instanceID) != nil || !sameJournal(observed, executor.state) {
		return ErrReconciliationRequired
	}
	digest, err := journalSHA256(observed)
	if err != nil || digest != executor.lock.record.JournalSHA256 {
		return ErrReconciliationRequired
	}
	guardInfo, err := privateRegular(journalGuardPath(executor.journalPath))
	if err != nil || !os.SameFile(journalInfo, guardInfo) {
		return ErrReconciliationRequired
	}
	if err := executor.lock.ensure(); err != nil {
		return err
	}
	return nil
}

func fixtureFileKey(daemonID, instanceID string) string {
	digest := sha256.Sum256([]byte(daemonID + "\x00" + instanceID))
	return "adapter-" + hex.EncodeToString(digest[:16])
}

func readExactJSON(path string, maximum int64, target any) error {
	_, err := readExactJSONInfo(path, maximum, target)
	return err
}

func readExactJSONInfo(path string, maximum int64, target any) (os.FileInfo, error) {
	pathInfo, err := privateRegular(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("adapter state unavailable")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, openedInfo) {
		return nil, errors.New("adapter state changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(raw)) > maximum || !strictjson.Valid(raw) {
		return nil, errors.New("adapter state unavailable")
	}
	currentInfo, err := privateRegular(path)
	if err != nil || !os.SameFile(pathInfo, currentInfo) || !os.SameFile(openedInfo, currentInfo) {
		return nil, errors.New("adapter state changed while reading")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid adapter state")
	}
	return pathInfo, nil
}

// replaceJSON writes, fsyncs, atomically renames and fsyncs the containing
// directory. A successful return is the journal's durability boundary.
func replaceJSON(path string, value any, replacing bool) error {
	directory := filepath.Dir(path)
	if err := privateDirectory(directory); err != nil {
		return err
	}
	if replacing {
		if _, err := privateRegular(path); err != nil {
			return err
		}
	} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return errors.New("adapter state already exists")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return errors.New("cannot encode adapter state")
	}
	raw = append(raw, '\n')
	if len(raw) > maximumJournalBytes {
		return errors.New("adapter state size exceeded")
	}
	temporary, err := os.CreateTemp(directory, ".adapter-state-")
	if err != nil {
		return errors.New("cannot create adapter state")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(raw)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("cannot persist adapter state")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("cannot replace adapter state")
	}
	dir, err := os.Open(directory)
	if err != nil {
		return errors.New("cannot sync adapter state directory")
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return errors.New("cannot sync adapter state directory")
	}
	return nil
}
