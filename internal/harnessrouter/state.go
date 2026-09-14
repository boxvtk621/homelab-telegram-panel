package harnessrouter

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const (
	LegacyStateSchema     = 1
	ProjectionStateSchema = 2
	StateSchema           = LegacyStateSchema
	maximumStateBytes     = 1 << 20
	ModeEligible          = "eligible"
	ModeDraining          = "draining"
	ModeSealed            = "sealed"
	bootstrapOpID         = "bootstrap"
)

var operationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
var adapterVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NodeState is the durable per-node admission fence. IdentityEpoch and adapter
// version bind reopening to the exact healthy node observed after replacement.
type NodeState struct {
	Mode                 string `json:"mode"`
	StateVersion         int64  `json:"stateVersion"`
	Generation           int64  `json:"generation"`
	OperationID          string `json:"operationId,omitempty"`
	RegistrationRevision int64  `json:"registrationRevision,omitempty"`
	IdentityEpoch        int64  `json:"identityEpoch"`
	Compatibility        string `json:"compatibility,omitempty"`
	AdapterKind          string `json:"adapterKind"`
	AdapterVersion       string `json:"adapterVersion"`
}

// State is a private durable file. RegistryEnvelope contains only the signed
// routing projection (including endpoints and certificate pins), never a
// private key, browser session, command body or provider credential. Operator
// readback strips the envelope.
type State struct {
	Schema                int                  `json:"schema"`
	OwnerID               string               `json:"ownerId"`
	RegistryVersion       int64                `json:"registryVersion"`
	RegistrySHA256        string               `json:"registrySHA256"`
	RegistryOperationID   string               `json:"registryOperationId,omitempty"`
	RegistryRequestSHA256 string               `json:"registryRequestSHA256,omitempty"`
	RegistryEnvelope      json.RawMessage      `json:"registryEnvelope,omitempty"`
	Nodes                 map[string]NodeState `json:"nodes"`
}

type stateLock struct {
	file      *os.File
	path      string
	guardPath string
	info      os.FileInfo
	record    routerLockRecord
}

type routerLockRecord struct {
	SchemaID string `json:"schemaId"`
	LockID   string `json:"lockId"`
}

const routerLockSchemaID = "harness-router-lock-v1"

func ownerUID(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return int(stat.Uid), ok
}

func requirePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("invalid Harness Router state directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("Harness Router state directory must be private")
	}
	uid, uidOK := ownerUID(info)
	if !uidOK || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || uid != os.Geteuid() {
		return errors.New("Harness Router state directory must be private")
	}
	return nil
}

func requirePrivateRegular(path string) error {
	_, err := privateRegularInfo(path)
	return err
}

func privateRegularInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("Harness Router state file must be private")
	}
	uid, uidOK := ownerUID(info)
	if !uidOK || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || uid != os.Geteuid() {
		return nil, errors.New("Harness Router state file must be private")
	}
	return info, nil
}

func acquireStateLock(directory string, allowCreate bool) (*stateLock, error) {
	return acquireStateLockMode(directory, allowCreate, false)
}

func acquireStateLockMode(directory string, allowCreate, allowLegacyEmpty bool) (*stateLock, error) {
	if err := requirePrivateDirectory(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "router.lock")
	guardPath := path + ".guard"
	_, pathErr := os.Lstat(path)
	pathMissing := os.IsNotExist(pathErr)
	if pathErr != nil && !pathMissing || pathMissing && !allowCreate {
		return nil, errors.New("Harness Router state lock missing")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("cannot open Harness Router state lock")
	}
	pathInfo, err := privateRegularInfo(path)
	if err != nil {
		file.Close()
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		file.Close()
		return nil, errors.New("Harness Router state lock changed while opening")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("Harness Router state is already owned")
	}
	lock := &stateLock{file: file, path: path, guardPath: guardPath, info: pathInfo}
	guardInfo, guardErr := privateRegularInfo(guardPath)
	guardMissing := guardErr != nil && !existsPath(guardPath)
	if guardErr != nil && !guardMissing {
		lock.close()
		return nil, errors.New("Harness Router state lock guard invalid")
	}
	if guardMissing {
		initialize := allowCreate && pathMissing && pathInfo.Size() == 0
		upgradeLegacy := allowLegacyEmpty && !pathMissing && pathInfo.Size() == 0
		if !initialize && !upgradeLegacy {
			lock.close()
			return nil, errors.New("Harness Router state lock guard missing")
		}
		record, err := newRouterLockRecord()
		if err != nil || writeRouterLockRecord(file, record) != nil || os.Link(path, guardPath) != nil || syncDirectory(directory) != nil {
			lock.close()
			return nil, errors.New("Harness Router state lock initialization failed")
		}
		lock.record = record
		guardInfo, guardErr = privateRegularInfo(guardPath)
	} else {
		lock.record, err = readRouterLockRecord(file)
		if err != nil {
			lock.close()
			return nil, errors.New("Harness Router state lock invalid")
		}
	}
	openedInfo, openedErr := file.Stat()
	if guardErr != nil || openedErr != nil || !os.SameFile(pathInfo, guardInfo) || !os.SameFile(openedInfo, guardInfo) {
		lock.close()
		return nil, errors.New("Harness Router state lock guard changed")
	}
	return lock, nil
}

func (lock *stateLock) ensure() error {
	if lock == nil || lock.file == nil || lock.info == nil {
		return errors.New("Harness Router state lock lost")
	}
	current, err := os.Lstat(lock.path)
	if err != nil || !os.SameFile(current, lock.info) {
		return errors.New("Harness Router state lock lost")
	}
	guard, err := os.Lstat(lock.guardPath)
	if err != nil || !os.SameFile(guard, lock.info) {
		return errors.New("Harness Router state lock lost")
	}
	opened, err := lock.file.Stat()
	if err != nil || !os.SameFile(opened, lock.info) {
		return errors.New("Harness Router state lock lost")
	}
	if err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("Harness Router state lock lost")
	}
	if record, err := readRouterLockRecord(lock.file); err != nil || record != lock.record {
		return errors.New("Harness Router state lock lost")
	}
	return nil
}

func existsPath(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func newRouterLockRecord() (routerLockRecord, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return routerLockRecord{}, err
	}
	return routerLockRecord{SchemaID: routerLockSchemaID, LockID: hex.EncodeToString(value)}, nil
}

func writeRouterLockRecord(file *os.File, record routerLockRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := file.Truncate(0); err != nil {
		return err
	}
	if written, err := file.WriteAt(raw, 0); err != nil || written != len(raw) {
		return errors.New("short Router lock write")
	}
	return file.Sync()
}

func readRouterLockRecord(file *os.File) (routerLockRecord, error) {
	if file == nil {
		return routerLockRecord{}, errors.New("missing Router lock")
	}
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 16<<10 {
		return routerLockRecord{}, errors.New("invalid Router lock")
	}
	raw := make([]byte, info.Size())
	if read, err := file.ReadAt(raw, 0); err != nil && err != io.EOF || int64(read) != info.Size() || !strictjson.Valid(raw) {
		return routerLockRecord{}, errors.New("invalid Router lock")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record routerLockRecord
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.SchemaID != routerLockSchemaID || !sha256Hex.MatchString(record.LockID) {
		return routerLockRecord{}, errors.New("invalid Router lock")
	}
	return record, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	_ = directory.Close()
	return err
}

func (lock *stateLock) close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
	lock.file = nil
}

func decodeState(path string) (State, error) {
	pathInfo, err := privateRegularInfo(path)
	if err != nil {
		return State{}, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return State{}, errors.New("invalid Harness Router state")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		return State{}, errors.New("invalid Harness Router state")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximumStateBytes+1))
	currentInfo, currentErr := os.Lstat(path)
	if err != nil || len(raw) > maximumStateBytes || currentErr != nil || !os.SameFile(currentInfo, pathInfo) || !strictjson.Valid(raw) {
		return State{}, errors.New("invalid Harness Router state")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var state State
	if decoder.Decode(&state) != nil || decoder.Decode(new(any)) != io.EOF {
		return State{}, errors.New("invalid Harness Router state")
	}
	return state, nil
}

func validateState(state State, registry harnessclient.RoutingRegistry) error {
	dynamic := registry.SchemaID == harnessclient.RouterRegistrySchemaID
	if (state.Schema != LegacyStateSchema && state.Schema != ProjectionStateSchema) ||
		(state.Schema == LegacyStateSchema && dynamic) || (state.Schema == ProjectionStateSchema && !dynamic) ||
		state.OwnerID != registry.OwnerID || state.RegistryVersion != registry.RegistryVersion ||
		state.RegistrySHA256 != registry.ManifestSHA256 || !sha256Hex.MatchString(state.RegistrySHA256) ||
		state.Nodes == nil || len(state.Nodes) != len(registry.Nodes) {
		return errors.New("Harness Router state does not match signed registry")
	}
	if dynamic {
		if len(state.RegistryEnvelope) == 0 || !operationID.MatchString(state.RegistryOperationID) ||
			!sha256Hex.MatchString(state.RegistryRequestSHA256) {
			return errors.New("Harness Router projection metadata is invalid")
		}
	} else if len(state.RegistryEnvelope) != 0 || state.RegistryOperationID != "" || state.RegistryRequestSHA256 != "" {
		return errors.New("legacy Harness Router state contains projection metadata")
	}
	want := make(map[string]harnessclient.RoutingNode, len(registry.Nodes))
	for _, node := range registry.Nodes {
		if _, duplicate := want[node.NodeID]; duplicate {
			return errors.New("duplicate Harness Router node")
		}
		want[node.NodeID] = node
	}
	for nodeID, node := range state.Nodes {
		expected, present := want[nodeID]
		if !present || node.AdapterKind != expected.Adapter || node.StateVersion < 1 || node.StateVersion > harnessprotocol.MaximumSafeInteger ||
			node.Generation < 0 || node.Generation > harnessprotocol.MaximumSafeInteger || node.IdentityEpoch < 0 ||
			node.IdentityEpoch > harnessprotocol.MaximumSafeInteger {
			return errors.New("invalid Harness Router node state")
		}
		if dynamic {
			if node.RegistrationRevision != expected.RegistrationRevision || node.IdentityEpoch != expected.RegistrationEpoch ||
				node.Compatibility != expected.Compatibility {
				return errors.New("Harness Router node registration does not match projection")
			}
		} else if node.RegistrationRevision != 0 || node.Compatibility != "" {
			return errors.New("legacy Harness Router node contains projection registration")
		}
		switch node.Mode {
		case ModeEligible:
			if node.OperationID != "" || node.Generation < 1 || node.IdentityEpoch < 1 || !adapterVersion.MatchString(node.AdapterVersion) ||
				(dynamic && node.Compatibility != "compatible") {
				return errors.New("invalid eligible Harness Router node")
			}
		case ModeDraining, ModeSealed:
			if !operationID.MatchString(node.OperationID) {
				return errors.New("invalid Harness Router operation")
			}
			initial := node.Mode == ModeSealed && node.OperationID == bootstrapOpID && node.Generation == 0 && node.AdapterVersion == "" &&
				((!dynamic && node.IdentityEpoch == 0) || (dynamic && node.IdentityEpoch >= 1))
			registrationPending := dynamic && node.Mode == ModeSealed && node.Generation >= 0 && node.IdentityEpoch >= 1 && node.AdapterVersion == ""
			if !initial && !registrationPending && (node.Generation < 1 || node.IdentityEpoch < 1 || !adapterVersion.MatchString(node.AdapterVersion)) {
				return errors.New("invalid fenced Harness Router node")
			}
		default:
			return errors.New("invalid Harness Router mode")
		}
	}
	return nil
}

// writeState reports whether rename crossed the commit point. An error after
// that point is an ambiguous durability result: callers must expose the new
// in-memory fence and poison the process until a clean restart verifies disk.
func writeState(path string, state State, replacing bool) (bool, error) {
	return writeStateGuarded(path, state, replacing, nil)
}

func writeStateGuarded(path string, state State, replacing bool, guard func() error) (bool, error) {
	directory := filepath.Dir(path)
	if err := requirePrivateDirectory(directory); err != nil {
		return false, err
	}
	if replacing {
		if err := requirePrivateRegular(path); err != nil {
			return false, err
		}
	} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return false, errors.New("Harness Router state already exists")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return false, errors.New("cannot encode Harness Router state")
	}
	raw = append(raw, '\n')
	if len(raw) > maximumStateBytes {
		return false, errors.New("Harness Router state is too large")
	}
	temporary, err := os.CreateTemp(directory, ".router-state-")
	if err != nil {
		return false, errors.New("cannot create Harness Router state")
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
		return false, errors.New("cannot persist Harness Router state")
	}
	if guard != nil {
		if err := guard(); err != nil {
			return false, err
		}
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return false, errors.New("cannot replace Harness Router state")
	}
	dir, err := os.Open(directory)
	if err != nil {
		return true, errors.New("cannot sync Harness Router state directory")
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return true, errors.New("cannot sync Harness Router state directory")
	}
	return true, nil
}

func exactState(left, right State) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func bootstrapState(registry harnessclient.RoutingRegistry) State {
	state := State{Schema: LegacyStateSchema, OwnerID: registry.OwnerID, RegistryVersion: registry.RegistryVersion, RegistrySHA256: registry.ManifestSHA256, Nodes: map[string]NodeState{}}
	if registry.SchemaID == harnessclient.RouterRegistrySchemaID {
		state.Schema = ProjectionStateSchema
		state.RegistryOperationID = bootstrapOpID
		state.RegistryRequestSHA256 = registry.ManifestSHA256
	}
	for _, node := range registry.Nodes {
		state.Nodes[node.NodeID] = NodeState{
			Mode: ModeSealed, StateVersion: 1, OperationID: bootstrapOpID,
			RegistrationRevision: node.RegistrationRevision, IdentityEpoch: node.RegistrationEpoch,
			Compatibility: node.Compatibility, AdapterKind: node.Adapter,
		}
	}
	return state
}

// Bootstrap creates the first fail-closed state while the old Panel is stopped.
// It never contacts nodes and never overwrites an existing state file.
func Bootstrap(paths harnessclient.Paths, statePath string) error {
	if paths.Registry == "" || !filepath.IsAbs(statePath) || filepath.Clean(statePath) != statePath {
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
	lock, err := acquireStateLock(filepath.Dir(statePath), true)
	if err != nil {
		return err
	}
	defer lock.close()
	state := bootstrapState(registry)
	if state.Schema == ProjectionStateSchema {
		state.RegistryEnvelope = client.Envelope()
	}
	if err := validateState(state, registry); err != nil {
		return err
	}
	_, err = writeStateGuarded(statePath, state, false, lock.ensure)
	return err
}
