package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

const (
	OperationSchemaID        = "agent-operation-v1"
	OperationReceiptSchemaID = "agent-operation-receipt-v1"
	OperationStatusSchemaID  = "agent-operation-status-v1"
	OperationTargetSchemaID  = "agent-operation-target-v1"
	OperationClaimSchemaID   = "agent-operation-claim-request-v1"
	OperationProofSchemaID   = "agent-operation-proof-v1"
	OperationWorkSchemaID    = "agent-operation-work-v1"
	OperationAuthoritySchema = "agent-operation-authority-v1"
	OperationSentSchemaID    = "agent-operation-sent-v1"
	OperationAdvanceSchemaID = "agent-operation-advance-v1"
)

type OperationTarget struct {
	NodeID               string `json:"nodeId"`
	HostID               string `json:"hostId"`
	RegistrationRevision int64  `json:"registrationRevision"`
	RegistrationEpoch    int64  `json:"registrationEpoch"`
	Generation           int64  `json:"generation"`
}

type OperationStepIntent struct {
	StepID      string   `json:"stepId"`
	Action      string   `json:"action"`
	ResourceIDs []string `json:"resourceIds"`
}

// OperationIntent is deliberately closed and contains no Docker parameters.
// Later lifecycle stages may introduce another version after their effects are
// specified; R02 proves durable acceptance and fencing with a controlled effect.
type OperationIntent struct {
	SchemaID    string              `json:"schemaId"`
	OperationID string              `json:"operationId"`
	Kind        string              `json:"kind"`
	Target      OperationTarget     `json:"target"`
	Step        OperationStepIntent `json:"step"`
}

type OperationReceipt struct {
	SchemaID           string          `json:"schemaId"`
	OperationID        string          `json:"operationId"`
	RequestHash        string          `json:"requestHash"`
	Target             OperationTarget `json:"target"`
	AcceptedGeneration int64           `json:"acceptedGeneration"`
	AcceptedAt         string          `json:"acceptedAt"`
}

type OperationStatus struct {
	SchemaID         string           `json:"schemaId"`
	Receipt          OperationReceipt `json:"receipt"`
	Phase            string           `json:"phase"`
	EffectState      string           `json:"effectState"`
	OperationVersion int64            `json:"operationVersion"`
	UpdatedAt        string           `json:"updatedAt"`
	ResultCode       *string          `json:"resultCode"`
}

// OperationTargetStatus is a separate R02 projection. Keeping it out of the
// R01 inventory schema preserves strict clients while still exposing the exact
// CAS coordinates needed to construct an operation.
type OperationTargetStatus struct {
	SchemaID         string          `json:"schemaId"`
	Target           OperationTarget `json:"target"`
	RegistryMode     string          `json:"registryMode"`
	RegistrationMode string          `json:"registrationMode"`
}

type OperationClaimRequest struct {
	SchemaID          string `json:"schemaId"`
	NodeID            string `json:"nodeId"`
	WorkerID          string `json:"workerId"`
	LeaseMilliseconds int64  `json:"leaseMilliseconds"`
}

type OperationClaimProof struct {
	SchemaID         string `json:"schemaId"`
	OperationID      string `json:"operationId"`
	RequestHash      string `json:"requestHash"`
	NodeID           string `json:"nodeId"`
	Generation       int64  `json:"generation"`
	WorkerID         string `json:"workerId"`
	WorkerToken      string `json:"workerToken"`
	OperationVersion int64  `json:"operationVersion"`
	LeaseExpiresAt   string `json:"leaseExpiresAt"`
}

type OperationWork struct {
	SchemaID    string              `json:"schemaId"`
	Intent      OperationIntent     `json:"intent"`
	Proof       OperationClaimProof `json:"proof"`
	EffectState string              `json:"effectState"`
}

type OperationAuthorityRequest struct {
	SchemaID string              `json:"schemaId"`
	Proof    OperationClaimProof `json:"proof"`
}

type OperationAuthority struct {
	SchemaID string `json:"schemaId"`
	Active   bool   `json:"active"`
}

type OperationSentRequest struct {
	SchemaID string              `json:"schemaId"`
	Proof    OperationClaimProof `json:"proof"`
}

type OperationAdvanceRequest struct {
	SchemaID    string              `json:"schemaId"`
	Proof       OperationClaimProof `json:"proof"`
	Phase       string              `json:"phase"`
	EffectState string              `json:"effectState"`
	ResultCode  *string             `json:"resultCode"`
}

func ValidateOperationIntent(value OperationIntent) error {
	if value.SchemaID != OperationSchemaID || !ValidActor(value.OperationID) || value.Kind != "adapter.fixture" ||
		!ValidUUID(value.Target.NodeID) || !ValidUUID(value.Target.HostID) || value.Target.RegistrationRevision < 1 ||
		value.Target.RegistrationRevision > MaximumSafeInt || value.Target.Generation < 0 ||
		value.Target.RegistrationEpoch < 1 || value.Target.RegistrationEpoch > MaximumSafeInt ||
		value.Target.Generation >= MaximumSafeInt || !ValidActor(value.Step.StepID) ||
		len(value.Step.ResourceIDs) == 0 || len(value.Step.ResourceIDs) > 32 {
		return errors.New("invalid operation intent")
	}
	if value.Step.Action != "adapter.fixture.apply" {
		return errors.New("operation kind and action do not match")
	}
	if !slices.IsSorted(value.Step.ResourceIDs) {
		return errors.New("operation resource identifiers must be sorted")
	}
	for index, resourceID := range value.Step.ResourceIDs {
		if !ValidActor(resourceID) || (index > 0 && resourceID == value.Step.ResourceIDs[index-1]) {
			return errors.New("invalid operation resource identifier")
		}
	}
	return nil
}

func ValidateOperationTargetStatus(value OperationTargetStatus) error {
	if value.SchemaID != OperationTargetSchemaID || !ValidUUID(value.Target.NodeID) || !ValidUUID(value.Target.HostID) ||
		value.Target.RegistrationRevision < 1 || value.Target.RegistrationRevision > MaximumSafeInt ||
		value.Target.RegistrationEpoch < 1 || value.Target.RegistrationEpoch > MaximumSafeInt ||
		value.Target.Generation < 0 || value.Target.Generation >= MaximumSafeInt ||
		(value.RegistryMode != "live" && value.RegistryMode != "fixture") ||
		(value.RegistrationMode != "compatible" && value.RegistrationMode != "legacy_readonly") {
		return errors.New("invalid operation target")
	}
	return nil
}

func ValidateOperationClaimRequest(value OperationClaimRequest) error {
	if value.SchemaID != OperationClaimSchemaID || !ValidUUID(value.NodeID) || !ValidActor(value.WorkerID) ||
		value.LeaseMilliseconds < 1000 || value.LeaseMilliseconds > 300000 {
		return errors.New("invalid operation claim")
	}
	return nil
}

func ValidateOperationClaimProof(value OperationClaimProof) error {
	if value.SchemaID != OperationProofSchemaID || !ValidActor(value.OperationID) || !sha256Pattern.MatchString(value.RequestHash) ||
		!ValidUUID(value.NodeID) || value.Generation < 1 || value.Generation > MaximumSafeInt ||
		!ValidActor(value.WorkerID) || !ValidUUID(value.WorkerToken) || value.OperationVersion < 1 ||
		value.OperationVersion > MaximumSafeInt {
		return errors.New("invalid operation claim proof")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value.LeaseExpiresAt); err != nil || value.LeaseExpiresAt != FormatOperationTime(parsed) {
		return errors.New("invalid operation claim expiry")
	}
	return nil
}

func ValidateOperationWork(value OperationWork) error {
	requestHash, err := OperationRequestHash(value.Intent)
	if value.SchemaID != OperationWorkSchemaID || err != nil || !ValidEffectState(value.EffectState) ||
		ValidateOperationClaimProof(value.Proof) != nil || value.Proof.OperationID != value.Intent.OperationID ||
		value.Proof.RequestHash != requestHash || value.Proof.NodeID != value.Intent.Target.NodeID ||
		value.Proof.Generation != value.Intent.Target.Generation+1 {
		return errors.New("invalid operation work")
	}
	return nil
}

func ValidateOperationAuthorityRequest(value OperationAuthorityRequest) error {
	if value.SchemaID != OperationAuthoritySchema || ValidateOperationClaimProof(value.Proof) != nil {
		return errors.New("invalid operation authority request")
	}
	return nil
}

func ValidateOperationSentRequest(value OperationSentRequest) error {
	if value.SchemaID != OperationSentSchemaID || ValidateOperationClaimProof(value.Proof) != nil {
		return errors.New("invalid operation sent request")
	}
	return nil
}

func ValidateOperationAdvanceRequest(value OperationAdvanceRequest) error {
	if value.SchemaID != OperationAdvanceSchemaID || ValidateOperationClaimProof(value.Proof) != nil ||
		!ValidOperationPhase(value.Phase) || !ValidEffectState(value.EffectState) ||
		(value.ResultCode != nil && !ValidActor(*value.ResultCode)) {
		return errors.New("invalid operation advance request")
	}
	return nil
}

// OperationRequestHash hashes the normalized, closed typed representation.
// Object key order and insignificant input whitespace therefore cannot create
// a second effect for an otherwise identical intent.
func OperationRequestHash(value OperationIntent) (string, error) {
	if err := ValidateOperationIntent(value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("cannot encode operation intent")
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func ValidOperationPhase(value string) bool {
	return slices.Contains([]string{
		"accepted", "validating", "waiting", "building", "preparing", "applying",
		"verifying", "reconciling", "succeeded", "failed",
	}, value)
}

func ValidEffectState(value string) bool {
	return slices.Contains([]string{"not_sent", "sent", "acknowledged", "reconciled", "unknown", "failed"}, value)
}

func TerminalOperationPhase(value string) bool {
	return value == "succeeded" || value == "failed"
}

func ValidOperationTransition(currentPhase, currentEffectState, nextPhase, nextEffectState string) bool {
	if !ValidOperationPhase(currentPhase) || !ValidOperationPhase(nextPhase) ||
		!ValidEffectState(currentEffectState) || !ValidEffectState(nextEffectState) ||
		TerminalOperationPhase(currentPhase) || !validEffectTransition(currentEffectState, nextEffectState) {
		return false
	}
	if nextEffectState == "unknown" {
		return nextPhase == "reconciling"
	}
	if nextPhase == "succeeded" {
		return nextEffectState == "acknowledged" || nextEffectState == "reconciled"
	}
	if nextPhase == "failed" {
		return nextEffectState == "failed"
	}
	order := map[string]int{
		"accepted": 0, "validating": 1, "waiting": 2, "building": 3, "preparing": 4,
		"applying": 5, "verifying": 6, "reconciling": 7,
	}
	current, currentOK := order[currentPhase]
	next, nextOK := order[nextPhase]
	return currentOK && nextOK && next >= current
}

func validEffectTransition(current, next string) bool {
	allowed := map[string][]string{
		"not_sent":     {"not_sent", "sent", "failed"},
		"sent":         {"sent", "acknowledged", "reconciled", "unknown", "failed"},
		"acknowledged": {"acknowledged", "reconciled", "failed"},
		"unknown":      {"unknown", "reconciled", "failed"},
		"reconciled":   {"reconciled"},
		"failed":       {"failed"},
	}
	return slices.Contains(allowed[current], next)
}

func FormatOperationTime(value time.Time) string {
	return value.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}
