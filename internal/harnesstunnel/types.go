// Package harnesstunnel defines the private Router-to-adapter byte tunnel.
// The browser never supplies a host, port, socket, or credential reference.
package harnesstunnel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const (
	BindingSchemaID = "harness-tunnel-bindings-v1"
	RequestSchemaID = "harness-tunnel-dial-v1"
	ReplySchemaID   = "harness-tunnel-dial-reply-v1"
	MaximumFrame    = 16 << 10
	MaximumSetup    = 30 * time.Second
)

type Purpose string

const (
	PurposeCommand       Purpose = "command"
	PurposeResponse      Purpose = "response"
	PurposeEvents        Purpose = "events"
	PurposeHealth        Purpose = "health"
	PurposeAdmin         Purpose = "admin"
	PurposeReplicaExport Purpose = "replica_export"
	PurposeReplicaImport Purpose = "replica_import"
)

func (p Purpose) Valid() bool {
	return slices.Contains([]Purpose{PurposeCommand, PurposeResponse, PurposeEvents, PurposeHealth, PurposeAdmin, PurposeReplicaExport, PurposeReplicaImport}, p)
}

func (p Purpose) Stream() bool {
	return p == PurposeEvents || p == PurposeReplicaExport || p == PurposeReplicaImport
}

// EndpointBinding is an operator-owned exact endpoint projection. Address and
// credential references remain adapter-side; DialRequest deliberately omits
// them. A later lifecycle stage may produce this record after Docker inspect.
type EndpointBinding struct {
	Kind                       string `json:"kind,omitempty"`
	NodeID                     string `json:"nodeId"`
	RegistrationRevision       int64  `json:"registrationRevision"`
	RegistrationEpoch          int64  `json:"registrationEpoch"`
	EndpointRevision           int64  `json:"endpointRevision"`
	HostID                     string `json:"hostId"`
	HostVersion                int64  `json:"hostVersion"`
	Transport                  string `json:"transport"`
	TargetRef                  string `json:"targetRef"`
	CredentialRef              string `json:"credentialRef"`
	DockerContextRef           string `json:"dockerContextRef"`
	ExpectedHostKey            string `json:"expectedHostKey"`
	ExpectedHostIdentitySHA256 string `json:"expectedHostIdentitySHA256"`
	HostPlatform               string `json:"hostPlatform"`
	HostArchitecture           string `json:"hostArchitecture"`
	ContainerID                string `json:"containerId"`
	RuntimeGeneration          int64  `json:"runtimeGeneration"`
	Address                    string `json:"address"`
}

type BindingManifest struct {
	SchemaID                string            `json:"schemaId"`
	OwnerID                 string            `json:"ownerId"`
	RegistrySHA256          string            `json:"registrySHA256"`
	AcceptedRegistrySHA256s []string          `json:"acceptedRegistrySHA256s,omitempty"`
	Nodes                   []EndpointBinding `json:"nodes"`
}

type DialRequest struct {
	SchemaID             string  `json:"schemaId"`
	OwnerID              string  `json:"ownerId"`
	NodeID               string  `json:"nodeId"`
	RegistrationRevision int64   `json:"registrationRevision"`
	RegistrationEpoch    int64   `json:"registrationEpoch"`
	EndpointRevision     int64   `json:"endpointRevision"`
	Purpose              Purpose `json:"purpose"`
	DeadlineUnixMilli    int64   `json:"deadlineUnixMilli"`
}

type DialReply struct {
	SchemaID string `json:"schemaId"`
	Status   string `json:"status"`
	Code     string `json:"code"`
}

var (
	uuidPattern        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	refPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostKeyPattern     = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{20,64}$`)
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (m BindingManifest) Validate() error {
	if m.SchemaID != BindingSchemaID || !refPattern.MatchString(m.OwnerID) || !sha256Pattern.MatchString(m.RegistrySHA256) || len(m.Nodes) == 0 || len(m.Nodes) > 1000 {
		return errors.New("invalid tunnel binding manifest")
	}
	seenNodes := map[string]bool{}
	seenEndpoints := map[string]bool{}
	seenRegistry := map[string]bool{}
	if len(m.AcceptedRegistrySHA256s) > 3 {
		return errors.New("invalid tunnel binding manifest")
	}
	for _, digest := range m.AcceptedRegistrySHA256s {
		if !sha256Pattern.MatchString(digest) || seenRegistry[digest] {
			return errors.New("invalid tunnel binding manifest")
		}
		seenRegistry[digest] = true
	}
	if len(m.AcceptedRegistrySHA256s) > 0 && !seenRegistry[m.RegistrySHA256] {
		return errors.New("invalid tunnel binding manifest")
	}
	for _, binding := range m.Nodes {
		if err := binding.Validate(); err != nil || seenNodes[binding.NodeID] {
			return errors.New("invalid tunnel endpoint binding")
		}
		seenNodes[binding.NodeID] = true
		endpointKey := binding.HostID + "\x00" + binding.Address
		if seenEndpoints[endpointKey] {
			return errors.New("duplicate tunnel endpoint binding")
		}
		seenEndpoints[endpointKey] = true
	}
	return nil
}

func (b EndpointBinding) Validate() error {
	if !uuidPattern.MatchString(b.NodeID) || !uuidPattern.MatchString(b.HostID) ||
		b.RegistrationRevision < 1 || b.RegistrationRevision > 1<<53-1 ||
		b.RegistrationEpoch < 1 || b.RegistrationEpoch > 1<<53-1 ||
		b.EndpointRevision < 1 || b.EndpointRevision > 1<<53-1 ||
		b.HostVersion < 1 || b.HostVersion > 1<<53-1 || !refPattern.MatchString(b.TargetRef) ||
		validateAddress(b.Transport, b.Address) != nil {
		return errors.New("invalid tunnel endpoint binding")
	}
	external := b.Kind == "external"
	if b.Kind != "" && !external {
		return errors.New("invalid tunnel endpoint binding")
	}
	if external {
		if b.DockerContextRef != "" || b.ExpectedHostIdentitySHA256 != "" || b.HostPlatform != "" ||
			b.HostArchitecture != "" || b.ContainerID != "" || b.RuntimeGeneration != 0 {
			return errors.New("external tunnel binding contains managed runtime identity")
		}
	} else if b.RuntimeGeneration < 1 || b.RuntimeGeneration > 1<<53-1 ||
		!refPattern.MatchString(b.DockerContextRef) || !sha256Pattern.MatchString(b.ExpectedHostIdentitySHA256) ||
		!containerIDPattern.MatchString(b.ContainerID) || !slices.Contains([]string{"linux", "darwin", "windows"}, b.HostPlatform) ||
		!slices.Contains([]string{"amd64", "arm64"}, b.HostArchitecture) {
		return errors.New("invalid managed tunnel endpoint binding")
	}
	switch b.Transport {
	case "local":
		if b.CredentialRef != "" || b.ExpectedHostKey != "" {
			return errors.New("invalid local tunnel binding")
		}
	case "ssh":
		if !refPattern.MatchString(b.CredentialRef) || !hostKeyPattern.MatchString(b.ExpectedHostKey) {
			return errors.New("invalid ssh tunnel binding")
		}
	default:
		return errors.New("invalid tunnel transport")
	}
	return nil
}

func (b EndpointBinding) External() bool { return b.Kind == "external" }

func BindingSHA256(binding EndpointBinding) (string, error) {
	if binding.Validate() != nil {
		return "", errors.New("invalid tunnel endpoint binding")
	}
	canonical, err := json.Marshal(binding)
	if err != nil {
		return "", errors.New("cannot encode tunnel endpoint binding")
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func (m BindingManifest) AcceptsRegistry(digest string) bool {
	if len(m.AcceptedRegistrySHA256s) == 0 {
		return digest == m.RegistrySHA256
	}
	return slices.Contains(m.AcceptedRegistrySHA256s, digest)
}

func validateAddress(transport, value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" || strings.ContainsAny(value, "\x00\r\n \t") {
		return errors.New("invalid endpoint address")
	}
	ip := net.ParseIP(host)
	if ip == nil || transport == "ssh" && !ip.IsLoopback() || transport == "local" && !ip.IsPrivate() && !ip.IsLoopback() {
		return errors.New("endpoint address is not private")
	}
	if endpoint, err := net.ResolveTCPAddr("tcp", value); err != nil || endpoint.Port < 1 || endpoint.Port > 65535 {
		return errors.New("invalid endpoint port")
	}
	return nil
}

func (m BindingManifest) Binding(nodeID string) (EndpointBinding, bool) {
	for _, binding := range m.Nodes {
		if binding.NodeID == nodeID {
			return binding, true
		}
	}
	return EndpointBinding{}, false
}

func (r DialRequest) Validate(now time.Time) error {
	deadline := time.UnixMilli(r.DeadlineUnixMilli)
	if r.SchemaID != RequestSchemaID || !refPattern.MatchString(r.OwnerID) || !uuidPattern.MatchString(r.NodeID) ||
		r.RegistrationRevision < 1 || r.RegistrationRevision > 1<<53-1 ||
		r.RegistrationEpoch < 1 || r.RegistrationEpoch > 1<<53-1 ||
		r.EndpointRevision < 1 || r.EndpointRevision > 1<<53-1 || !r.Purpose.Valid() ||
		!deadline.After(now) || deadline.After(now.Add(MaximumSetup)) {
		return errors.New("invalid tunnel dial request")
	}
	return nil
}

func (r DialRequest) Matches(owner string, binding EndpointBinding) bool {
	return r.OwnerID == owner && r.NodeID == binding.NodeID &&
		r.RegistrationRevision == binding.RegistrationRevision &&
		r.RegistrationEpoch == binding.RegistrationEpoch &&
		r.EndpointRevision == binding.EndpointRevision
}

func LoadManifest(path string) (BindingManifest, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return BindingManifest{}, errors.New("invalid tunnel binding path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > 512<<10 {
		return BindingManifest{}, errors.New("private tunnel bindings required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return BindingManifest{}, errors.New("private tunnel bindings required")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return BindingManifest{}, errors.New("private tunnel bindings required")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return BindingManifest{}, errors.New("private tunnel bindings required")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (512<<10)+1))
	if err != nil || len(raw) == 0 || len(raw) > 512<<10 || !strictjson.Valid(raw) {
		return BindingManifest{}, errors.New("invalid tunnel bindings")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest BindingManifest
	if decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.Validate() != nil {
		return BindingManifest{}, errors.New("invalid tunnel bindings")
	}
	return manifest, nil
}

// StageExternalBinding adds one inert exact endpoint and allows both the
// current and candidate signed registry hashes. The new binding is unreachable
// until Router installs the candidate projection.
func StageExternalBinding(path, owner, currentRegistrySHA256, candidateRegistrySHA256 string, binding EndpointBinding) error {
	if binding.Kind != "external" || binding.Validate() != nil || !refPattern.MatchString(owner) ||
		!sha256Pattern.MatchString(currentRegistrySHA256) || !sha256Pattern.MatchString(candidateRegistrySHA256) {
		return errors.New("invalid external tunnel staging request")
	}
	lock, err := lockBindingManifest(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	manifest, err := LoadManifest(path)
	if err != nil || manifest.OwnerID != owner || !manifest.AcceptsRegistry(currentRegistrySHA256) {
		return errors.New("stale tunnel bindings")
	}
	if existing, present := manifest.Binding(binding.NodeID); present {
		left, _ := json.Marshal(existing)
		right, _ := json.Marshal(binding)
		if !bytes.Equal(left, right) {
			return errors.New("tunnel node binding conflict")
		}
	} else {
		manifest.Nodes = append(manifest.Nodes, binding)
		sort.Slice(manifest.Nodes, func(left, right int) bool { return manifest.Nodes[left].NodeID < manifest.Nodes[right].NodeID })
	}
	manifest.AcceptedRegistrySHA256s = []string{manifest.RegistrySHA256}
	for _, digest := range []string{currentRegistrySHA256, candidateRegistrySHA256} {
		if !slices.Contains(manifest.AcceptedRegistrySHA256s, digest) {
			manifest.AcceptedRegistrySHA256s = append(manifest.AcceptedRegistrySHA256s, digest)
		}
	}
	return persistBindingManifest(path, manifest)
}

// UnstageExternalBinding removes only the exact inert candidate after Router
// has rejected the registry CAS. It is idempotent and never removes a binding
// that differs from the durable enrollment intent.
func UnstageExternalBinding(path, owner, currentRegistrySHA256, candidateRegistrySHA256 string, binding EndpointBinding) error {
	if binding.Kind != "external" || binding.Validate() != nil || !refPattern.MatchString(owner) ||
		!sha256Pattern.MatchString(currentRegistrySHA256) || !sha256Pattern.MatchString(candidateRegistrySHA256) {
		return errors.New("invalid external tunnel unstaging request")
	}
	lock, err := lockBindingManifest(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	manifest, err := LoadManifest(path)
	if err != nil || manifest.OwnerID != owner || !manifest.AcceptsRegistry(currentRegistrySHA256) {
		return errors.New("stale tunnel bindings")
	}
	candidateAccepted := manifest.AcceptsRegistry(candidateRegistrySHA256)
	existing, present := manifest.Binding(binding.NodeID)
	if present {
		left, _ := json.Marshal(existing)
		right, _ := json.Marshal(binding)
		if !bytes.Equal(left, right) {
			return errors.New("tunnel node binding conflict")
		}
		manifest.Nodes = slices.DeleteFunc(manifest.Nodes, func(value EndpointBinding) bool { return value.NodeID == binding.NodeID })
	}
	if !present && !candidateAccepted {
		return nil
	}
	manifest.AcceptedRegistrySHA256s = []string{manifest.RegistrySHA256}
	if currentRegistrySHA256 != manifest.RegistrySHA256 {
		manifest.AcceptedRegistrySHA256s = append(manifest.AcceptedRegistrySHA256s, currentRegistrySHA256)
	}
	return persistBindingManifest(path, manifest)
}

func lockBindingManifest(path string) (*os.File, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("cannot lock tunnel bindings")
	}
	lockInfo, lockErr := lock.Stat()
	if lockErr != nil {
		_ = lock.Close()
		return nil, errors.New("cannot lock tunnel bindings")
	}
	lockStat, lockOwnerOK := lockInfo.Sys().(*syscall.Stat_t)
	if !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 || !lockOwnerOK || int(lockStat.Uid) != os.Geteuid() ||
		syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) != nil {
		_ = lock.Close()
		return nil, errors.New("cannot lock tunnel bindings")
	}
	return lock, nil
}

func persistBindingManifest(path string, manifest BindingManifest) error {
	if manifest.Validate() != nil {
		return errors.New("invalid staged tunnel bindings")
	}
	raw, err := json.Marshal(manifest)
	if err != nil || len(raw) > 512<<10 {
		return errors.New("cannot encode tunnel bindings")
	}
	raw = append(raw, '\n')
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".harness-bindings-")
	if err != nil {
		return errors.New("cannot persist tunnel bindings")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(raw)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil || os.Rename(temporaryPath, path) != nil {
		return errors.New("cannot persist tunnel bindings")
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return errors.New("cannot sync tunnel bindings")
	}
	err = directoryFile.Sync()
	_ = directoryFile.Close()
	if err != nil {
		return errors.New("cannot sync tunnel bindings")
	}
	return nil
}
