package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/logicaldelete"
)

type logicalDeleteBackend interface {
	BeginLogicalDelete(context.Context, string, logicaldelete.Request) (logicaldelete.Status, error)
	GetLogicalDelete(context.Context, string, string) (logicaldelete.Status, error)
	AdvanceLogicalDelete(context.Context, string, logicaldelete.AdvanceRequest) (logicaldelete.Status, error)
}

func (s *Server) logicalDeleteHTTP(w http.ResponseWriter, r *http.Request, current session) bool {
	if r.URL.Path == "/api/v2/logical-dialog-deletes" && r.Method == http.MethodPost {
		if s.logicalDelete == nil || !s.cfg.HarnessCommands {
			fail(w, http.StatusForbidden, "logical_delete_disabled")
			return true
		}
		var request logicaldelete.Request
		if !decodeLimit(w, r, &request, 64<<10) || logicaldelete.ValidateRequest(request) != nil {
			return true
		}
		status, err := s.logicalDelete.BeginLogicalDelete(r.Context(), current.ownerID, request)
		if err != nil {
			logicalDeleteFailure(w, err)
			return true
		}
		status, err = s.driveLogicalDelete(r.Context(), current.ownerID, status)
		if err != nil {
			logicalDeleteFailure(w, err)
			return true
		}
		reply(w, http.StatusOK, status)
		return true
	}
	prefix := "/api/v2/logical-dialog-deletes/"
	if strings.HasPrefix(r.URL.Path, prefix) && r.Method == http.MethodGet {
		if s.logicalDelete == nil || r.URL.RawQuery != "" {
			applyNotFound(w)
			return true
		}
		operationID := strings.TrimPrefix(r.URL.Path, prefix)
		status, err := s.logicalDelete.GetLogicalDelete(r.Context(), current.ownerID, operationID)
		if err != nil {
			logicalDeleteFailure(w, err)
			return true
		}
		status, err = s.reconcileLogicalDelete(r.Context(), current.ownerID, status)
		if err != nil {
			logicalDeleteFailure(w, err)
			return true
		}
		reply(w, http.StatusOK, status)
		return true
	}
	return false
}

// reconcileLogicalDelete performs readback only. A browser GET never installs
// or releases a hold and never sends the delete effect. Continuing an accepted
// or uncommitted holding operation requires exact replay through the
// CSRF-protected POST endpoint.
func (s *Server) reconcileLogicalDelete(ctx context.Context, owner string, status logicaldelete.Status) (logicaldelete.Status, error) {
	if status.Phase != "holding" && status.Phase != "reconciling" {
		return status, nil
	}
	response, err := s.router.LogicalDeleteStatus(ctx, status.NodeID, owner, status.OperationID)
	if err != nil {
		return status, err
	}
	if response.Status == http.StatusNotFound {
		return status, nil
	}
	if response.Status != http.StatusOK {
		return status, &harnessclient.Fault{Status: response.Status, Code: "node_unavailable"}
	}
	return s.completeLogicalDelete(ctx, owner, status, response.Body)
}

func (s *Server) driveLogicalDelete(ctx context.Context, owner string, status logicaldelete.Status) (logicaldelete.Status, error) {
	if status.Phase == "succeeded" || status.Phase == "failed" {
		return status, nil
	}
	if status.Phase == "accepted" {
		scope := harnessbarrier.Scope{Kind: "dialog", DialogID: status.NodeDialogID}
		request := harnessbarrier.InstallRequest{
			ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID,
			OperationID: status.OperationID, NodeID: status.NodeID, ExpectedEpoch: status.IdentityEpoch,
			BindingGeneration: status.RegistryVersion, Scope: scope, ExpectedScopeRevision: status.HoldScopeRevision,
		}
		raw, _ := json.Marshal(request)
		response, err := s.router.InstallHold(ctx, status.NodeID, owner, raw)
		if err != nil {
			return status, err
		}
		if response.Status >= http.StatusBadRequest && response.Status < http.StatusInternalServerError {
			return s.rejectLogicalDeleteBeforeHold(ctx, owner, status, "hold_rejected")
		}
		if response.Status != http.StatusCreated && response.Status != http.StatusOK {
			return status, &harnessclient.Fault{Status: response.Status, Code: "stale"}
		}
		var receipt harnessbarrier.HoldReceipt
		if json.Unmarshal(response.Body, &receipt) != nil || harnessbarrier.Validate("holdReceipt", response.Body) != nil ||
			receipt.OperationID != status.OperationID || receipt.NodeID != status.NodeID || receipt.Scope != scope ||
			receipt.Epoch != status.IdentityEpoch || receipt.BindingGeneration != status.RegistryVersion ||
			receipt.ScopeRevision != status.HoldScopeRevision+1 {
			return status, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "schema_mismatch"}
		}
		status, err = s.logicalDelete.AdvanceLogicalDelete(ctx, owner, logicaldelete.AdvanceRequest{
			SchemaID: logicaldelete.AdvanceSchemaID, OperationID: status.OperationID, RequestHash: status.RequestHash,
			ExpectedOperationVersion: status.OperationVersion, Action: "hold", HoldVersion: receipt.HoldVersion,
			ObservedHoldScopeRevision: receipt.ScopeRevision,
		})
		if err != nil {
			return status, err
		}
	}
	if status.Phase == "reconciling" {
		response, err := s.router.LogicalDeleteStatus(ctx, status.NodeID, owner, status.OperationID)
		if err != nil || response.Status != http.StatusOK {
			return status, nil
		}
		return s.completeLogicalDelete(ctx, owner, status, response.Body)
	}
	if status.Phase != "holding" {
		return status, nil
	}
	readback, err := s.router.LogicalDeleteStatus(ctx, status.NodeID, owner, status.OperationID)
	if err != nil {
		return status, err
	}
	if readback.Status == http.StatusOK {
		return s.completeLogicalDelete(ctx, owner, status, readback.Body)
	}
	if readback.Status != http.StatusNotFound {
		return status, &harnessclient.Fault{Status: readback.Status, Code: "node_unavailable"}
	}
	proofResponse, err := s.router.QuiescenceProof(ctx, status.NodeID, owner, status.OperationID)
	if err != nil {
		return status, err
	}
	if proofResponse.Status == http.StatusConflict {
		return s.rejectLogicalDelete(ctx, owner, status, "quiescence_rejected")
	}
	var proof harnessbarrier.QuiescenceProof
	if proofResponse.Status != http.StatusOK || json.Unmarshal(proofResponse.Body, &proof) != nil ||
		harnessbarrier.Validate("quiescenceProof", proofResponse.Body) != nil || proof.OperationID != status.OperationID ||
		proof.NodeID != status.NodeID || proof.Scope.Kind != "dialog" || proof.Scope.DialogID != status.NodeDialogID ||
		proof.HoldVersion != status.HoldVersion || proof.ScopeRevision != status.HoldScopeRevision ||
		proof.Epoch != status.IdentityEpoch || proof.BindingGeneration != status.RegistryVersion {
		return status, &harnessclient.Fault{Status: http.StatusConflict, Code: "stale"}
	}
	nodeRequest := logicaldelete.NodeRequest{
		SchemaID: logicaldelete.NodeSchemaID, OperationID: status.OperationID,
		CoordinatorRequestHash: status.RequestHash, CommandID: status.CommandID,
		LogicalDialogID: status.LogicalDialogID, NodeID: status.NodeID, NodeDialogID: status.NodeDialogID,
		ExpectedEpoch: status.IdentityEpoch, RegistryVersion: status.RegistryVersion,
		BindingVersion: status.ExpectedBindingVersion, ExpectedDialogVersion: status.ExpectedDialogVersion,
		HoldVersion: status.HoldVersion, HoldScopeRevision: status.HoldScopeRevision,
	}
	response, sendErr := s.router.DeleteLogicalDialog(ctx, status.NodeID, owner, nodeRequest)
	if sendErr != nil || response.Status >= http.StatusInternalServerError {
		advanced, advanceErr := s.logicalDelete.AdvanceLogicalDelete(ctx, owner, logicaldelete.AdvanceRequest{
			SchemaID: logicaldelete.AdvanceSchemaID, OperationID: status.OperationID, RequestHash: status.RequestHash,
			ExpectedOperationVersion: status.OperationVersion, Action: "unknown",
		})
		if advanceErr != nil {
			return status, advanceErr
		}
		return advanced, nil
	}
	if response.Status == http.StatusCreated || response.Status == http.StatusOK {
		return s.completeLogicalDelete(ctx, owner, status, response.Body)
	}
	if response.Status == http.StatusConflict || response.Status == http.StatusNotFound {
		return s.rejectLogicalDelete(ctx, owner, status, "node_guard_rejected")
	}
	return status, &harnessclient.Fault{Status: response.Status, Code: "stale"}
}

func (s *Server) rejectLogicalDeleteBeforeHold(ctx context.Context, owner string, status logicaldelete.Status, code string) (logicaldelete.Status, error) {
	return s.logicalDelete.AdvanceLogicalDelete(ctx, owner, logicaldelete.AdvanceRequest{
		SchemaID: logicaldelete.AdvanceSchemaID, OperationID: status.OperationID, RequestHash: status.RequestHash,
		ExpectedOperationVersion: status.OperationVersion, Action: "reject", ResultCode: code,
		ObservedHoldScopeRevision: status.HoldScopeRevision,
	})
}

func (s *Server) completeLogicalDelete(ctx context.Context, owner string, status logicaldelete.Status, raw []byte) (logicaldelete.Status, error) {
	var receipt logicaldelete.NodeReceipt
	if json.Unmarshal(raw, &receipt) != nil || logicaldelete.ValidateNodeReceipt(receipt) != nil {
		return status, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "schema_mismatch"}
	}
	return s.logicalDelete.AdvanceLogicalDelete(ctx, owner, logicaldelete.AdvanceRequest{
		SchemaID: logicaldelete.AdvanceSchemaID, OperationID: status.OperationID, RequestHash: status.RequestHash,
		ExpectedOperationVersion: status.OperationVersion, Action: "complete", NodeReceipt: &receipt,
	})
}

func (s *Server) rejectLogicalDelete(ctx context.Context, owner string, status logicaldelete.Status, code string) (logicaldelete.Status, error) {
	scope := harnessbarrier.Scope{Kind: "dialog", DialogID: status.NodeDialogID}
	release := harnessbarrier.ReleaseRequest{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID,
		OperationID: status.OperationID, NodeID: status.NodeID, ExpectedEpoch: status.IdentityEpoch,
		BindingGeneration: status.RegistryVersion, Scope: scope, HoldVersion: status.HoldVersion,
		ExpectedScopeRevision: status.HoldScopeRevision, Action: "cancel",
	}
	raw, _ := json.Marshal(release)
	response, err := s.router.ReleaseHold(ctx, status.NodeID, owner, raw)
	if err != nil {
		return status, err
	}
	var receipt harnessbarrier.ReleaseReceipt
	if (response.Status != http.StatusCreated && response.Status != http.StatusOK) || json.Unmarshal(response.Body, &receipt) != nil ||
		harnessbarrier.Validate("releaseReceipt", response.Body) != nil || receipt.Kind != "hold.cancelled" ||
		receipt.OperationID != status.OperationID || receipt.NodeID != status.NodeID || receipt.Epoch != status.IdentityEpoch ||
		receipt.BindingGeneration != status.RegistryVersion || receipt.Scope != scope || receipt.HoldVersion != status.HoldVersion ||
		receipt.ScopeRevision != status.HoldScopeRevision+1 {
		return status, &harnessclient.Fault{Status: http.StatusServiceUnavailable, Code: "node_unavailable"}
	}
	return s.logicalDelete.AdvanceLogicalDelete(ctx, owner, logicaldelete.AdvanceRequest{
		SchemaID: logicaldelete.AdvanceSchemaID, OperationID: status.OperationID, RequestHash: status.RequestHash,
		ExpectedOperationVersion: status.OperationVersion, Action: "reject", ResultCode: code,
		ObservedHoldScopeRevision: receipt.ScopeRevision,
	})
}

func logicalDeleteFailure(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "logical_delete_unavailable"
	var fault *agentserviceclient.Fault
	if errors.As(err, &fault) {
		status, code = fault.Status, fault.Code
	}
	var nodeFault *harnessclient.Fault
	if errors.As(err, &nodeFault) {
		status, code = nodeFault.Status, nodeFault.Code
	}
	reply(w, status, map[string]string{"error": code})
}
