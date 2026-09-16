package hostadapterclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func adapterResponse(status int, value any) *http.Response {
	body, _ := json.Marshal(value)
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(body))),
	}
}

func TestProvisionForwardsSecretOnlyToAuthenticatedAdapter(t *testing.T) {
	sentinel := "PRIVATE-KEY-SENTINEL"
	operationID := "30000000-0000-4000-8000-000000000001"
	client := &Client{token: "adapter-token-00000000000000000001", http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(request.Body)
		if request.Method != http.MethodPost || request.URL.Path != "/internal/v1/secrets" ||
			request.Header.Get(ownerHeader) != "owner-1" || request.Header.Get(tokenHeader) != "adapter-token-00000000000000000001" ||
			!strings.Contains(string(raw), "UFJJVkFURS1LRVktU0VOVElORUw=") || strings.Contains(string(raw), sentinel) {
			t.Fatalf("invalid adapter request: %s headers=%v body=%s", request.URL.Path, request.Header, raw)
		}
		return adapterResponse(http.StatusCreated, model.HostSecretProvision{
			SchemaID: model.HostSecretProvisionSchemaID, OperationID: operationID, Kind: "ssh", Status: "provisioned", CredentialRef: "cred_fixture",
		}), nil
	})}}
	ref, err := client.Provision(context.Background(), "owner-1", model.HostSecretInput{
		SchemaID: model.HostSecretInputSchemaID, OperationID: operationID, Kind: "ssh", PrivateKey: []byte(sentinel),
		Passphrase: []byte{}, Payload: []byte{},
	})
	if err != nil || !ref.Created || ref.CredentialRef != "cred_fixture" || ref.OperationID != operationID {
		t.Fatalf("ref=%+v err=%v", ref, err)
	}
}

func TestProvisionStatusPreservesNotFoundAndNeverReturnsSecret(t *testing.T) {
	operationID := "30000000-0000-4000-8000-000000000002"
	calls := 0
	client := &Client{token: "adapter-token-00000000000000000001", http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Path != "/internal/v1/secrets/"+operationID || request.Header.Get(ownerHeader) != "owner-1" {
			t.Fatalf("invalid status request: %s %s", request.Method, request.URL.Path)
		}
		if calls == 1 {
			return adapterResponse(http.StatusOK, model.HostSecretProvision{
				SchemaID: model.HostSecretProvisionSchemaID, OperationID: operationID, Kind: "registry", Status: "provisioned", CredentialRef: "cred_fixture",
			}), nil
		}
		return adapterResponse(http.StatusNotFound, map[string]any{"error": map[string]string{"code": "secret_provision_not_found"}}), nil
	})}}
	status, err := client.ProvisionStatus(context.Background(), "owner-1", operationID)
	if err != nil || status.CredentialRef != "cred_fixture" || status.Kind != "registry" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, err := client.ProvisionStatus(context.Background(), "owner-1", operationID); err != ErrSecretNotFound {
		t.Fatalf("not found was not preserved: %v", err)
	}
}

func TestProbeSendsOnlySafeDescriptorAndKeepsFailureClass(t *testing.T) {
	hostID := "20000000-0000-4000-8000-000000000001"
	client := &Client{token: "adapter-token-00000000000000000001", http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(request.Body)
		if request.URL.Path != "/internal/v1/probes" || !strings.Contains(string(raw), `"credentialRef":"cred_ssh"`) ||
			strings.Contains(string(raw), "privateKey") || strings.Contains(string(raw), "passphrase") {
			t.Fatalf("unsafe probe request: %s", raw)
		}
		return adapterResponse(http.StatusOK, model.HostObservation{
			SchemaID: model.HostObservationSchemaID, HostID: hostID, HostVersion: 3,
			ObservedAt: "2026-09-16T10:00:00Z", Availability: "unavailable",
			FailureStage: "transport_auth", FailureCode: "ssh_authentication_failed",
			NextAction: "Check SSH identity.", DockerContextRef: "default",
			Capabilities: []string{}, RegistryAvailability: "not_checked",
		}), nil
	})}}
	observation, err := client.Probe(context.Background(), "owner-1", model.HostRecord{
		SchemaID: model.HostSchemaID, HostID: hostID, HostVersion: 3, DisplayName: "Remote",
		Transport: "ssh", TargetRef: "ssh-engine", CredentialRef: "cred_ssh",
		ExpectedHostKey: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", DockerContextRef: "default",
		HostPlatform: "linux", HostArchitecture: "amd64", Capabilities: []string{}, RegistryAvailability: "not_configured",
	})
	if err != nil || observation.FailureCode != "ssh_authentication_failed" {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
}
