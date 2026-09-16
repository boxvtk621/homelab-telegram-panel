package panel

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
)

type hostFixture struct {
	owner, hostID, cursor string
	limit                 int
	secret                agentserviceclient.HostSecretInput
	upsert                agentserviceclient.HostUpsert
	probe                 agentserviceclient.HostProbeRequest
	host                  agentserviceclient.DockerHost
	page                  agentserviceclient.DockerHostPage
	err                   error
}

func (f *hostFixture) Hosts(_ context.Context, owner string, limit int, cursor string) (agentserviceclient.DockerHostPage, error) {
	f.owner, f.limit, f.cursor = owner, limit, cursor
	return f.page, f.err
}

func (f *hostFixture) Host(_ context.Context, owner, hostID string) (agentserviceclient.DockerHost, error) {
	f.owner, f.hostID = owner, hostID
	return f.host, f.err
}

func (f *hostFixture) UpsertHost(_ context.Context, owner string, input agentserviceclient.HostUpsert) (agentserviceclient.DockerHost, int, error) {
	f.owner, f.upsert = owner, input
	return f.host, http.StatusCreated, f.err
}

func (f *hostFixture) ProvisionHostSecret(_ context.Context, owner string, input agentserviceclient.HostSecretInput) (agentserviceclient.HostSecretProvision, error) {
	f.owner, f.secret = owner, input
	f.secret.PrivateKey = append([]byte(nil), input.PrivateKey...)
	f.secret.Passphrase = append([]byte(nil), input.Passphrase...)
	f.secret.Payload = append([]byte(nil), input.Payload...)
	return agentserviceclient.HostSecretProvision{
		SchemaID: agentserviceclient.HostSecretProvisionSchema, OperationID: input.OperationID, Kind: input.Kind,
		Status: "provisioned", CredentialRef: "cred_fixture", Created: true,
	}, f.err
}

func (f *hostFixture) HostSecretProvision(_ context.Context, owner, operationID string) (agentserviceclient.HostSecretProvision, error) {
	f.owner = owner
	return agentserviceclient.HostSecretProvision{
		SchemaID: agentserviceclient.HostSecretProvisionSchema, OperationID: operationID, Kind: "ssh",
		Status: "provisioned", CredentialRef: "cred_fixture",
	}, f.err
}

func (f *hostFixture) ProbeHost(_ context.Context, owner, hostID string, input agentserviceclient.HostProbeRequest) (agentserviceclient.DockerHost, error) {
	f.owner, f.hostID, f.probe = owner, hostID, input
	return f.host, f.err
}

func TestHostBFFForwardsWriteOnlySecretWithoutEcho(t *testing.T) {
	server := setup(t, upstream)
	fixture := &hostFixture{}
	server.hosts = fixture
	cookie, csrf := login(t, server)
	operationID := "30000000-0000-4000-8000-000000000001"
	payload := `{"schemaId":"docker-secret-input-v1","operationId":"` + operationID + `","kind":"ssh","privateKey":"UFJJVkFURS1LRVktU0VOVElORUw=","passphrase":"cGFzcw==","payload":""}`
	response := request(server, http.MethodPost, payload, "/api/v2/host-secrets", cookie, csrf)
	if response.Code != http.StatusCreated || fixture.owner != "1-1" || !strings.Contains(string(fixture.secret.PrivateKey), "PRIVATE-KEY-SENTINEL") ||
		strings.Contains(response.Body.String(), "PRIVATE") || strings.Contains(response.Body.String(), "privateKey") ||
		!strings.Contains(response.Body.String(), "cred_fixture") {
		t.Fatalf("status=%d owner=%q body=%s", response.Code, fixture.owner, response.Body.String())
	}
	status := request(server, http.MethodGet, "", "/api/v2/host-secrets/"+operationID, cookie, "")
	if status.Code != http.StatusOK || fixture.owner != "1-1" || !strings.Contains(status.Body.String(), `"status":"provisioned"`) ||
		strings.Contains(status.Body.String(), "privateKey") {
		t.Fatalf("status readback=%d owner=%q body=%s", status.Code, fixture.owner, status.Body.String())
	}

	fixture.owner = ""
	denied := request(server, http.MethodPost, payload, "/api/v2/host-secrets", cookie, "")
	if denied.Code != http.StatusForbidden || fixture.owner != "" {
		t.Fatalf("secret without CSRF reached service: %d", denied.Code)
	}
}

func TestHostBFFShowsConcreteProbeReasonAndUsesAuthenticatedOwner(t *testing.T) {
	hostID := "20000000-0000-4000-8000-000000000001"
	observedAt := "2026-09-16T10:00:00Z"
	fixture := &hostFixture{host: agentserviceclient.DockerHost{
		SchemaID: agentserviceclient.HostSchema, HostID: hostID, HostVersion: 1, DisplayName: "Desktop",
		Transport: "local", TargetRef: "desktop-local", DockerContextRef: "desktop-linux",
		HostPlatform: "darwin", HostArchitecture: "arm64", ObservedAt: &observedAt,
		Availability: "unavailable", FailureStage: "daemon_ping", FailureCode: "docker_permission_denied",
		NextAction: "Provision daemon access.", Capabilities: []string{}, RegistryAvailability: "not_checked",
	}}
	server := setup(t, upstream)
	server.hosts = fixture
	cookie, csrf := login(t, server)
	response := request(server, http.MethodPost, `{"schemaId":"agent-host-probe-v1","expectedHostVersion":1}`, "/api/v2/hosts/"+hostID+"/probe", cookie, csrf)
	if response.Code != http.StatusOK || fixture.owner != "1-1" || fixture.hostID != hostID || fixture.probe.ExpectedHostVersion != 1 ||
		!strings.Contains(response.Body.String(), `"failureCode":"docker_permission_denied"`) ||
		!strings.Contains(response.Body.String(), `"nextAction":"Provision daemon access."`) {
		t.Fatalf("status=%d owner=%q host=%q body=%s", response.Code, fixture.owner, fixture.hostID, response.Body.String())
	}
}
