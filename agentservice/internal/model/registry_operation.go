package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"
)

const (
	RegistryOperationSchemaID        = "agent-registry-operation-v1"
	RegistryOperationReceiptSchemaID = "agent-registry-operation-receipt-v1"
	RegistryOperationStatusSchemaID  = "agent-registry-operation-status-v1"
	RegistryOperationCommandSchemaID = "agent-registry-operation-command-v1"
	RegistryOperationFinishSchemaID  = "agent-registry-operation-finish-v1"
	RegistryOperationFailureSchemaID = "agent-registry-operation-failure-v1"
)

type RegistryExpected struct {
	RegistryVersion int64  `json:"registryVersion"`
	RegistrySHA256  string `json:"registrySHA256"`
}

type RegistryOperationIntent struct {
	SchemaID        string                   `json:"schemaId"`
	OperationID     string                   `json:"operationId"`
	Expected        RegistryExpected         `json:"expected"`
	Registry        json.RawMessage          `json:"registry"`
	NewNodeHostID   *string                  `json:"newNodeHostId"`
	ExternalBinding *ExternalEndpointBinding `json:"externalBinding,omitempty"`
}

type RegistryOperationReceipt struct {
	SchemaID                 string   `json:"schemaId"`
	OperationID              string   `json:"operationId"`
	RequestHash              string   `json:"requestHash"`
	ExpectedRegistryVersion  int64    `json:"expectedRegistryVersion"`
	ExpectedRegistrySHA256   string   `json:"expectedRegistrySHA256"`
	CandidateRegistryVersion int64    `json:"candidateRegistryVersion"`
	CandidateRegistrySHA256  string   `json:"candidateRegistrySHA256"`
	AffectedNodeIDs          []string `json:"affectedNodeIds"`
	AcceptedAt               string   `json:"acceptedAt"`
}

type RegistryOperationStatus struct {
	SchemaID         string                   `json:"schemaId"`
	Receipt          RegistryOperationReceipt `json:"receipt"`
	Phase            string                   `json:"phase"`
	EffectState      string                   `json:"effectState"`
	OperationVersion int64                    `json:"operationVersion"`
	UpdatedAt        string                   `json:"updatedAt"`
	ResultCode       *string                  `json:"resultCode"`
}

type RegistryOperationCommand struct {
	SchemaID         string `json:"schemaId"`
	OperationID      string `json:"operationId"`
	RequestHash      string `json:"requestHash"`
	OperationVersion int64  `json:"operationVersion"`
}

type RegistryOperationFinish struct {
	SchemaID         string `json:"schemaId"`
	OperationID      string `json:"operationId"`
	RequestHash      string `json:"requestHash"`
	OperationVersion int64  `json:"operationVersion"`
	RegistryVersion  int64  `json:"registryVersion"`
	RegistrySHA256   string `json:"registrySHA256"`
	EffectState      string `json:"effectState"`
}

type RegistryOperationFailure struct {
	SchemaID         string `json:"schemaId"`
	OperationID      string `json:"operationId"`
	RequestHash      string `json:"requestHash"`
	OperationVersion int64  `json:"operationVersion"`
	ResultCode       string `json:"resultCode"`
}

func ValidateRegistryOperationIntent(value RegistryOperationIntent) error {
	if value.SchemaID != RegistryOperationSchemaID || !ValidActor(value.OperationID) ||
		value.Expected.RegistryVersion < 1 || value.Expected.RegistryVersion >= MaximumSafeInt ||
		!sha256Pattern.MatchString(value.Expected.RegistrySHA256) || len(value.Registry) == 0 || len(value.Registry) > 256<<10 ||
		(value.NewNodeHostID != nil && !ValidUUID(*value.NewNodeHostID)) ||
		(value.ExternalBinding != nil && externalBindingValid(*value.ExternalBinding) != nil) {
		return errors.New("invalid registry operation intent")
	}
	return nil
}

func RegistryOperationRequestHash(value RegistryOperationIntent) (string, error) {
	if err := ValidateRegistryOperationIntent(value); err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(value.Registry))
	decoder.UseNumber()
	var registryValue any
	if decoder.Decode(&registryValue) != nil || decoder.Decode(new(any)) != io.EOF {
		return "", errors.New("cannot encode registry operation intent")
	}
	canonicalRegistry, err := json.Marshal(registryValue)
	if err != nil {
		return "", errors.New("cannot encode registry operation intent")
	}
	canonical := value
	canonical.Registry = canonicalRegistry
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", errors.New("cannot encode registry operation intent")
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func ValidateRegistryOperationCommand(value RegistryOperationCommand) error {
	if value.SchemaID != RegistryOperationCommandSchemaID || !ValidActor(value.OperationID) ||
		!sha256Pattern.MatchString(value.RequestHash) || value.OperationVersion < 1 || value.OperationVersion > MaximumSafeInt {
		return errors.New("invalid registry operation command")
	}
	return nil
}

func ValidateRegistryOperationFinish(value RegistryOperationFinish) error {
	if value.SchemaID != RegistryOperationFinishSchemaID || !ValidActor(value.OperationID) ||
		!sha256Pattern.MatchString(value.RequestHash) || value.OperationVersion < 1 || value.OperationVersion > MaximumSafeInt ||
		value.RegistryVersion < 2 || value.RegistryVersion > MaximumSafeInt || !sha256Pattern.MatchString(value.RegistrySHA256) ||
		!slices.Contains([]string{"acknowledged", "reconciled"}, value.EffectState) {
		return errors.New("invalid registry operation finish")
	}
	return nil
}

func ValidateRegistryOperationFailure(value RegistryOperationFailure) error {
	if value.SchemaID != RegistryOperationFailureSchemaID || !ValidActor(value.OperationID) ||
		!sha256Pattern.MatchString(value.RequestHash) || value.OperationVersion < 1 ||
		value.OperationVersion > MaximumSafeInt || !ValidActor(value.ResultCode) {
		return errors.New("invalid registry operation failure")
	}
	return nil
}

func ValidateRegistryOperationStatus(value RegistryOperationStatus) error {
	if value.SchemaID != RegistryOperationStatusSchemaID || value.Receipt.SchemaID != RegistryOperationReceiptSchemaID ||
		!ValidActor(value.Receipt.OperationID) || !sha256Pattern.MatchString(value.Receipt.RequestHash) ||
		value.Receipt.ExpectedRegistryVersion < 1 || value.Receipt.CandidateRegistryVersion != value.Receipt.ExpectedRegistryVersion+1 ||
		!sha256Pattern.MatchString(value.Receipt.ExpectedRegistrySHA256) || !sha256Pattern.MatchString(value.Receipt.CandidateRegistrySHA256) ||
		len(value.Receipt.AffectedNodeIDs) == 0 || value.OperationVersion < 1 || value.OperationVersion > MaximumSafeInt ||
		!slices.Contains([]string{"accepted", "applying", "reconciling", "succeeded", "failed"}, value.Phase) ||
		!slices.Contains([]string{"not_sent", "sent", "unknown", "acknowledged", "reconciled", "failed"}, value.EffectState) ||
		(value.ResultCode != nil && !ValidActor(*value.ResultCode)) {
		return errors.New("invalid registry operation status")
	}
	for index, nodeID := range value.Receipt.AffectedNodeIDs {
		if !ValidUUID(nodeID) || index > 0 && nodeID <= value.Receipt.AffectedNodeIDs[index-1] {
			return errors.New("invalid registry operation node set")
		}
	}
	for _, timestamp := range []string{value.Receipt.AcceptedAt, value.UpdatedAt} {
		parsed, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil || timestamp != FormatOperationTime(parsed) {
			return errors.New("invalid registry operation timestamp")
		}
	}
	validPair := value.Phase == "accepted" && value.EffectState == "not_sent" ||
		value.Phase == "applying" && value.EffectState == "sent" ||
		value.Phase == "reconciling" && value.EffectState == "unknown" ||
		value.Phase == "succeeded" && (value.EffectState == "acknowledged" || value.EffectState == "reconciled") ||
		value.Phase == "failed" && value.EffectState == "failed"
	if !validPair {
		return errors.New("invalid registry operation result")
	}
	return nil
}
