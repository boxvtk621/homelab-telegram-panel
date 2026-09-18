package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessrouter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
	"github.com/boxvtk621/homelab-telegram-panel/internal/logicaldelete"
)

type logicalDeleteStoreFixture struct {
	current               logicaldelete.Status
	actions               []string
	begin                 logicaldelete.Request
	getCalls              int
	rejectAdvanceFailures int
}

func (fixture *logicalDeleteStoreFixture) BeginLogicalDelete(_ context.Context, owner string, request logicaldelete.Request) (logicaldelete.Status, error) {
	if owner != "1-1" {
		return logicaldelete.Status{}, errors.New("wrong owner")
	}
	fixture.begin = request
	return fixture.current, nil
}

func (fixture *logicalDeleteStoreFixture) GetLogicalDelete(_ context.Context, owner, operationID string) (logicaldelete.Status, error) {
	if owner != "1-1" || operationID != fixture.current.OperationID {
		return logicaldelete.Status{}, errors.New("wrong operation")
	}
	fixture.getCalls++
	return fixture.current, nil
}

func (fixture *logicalDeleteStoreFixture) AdvanceLogicalDelete(_ context.Context, owner string, request logicaldelete.AdvanceRequest) (logicaldelete.Status, error) {
	if owner != "1-1" || request.OperationID != fixture.current.OperationID ||
		request.RequestHash != fixture.current.RequestHash || request.ExpectedOperationVersion != fixture.current.OperationVersion {
		return logicaldelete.Status{}, errors.New("wrong transition fence")
	}
	if request.Action == "reject" && fixture.rejectAdvanceFailures > 0 {
		fixture.rejectAdvanceFailures--
		return logicaldelete.Status{}, errors.New("synthetic lost reject advance")
	}
	fixture.actions = append(fixture.actions, request.Action)
	fixture.current.OperationVersion++
	switch request.Action {
	case "hold":
		fixture.current.Phase, fixture.current.EffectState = "holding", "sent"
		fixture.current.HoldVersion = request.HoldVersion
		fixture.current.HoldScopeRevision = request.ObservedHoldScopeRevision
	case "unknown":
		fixture.current.Phase, fixture.current.EffectState = "reconciling", "unknown"
	case "complete":
		fixture.current.Phase, fixture.current.EffectState = "succeeded", "reconciled"
		fixture.current.ResultCode, fixture.current.NodeReceipt = "node_tombstoned", request.NodeReceipt
	case "reject":
		fixture.current.Phase, fixture.current.EffectState = "failed", "failed"
		fixture.current.HoldScopeRevision, fixture.current.ResultCode = request.ObservedHoldScopeRevision, request.ResultCode
	default:
		return logicaldelete.Status{}, errors.New("unexpected transition")
	}
	return fixture.current, nil
}

type logicalDeleteRouterFixture struct {
	mode           string
	installCalls   int
	deleteCalls    int
	statusCalls    int
	releaseCalls   int
	committed      bool
	receipt        logicaldelete.NodeReceipt
	lastNodeDelete logicaldelete.NodeRequest
}

func (fixture *logicalDeleteRouterFixture) Public(owner string) (harnessclient.PublicRegistry, bool) {
	return harnessclient.PublicRegistry{RegistryVersion: 1, Mode: "fixture", Nodes: []harnessclient.PublicNode{}}, owner == "1-1"
}
func (fixture *logicalDeleteRouterFixture) RoutingRegistry() harnessclient.RoutingRegistry {
	return harnessclient.RoutingRegistry{RegistryVersion: 1, OwnerID: "1-1", Mode: "fixture"}
}
func (fixture *logicalDeleteRouterFixture) Read(context.Context, string, string, string, string) (harnessclient.Response, error) {
	return harnessclient.Response{}, errors.New("unexpected read")
}
func (fixture *logicalDeleteRouterFixture) Command(context.Context, string, string, []byte) (harnessclient.Response, error) {
	return harnessclient.Response{}, errors.New("unexpected command")
}
func (fixture *logicalDeleteRouterFixture) CommandFenced(context.Context, string, string, []byte, hp.NodeIdentity) (harnessclient.Response, error) {
	return harnessclient.Response{}, errors.New("unexpected fenced command")
}
func (fixture *logicalDeleteRouterFixture) OpenEvents(context.Context, string, string, int64) (*harnessclient.Stream, error) {
	return nil, errors.New("unexpected events")
}
func (fixture *logicalDeleteRouterFixture) Artifact(context.Context, string, string, string, string) (harnessclient.BinaryResponse, error) {
	return harnessclient.BinaryResponse{}, errors.New("unexpected artifact")
}
func (fixture *logicalDeleteRouterFixture) TranscriptChunk(context.Context, string, string, harnessclient.TranscriptChunkRequest) (harnessclient.TranscriptChunkResponse, error) {
	return harnessclient.TranscriptChunkResponse{}, errors.New("unexpected text")
}
func (fixture *logicalDeleteRouterFixture) ExportHistory(context.Context, string, string, historyreplica.StreamIdentity, int64, int) (historyreplica.ExportPage, error) {
	return historyreplica.ExportPage{}, errors.New("unexpected export")
}
func (fixture *logicalDeleteRouterFixture) Close() {}

func (fixture *logicalDeleteRouterFixture) InstallHold(_ context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	fixture.installCalls++
	var request harnessbarrier.InstallRequest
	if owner != "1-1" || json.Unmarshal(body, &request) != nil || request.NodeID != nodeID {
		return harnessclient.Response{}, errors.New("invalid hold request")
	}
	if fixture.mode == "hold-reject" {
		return harnessclient.Response{Status: http.StatusConflict}, nil
	}
	receipt := harnessbarrier.HoldReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "hold.installed",
		OperationID: request.OperationID, ReceiptID: "91000000-0000-4000-8000-000000000001", NodeID: nodeID,
		Epoch: request.ExpectedEpoch, BindingGeneration: request.BindingGeneration, Scope: request.Scope,
		HoldVersion: 1, ScopeRevision: request.ExpectedScopeRevision + 1, InstalledAt: "2026-09-17T10:00:00Z",
	}
	raw, _ := json.Marshal(receipt)
	return harnessclient.Response{Status: http.StatusCreated, Body: raw}, nil
}

func (fixture *logicalDeleteRouterFixture) QuiescenceProof(_ context.Context, nodeID, owner, operationID string) (harnessclient.Response, error) {
	if owner != "1-1" {
		return harnessclient.Response{}, errors.New("invalid proof owner")
	}
	if fixture.releaseCalls > 0 {
		return harnessclient.Response{Status: http.StatusConflict}, nil
	}
	proof := harnessbarrier.QuiescenceProof{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "scope.parked",
		OperationID: operationID, NodeID: nodeID, Epoch: 1, BindingGeneration: 1,
		Scope:       harnessbarrier.Scope{Kind: "dialog", DialogID: "91000000-0000-4000-8000-000000000004"},
		HoldVersion: 1, ScopeRevision: 1, StateVersion: 2, QueueRevision: 1,
		ParkedRequestIDsDigest: strings.Repeat("a", 64), CheckpointStreamID: nodeID, CheckpointSeq: 2,
		EffectStatus: "known",
	}
	proof.ProofHash = harnessbarrier.ComputeProofHash(proof)
	raw, _ := json.Marshal(proof)
	return harnessclient.Response{Status: http.StatusOK, Body: raw}, nil
}

func (fixture *logicalDeleteRouterFixture) ReleaseHold(_ context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	fixture.releaseCalls++
	var request harnessbarrier.ReleaseRequest
	if owner != "1-1" || json.Unmarshal(body, &request) != nil || request.NodeID != nodeID || request.Action != "cancel" {
		return harnessclient.Response{}, errors.New("invalid release request")
	}
	receipt := harnessbarrier.ReleaseReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "hold.cancelled",
		OperationID: request.OperationID, ReceiptID: "91000000-0000-4000-8000-000000000002", NodeID: nodeID,
		Epoch: request.ExpectedEpoch, BindingGeneration: request.BindingGeneration, Scope: request.Scope,
		HoldVersion: request.HoldVersion, ScopeRevision: request.ExpectedScopeRevision + 1,
		ReleasedAt: "2026-09-17T10:00:01Z",
	}
	raw, _ := json.Marshal(receipt)
	return harnessclient.Response{Status: http.StatusCreated, Body: raw}, nil
}

func (fixture *logicalDeleteRouterFixture) DeleteLogicalDialog(_ context.Context, nodeID, owner string, request logicaldelete.NodeRequest) (harnessclient.Response, error) {
	fixture.deleteCalls++
	fixture.lastNodeDelete = request
	if owner != "1-1" || nodeID != request.NodeID {
		return harnessclient.Response{}, errors.New("invalid delete owner")
	}
	requestHash, _ := logicaldelete.NodeRequestHash(request)
	deletedAt := "2026-09-17T10:00:02Z"
	references, _ := json.Marshal(hp.DialogDeleteReferences{DialogID: request.NodeDialogID})
	commandReceipt, _ := json.Marshal(hp.Receipt{
		ProtocolVersion: hp.ProtocolVersion, SchemaID: hp.SchemaID, CommandID: request.CommandID,
		CommandKind: hp.CommandDialogDelete, ReceiptID: "91000000-0000-4000-8000-000000000003",
		AcceptedAt: deletedAt, NodeID: nodeID, EventSeq: 9, Result: "deleted", References: references,
	})
	fixture.receipt = logicaldelete.NodeReceipt{
		SchemaID: logicaldelete.NodeSchemaID, OperationID: request.OperationID, NodeRequestHash: requestHash,
		CoordinatorRequestHash: request.CoordinatorRequestHash, ReceiptID: "91000000-0000-4000-8000-000000000005",
		CommandID: request.CommandID, LogicalDialogID: request.LogicalDialogID, NodeID: nodeID,
		NodeDialogID: request.NodeDialogID, Epoch: request.ExpectedEpoch, RegistryVersion: request.RegistryVersion,
		BindingVersion: request.BindingVersion, DeletedDialogVersion: request.ExpectedDialogVersion + 1,
		HoldVersion: request.HoldVersion, HoldScopeRevision: request.HoldScopeRevision + 1,
		TombstoneEventSeq: 9, CommandReceipt: commandReceipt, DeletedAt: deletedAt,
	}
	switch fixture.mode {
	case "success":
		fixture.committed = true
		raw, _ := json.Marshal(fixture.receipt)
		return harnessclient.Response{Status: http.StatusCreated, Body: raw}, nil
	case "lost":
		fixture.committed = true
		return harnessclient.Response{Status: http.StatusServiceUnavailable}, nil
	case "reject":
		return harnessclient.Response{Status: http.StatusConflict}, nil
	default:
		return harnessclient.Response{}, errors.New("invalid fixture mode")
	}
}

func (fixture *logicalDeleteRouterFixture) LogicalDeleteStatus(_ context.Context, nodeID, owner, operationID string) (harnessclient.Response, error) {
	fixture.statusCalls++
	if owner != "1-1" || nodeID != harnessNode || operationID != "91000000-0000-4000-8000-000000000001" {
		return harnessclient.Response{}, errors.New("invalid status scope")
	}
	if !fixture.committed {
		return harnessclient.Response{Status: http.StatusNotFound}, nil
	}
	raw, _ := json.Marshal(fixture.receipt)
	return harnessclient.Response{Status: http.StatusOK, Body: raw}, nil
}

func logicalDeletePanelFixture(t *testing.T, mode string) (*Server, *logicalDeleteStoreFixture, *logicalDeleteRouterFixture, logicaldelete.Request) {
	t.Helper()
	request := logicaldelete.Request{
		SchemaID: logicaldelete.SchemaID, OperationID: "91000000-0000-4000-8000-000000000001",
		CommandID:              "91000000-0000-4000-8000-000000000002",
		LogicalDialogID:        "91000000-0000-4000-8000-000000000003",
		ExpectedBindingVersion: 1, ExpectedDialogVersion: 2,
	}
	hash, err := logicaldelete.RequestHash(request)
	if err != nil {
		t.Fatal(err)
	}
	storeFixture := &logicalDeleteStoreFixture{current: logicaldelete.Status{
		SchemaID: logicaldelete.SchemaID, OperationID: request.OperationID, RequestHash: hash,
		CommandID: request.CommandID, LogicalDialogID: request.LogicalDialogID,
		ExpectedBindingVersion: request.ExpectedBindingVersion, ExpectedDialogVersion: request.ExpectedDialogVersion,
		NodeID: harnessNode, NodeDialogID: "91000000-0000-4000-8000-000000000004",
		RegistryVersion: 1, IdentityEpoch: 1, Phase: "accepted", EffectState: "not_sent", OperationVersion: 1,
		UpdatedAt: "2026-09-17T10:00:00Z",
	}}
	routerFixture := &logicalDeleteRouterFixture{mode: mode}
	s := setup(t, upstream)
	router, err := harnessrouter.New(routerFixture)
	if err != nil {
		t.Fatal(err)
	}
	s.router.Close()
	s.router = router
	s.logicalDelete = storeFixture
	return s, storeFixture, routerFixture, request
}

func TestPanelLogicalDeleteLostACKUsesStatusWithoutResend(t *testing.T) {
	s, storeFixture, routerFixture, deleteRequest := logicalDeletePanelFixture(t, "lost")
	cookie, csrf := login(t, s)
	body, _ := json.Marshal(deleteRequest)
	started := request(s, http.MethodPost, string(body), "/api/v2/logical-dialog-deletes", cookie, csrf)
	if started.Code != http.StatusOK || storeFixture.current.Phase != "reconciling" || routerFixture.deleteCalls != 1 {
		t.Fatalf("start status=%d state=%+v deletes=%d body=%s", started.Code, storeFixture.current, routerFixture.deleteCalls, started.Body.String())
	}
	resolved := request(s, http.MethodGet, "", "/api/v2/logical-dialog-deletes/"+deleteRequest.OperationID, cookie, "")
	if resolved.Code != http.StatusOK || storeFixture.current.Phase != "succeeded" || routerFixture.deleteCalls != 1 ||
		strings.Join(storeFixture.actions, ",") != "hold,unknown,complete" || storeFixture.getCalls != 1 {
		t.Fatalf("resolve status=%d state=%+v actions=%v deletes=%d gets=%d body=%s", resolved.Code,
			storeFixture.current, storeFixture.actions, routerFixture.deleteCalls, storeFixture.getCalls, resolved.Body.String())
	}
}

func TestPanelLogicalDeleteKnownRejectionCancelsOwnHold(t *testing.T) {
	s, storeFixture, routerFixture, deleteRequest := logicalDeletePanelFixture(t, "reject")
	cookie, csrf := login(t, s)
	body, _ := json.Marshal(deleteRequest)
	response := request(s, http.MethodPost, string(body), "/api/v2/logical-dialog-deletes", cookie, csrf)
	if response.Code != http.StatusOK || storeFixture.current.Phase != "failed" || routerFixture.releaseCalls != 1 ||
		routerFixture.deleteCalls != 1 || strings.Join(storeFixture.actions, ",") != "hold,reject" {
		t.Fatalf("reject status=%d state=%+v actions=%v deletes=%d releases=%d body=%s", response.Code,
			storeFixture.current, storeFixture.actions, routerFixture.deleteCalls, routerFixture.releaseCalls, response.Body.String())
	}
}

func TestPanelLogicalDeleteHoldRejectionRestoresWithoutRelease(t *testing.T) {
	s, storeFixture, routerFixture, deleteRequest := logicalDeletePanelFixture(t, "hold-reject")
	cookie, csrf := login(t, s)
	body, _ := json.Marshal(deleteRequest)
	response := request(s, http.MethodPost, string(body), "/api/v2/logical-dialog-deletes", cookie, csrf)
	if response.Code != http.StatusOK || storeFixture.current.Phase != "failed" || storeFixture.current.HoldVersion != 0 ||
		routerFixture.installCalls != 1 || routerFixture.releaseCalls != 0 || routerFixture.deleteCalls != 0 ||
		strings.Join(storeFixture.actions, ",") != "reject" {
		t.Fatalf("hold reject status=%d state=%+v actions=%v installs=%d deletes=%d releases=%d body=%s", response.Code,
			storeFixture.current, storeFixture.actions, routerFixture.installCalls, routerFixture.deleteCalls,
			routerFixture.releaseCalls, response.Body.String())
	}
}

func TestPanelLogicalDeleteGETNeverStartsOrReplaysEffect(t *testing.T) {
	s, storeFixture, routerFixture, deleteRequest := logicalDeletePanelFixture(t, "success")
	cookie, _ := login(t, s)
	response := request(s, http.MethodGet, "", "/api/v2/logical-dialog-deletes/"+deleteRequest.OperationID, cookie, "")
	if response.Code != http.StatusOK || storeFixture.current.Phase != "accepted" || routerFixture.installCalls != 0 ||
		routerFixture.statusCalls != 0 || routerFixture.deleteCalls != 0 || routerFixture.releaseCalls != 0 || len(storeFixture.actions) != 0 {
		t.Fatalf("status-only GET status=%d state=%+v actions=%v installs=%d statusReads=%d deletes=%d releases=%d body=%s",
			response.Code, storeFixture.current, storeFixture.actions, routerFixture.installCalls, routerFixture.statusCalls,
			routerFixture.deleteCalls, routerFixture.releaseCalls, response.Body.String())
	}
}

func TestPanelLogicalDeleteGETIsReadOnlyBeforePOSTReplaysCancellation(t *testing.T) {
	s, storeFixture, routerFixture, deleteRequest := logicalDeletePanelFixture(t, "reject")
	storeFixture.rejectAdvanceFailures = 1
	cookie, csrf := login(t, s)
	body, _ := json.Marshal(deleteRequest)
	started := request(s, http.MethodPost, string(body), "/api/v2/logical-dialog-deletes", cookie, csrf)
	if started.Code != http.StatusServiceUnavailable || storeFixture.current.Phase != "holding" || routerFixture.releaseCalls != 1 {
		t.Fatalf("lost reject advance status=%d state=%+v releases=%d body=%s", started.Code,
			storeFixture.current, routerFixture.releaseCalls, started.Body.String())
	}
	readback := request(s, http.MethodGet, "", "/api/v2/logical-dialog-deletes/"+deleteRequest.OperationID, cookie, "")
	if readback.Code != http.StatusOK || storeFixture.current.Phase != "holding" || routerFixture.releaseCalls != 1 ||
		routerFixture.deleteCalls != 1 || strings.Join(storeFixture.actions, ",") != "hold" {
		t.Fatalf("read-only status=%d state=%+v actions=%v deletes=%d releases=%d body=%s", readback.Code,
			storeFixture.current, storeFixture.actions, routerFixture.deleteCalls, routerFixture.releaseCalls, readback.Body.String())
	}
	resolved := request(s, http.MethodPost, string(body), "/api/v2/logical-dialog-deletes", cookie, csrf)
	if resolved.Code != http.StatusOK || storeFixture.current.Phase != "failed" || routerFixture.releaseCalls != 2 ||
		routerFixture.deleteCalls != 1 || strings.Join(storeFixture.actions, ",") != "hold,reject" {
		t.Fatalf("POST replay reject status=%d state=%+v actions=%v deletes=%d releases=%d body=%s", resolved.Code,
			storeFixture.current, storeFixture.actions, routerFixture.deleteCalls, routerFixture.releaseCalls, resolved.Body.String())
	}
}

func TestPanelBrowserCannotBypassLogicalDeleteCoordinator(t *testing.T) {
	s, _, routerFixture, _ := logicalDeletePanelFixture(t, "success")
	cookie, csrf := login(t, s)
	command := `{"protocolVersion":1,"schemaId":"harness-wire-v2","commandId":"92000000-0000-4000-8000-000000000001","kind":"dialog.delete","target":{"nodeId":"20000000-0000-4000-8000-000000000001","dialogId":"92000000-0000-4000-8000-000000000002"},"expected":{"dialogVersion":1},"payload":{}}`
	response := request(s, http.MethodPost, command, harnessPath+"/commands", cookie, csrf)
	if response.Code != http.StatusForbidden || routerFixture.deleteCalls != 0 {
		t.Fatalf("browser bypass status=%d deletes=%d body=%s", response.Code, routerFixture.deleteCalls, response.Body.String())
	}
}
