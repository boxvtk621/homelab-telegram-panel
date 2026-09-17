package agentserviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func validTestPage() Page {
	return Page{
		SchemaID: InventorySchema,
		Items: []Item{{
			NodeID: "20000000-0000-4000-8000-000000000001", Name: "Agent", Engine: "cursor", SourceMode: "fixture",
			Host:             Host{HostID: "10000000-0000-4000-8000-000000000001", Name: "Mac"},
			RegistrationMode: "compatible", Status: "unknown",
			State: State{Process: "unknown", Connection: "unknown", Readiness: "unknown", Occupancy: "unknown"},
			Actions: Actions{
				OpenWorkspace: Action{Allowed: true},
				SendMessage:   Action{Reason: "state_unknown", NextAction: "Обновите состояние."},
				Lifecycle:     Action{Reason: "r01_read_only", NextAction: "Доступно в следующем этапе."},
			},
			DialogCount: 20_000,
		}},
	}
}

func TestDialogBindingsUseBoundedNodeScopedPages(t *testing.T) {
	nodeID := "20000000-0000-4000-8000-000000000001"
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/internal/v1/dialog-bindings" ||
			request.URL.Query().Get("nodeId") != nodeID || request.URL.Query().Get("limit") != "100" ||
			request.URL.Query().Get("cursor") != "cursor-1" {
			t.Fatalf("unexpected private request: %s %s", request.Method, request.URL.String())
		}
		return response(http.StatusOK, DialogPage{
			SchemaID: BindingsSchema, NodeID: nodeID,
			Items: []Dialog{{
				NodeDialogID:    "30000000-0000-4000-8000-000000000001",
				LogicalDialogID: "40000000-0000-4000-8000-000000000001", BindingVersion: 1,
			}},
		}), nil
	})}}
	page, err := client.DialogBindings(context.Background(), "owner-1", nodeID, 100, "cursor-1")
	if err != nil || len(page.Items) != 1 || page.Items[0].BindingVersion != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func response(status int, value any) *http.Response {
	body, _ := json.Marshal(value)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

func TestInventoryUsesUDSPrivateRequestAndTrustedOwner(t *testing.T) {
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/internal/v1/inventory" ||
			request.URL.Query().Get("limit") != "100" || request.URL.Query().Get("cursor") != "cursor-1" {
			t.Fatalf("unexpected private request: %s %s", request.Method, request.URL.String())
		}
		if values := request.Header.Values(OwnerHeader); len(values) != 1 || values[0] != "owner-1" {
			t.Fatalf("owner header=%q", values)
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Fatal("browser credentials reached agent-service")
		}
		return response(http.StatusOK, validTestPage()), nil
	})}}
	page, err := client.Inventory(context.Background(), "owner-1", 100, "cursor-1")
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != "unknown" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestSecretProvisionReplayAndStatusUseOperationIDWithoutEcho(t *testing.T) {
	operationID := "30000000-0000-4000-8000-000000000001"
	sentinel := "PRIVATE-KEY-SENTINEL"
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get(OwnerHeader) != "owner-1" {
			t.Fatalf("owner=%q", request.Header.Get(OwnerHeader))
		}
		provision := HostSecretProvision{
			SchemaID: HostSecretProvisionSchema, OperationID: operationID, Kind: "ssh", Status: "provisioned", CredentialRef: "cred_fixture",
		}
		switch request.Method {
		case http.MethodPost:
			raw, _ := io.ReadAll(request.Body)
			if request.URL.Path != "/internal/v1/host-secrets" || !strings.Contains(string(raw), operationID) ||
				strings.Contains(string(raw), sentinel) || !strings.Contains(string(raw), "UFJJVkFURS1LRVktU0VOVElORUw=") {
				t.Fatalf("unsafe provision request: path=%s body=%s", request.URL.Path, raw)
			}
			return response(http.StatusOK, provision), nil
		case http.MethodGet:
			if request.URL.Path != "/internal/v1/host-secrets/"+operationID {
				t.Fatalf("status path=%s", request.URL.Path)
			}
			return response(http.StatusOK, provision), nil
		default:
			t.Fatalf("method=%s", request.Method)
			return nil, nil
		}
	})}}
	input := HostSecretInput{
		SchemaID: HostSecretSchema, OperationID: operationID, Kind: "ssh", PrivateKey: []byte(sentinel), Passphrase: []byte{}, Payload: []byte{},
	}
	provision, err := client.ProvisionHostSecret(context.Background(), "owner-1", input)
	if err != nil || provision.Created || provision.CredentialRef != "cred_fixture" {
		t.Fatalf("provision=%+v err=%v", provision, err)
	}
	status, err := client.HostSecretProvision(context.Background(), "owner-1", operationID)
	if err != nil || status.CredentialRef != provision.CredentialRef {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestInventoryFailsClosedOnTransportAndContractViolations(t *testing.T) {
	tests := []struct {
		name      string
		transport roundTripFunc
	}{
		{
			name: "transport",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("private socket path")
			},
		},
		{
			name: "provenance mismatch",
			transport: func(*http.Request) (*http.Response, error) {
				page := validTestPage()
				observed, source := "2026-09-14T10:00:00Z", "registry-import"
				page.Items[0].ObservedAt, page.Items[0].Source = &observed, &source
				page.Items[0].PendingCount = &Metric{Value: 1, ObservedAt: observed, Source: "different-source"}
				return response(http.StatusOK, page), nil
			},
		},
		{
			name: "unsafe pending count",
			transport: func(*http.Request) (*http.Response, error) {
				page := validTestPage()
				observed, source := "2026-09-14T10:00:00Z", "registry-import"
				page.Items[0].ObservedAt, page.Items[0].Source = &observed, &source
				page.Items[0].PendingCount = &Metric{Value: 1 << 53, ObservedAt: observed, Source: source}
				return response(http.StatusOK, page), nil
			},
		},
		{
			name: "duplicate json",
			transport: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"schemaId":"agent-management-v1","schemaId":"agent-management-v1","items":[],"nextCursor":null}`)),
				}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{http: &http.Client{Transport: test.transport}}
			_, err := client.Inventory(context.Background(), "owner-1", 10, "")
			var fault *Fault
			if !errors.As(err, &fault) || fault.Code != "invalid_inventory_response" && fault.Code != "inventory_unavailable" || !fault.Retryable {
				t.Fatalf("fault=%#v err=%v", fault, err)
			}
		})
	}
}

func TestInventoryMapsOnlyAllowlistedSafeFaults(t *testing.T) {
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"code": "postgres_password=secret", "message": "raw details", "retryable": false},
		}), nil
	})}}
	_, err := client.Inventory(context.Background(), "owner-1", 10, "")
	var fault *Fault
	if !errors.As(err, &fault) || fault.Code != "inventory_unavailable" || fault.Status != http.StatusInternalServerError {
		t.Fatalf("fault=%#v err=%v", fault, err)
	}
}

func TestExternalEnrollmentUsesWorkerScopeAndRejectsMismatchedReplay(t *testing.T) {
	input := ExternalEnrollmentRequest{
		SchemaID: ExternalEnrollmentSchema, OperationID: "30000000-0000-4000-8000-000000000001",
		HostID: "10000000-0000-4000-8000-000000000001", ExpectedHostVersion: 3,
		NodeID: "20000000-0000-4000-8000-000000000001", Name: "External Codex", Adapter: "codex",
		EndpointURI: "https://127.0.0.1:9443", CertificateSHA256: strings.Repeat("a", 64),
	}
	status := RegistryOperationStatus{
		SchemaID: "agent-registry-operation-status-v1",
		Receipt: RegistryOperationReceipt{
			SchemaID: "agent-registry-operation-receipt-v1", OperationID: input.OperationID,
			RequestHash: strings.Repeat("b", 64), ExpectedRegistryVersion: 2,
			ExpectedRegistrySHA256: strings.Repeat("c", 64), CandidateRegistryVersion: 3,
			CandidateRegistrySHA256: strings.Repeat("d", 64), AffectedNodeIDs: []string{input.NodeID},
			AcceptedAt: "2026-09-17T10:00:00Z",
		},
		Phase: "accepted", EffectState: "not_sent", OperationVersion: 1,
		UpdatedAt: "2026-09-17T10:00:00Z",
	}
	plan := ExternalEnrollmentPlan{
		SchemaID: ExternalEnrollmentPlanSchema, OperationID: input.OperationID,
		RequestHash: externalEnrollmentRequestHash(input), NodeID: input.NodeID, Registry: json.RawMessage(`{}`),
		Binding: &ExternalEndpointBinding{
			Kind: "external", NodeID: input.NodeID, RegistrationRevision: 1, RegistrationEpoch: 1,
			EndpointRevision: 1, HostID: input.HostID, HostVersion: 3, Transport: "ssh",
			TargetRef: "host-one", CredentialRef: "ssh-one", ExpectedHostKey: "SHA256:" + strings.Repeat("A", 43),
			Address: "127.0.0.1:9443",
		},
		Status: status,
	}
	calls := 0
	client := &Client{workerToken: "test-worker-token-0000000000000001", http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || request.URL.Path != "/internal/v1/external-enrollments" ||
			request.Header.Get(OwnerHeader) != "owner-1" || request.Header.Get("X-Agent-Service-Worker-Token") != "test-worker-token-0000000000000001" {
			t.Fatalf("unexpected enrollment request: %s %s headers=%v", request.Method, request.URL.Path, request.Header)
		}
		raw, _ := io.ReadAll(request.Body)
		if strings.Contains(string(raw), "ssh-one") || strings.Contains(string(raw), "host-one") {
			t.Fatalf("private host refs reached browser request: %s", raw)
		}
		result := plan
		if calls == 2 {
			result.RequestHash = strings.Repeat("e", 64)
		}
		return response(http.StatusAccepted, result), nil
	})}}
	got, err := client.PrepareExternalEnrollment(context.Background(), "owner-1", input)
	if err != nil || got.RequestHash != plan.RequestHash || got.Binding == nil {
		t.Fatalf("plan=%+v err=%v", got, err)
	}
	_, err = client.PrepareExternalEnrollment(context.Background(), "owner-1", input)
	var fault *Fault
	if !errors.As(err, &fault) || fault.Code != "invalid_enrollment_response" || !fault.Retryable {
		t.Fatalf("mismatched replay fault=%#v err=%v", fault, err)
	}
	inconsistent := status
	inconsistent.Phase = "succeeded"
	inconsistent.EffectState = "unknown"
	if validRegistryOperationStatus(inconsistent, input.OperationID) {
		t.Fatal("inconsistent terminal enrollment status accepted")
	}
}
