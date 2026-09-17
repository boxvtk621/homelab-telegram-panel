package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const (
	ExternalEnrollmentSchemaID     = "external-harness-enrollment-v1"
	ExternalEnrollmentPlanSchemaID = "external-harness-enrollment-plan-v1"
)

type ExternalEnrollmentRequest struct {
	SchemaID            string `json:"schemaId"`
	OperationID         string `json:"operationId"`
	HostID              string `json:"hostId"`
	ExpectedHostVersion int64  `json:"expectedHostVersion"`
	NodeID              string `json:"nodeId"`
	Name                string `json:"name"`
	Adapter             string `json:"adapter"`
	EndpointURI         string `json:"endpointUri"`
	CertificateSHA256   string `json:"certificateSHA256"`
}

type ExternalEndpointBinding struct {
	Kind                       string `json:"kind"`
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

type ExternalEnrollmentPlan struct {
	SchemaID    string                   `json:"schemaId"`
	OperationID string                   `json:"operationId"`
	RequestHash string                   `json:"requestHash"`
	NodeID      string                   `json:"nodeId"`
	Registry    json.RawMessage          `json:"registry"`
	Binding     *ExternalEndpointBinding `json:"binding"`
	Status      RegistryOperationStatus  `json:"status"`
}

func externalBindingValid(value ExternalEndpointBinding) error {
	_, err := ExternalBindingSHA256(value)
	return err
}

func ExternalBindingSHA256(value ExternalEndpointBinding) (string, error) {
	if value.Kind != "external" || !ValidUUID(value.NodeID) || value.RegistrationRevision != 1 ||
		value.RegistrationEpoch != 1 || value.EndpointRevision != 1 || !ValidUUID(value.HostID) ||
		value.HostVersion < 1 || value.HostVersion > MaximumSafeInt || !ValidActor(value.TargetRef) ||
		(value.Transport != "local" && value.Transport != "ssh") || value.DockerContextRef != "" ||
		value.ExpectedHostIdentitySHA256 != "" || value.HostPlatform != "" || value.HostArchitecture != "" ||
		value.ContainerID != "" || value.RuntimeGeneration != 0 {
		return "", errors.New("invalid external endpoint binding")
	}
	if value.Transport == "local" && (value.CredentialRef != "" || value.ExpectedHostKey != "") ||
		value.Transport == "ssh" && (!ValidActor(value.CredentialRef) || !sshHostKeyPattern.MatchString(value.ExpectedHostKey)) {
		return "", errors.New("invalid external endpoint binding")
	}
	_, address, err := ExternalEndpoint("https://"+value.Address, value.Transport)
	if err != nil || address != value.Address {
		return "", errors.New("invalid external endpoint binding")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func ValidateExternalEnrollmentRequest(value ExternalEnrollmentRequest) error {
	if value.SchemaID != ExternalEnrollmentSchemaID || !ValidActor(value.OperationID) || !ValidUUID(value.HostID) ||
		value.ExpectedHostVersion < 1 || value.ExpectedHostVersion > MaximumSafeInt || !ValidUUID(value.NodeID) ||
		!validText(value.Name, 200) || (value.Adapter != "cursor" && value.Adapter != "codex") ||
		!sha256Pattern.MatchString(value.CertificateSHA256) || len(value.EndpointURI) == 0 || len(value.EndpointURI) > 512 {
		return errors.New("invalid external Harness enrollment")
	}
	return nil
}

func ExternalEnrollmentRequestHash(value ExternalEnrollmentRequest) (string, error) {
	if err := ValidateExternalEnrollmentRequest(value); err != nil {
		return "", err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// ExternalEndpoint accepts only a canonical HTTPS URI with an explicit private
// IP literal. Hostnames are rejected so DNS rebinding cannot change the target
// after admission; SSH endpoints are restricted to the remote loopback.
func ExternalEndpoint(value, transport string) (string, string, error) {
	if !strings.HasPrefix(value, "https://") || strings.ContainsAny(value, "\x00\r\n \t") {
		return "", "", errors.New("invalid external Harness URI")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawPath != "" || parsed.Path != "" || parsed.Hostname() == "" || parsed.Port() == "" ||
		strings.Contains(parsed.Hostname(), "%") {
		return "", "", errors.New("invalid external Harness URI")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || strings.Contains(parsed.Hostname(), ":") && ip.To4() != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "", "", errors.New("external Harness endpoint must use a private IP literal")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != parsed.Port() {
		return "", "", errors.New("invalid external Harness port")
	}
	if transport == "ssh" {
		if !ip.IsLoopback() {
			return "", "", errors.New("SSH Harness endpoint must use remote loopback")
		}
	} else if transport != "local" || !ip.IsPrivate() && !ip.IsLoopback() {
		return "", "", errors.New("external Harness endpoint must be private")
	}
	address := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	canonical := "https://" + address
	if canonical != value {
		return "", "", errors.New("external Harness URI is not canonical")
	}
	return canonical, address, nil
}
