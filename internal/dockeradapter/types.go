// Package dockeradapter implements the R02 durable effect boundary. It has no
// Docker client or lifecycle effects yet; those belong to later roadmap stages.
package dockeradapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	RequestSchemaID = "docker-adapter-request-v1"
	JournalSchemaID = "docker-adapter-journal-v1"
	lockSchemaID    = "docker-adapter-lock-v1"
	maximumSafeInt  = int64(1<<53 - 1)
)

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

var (
	ErrConflict               = errors.New("adapter operation conflict")
	ErrUnknownResult          = errors.New("adapter result is unknown")
	ErrReconciliationRequired = errors.New("adapter journal reconciliation required")
	ErrLockLost               = errors.New("adapter executor lock lost")
	ErrStaleAuthority         = errors.New("adapter operation authority is stale")
)

type Request struct {
	SchemaID    string   `json:"schemaId"`
	OperationID string   `json:"operationId"`
	StepID      string   `json:"stepId"`
	Generation  int64    `json:"generation"`
	Action      string   `json:"action"`
	ResourceIDs []string `json:"resourceIds"`
}

type Result struct {
	Outcome   string `json:"outcome"`
	ReceiptID string `json:"receiptId"`
}

type Execution struct {
	Result       Result
	Proof        AuthorityProof
	JournalState string
	Replayed     bool
}

// AuthorityProof identifies the exact Agent Service worker lease that is
// allowed to begin a new effect. It is checked immediately around the durable
// sent barrier and is deliberately not persisted in the journal.
type AuthorityProof struct {
	OperationID      string
	Generation       int64
	WorkerID         string
	WorkerToken      string
	OperationVersion int64
}

// Authority verifies the current DB-backed operation generation and worker
// lease. Real transport wiring is a later stage; R02 exercises the boundary
// with FixtureAuthority and never treats a caller-supplied proof as sufficient.
type Authority interface {
	VerifyActive(context.Context, string, string, Request, AuthorityProof) error
	RecordSent(context.Context, string, string, Request, AuthorityProof) (AuthorityProof, error)
}

type Entry struct {
	OperationID string   `json:"operationId"`
	StepID      string   `json:"stepId"`
	Generation  int64    `json:"generation"`
	RequestHash string   `json:"requestHash"`
	ResourceIDs []string `json:"resourceIds"`
	State       string   `json:"state"`
	Outcome     string   `json:"outcome,omitempty"`
	ReceiptID   string   `json:"receiptId,omitempty"`
	UpdatedAt   string   `json:"updatedAt"`
}

type journalState struct {
	SchemaID   string  `json:"schemaId"`
	DaemonID   string  `json:"daemonId"`
	InstanceID string  `json:"instanceId"`
	Entries    []Entry `json:"entries"`
}

type anchor struct {
	SchemaID   string `json:"schemaId"`
	DaemonID   string `json:"daemonId"`
	InstanceID string `json:"instanceId"`
	LockID     string `json:"lockId"`
}

type lockRecord struct {
	SchemaID      string `json:"schemaId"`
	DaemonID      string `json:"daemonId"`
	InstanceID    string `json:"instanceId"`
	LockID        string `json:"lockId"`
	JournalSHA256 string `json:"journalSHA256"`
}

func validateRequest(value Request) error {
	if value.SchemaID != RequestSchemaID || !identifier.MatchString(value.OperationID) ||
		!identifier.MatchString(value.StepID) || value.Generation < 1 || value.Generation > maximumSafeInt ||
		value.Action != "fixture.apply" || len(value.ResourceIDs) == 0 || len(value.ResourceIDs) > 32 ||
		!slices.IsSorted(value.ResourceIDs) {
		return errors.New("invalid adapter request")
	}
	for index, resourceID := range value.ResourceIDs {
		if !identifier.MatchString(resourceID) || (index > 0 && resourceID == value.ResourceIDs[index-1]) {
			return errors.New("invalid adapter resource identifier")
		}
	}
	return nil
}

func requestHash(value Request) (string, error) {
	if err := validateRequest(value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("cannot encode adapter request")
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func validateAuthorityProof(request Request, proof AuthorityProof) error {
	if proof.OperationID != request.OperationID || proof.Generation != request.Generation ||
		!identifier.MatchString(proof.WorkerID) || !identifier.MatchString(proof.WorkerToken) ||
		proof.OperationVersion < 1 || proof.OperationVersion > maximumSafeInt {
		return ErrStaleAuthority
	}
	return nil
}

func validResult(value Result) bool {
	return (value.Outcome == "applied" || value.Outcome == "not_applied" || value.Outcome == "failed") &&
		identifier.MatchString(value.ReceiptID)
}

func validateJournal(value journalState, daemonID, instanceID string) error {
	if value.SchemaID != JournalSchemaID || value.DaemonID != daemonID || value.InstanceID != instanceID ||
		value.Entries == nil || len(value.Entries) > 10_000 {
		return errors.New("invalid adapter journal")
	}
	seen := map[string]bool{}
	lastGeneration := int64(0)
	for index, entry := range value.Entries {
		key := entry.OperationID
		if seen[key] || !identifier.MatchString(entry.OperationID) || !identifier.MatchString(entry.StepID) ||
			entry.Generation < 1 || entry.Generation > maximumSafeInt || entry.Generation <= lastGeneration ||
			!sha256Hex.MatchString(entry.RequestHash) || len(entry.ResourceIDs) == 0 || len(entry.ResourceIDs) > 32 ||
			!slices.IsSorted(entry.ResourceIDs) || !slices.Contains([]string{"sent", "acknowledged", "reconciled", "failed"}, entry.State) {
			return errors.New("invalid adapter journal entry")
		}
		for index, resourceID := range entry.ResourceIDs {
			if !identifier.MatchString(resourceID) || (index > 0 && resourceID == entry.ResourceIDs[index-1]) {
				return errors.New("invalid adapter journal resource")
			}
		}
		if _, err := time.Parse(time.RFC3339Nano, entry.UpdatedAt); err != nil {
			return errors.New("invalid adapter journal time")
		}
		if entry.State == "sent" {
			if entry.Outcome != "" || entry.ReceiptID != "" || index != len(value.Entries)-1 {
				return errors.New("unresolved adapter entry has a result")
			}
		} else if !validResult(Result{Outcome: entry.Outcome, ReceiptID: entry.ReceiptID}) {
			return errors.New("resolved adapter entry has no result")
		}
		seen[key] = true
		lastGeneration = entry.Generation
	}
	return nil
}

func validIdentity(value string) bool {
	return identifier.MatchString(value) && !strings.ContainsAny(value, "/\\")
}
