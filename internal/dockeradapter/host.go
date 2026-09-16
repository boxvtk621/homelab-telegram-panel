package dockeradapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	HostDescriptorSchemaID  = "docker-host-descriptor-v1"
	HostObservationSchemaID = "docker-host-observation-v1"
)

var (
	hostUUIDPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	refPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	fingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{20,64}$`)
)

// HostDescriptor is the safe Agent Service to adapter contract. TargetRef and
// credential refs resolve only inside the adapter; paths and secret bytes are
// deliberately absent.
type HostDescriptor struct {
	SchemaID               string `json:"schemaId"`
	HostID                 string `json:"hostId"`
	HostVersion            int64  `json:"hostVersion"`
	DisplayName            string `json:"displayName"`
	Transport              string `json:"transport"`
	TargetRef              string `json:"targetRef"`
	CredentialRef          string `json:"credentialRef"`
	RegistryCredentialRef  string `json:"registryCredentialRef"`
	ExpectedHostKey        string `json:"expectedHostKey"`
	DockerContextRef       string `json:"dockerContextRef"`
	ExpectedIdentitySHA256 string `json:"expectedIdentitySHA256"`
	HostPlatform           string `json:"hostPlatform"`
	HostArchitecture       string `json:"hostArchitecture"`
}

type HostObservation struct {
	SchemaID             string   `json:"schemaId"`
	HostID               string   `json:"hostId"`
	HostVersion          int64    `json:"hostVersion"`
	ObservedAt           string   `json:"observedAt"`
	Availability         string   `json:"availability"`
	FailureStage         string   `json:"failureStage"`
	FailureCode          string   `json:"failureCode"`
	NextAction           string   `json:"nextAction"`
	HostKeySHA256        string   `json:"hostKeySHA256"`
	DaemonID             string   `json:"daemonId"`
	DockerContextRef     string   `json:"dockerContextRef"`
	ContextEndpoint      string   `json:"contextEndpoint"`
	EngineOS             string   `json:"engineOS"`
	Architecture         string   `json:"architecture"`
	APIVersion           string   `json:"apiVersion"`
	EngineVersion        string   `json:"engineVersion"`
	Capabilities         []string `json:"capabilities"`
	IdentitySHA256       string   `json:"identitySHA256"`
	RegistryAvailability string   `json:"registryAvailability"`
}

type ConnectionIdentity struct {
	HostKeySHA256   string
	ContextEndpoint string
}

type probeFault struct {
	stage     string
	code      string
	next      string
	retryable bool
}

func (f *probeFault) Error() string { return f.code }

func ValidateHostDescriptor(value HostDescriptor) error {
	if value.SchemaID != HostDescriptorSchemaID || !hostUUIDPattern.MatchString(value.HostID) ||
		value.HostVersion < 1 || value.HostVersion > maximumSafeInt ||
		!validHostText(value.DisplayName, 120) || !refPattern.MatchString(value.TargetRef) ||
		!refPattern.MatchString(value.DockerContextRef) ||
		!slices.Contains([]string{"local", "ssh"}, value.Transport) ||
		!slices.Contains([]string{"linux", "darwin", "windows"}, value.HostPlatform) ||
		!slices.Contains([]string{"amd64", "arm64"}, value.HostArchitecture) {
		return errors.New("invalid host descriptor")
	}
	for _, ref := range []string{value.CredentialRef, value.RegistryCredentialRef} {
		if ref != "" && !refPattern.MatchString(ref) {
			return errors.New("invalid host descriptor")
		}
	}
	if value.ExpectedIdentitySHA256 != "" && !sha256Hex.MatchString(value.ExpectedIdentitySHA256) {
		return errors.New("invalid host descriptor")
	}
	if value.Transport == "local" {
		if value.CredentialRef != "" || value.ExpectedHostKey != "" {
			return errors.New("invalid local host descriptor")
		}
	} else if value.CredentialRef == "" || value.ExpectedHostKey != "" && !fingerprintPattern.MatchString(value.ExpectedHostKey) {
		return errors.New("invalid ssh host descriptor")
	}
	return nil
}

func validHostText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func failureObservation(now time.Time, descriptor HostDescriptor, fault *probeFault) HostObservation {
	return HostObservation{
		SchemaID: HostObservationSchemaID, HostID: descriptor.HostID, HostVersion: descriptor.HostVersion,
		ObservedAt: now.UTC().Format(time.RFC3339Nano), Availability: "unavailable",
		FailureStage: fault.stage, FailureCode: fault.code, NextAction: fault.next,
		DockerContextRef: descriptor.DockerContextRef, Capabilities: []string{}, RegistryAvailability: "not_checked",
	}
}

func identitySHA256(descriptor HostDescriptor, connection ConnectionIdentity, version engineVersion, info engineInfo) (string, error) {
	canonical := struct {
		Domain          string `json:"domain"`
		HostID          string `json:"hostId"`
		Transport       string `json:"transport"`
		TargetRef       string `json:"targetRef"`
		HostKey         string `json:"hostKey"`
		ContextRef      string `json:"contextRef"`
		ContextEndpoint string `json:"contextEndpoint"`
		DaemonID        string `json:"daemonId"`
		EngineOS        string `json:"engineOS"`
		Architecture    string `json:"architecture"`
		APIVersion      string `json:"apiVersion"`
		EngineVersion   string `json:"engineVersion"`
	}{
		Domain: "hl263-host-identity/v1", HostID: descriptor.HostID,
		Transport: descriptor.Transport, TargetRef: descriptor.TargetRef, HostKey: connection.HostKeySHA256,
		ContextRef: descriptor.DockerContextRef, ContextEndpoint: connection.ContextEndpoint,
		DaemonID: info.ID, EngineOS: info.OSType, Architecture: info.Architecture,
		APIVersion: version.APIVersion, EngineVersion: version.Version,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func PlanIdentityValid(expected string, observation HostObservation) bool {
	return sha256Hex.MatchString(expected) && observation.Availability == "ready" &&
		observation.IdentitySHA256 == expected && observation.FailureCode == ""
}
