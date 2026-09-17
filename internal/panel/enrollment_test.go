package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessrouter"
)

type enrollmentFixture struct {
	hosts        agentserviceclient.DockerHostPage
	plan         agentserviceclient.ExternalEnrollmentPlan
	sentCalls    int
	unknownCalls int
	finishCalls  int
	failCalls    int
	finishEffect string
	err          error
}

func (fixture *enrollmentFixture) Hosts(context.Context, string, int, string) (agentserviceclient.DockerHostPage, error) {
	return fixture.hosts, fixture.err
}
func (fixture *enrollmentFixture) PrepareExternalEnrollment(context.Context, string, agentserviceclient.ExternalEnrollmentRequest) (agentserviceclient.ExternalEnrollmentPlan, error) {
	return fixture.plan, fixture.err
}
func (fixture *enrollmentFixture) RegistryOperationSent(_ context.Context, _ string, status agentserviceclient.RegistryOperationStatus) (agentserviceclient.RegistryOperationStatus, error) {
	fixture.sentCalls++
	status.Phase, status.EffectState, status.OperationVersion = "applying", "sent", status.OperationVersion+1
	return status, fixture.err
}
func (fixture *enrollmentFixture) RegistryOperationUnknown(_ context.Context, _ string, status agentserviceclient.RegistryOperationStatus) (agentserviceclient.RegistryOperationStatus, error) {
	fixture.unknownCalls++
	status.Phase, status.EffectState, status.OperationVersion = "reconciling", "unknown", status.OperationVersion+1
	return status, fixture.err
}
func (fixture *enrollmentFixture) FinishRegistryOperation(_ context.Context, _ string, status agentserviceclient.RegistryOperationStatus, effect string) (agentserviceclient.RegistryOperationStatus, error) {
	fixture.finishCalls++
	fixture.finishEffect = effect
	status.Phase, status.EffectState, status.OperationVersion = "succeeded", effect, status.OperationVersion+1
	return status, fixture.err
}
func (fixture *enrollmentFixture) FailRegistryOperation(_ context.Context, _ string, status agentserviceclient.RegistryOperationStatus, _ string) (agentserviceclient.RegistryOperationStatus, error) {
	fixture.failCalls++
	status.Phase, status.EffectState, status.OperationVersion = "failed", "failed", status.OperationVersion+1
	return status, fixture.err
}

type enrollmentRouterFixture struct {
	installCalls  int
	activateCalls int
	installErr    error
	activateErr   error
	node          harnessrouter.NodeState
}

func (fixture *enrollmentRouterFixture) InstallEnrollmentRegistry(harnessrouter.EnrollmentRegistryInput) (harnessrouter.State, error) {
	fixture.installCalls++
	return harnessrouter.State{}, fixture.installErr
}
func (fixture *enrollmentRouterFixture) ActivateEnrollment(context.Context, string, string) (harnessrouter.NodeState, error) {
	fixture.activateCalls++
	return fixture.node, fixture.activateErr
}

func enrollmentPlan(phase string) agentserviceclient.ExternalEnrollmentPlan {
	operationID := "30000000-0000-4000-8000-000000000001"
	nodeID := "20000000-0000-4000-8000-000000000001"
	return agentserviceclient.ExternalEnrollmentPlan{
		SchemaID: agentserviceclient.ExternalEnrollmentPlanSchema, OperationID: operationID,
		RequestHash: strings.Repeat("a", 64), NodeID: nodeID, Registry: json.RawMessage(`{"private":"registry"}`),
		Binding: &agentserviceclient.ExternalEndpointBinding{
			Kind: "external", NodeID: nodeID, RegistrationRevision: 1, RegistrationEpoch: 1, EndpointRevision: 1,
			HostID: "10000000-0000-4000-8000-000000000001", HostVersion: 3, Transport: "local",
			TargetRef: "private-target", Address: "10.20.30.40:9443",
		},
		Status: agentserviceclient.RegistryOperationStatus{
			SchemaID: "agent-registry-operation-status-v1", Phase: phase, EffectState: map[string]string{
				"accepted": "not_sent", "applying": "sent", "reconciling": "unknown", "succeeded": "acknowledged",
			}[phase], OperationVersion: 1, UpdatedAt: "2026-09-17T10:00:00Z",
			Receipt: agentserviceclient.RegistryOperationReceipt{
				SchemaID: "agent-registry-operation-receipt-v1", OperationID: operationID, RequestHash: strings.Repeat("a", 64),
				ExpectedRegistryVersion: 2, ExpectedRegistrySHA256: strings.Repeat("b", 64),
				CandidateRegistryVersion: 3, CandidateRegistrySHA256: strings.Repeat("c", 64),
				AffectedNodeIDs: []string{nodeID}, AcceptedAt: "2026-09-17T10:00:00Z",
			},
		},
	}
}

func enrollmentRequestBody() string {
	return `{"schemaId":"external-harness-enrollment-v1","operationId":"30000000-0000-4000-8000-000000000001","hostId":"10000000-0000-4000-8000-000000000001","expectedHostVersion":3,"nodeId":"20000000-0000-4000-8000-000000000001","name":"External Codex","adapter":"codex","endpointUri":"https://10.20.30.40:9443","certificateSHA256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}`
}

func TestEnrollmentBFFReturnsOnlyReadyResultAndReconcilesLostAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		phase      string
		wantSent   int
		wantEffect string
	}{
		{phase: "accepted", wantSent: 1, wantEffect: "acknowledged"},
		{phase: "reconciling", wantEffect: "reconciled"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			server := setup(t, upstream)
			backend := &enrollmentFixture{plan: enrollmentPlan(test.phase)}
			router := &enrollmentRouterFixture{node: harnessrouter.NodeState{
				Mode: harnessrouter.ModeEligible, RegistrationRevision: 1, IdentityEpoch: 1,
			}}
			server.enrollment, server.enrollmentControl = backend, router
			cookie, csrf := login(t, server)
			response := request(server, http.MethodPost, enrollmentRequestBody(), "/api/v2/external-enrollments", cookie, csrf)
			if response.Code != http.StatusOK || backend.sentCalls != test.wantSent || backend.finishCalls != 1 ||
				backend.finishEffect != test.wantEffect || router.installCalls != 1 || router.activateCalls != 1 {
				t.Fatalf("status=%d body=%s backend=%+v router=%+v", response.Code, response.Body.String(), backend, router)
			}
			for _, secret := range []string{"private-target", "10.20.30.40", "registry", "credential"} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatalf("private enrollment detail escaped: %s", response.Body.String())
				}
			}
			if !strings.Contains(response.Body.String(), `"status":"ready"`) || !strings.Contains(response.Body.String(), `"registrationRevision":1`) {
				t.Fatalf("missing exact ready result: %s", response.Body.String())
			}
		})
	}
}

func TestEnrollmentBFFNeverReportsPartialOrIncompatibleActivationAsReady(t *testing.T) {
	for _, test := range []struct {
		name        string
		phase       string
		installErr  error
		activateErr error
		want        int
		wantCode    string
		unknown     int
		finish      int
	}{
		{name: "unknown registry effect", phase: "applying", installErr: &harnessrouter.EnrollmentFault{Status: http.StatusServiceUnavailable, Code: "state_unavailable"}, want: http.StatusServiceUnavailable, wantCode: "enrollment_reconciliation_required", unknown: 1},
		{name: "incompatible profile", phase: "applying", activateErr: &harnessrouter.EnrollmentFault{Status: http.StatusUnprocessableEntity, Code: "admission_capability_missing"}, want: http.StatusUnprocessableEntity, wantCode: "admission_capability_missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := setup(t, upstream)
			plan := enrollmentPlan(test.phase)
			backend := &enrollmentFixture{plan: plan}
			router := &enrollmentRouterFixture{installErr: test.installErr, activateErr: test.activateErr}
			server.enrollment, server.enrollmentControl = backend, router
			cookie, csrf := login(t, server)
			response := request(server, http.MethodPost, enrollmentRequestBody(), "/api/v2/external-enrollments", cookie, csrf)
			if response.Code != test.want || !strings.Contains(response.Body.String(), test.wantCode) || strings.Contains(response.Body.String(), `"status":"ready"`) ||
				backend.unknownCalls != test.unknown || backend.finishCalls != test.finish {
				t.Fatalf("status=%d body=%s backend=%+v", response.Code, response.Body.String(), backend)
			}
		})
	}
}

func TestEnrollmentHostProjectionDoesNotExposePrivateRefs(t *testing.T) {
	server := setup(t, upstream)
	backend := &enrollmentFixture{hosts: agentserviceclient.DockerHostPage{
		SchemaID: agentserviceclient.HostPageSchema, Items: []agentserviceclient.DockerHost{{
			SchemaID: agentserviceclient.HostSchema, HostID: "10000000-0000-4000-8000-000000000001", HostVersion: 3,
			DisplayName: "External host", Transport: "ssh", Availability: "ready",
			TargetRef: "private-target", CredentialRef: "private-credential", RegistryCredentialRef: "private-registry",
			DockerContextRef: "private-context", ExpectedHostKey: "SHA256:abcdefghijklmnopqrstuvwx12345678",
		}}, NextCursor: nil,
	}}
	server.enrollment, server.enrollmentControl = backend, &enrollmentRouterFixture{}
	cookie, _ := login(t, server)
	response := request(server, http.MethodGet, "", "/api/v2/external-enrollment-hosts", cookie, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "External host") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"private-target", "private-credential", "private-registry", "private-context", "expectedHostKey"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("private host detail escaped: %s", response.Body.String())
		}
	}
}
