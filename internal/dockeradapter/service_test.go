package dockeradapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type serviceSecretFixture struct {
	owner, operationID, kind string
	value                    []byte
	err                      error
}

func (f *serviceSecretFixture) ProvisionSSH(_ context.Context, owner, operationID string, key, passphrase []byte) (SecretProvision, error) {
	f.owner, f.operationID, f.kind = owner, operationID, "ssh"
	f.value = append(append([]byte(nil), key...), passphrase...)
	return SecretProvision{SchemaID: SecretProvisionSchema, OperationID: operationID, Kind: "ssh", Status: "provisioned", CredentialRef: "cred_ssh_fixture", Created: true}, f.err
}

func (f *serviceSecretFixture) ProvisionRegistry(_ context.Context, owner, operationID string, payload []byte) (SecretProvision, error) {
	f.owner, f.operationID, f.kind = owner, operationID, "registry"
	f.value = append([]byte(nil), payload...)
	return SecretProvision{SchemaID: SecretProvisionSchema, OperationID: operationID, Kind: "registry", Status: "provisioned", CredentialRef: "cred_registry_fixture", Created: true}, f.err
}

func (f *serviceSecretFixture) ProvisionStatus(_ context.Context, owner, operationID string) (SecretProvision, error) {
	f.owner, f.operationID = owner, operationID
	return SecretProvision{SchemaID: SecretProvisionSchema, OperationID: operationID, Kind: "ssh", Status: "provisioned", CredentialRef: "cred_ssh_fixture"}, f.err
}

type serviceProbeFixture struct {
	owner      string
	descriptor HostDescriptor
}

func (f *serviceProbeFixture) Probe(_ context.Context, owner string, descriptor HostDescriptor) HostObservation {
	f.owner, f.descriptor = owner, descriptor
	return failureObservation(time.Unix(1, 0), descriptor, &probeFault{
		stage: "daemon_ping", code: "docker_permission_denied", next: "Provision daemon access.",
	})
}

func TestHostServiceForwardsSecretOnceAndReturnsOnlyOpaqueRef(t *testing.T) {
	secrets := &serviceSecretFixture{}
	prober := &serviceProbeFixture{}
	service, err := NewHostService("adapter-token-00000000000000000001", secrets, prober)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := "DO_NOT_ECHO_PRIVATE_KEY"
	operationID := "30000000-0000-4000-8000-000000000001"
	body, _ := json.Marshal(map[string]any{
		"schemaId": SecretInputSchema, "operationId": operationID, "kind": "ssh", "privateKey": []byte(sentinel),
		"passphrase": []byte("passphrase"), "payload": []byte{},
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/secrets", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(AdapterOwnerHeader, "owner-1")
	request.Header.Set(AdapterTokenHeader, "adapter-token-00000000000000000001")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || secrets.owner != "owner-1" || secrets.kind != "ssh" ||
		secrets.operationID != operationID ||
		!strings.Contains(string(secrets.value), sentinel) || strings.Contains(response.Body.String(), sentinel) ||
		strings.Contains(response.Body.String(), "privateKey") || !strings.Contains(response.Body.String(), "cred_ssh_fixture") {
		t.Fatalf("status=%d owner=%q kind=%q response=%s", response.Code, secrets.owner, secrets.kind, response.Body.String())
	}
}

func TestHostServiceSecretStatusAndConflictAreOwnerScopedAndSecretFree(t *testing.T) {
	secrets := &serviceSecretFixture{}
	service, _ := NewHostService("adapter-token-00000000000000000001", secrets, &serviceProbeFixture{})
	operationID := "30000000-0000-4000-8000-000000000002"
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/secrets/"+operationID, nil)
	request.Header.Set(AdapterOwnerHeader, "owner-1")
	request.Header.Set(AdapterTokenHeader, "adapter-token-00000000000000000001")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK || secrets.owner != "owner-1" || secrets.operationID != operationID ||
		!strings.Contains(response.Body.String(), `"status":"provisioned"`) || strings.Contains(response.Body.String(), "privateKey") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	secrets.err = ErrSecretConflict
	body, _ := json.Marshal(map[string]any{
		"schemaId": SecretInputSchema, "operationId": operationID, "kind": "ssh", "privateKey": []byte("different"),
		"passphrase": []byte{}, "payload": []byte{},
	})
	conflictRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/secrets", strings.NewReader(string(body)))
	conflictRequest.Header.Set("Content-Type", "application/json")
	conflictRequest.Header.Set(AdapterOwnerHeader, "owner-1")
	conflictRequest.Header.Set(AdapterTokenHeader, "adapter-token-00000000000000000001")
	conflict := httptest.NewRecorder()
	service.ServeHTTP(conflict, conflictRequest)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "secret_operation_conflict") || strings.Contains(conflict.Body.String(), "different") {
		t.Fatalf("status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestHostServiceProbeIsAuthenticatedAndReturnsConcreteSafeReason(t *testing.T) {
	secrets := &serviceSecretFixture{}
	prober := &serviceProbeFixture{}
	service, _ := NewHostService("adapter-token-00000000000000000001", secrets, prober)
	descriptor := HostDescriptor{
		SchemaID: HostDescriptorSchemaID, HostID: "20000000-0000-4000-8000-000000000001", HostVersion: 1,
		DisplayName: "Desktop", Transport: "local", TargetRef: "desktop-local", DockerContextRef: "desktop-linux",
		HostPlatform: "darwin", HostArchitecture: "arm64",
	}
	raw, _ := json.Marshal(descriptor)
	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/v1/probes", strings.NewReader(string(raw)))
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorized.Header.Set(AdapterOwnerHeader, "owner-1")
	denied := httptest.NewRecorder()
	service.ServeHTTP(denied, unauthorized)
	if denied.Code != http.StatusForbidden || prober.owner != "" {
		t.Fatalf("unauthorized probe reached adapter: %d", denied.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/internal/v1/probes", strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(AdapterOwnerHeader, "owner-1")
	request.Header.Set(AdapterTokenHeader, "adapter-token-00000000000000000001")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK || prober.owner != "owner-1" ||
		!strings.Contains(response.Body.String(), `"failureCode":"docker_permission_denied"`) ||
		strings.Contains(response.Body.String(), "adapter-token") {
		t.Fatalf("status=%d owner=%q body=%s", response.Code, prober.owner, response.Body.String())
	}
}
