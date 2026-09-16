package dockeradapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const (
	maximumPingBody     = 32
	maximumVersionBody  = 128 << 10
	maximumInfoBody     = 256 << 10
	defaultProbeTimeout = 45 * time.Second
)

type EngineSession interface {
	Do(*http.Request) (*http.Response, error)
	Close() error
}

type HostConnector interface {
	Open(context.Context, string, HostDescriptor) (EngineSession, ConnectionIdentity, *probeFault)
}

type RegistryProbe interface {
	Check(context.Context, string, string) *probeFault
}

type Prober struct {
	Connector HostConnector
	Registry  RegistryProbe
	Now       func() time.Time
	Timeout   time.Duration
}

type engineVersion struct {
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	Version       string `json:"Version"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
}

type engineInfo struct {
	ID              string `json:"ID"`
	OSType          string `json:"OSType"`
	Architecture    string `json:"Architecture"`
	OperatingSystem string `json:"OperatingSystem"`
	ServerVersion   string `json:"ServerVersion"`
}

func (p Prober) Probe(ctx context.Context, owner string, descriptor HostDescriptor) HostObservation {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	if !refPattern.MatchString(owner) || ValidateHostDescriptor(descriptor) != nil || p.Connector == nil {
		return failureObservation(now(), descriptor, &probeFault{stage: "descriptor", code: "host_descriptor_invalid", next: "Проверьте descriptor и сохранённые ссылки."})
	}
	timeout := p.Timeout
	if timeout <= 0 || timeout > defaultProbeTimeout {
		timeout = defaultProbeTimeout
	}
	probeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	session, connection, fault := p.Connector.Open(probeContext, owner, descriptor)
	if fault != nil {
		return failureObservation(now(), descriptor, fault)
	}
	defer session.Close()
	if fault = pingEngine(probeContext, session); fault != nil {
		return failureObservation(now(), descriptor, fault)
	}
	var version engineVersion
	if fault = readEngineJSON(probeContext, session, "/version", maximumVersionBody, &version); fault != nil {
		return failureObservation(now(), descriptor, fault)
	}
	var info engineInfo
	if fault = readEngineJSON(probeContext, session, "/info", maximumInfoBody, &info); fault != nil {
		return failureObservation(now(), descriptor, fault)
	}
	if fault = validateEngine(descriptor, version, info); fault != nil {
		observation := failureObservation(now(), descriptor, fault)
		if fingerprintPattern.MatchString(connection.HostKeySHA256) {
			observation.HostKeySHA256 = connection.HostKeySHA256
		}
		observation.ContextEndpoint = safeObservationField(connection.ContextEndpoint, 80)
		observation.DaemonID = safeObservationField(info.ID, 128)
		observation.EngineOS = safeObservationField(info.OSType, 160)
		observation.Architecture = safeObservationField(normalizeArchitecture(info.Architecture), 160)
		observation.APIVersion = safeObservationField(version.APIVersion, 32)
		observation.EngineVersion = safeObservationField(version.Version, 64)
		return observation
	}
	identity, err := identitySHA256(descriptor, connection, version, info)
	if err != nil {
		return failureObservation(now(), descriptor, &probeFault{stage: "host_identity", code: "host_identity_unavailable", next: "Повторите проверку identity."})
	}
	capabilities := []string{"docker-api-read", "linux-containers", "platform-" + normalizeArchitecture(info.Architecture)}
	slices.Sort(capabilities)
	observation := HostObservation{
		SchemaID: HostObservationSchemaID, HostID: descriptor.HostID, HostVersion: descriptor.HostVersion,
		ObservedAt: now().UTC().Format(time.RFC3339Nano), Availability: "ready",
		HostKeySHA256: connection.HostKeySHA256, DaemonID: info.ID,
		DockerContextRef: descriptor.DockerContextRef, ContextEndpoint: connection.ContextEndpoint,
		EngineOS: info.OSType, Architecture: normalizeArchitecture(info.Architecture),
		APIVersion: version.APIVersion, EngineVersion: version.Version,
		Capabilities: capabilities, IdentitySHA256: identity, RegistryAvailability: "not_configured",
	}
	if descriptor.ExpectedIdentitySHA256 != "" && descriptor.ExpectedIdentitySHA256 != identity {
		observation.Availability = "unavailable"
		observation.FailureStage = "host_identity"
		observation.FailureCode = "host_identity_changed"
		observation.NextAction = "Перепроверьте host key, daemon и Docker context; создайте новый plan."
		observation.IdentitySHA256 = ""
	}
	if descriptor.RegistryCredentialRef != "" {
		observation.RegistryAvailability = "not_checked"
		if p.Registry != nil {
			if registryFault := p.Registry.Check(probeContext, owner, descriptor.RegistryCredentialRef); registryFault != nil {
				observation.RegistryAvailability = "unavailable"
				if observation.Availability == "ready" {
					observation.Availability = "unavailable"
					observation.FailureStage = registryFault.stage
					observation.FailureCode = registryFault.code
					observation.NextAction = registryFault.next
					observation.IdentitySHA256 = ""
				}
			}
		}
	}
	return observation
}

func pingEngine(ctx context.Context, session EngineSession) *probeFault {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	response, err := session.Do(request)
	if err != nil {
		return engineRequestFault(ctx, err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumPingBody+1))
	if fault := engineStatusFault(response.StatusCode); fault != nil {
		return fault
	}
	if readErr != nil || len(body) > maximumPingBody || strings.TrimSpace(string(body)) != "OK" {
		return &probeFault{stage: "daemon_ping", code: "docker_response_invalid", next: "Проверьте Docker daemon и endpoint context."}
	}
	return nil
}

func readEngineJSON(ctx context.Context, session EngineSession, path string, maximum int64, target any) *probeFault {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	response, err := session.Do(request)
	if err != nil {
		return engineRequestFault(ctx, err)
	}
	defer response.Body.Close()
	if fault := engineStatusFault(response.StatusCode); fault != nil {
		return fault
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil || int64(len(body)) > maximum || len(body) == 0 || !strictjson.Valid(body) || json.Unmarshal(body, target) != nil {
		return &probeFault{stage: "daemon_info", code: "docker_response_invalid", next: "Проверьте совместимость Docker API."}
	}
	return nil
}

func engineRequestFault(ctx context.Context, err error) *probeFault {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &probeFault{stage: "daemon_ping", code: "probe_timeout", next: "Проверьте сеть, SSH и состояние daemon.", retryable: true}
	}
	if errors.Is(err, context.Canceled) {
		return &probeFault{stage: "daemon_ping", code: "probe_cancelled", next: "Запустите проверку повторно.", retryable: true}
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, errDockerPermission) {
		return &probeFault{stage: "daemon_ping", code: "docker_permission_denied", next: "Проверьте provisioning доступа пользователя к Docker daemon."}
	}
	return &probeFault{stage: "daemon_ping", code: "docker_daemon_unavailable", next: "Проверьте доступ пользователя к Docker daemon.", retryable: true}
}

func engineStatusFault(status int) *probeFault {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return &probeFault{stage: "daemon_ping", code: "docker_permission_denied", next: "Проверьте provisioning доступа пользователя к Docker daemon."}
	default:
		return &probeFault{stage: "daemon_ping", code: "docker_daemon_unavailable", next: "Проверьте Docker daemon и выбранный context.", retryable: status >= 500}
	}
}

func validateEngine(descriptor HostDescriptor, version engineVersion, info engineInfo) *probeFault {
	architecture := normalizeArchitecture(info.Architecture)
	if !validHostText(info.ID, 128) || !validHostText(version.APIVersion, 32) || !validHostText(version.Version, 64) ||
		!validHostText(info.OSType, 160) || !validHostText(architecture, 160) ||
		(version.Os != "" && !validHostText(version.Os, 160)) {
		return &probeFault{stage: "daemon_info", code: "docker_response_invalid", next: "Проверьте совместимость Docker API."}
	}
	if !slices.Contains([]string{"amd64", "arm64"}, architecture) {
		return &probeFault{stage: "target_platform", code: "platform_unverified", next: "Эта архитектура Docker Engine не подтверждена; выберите linux/amd64 или linux/arm64."}
	}
	major, err := strconv.Atoi(strings.SplitN(version.Version, ".", 2)[0])
	if err != nil || major < 28 {
		return &probeFault{stage: "target_platform", code: "engine_version_unsupported", next: "Обновите Docker Engine до версии 28 или новее."}
	}
	if info.OSType != "linux" || (version.Os != "" && version.Os != "linux") {
		return &probeFault{stage: "target_platform", code: "platform_incompatible", next: "Выберите Docker daemon в режиме Linux containers."}
	}
	if architecture != descriptor.HostArchitecture {
		return &probeFault{stage: "target_platform", code: "platform_incompatible", next: "Выберите образ и host с совпадающей архитектурой."}
	}
	desktop := strings.Contains(strings.ToLower(info.OperatingSystem), "docker desktop")
	if (descriptor.HostPlatform == "darwin" || descriptor.HostPlatform == "windows") && !desktop {
		return &probeFault{stage: "target_platform", code: "platform_incompatible", next: "Проверьте, что выбран Docker Desktop daemon."}
	}
	if descriptor.HostPlatform == "windows" && architecture == "arm64" {
		return &probeFault{stage: "target_platform", code: "platform_unverified", next: "Windows Desktop ARM64 требует отдельного platform test."}
	}
	return nil
}

func normalizeArchitecture(value string) string {
	switch strings.ToLower(value) {
	case "x86_64", "x86-64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.ToLower(value)
	}
}

func safeObservationField(value string, maximum int) string {
	if !validHostText(value, maximum) {
		return ""
	}
	return value
}
