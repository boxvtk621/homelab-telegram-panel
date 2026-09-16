package model

import (
	"errors"
	"regexp"
	"slices"
	"time"
)

var sshHostKeyPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{20,64}$`)

const (
	HostUpsertSchemaID          = "agent-host-upsert-v1"
	HostSchemaID                = "agent-host-v1"
	HostPageSchemaID            = "agent-host-page-v1"
	HostSecretInputSchemaID     = "docker-secret-input-v1"
	HostSecretProvisionSchemaID = "docker-secret-provision-v1"
	HostProbeSchemaID           = "agent-host-probe-v1"
	HostObservationSchemaID     = "docker-host-observation-v1"
)

type HostSecretInput struct {
	SchemaID    string `json:"schemaId"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	PrivateKey  []byte `json:"privateKey"`
	Passphrase  []byte `json:"passphrase"`
	Payload     []byte `json:"payload"`
}

type HostSecretProvision struct {
	SchemaID      string `json:"schemaId"`
	OperationID   string `json:"operationId"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	CredentialRef string `json:"credentialRef"`
	Created       bool   `json:"-"`
}

type HostProbeRequest struct {
	SchemaID            string `json:"schemaId"`
	ExpectedHostVersion int64  `json:"expectedHostVersion"`
}

type HostUpsert struct {
	SchemaID               string `json:"schemaId"`
	HostID                 string `json:"hostId"`
	ExpectedHostVersion    int64  `json:"expectedHostVersion"`
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

type HostRecord struct {
	SchemaID               string   `json:"schemaId"`
	HostID                 string   `json:"hostId"`
	HostVersion            int64    `json:"hostVersion"`
	DisplayName            string   `json:"displayName"`
	Transport              string   `json:"transport"`
	TargetRef              string   `json:"targetRef"`
	CredentialRef          string   `json:"credentialRef"`
	RegistryCredentialRef  string   `json:"registryCredentialRef"`
	ExpectedHostKey        string   `json:"expectedHostKey"`
	DockerContextRef       string   `json:"dockerContextRef"`
	ExpectedIdentitySHA256 string   `json:"expectedIdentitySHA256"`
	HostPlatform           string   `json:"hostPlatform"`
	HostArchitecture       string   `json:"hostArchitecture"`
	ObservedAt             *string  `json:"observedAt"`
	Availability           string   `json:"availability"`
	FailureStage           string   `json:"failureStage"`
	FailureCode            string   `json:"failureCode"`
	NextAction             string   `json:"nextAction"`
	HostKeySHA256          string   `json:"hostKeySHA256"`
	DaemonID               string   `json:"daemonId"`
	ContextEndpoint        string   `json:"contextEndpoint"`
	EngineOS               string   `json:"engineOS"`
	Architecture           string   `json:"architecture"`
	APIVersion             string   `json:"apiVersion"`
	EngineVersion          string   `json:"engineVersion"`
	Capabilities           []string `json:"capabilities"`
	IdentitySHA256         string   `json:"identitySHA256"`
	RegistryAvailability   string   `json:"registryAvailability"`
}

type HostPage struct {
	SchemaID   string       `json:"schemaId"`
	Items      []HostRecord `json:"items"`
	NextCursor *string      `json:"nextCursor"`
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

func ValidateHostSecretInput(value HostSecretInput) error {
	if value.SchemaID != HostSecretInputSchemaID || !ValidUUID(value.OperationID) {
		return errors.New("invalid host secret input")
	}
	switch value.Kind {
	case "ssh":
		if len(value.PrivateKey) == 0 || len(value.PrivateKey) > 64<<10 || len(value.Passphrase) > 4<<10 || len(value.Payload) != 0 {
			return errors.New("invalid ssh secret input")
		}
	case "registry":
		if len(value.Payload) == 0 || len(value.Payload) > 64<<10 || len(value.PrivateKey) != 0 || len(value.Passphrase) != 0 {
			return errors.New("invalid registry secret input")
		}
	default:
		return errors.New("invalid host secret kind")
	}
	return nil
}

func ValidateHostSecretProvision(value HostSecretProvision, operationID string) error {
	if value.SchemaID != HostSecretProvisionSchemaID || value.OperationID != operationID || !ValidUUID(operationID) ||
		!slices.Contains([]string{"ssh", "registry"}, value.Kind) || value.Status != "provisioned" || !ValidActor(value.CredentialRef) {
		return errors.New("invalid host secret provision")
	}
	return nil
}

func ValidateHostProbeRequest(value HostProbeRequest) error {
	if value.SchemaID != HostProbeSchemaID || value.ExpectedHostVersion < 1 || value.ExpectedHostVersion > MaximumSafeInt {
		return errors.New("invalid host probe request")
	}
	return nil
}

func ValidateHostUpsert(value HostUpsert) error {
	if value.SchemaID != HostUpsertSchemaID || !ValidUUID(value.HostID) ||
		value.ExpectedHostVersion < 0 || value.ExpectedHostVersion >= MaximumSafeInt ||
		!validText(value.DisplayName, 120) || !ValidActor(value.TargetRef) ||
		!ValidActor(value.DockerContextRef) || !slices.Contains([]string{"local", "ssh"}, value.Transport) ||
		!slices.Contains([]string{"linux", "darwin", "windows"}, value.HostPlatform) ||
		!slices.Contains([]string{"amd64", "arm64"}, value.HostArchitecture) {
		return errors.New("invalid host descriptor")
	}
	for _, ref := range []string{value.CredentialRef, value.RegistryCredentialRef} {
		if ref != "" && !ValidActor(ref) {
			return errors.New("invalid host credential reference")
		}
	}
	if value.ExpectedIdentitySHA256 != "" && !sha256Pattern.MatchString(value.ExpectedIdentitySHA256) {
		return errors.New("invalid expected host identity")
	}
	if value.ExpectedHostKey != "" && !sshHostKeyPattern.MatchString(value.ExpectedHostKey) {
		return errors.New("invalid expected host key")
	}
	if value.Transport == "local" && (value.CredentialRef != "" || value.ExpectedHostKey != "") {
		return errors.New("invalid local host descriptor")
	}
	if value.Transport == "ssh" && value.CredentialRef == "" {
		return errors.New("invalid ssh host descriptor")
	}
	return nil
}

func ValidateHostObservation(value HostObservation) error {
	if value.SchemaID != HostObservationSchemaID || !ValidUUID(value.HostID) || value.HostVersion < 1 ||
		value.HostVersion > MaximumSafeInt || value.DockerContextRef == "" || !ValidActor(value.DockerContextRef) ||
		!slices.Contains([]string{"ready", "unavailable"}, value.Availability) || value.Capabilities == nil ||
		!slices.Contains([]string{"not_configured", "not_checked", "ready", "unavailable"}, value.RegistryAvailability) {
		return errors.New("invalid host observation")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.ObservedAt); err != nil {
		return errors.New("invalid host observation time")
	}
	if len(value.Capabilities) > 32 {
		return errors.New("invalid host capabilities")
	}
	seen := map[string]bool{}
	for _, capability := range value.Capabilities {
		if !ValidActor(capability) || seen[capability] {
			return errors.New("invalid host capabilities")
		}
		seen[capability] = true
	}
	if value.Availability == "ready" {
		if value.FailureStage != "" || value.FailureCode != "" || value.NextAction != "" ||
			!validObservationText(value.DaemonID, 128) || !sha256Pattern.MatchString(value.IdentitySHA256) ||
			!validObservationText(value.ContextEndpoint, 80) || value.EngineOS != "linux" ||
			!slices.Contains([]string{"amd64", "arm64"}, value.Architecture) ||
			!validObservationText(value.APIVersion, 32) || !validObservationText(value.EngineVersion, 64) {
			return errors.New("invalid ready host observation")
		}
	} else if !ValidActor(value.FailureStage) || !ValidActor(value.FailureCode) || !validText(value.NextAction, 300) || value.IdentitySHA256 != "" {
		return errors.New("invalid unavailable host observation")
	}
	if value.HostKeySHA256 != "" && !sshHostKeyPattern.MatchString(value.HostKeySHA256) {
		return errors.New("invalid host key observation")
	}
	for _, field := range []struct {
		value   string
		maximum int
	}{
		{value.ContextEndpoint, 80}, {value.DaemonID, 128}, {value.EngineOS, 160},
		{value.Architecture, 160}, {value.APIVersion, 32}, {value.EngineVersion, 64},
	} {
		if field.value != "" && !validObservationText(field.value, field.maximum) {
			return errors.New("invalid host observation field")
		}
	}
	return nil
}

func validObservationText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !containsUnsafeText(value)
}

func containsUnsafeText(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
