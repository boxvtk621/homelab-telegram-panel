// Package harnesstunnel defines the private Router-to-adapter byte tunnel.
// The browser never supplies a host, port, socket, or credential reference.
package harnesstunnel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	SchemaID       string            `json:"schemaId"`
	OwnerID        string            `json:"ownerId"`
	RegistrySHA256 string            `json:"registrySHA256"`
	Nodes          []EndpointBinding `json:"nodes"`
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
		b.HostVersion < 1 || b.HostVersion > 1<<53-1 ||
		b.RuntimeGeneration < 1 || b.RuntimeGeneration > 1<<53-1 ||
		!refPattern.MatchString(b.TargetRef) || !refPattern.MatchString(b.DockerContextRef) ||
		!sha256Pattern.MatchString(b.ExpectedHostIdentitySHA256) || !containerIDPattern.MatchString(b.ContainerID) ||
		!slices.Contains([]string{"linux", "darwin", "windows"}, b.HostPlatform) ||
		!slices.Contains([]string{"amd64", "arm64"}, b.HostArchitecture) || validateAddress(b.Transport, b.Address) != nil {
		return errors.New("invalid tunnel endpoint binding")
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

func validateAddress(transport, value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" || strings.ContainsAny(value, "\x00\r\n \t") {
		return errors.New("invalid endpoint address")
	}
	ip := net.ParseIP(host)
	if ip == nil || transport == "ssh" && !ip.Equal(net.ParseIP("127.0.0.1")) || transport == "local" && !ip.IsPrivate() && !ip.IsLoopback() {
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
