package operationclient

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/dockeradapter"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func operationResponse(status int, value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		Body: io.NopCloser(strings.NewReader(string(raw))),
	}
}

func TestRemoteAuthorityConnectsDurableSentBarrierAndTerminalReadback(t *testing.T) {
	target := Target{
		NodeID: "10000000-0000-4000-8000-000000000001", HostID: "20000000-0000-4000-8000-000000000001",
		RegistrationRevision: 3, RegistrationEpoch: 5, Generation: 7,
	}
	intent := Intent{
		SchemaID: IntentSchemaID, OperationID: "op-connected", Kind: "adapter.fixture", Target: target,
		Step: Step{StepID: "apply-1", Action: "adapter.fixture.apply", ResourceIDs: []string{"container:agent-1"}},
	}
	requestHash, err := intentRequestHash(intent)
	if err != nil {
		t.Fatal(err)
	}
	proof := Proof{
		SchemaID: ProofSchemaID, OperationID: intent.OperationID, RequestHash: requestHash,
		NodeID: target.NodeID, Generation: 8, WorkerID: "worker-1",
		WorkerToken: "30000000-0000-4000-8000-000000000001", OperationVersion: 2,
		LeaseExpiresAt: "2026-09-14T14:00:00.123456Z",
	}
	work := Work{SchemaID: WorkSchemaID, Intent: intent, Proof: proof, EffectState: "not_sent"}
	request, err := AdapterRequest(work)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	paths := []string{}
	client := &Client{workerToken: "test-worker-token-0000000000000001", http: &http.Client{Transport: roundTripFunc(func(httpRequest *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, httpRequest.URL.Path)
		if values := httpRequest.Header.Values(OwnerHeader); len(values) != 1 || values[0] != "owner-1" {
			t.Fatalf("owner header=%q", values)
		}
		if values := httpRequest.Header.Values(WorkerTokenHeader); len(values) != 1 || values[0] != "test-worker-token-0000000000000001" {
			t.Fatalf("worker header=%q", values)
		}
		switch httpRequest.URL.Path {
		case "/internal/v1/operation-workers/authority":
			return operationResponse(http.StatusOK, authorityResponse{SchemaID: AuthoritySchemaID, Active: true}), nil
		case "/internal/v1/operation-workers/sent":
			var sent proofRequest
			if json.NewDecoder(httpRequest.Body).Decode(&sent) != nil || sent.SchemaID != SentSchemaID || sent.Proof != proof {
				t.Fatal("sent request changed", sent)
			}
			proof.OperationVersion++
			return operationResponse(http.StatusOK, proof), nil
		case "/internal/v1/operation-workers/advance":
			var advance advanceRequest
			if json.NewDecoder(httpRequest.Body).Decode(&advance) != nil || advance.Proof != proof ||
				advance.Phase != "succeeded" || advance.EffectState != "acknowledged" {
				t.Fatal("advance request changed", advance)
			}
			return operationResponse(http.StatusOK, Status{
				SchemaID: StatusSchemaID,
				Receipt: Receipt{
					SchemaID: ReceiptSchemaID, OperationID: proof.OperationID, RequestHash: requestHash,
					Target: target, AcceptedGeneration: proof.Generation, AcceptedAt: "2026-09-14T13:00:00Z",
				},
				Phase: "succeeded", EffectState: "acknowledged", OperationVersion: 4,
				UpdatedAt: "2026-09-14T13:00:01Z", ResultCode: advance.ResultCode,
			}), nil
		default:
			t.Fatalf("unexpected path %s", httpRequest.URL.Path)
			return nil, nil
		}
	})}}
	authority, err := NewRemoteAuthority(client, "owner-1", work)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	executor, err := dockeradapter.Initialize(directory, target.HostID, target.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	execution, err := executor.Execute(context.Background(), authority, AdapterProof(proof), dockeradapter.NewFixtureBackend(), request)
	if err != nil || execution.Result.Outcome != "applied" || execution.JournalState != "acknowledged" || execution.Proof.OperationVersion != 3 {
		t.Fatalf("execution=%+v err=%v", execution, err)
	}
	resultCode := execution.Result.ReceiptID
	status, err := client.Advance(context.Background(), "owner-1", authority.Proof(), "succeeded", "acknowledged", &resultCode)
	if err != nil || status.Phase != "succeeded" || status.ResultCode == nil || *status.ResultCode != resultCode {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"/internal/v1/operation-workers/authority", "/internal/v1/operation-workers/sent",
		"/internal/v1/operation-workers/authority", "/internal/v1/operation-workers/authority",
		"/internal/v1/operation-workers/advance",
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths=%v want=%v", paths, want)
	}
}

func TestRemoteAuthorityRejectsClaimedStepMutationBeforeJournalOrEffect(t *testing.T) {
	target := Target{
		NodeID: "10000000-0000-4000-8000-000000000001", HostID: "20000000-0000-4000-8000-000000000001",
		RegistrationRevision: 3, RegistrationEpoch: 5, Generation: 7,
	}
	intent := Intent{
		SchemaID: IntentSchemaID, OperationID: "op-exact-work", Kind: "adapter.fixture", Target: target,
		Step: Step{StepID: "apply-1", Action: "adapter.fixture.apply", ResourceIDs: []string{"container:agent-1"}},
	}
	requestHash, err := intentRequestHash(intent)
	if err != nil {
		t.Fatal(err)
	}
	proof := Proof{
		SchemaID: ProofSchemaID, OperationID: intent.OperationID, RequestHash: requestHash,
		NodeID: target.NodeID, Generation: 8, WorkerID: "worker-1",
		WorkerToken: "30000000-0000-4000-8000-000000000001", OperationVersion: 2,
		LeaseExpiresAt: "2026-09-14T14:00:00.123456Z",
	}
	work := Work{SchemaID: WorkSchemaID, Intent: intent, Proof: proof, EffectState: "not_sent"}
	baseRequest, err := AdapterRequest(work)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{workerToken: "test-worker-token-0000000000000001", http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("mutated request reached remote authority")
		return nil, nil
	})}}

	for name, mutate := range map[string]func(*dockeradapter.Request){
		"step":      func(request *dockeradapter.Request) { request.StepID = "apply-2" },
		"action":    func(request *dockeradapter.Request) { request.Action = "fixture.changed" },
		"resources": func(request *dockeradapter.Request) { request.ResourceIDs = []string{"container:agent-2"} },
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			executor, err := dockeradapter.Initialize(directory, target.HostID, target.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			defer executor.Close()
			authority, err := NewRemoteAuthority(client, "owner-1", work)
			if err != nil {
				t.Fatal(err)
			}
			request := baseRequest
			request.ResourceIDs = append([]string(nil), baseRequest.ResourceIDs...)
			mutate(&request)
			backend := dockeradapter.NewFixtureBackend()
			if _, err := executor.Execute(context.Background(), authority, AdapterProof(proof), backend, request); err == nil {
				t.Fatal("mutated claimed step was accepted")
			}
			if backend.ApplyCalls() != 0 || len(executor.Snapshot()) != 0 {
				t.Fatal("mutated claimed step crossed the journal/effect boundary", backend.ApplyCalls(), executor.Snapshot())
			}
		})
	}

	mutatedWork := work
	mutatedWork.Intent.Step.ResourceIDs = []string{"container:agent-2"}
	if _, err := NewRemoteAuthority(client, "owner-1", mutatedWork); err == nil {
		t.Fatal("proof request hash did not bind claimed work")
	}
}

func TestWorkerMethodsRequireCapabilityClient(t *testing.T) {
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("worker request escaped without a worker capability")
		return nil, nil
	})}}
	if _, err := client.Claim(context.Background(), "owner-1", "10000000-0000-4000-8000-000000000001", "worker-1", time.Second); err == nil {
		t.Fatal("plain management client admitted a worker call")
	}
	if _, err := NewWorker("/tmp/agent-service.sock", "short"); err == nil {
		t.Fatal("short worker token accepted")
	}
}

func TestWorkerClientRequiresOwnerPrivateSocketBeforeHoldingCapability(t *testing.T) {
	directory, err := os.MkdirTemp("/private/tmp", "oc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(directory, "agent-service.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewWorker(socket, "test-worker-token-0000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if client, err := NewWorker(socket, "test-worker-token-0000000000000001"); err == nil {
		client.Close()
		t.Fatal("worker capability accepted a socket in a non-private directory")
	}
}
