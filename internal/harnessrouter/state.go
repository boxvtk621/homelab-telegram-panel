package harnessrouter

import (
	"bytes"
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
	StateSchema   = 1
	ModeEligible  = "eligible"
	ModeDraining  = "draining"
	ModeSealed    = "sealed"
	bootstrapOpID = "bootstrap"
)

var operationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
var adapterVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NodeState is the durable per-node admission fence. IdentityEpoch and adapter
// version bind reopening to the exact healthy node observed after replacement.
type NodeState struct {
	Mode           string `json:"mode"`
	StateVersion   int64  `json:"stateVersion"`
	Generation     int64  `json:"generation"`
	OperationID    string `json:"operationId,omitempty"`
	IdentityEpoch  int64  `json:"identityEpoch"`
	AdapterKind    string `json:"adapterKind"`
	AdapterVersion string `json:"adapterVersion"`
}

// State is intentionally safe for the private operator socket. It contains no
// endpoint, certificate, browser session, command body or provider credential.
type State struct {
	Schema          int                  `json:"schema"`
	OwnerID         string               `json:"ownerId"`
	RegistryVersion int64                `json:"registryVersion"`
	RegistrySHA256  string               `json:"registrySHA256"`
	Nodes           map[string]NodeState `json:"nodes"`
}

type stateLock struct {
	file *os.File
}

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
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("Harness Router state file must be private")
	}
	uid, uidOK := ownerUID(info)
	if !uidOK || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || uid != os.Geteuid() {
		return errors.New("Harness Router state file must be private")
	}
	return nil
}

func acquireStateLock(directory string) (*stateLock, error) {
	if err := requirePrivateDirectory(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "router.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("cannot open Harness Router state lock")
	}
	if err := requirePrivateRegular(path); err != nil {
		file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("Harness Router state is already owned")
	}
	return &stateLock{file: file}, nil
}

func (lock *stateLock) close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
}

func decodeState(path string) (State, error) {
	if err := requirePrivateRegular(path); err != nil {
		return State{}, err
	}
	info, _ := os.Stat(path)
	if info.Size() > 256<<10 {
		return State{}, errors.New("Harness Router state is too large")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strictjson.Valid(raw) {
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
	if state.Schema != StateSchema || state.OwnerID != registry.OwnerID || state.RegistryVersion != registry.RegistryVersion ||
		state.RegistrySHA256 != registry.ManifestSHA256 || !sha256Hex.MatchString(state.RegistrySHA256) ||
		state.Nodes == nil || len(state.Nodes) != len(registry.Nodes) {
		return errors.New("Harness Router state does not match signed registry")
	}
	want := make(map[string]string, len(registry.Nodes))
	for _, node := range registry.Nodes {
		if _, duplicate := want[node.NodeID]; duplicate {
			return errors.New("duplicate Harness Router node")
		}
		want[node.NodeID] = node.Adapter
	}
	for nodeID, node := range state.Nodes {
		if want[nodeID] == "" || node.AdapterKind != want[nodeID] || node.StateVersion < 1 || node.StateVersion > harnessprotocol.MaximumSafeInteger ||
			node.Generation < 0 || node.Generation > harnessprotocol.MaximumSafeInteger || node.IdentityEpoch < 0 ||
			node.IdentityEpoch > harnessprotocol.MaximumSafeInteger {
			return errors.New("invalid Harness Router node state")
		}
		switch node.Mode {
		case ModeEligible:
			if node.OperationID != "" || node.Generation < 1 || node.IdentityEpoch < 1 || !adapterVersion.MatchString(node.AdapterVersion) {
				return errors.New("invalid eligible Harness Router node")
			}
		case ModeDraining, ModeSealed:
			if !operationID.MatchString(node.OperationID) {
				return errors.New("invalid Harness Router operation")
			}
			initial := node.Mode == ModeSealed && node.OperationID == bootstrapOpID && node.Generation == 0 && node.IdentityEpoch == 0 && node.AdapterVersion == ""
			if !initial && (node.Generation < 1 || node.IdentityEpoch < 1 || !adapterVersion.MatchString(node.AdapterVersion)) {
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

func bootstrapState(registry harnessclient.RoutingRegistry) State {
	state := State{Schema: StateSchema, OwnerID: registry.OwnerID, RegistryVersion: registry.RegistryVersion, RegistrySHA256: registry.ManifestSHA256, Nodes: map[string]NodeState{}}
	for _, node := range registry.Nodes {
		state.Nodes[node.NodeID] = NodeState{Mode: ModeSealed, StateVersion: 1, OperationID: bootstrapOpID, AdapterKind: node.Adapter}
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
	if registry.OwnerID == "" || len(registry.Nodes) == 0 {
		return errors.New("managed Harness Router requires at least one node")
	}
	lock, err := acquireStateLock(filepath.Dir(statePath))
	if err != nil {
		return err
	}
	defer lock.close()
	state := bootstrapState(registry)
	if err := validateState(state, registry); err != nil {
		return err
	}
	_, err = writeState(statePath, state, false)
	return err
}
