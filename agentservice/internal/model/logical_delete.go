package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const (
	LogicalDeleteSchemaID        = "logical-dialog-delete-v1"
	LogicalDeleteAdvanceSchemaID = "logical-dialog-delete-advance-v1"
	LogicalDeleteNodeSchemaID    = "logical-dialog-node-delete-v1"
)

type LogicalDeleteRequest struct {
	SchemaID               string `json:"schemaId"`
	OperationID            string `json:"operationId"`
	CommandID              string `json:"commandId"`
	LogicalDialogID        string `json:"logicalDialogId"`
	ExpectedBindingVersion int64  `json:"expectedBindingVersion"`
	ExpectedDialogVersion  int64  `json:"expectedDialogVersion"`
}

type LogicalDeleteStatus struct {
	SchemaID               string                    `json:"schemaId"`
	OperationID            string                    `json:"operationId"`
	RequestHash            string                    `json:"requestHash"`
	CommandID              string                    `json:"commandId"`
	LogicalDialogID        string                    `json:"logicalDialogId"`
	ExpectedBindingVersion int64                     `json:"expectedBindingVersion"`
	ExpectedDialogVersion  int64                     `json:"expectedDialogVersion"`
	NodeID                 string                    `json:"nodeId"`
	NodeDialogID           string                    `json:"nodeDialogId"`
	RegistryVersion        int64                     `json:"registryVersion"`
	IdentityEpoch          int64                     `json:"identityEpoch"`
	HoldScopeRevision      int64                     `json:"holdScopeRevision"`
	HoldVersion            int64                     `json:"holdVersion,omitempty"`
	Phase                  string                    `json:"phase"`
	EffectState            string                    `json:"effectState"`
	OperationVersion       int64                     `json:"operationVersion"`
	Archived               bool                      `json:"archived"`
	ResultCode             string                    `json:"resultCode,omitempty"`
	NodeReceipt            *LogicalDeleteNodeReceipt `json:"nodeReceipt,omitempty"`
	UpdatedAt              string                    `json:"updatedAt"`
}

type LogicalDeleteAdvance struct {
	SchemaID                  string                    `json:"schemaId"`
	OperationID               string                    `json:"operationId"`
	RequestHash               string                    `json:"requestHash"`
	ExpectedOperationVersion  int64                     `json:"expectedOperationVersion"`
	Action                    string                    `json:"action"`
	HoldVersion               int64                     `json:"holdVersion"`
	ObservedHoldScopeRevision int64                     `json:"observedHoldScopeRevision"`
	ResultCode                string                    `json:"resultCode"`
	NodeReceipt               *LogicalDeleteNodeReceipt `json:"nodeReceipt"`
}

type LogicalDeleteNodeReceipt struct {
	SchemaID               string          `json:"schemaId"`
	OperationID            string          `json:"operationId"`
	NodeRequestHash        string          `json:"nodeRequestHash"`
	CoordinatorRequestHash string          `json:"coordinatorRequestHash"`
	ReceiptID              string          `json:"receiptId"`
	CommandID              string          `json:"commandId"`
	LogicalDialogID        string          `json:"logicalDialogId"`
	NodeID                 string          `json:"nodeId"`
	NodeDialogID           string          `json:"nodeDialogId"`
	Epoch                  int64           `json:"epoch"`
	RegistryVersion        int64           `json:"registryVersion"`
	BindingVersion         int64           `json:"bindingVersion"`
	DeletedDialogVersion   int64           `json:"deletedDialogVersion"`
	HoldVersion            int64           `json:"holdVersion"`
	HoldScopeRevision      int64           `json:"holdScopeRevision"`
	TombstoneEventSeq      int64           `json:"tombstoneEventSeq"`
	CommandReceipt         json.RawMessage `json:"commandReceipt"`
	DeletedAt              string          `json:"deletedAt"`
}

type LogicalDialogClosure struct {
	LogicalDialogID       string          `json:"logicalDialogId"`
	BindingVersion        int64           `json:"bindingVersion"`
	NodeID                string          `json:"nodeId"`
	NodeDialogID          string          `json:"nodeDialogId"`
	DialogVersion         int64           `json:"dialogVersion"`
	DescriptorVersion     int64           `json:"descriptorVersion"`
	ZeroPending           bool            `json:"zeroPending"`
	FinalCheckpoint       json.RawMessage `json:"finalCheckpoint"`
	FinalCheckpointSHA256 string          `json:"finalCheckpointSHA256"`
	VerifiedAt            string          `json:"verifiedAt"`
}

func ValidateLogicalDeleteRequest(value LogicalDeleteRequest) error {
	if value.SchemaID != LogicalDeleteSchemaID || !ValidUUID(value.OperationID) || !ValidUUID(value.CommandID) ||
		!ValidUUID(value.LogicalDialogID) || !validLogicalDeleteNumber(value.ExpectedBindingVersion) ||
		!incrementableLogicalDeleteNumber(value.ExpectedDialogVersion) {
		return errors.New("logical delete request is invalid")
	}
	return nil
}

func LogicalDeleteRequestHash(value LogicalDeleteRequest) (string, error) {
	if err := ValidateLogicalDeleteRequest(value); err != nil {
		return "", err
	}
	return logicalDeleteHash(value)
}

func ValidateLogicalDeleteStatus(value LogicalDeleteStatus) error {
	if value.SchemaID != LogicalDeleteSchemaID || !ValidUUID(value.OperationID) || !ValidSHA256(value.RequestHash) ||
		!ValidUUID(value.CommandID) || !ValidUUID(value.LogicalDialogID) || !validLogicalDeleteNumber(value.ExpectedBindingVersion) ||
		!incrementableLogicalDeleteNumber(value.ExpectedDialogVersion) || !ValidUUID(value.NodeID) || !ValidUUID(value.NodeDialogID) ||
		!validLogicalDeleteNumber(value.RegistryVersion) || !validLogicalDeleteNumber(value.IdentityEpoch) ||
		value.HoldScopeRevision < 0 || value.HoldScopeRevision > MaximumSafeInt || !validLogicalDeleteNumber(value.OperationVersion) ||
		!logicalDeleteOneOf(value.Phase, "accepted", "holding", "reconciling", "succeeded", "failed") ||
		!logicalDeleteOneOf(value.EffectState, "not_sent", "sent", "unknown", "reconciled", "failed") {
		return errors.New("logical delete status is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.UpdatedAt); err != nil {
		return errors.New("logical delete status time is invalid")
	}
	switch value.Phase {
	case "accepted":
		if value.EffectState != "not_sent" || value.HoldVersion != 0 || value.Archived || value.ResultCode != "" || value.NodeReceipt != nil {
			return errors.New("logical delete accepted state is invalid")
		}
	case "holding":
		if value.EffectState != "sent" || !validLogicalDeleteNumber(value.HoldVersion) || value.Archived || value.ResultCode != "" || value.NodeReceipt != nil {
			return errors.New("logical delete hold is invalid")
		}
	case "reconciling":
		if value.EffectState != "unknown" || !validLogicalDeleteNumber(value.HoldVersion) || value.Archived || value.ResultCode != "" || value.NodeReceipt != nil {
			return errors.New("logical delete reconciliation state is invalid")
		}
	case "succeeded":
		if value.EffectState != "reconciled" || !ValidActor(value.ResultCode) {
			return errors.New("logical delete success state is invalid")
		}
		if value.Archived {
			if value.HoldVersion != 0 || value.NodeReceipt != nil || value.ResultCode != "archive_tombstoned" {
				return errors.New("logical delete archive state is invalid")
			}
		} else if !validLogicalDeleteNumber(value.HoldVersion) || value.NodeReceipt == nil ||
			!logicalDeleteStatusReceiptMatches(value, *value.NodeReceipt) || value.ResultCode != "node_tombstoned" {
			return errors.New("logical delete node receipt is missing")
		}
	case "failed":
		if value.EffectState != "failed" || value.HoldVersion < 0 || value.HoldVersion > MaximumSafeInt ||
			value.Archived || !ValidActor(value.ResultCode) || value.NodeReceipt != nil {
			return errors.New("logical delete failure state is invalid")
		}
	}
	return nil
}

func logicalDeleteStatusReceiptMatches(status LogicalDeleteStatus, receipt LogicalDeleteNodeReceipt) bool {
	return ValidateLogicalDeleteNodeReceipt(receipt) == nil && receipt.OperationID == status.OperationID &&
		receipt.CoordinatorRequestHash == status.RequestHash && receipt.CommandID == status.CommandID &&
		receipt.LogicalDialogID == status.LogicalDialogID && receipt.NodeID == status.NodeID &&
		receipt.NodeDialogID == status.NodeDialogID && receipt.Epoch == status.IdentityEpoch &&
		receipt.RegistryVersion == status.RegistryVersion && receipt.BindingVersion == status.ExpectedBindingVersion &&
		receipt.DeletedDialogVersion == status.ExpectedDialogVersion+1 && receipt.HoldVersion == status.HoldVersion &&
		receipt.HoldScopeRevision == status.HoldScopeRevision
}

func ValidateLogicalDeleteAdvance(value LogicalDeleteAdvance) error {
	if value.SchemaID != LogicalDeleteAdvanceSchemaID || !ValidUUID(value.OperationID) || !ValidSHA256(value.RequestHash) ||
		!validLogicalDeleteNumber(value.ExpectedOperationVersion) ||
		!logicalDeleteOneOf(value.Action, "hold", "unknown", "complete", "reject") {
		return errors.New("logical delete advance is invalid")
	}
	switch value.Action {
	case "hold":
		if !validLogicalDeleteNumber(value.HoldVersion) || !validLogicalDeleteNumber(value.ObservedHoldScopeRevision) || value.NodeReceipt != nil || value.ResultCode != "" {
			return errors.New("logical delete hold advance is invalid")
		}
	case "unknown":
		if value.HoldVersion != 0 || value.ObservedHoldScopeRevision != 0 || value.NodeReceipt != nil || value.ResultCode != "" {
			return errors.New("logical delete unknown advance is invalid")
		}
	case "complete":
		if value.HoldVersion != 0 || value.ObservedHoldScopeRevision != 0 || value.NodeReceipt == nil || ValidateLogicalDeleteNodeReceipt(*value.NodeReceipt) != nil || value.ResultCode != "" {
			return errors.New("logical delete completion is invalid")
		}
	case "reject":
		if value.HoldVersion != 0 || value.NodeReceipt != nil || !ValidActor(value.ResultCode) ||
			value.ObservedHoldScopeRevision < 0 || value.ObservedHoldScopeRevision > MaximumSafeInt {
			return errors.New("logical delete rejection is invalid")
		}
	}
	return nil
}

func ValidateLogicalDeleteNodeReceipt(value LogicalDeleteNodeReceipt) error {
	if value.SchemaID != LogicalDeleteNodeSchemaID || !ValidUUID(value.OperationID) || !ValidSHA256(value.NodeRequestHash) ||
		!ValidSHA256(value.CoordinatorRequestHash) || !ValidUUID(value.ReceiptID) || !ValidUUID(value.CommandID) ||
		!ValidUUID(value.LogicalDialogID) || !ValidUUID(value.NodeID) || !ValidUUID(value.NodeDialogID) ||
		!validLogicalDeleteNumber(value.Epoch) || !validLogicalDeleteNumber(value.RegistryVersion) ||
		!validLogicalDeleteNumber(value.BindingVersion) || !validLogicalDeleteNumber(value.DeletedDialogVersion) ||
		!validLogicalDeleteNumber(value.HoldVersion) || !validLogicalDeleteNumber(value.HoldScopeRevision) ||
		!validLogicalDeleteNumber(value.TombstoneEventSeq) || !validLogicalDeleteCommandReceipt(value) {
		return errors.New("node logical delete receipt is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.DeletedAt); err != nil {
		return errors.New("node logical delete receipt time is invalid")
	}
	return nil
}

func validLogicalDeleteCommandReceipt(value LogicalDeleteNodeReceipt) bool {
	var receipt struct {
		ProtocolVersion int             `json:"protocolVersion"`
		SchemaID        string          `json:"schemaId"`
		CommandID       string          `json:"commandId"`
		CommandKind     string          `json:"commandKind"`
		ReceiptID       string          `json:"receiptId"`
		AcceptedAt      string          `json:"acceptedAt"`
		NodeID          string          `json:"nodeId"`
		EventSeq        int64           `json:"eventSeq"`
		Result          string          `json:"result"`
		BlockingReason  string          `json:"blockingReason,omitempty"`
		References      json.RawMessage `json:"references"`
	}
	decoder := json.NewDecoder(bytes.NewReader(value.CommandReceipt))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || receipt.ProtocolVersion != 1 ||
		receipt.SchemaID != "harness-wire-v2" || receipt.CommandID != value.CommandID || receipt.CommandKind != "dialog.delete" ||
		!ValidUUID(receipt.ReceiptID) || receipt.AcceptedAt != value.DeletedAt || receipt.NodeID != value.NodeID ||
		receipt.EventSeq != value.TombstoneEventSeq || receipt.Result != "deleted" || receipt.BlockingReason != "" {
		return false
	}
	var references struct {
		DialogID string `json:"dialogId"`
	}
	decoder = json.NewDecoder(bytes.NewReader(receipt.References))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&references) == nil && decoder.Decode(new(any)) == io.EOF && references.DialogID == value.NodeDialogID
}

func ValidateLogicalDialogClosure(value LogicalDialogClosure) error {
	if !ValidUUID(value.LogicalDialogID) || !validLogicalDeleteNumber(value.BindingVersion) || !ValidUUID(value.NodeID) ||
		!ValidUUID(value.NodeDialogID) || !validLogicalDeleteNumber(value.DialogVersion) ||
		!validLogicalDeleteNumber(value.DescriptorVersion) || !value.ZeroPending || len(value.FinalCheckpoint) == 0 ||
		!ValidSHA256(value.FinalCheckpointSHA256) {
		return errors.New("logical dialog closure is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.VerifiedAt); err != nil {
		return errors.New("logical dialog closure time is invalid")
	}
	digest := sha256.Sum256(value.FinalCheckpoint)
	if hex.EncodeToString(digest[:]) != value.FinalCheckpointSHA256 {
		return errors.New("logical dialog closure checkpoint hash is invalid")
	}
	return nil
}

func validLogicalDeleteNumber(value int64) bool { return value >= 1 && value <= MaximumSafeInt }

func incrementableLogicalDeleteNumber(value int64) bool { return value >= 1 && value < MaximumSafeInt }

func logicalDeleteOneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func logicalDeleteHash(value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}
