package mobilegateway

import (
	"context"
	"errors"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
	"github.com/boxvtk621/homelab-telegram-panel/internal/observability"
)

// AuthAuditOutcome is the complete low-cardinality result vocabulary for one
// Telegram initData exchange. Detailed verifier/session reasons never cross
// this boundary.
type AuthAuditOutcome string

const (
	AuthAuditAccepted AuthAuditOutcome = "accepted"
	AuthAuditRejected AuthAuditOutcome = "rejected"
)

// AuthAuditRecord contains only the identity fields explicitly permitted by
// ADR-MW-002. Zero UserID/AuthDate mean that verification did not establish an
// identity; raw initData, signatures, fingerprints and tokens are impossible to
// represent in this type.
type AuthAuditRecord struct {
	Outcome     AuthAuditOutcome
	Environment mobileauth.Environment
	UserID      int64
	AuthDate    time.Time
}

// AuthAuditor persists one bounded authentication result. An unavailable audit
// sink fails the exchange closed.
type AuthAuditor interface {
	Record(context.Context, AuthAuditRecord) error
}

type authEventLogger interface {
	Log(context.Context, observability.Event) error
}

// ObservabilityAuthAuditor emits a redacted structured event through the
// process logger without accepting arbitrary attributes or free-form messages.
type ObservabilityAuthAuditor struct {
	logger authEventLogger
}

// NewObservabilityAuthAuditor binds the auth boundary to the structured logger.
func NewObservabilityAuthAuditor(logger authEventLogger) (*ObservabilityAuthAuditor, error) {
	if isNilDependency(logger) {
		return nil, errors.New("mobile auth audit logger is not configured")
	}
	return &ObservabilityAuthAuditor{logger: logger}, nil
}

// Record validates the bounded vocabulary before it reaches the logger.
func (auditor *ObservabilityAuthAuditor) Record(ctx context.Context, record AuthAuditRecord) error {
	if auditor == nil || isNilDependency(auditor.logger) {
		return errors.New("mobile auth auditor is not configured")
	}
	if ctx == nil {
		return errors.New("mobile auth audit context is not configured")
	}
	if record.Environment != mobileauth.EnvironmentTest && record.Environment != mobileauth.EnvironmentProduction {
		return errors.New("mobile auth audit environment is invalid")
	}
	if record.Outcome != AuthAuditAccepted && record.Outcome != AuthAuditRejected {
		return errors.New("mobile auth audit outcome is invalid")
	}
	if record.Outcome == AuthAuditAccepted && (record.UserID <= 0 || record.AuthDate.IsZero()) {
		return errors.New("accepted mobile auth audit identity is incomplete")
	}
	if (record.UserID == 0) != record.AuthDate.IsZero() || record.UserID < 0 {
		return errors.New("mobile auth audit identity is inconsistent")
	}

	attributes := map[string]any{
		"result_class":         string(record.Outcome),
		"telegram_environment": string(record.Environment),
	}
	if record.UserID > 0 {
		attributes["actor_id"] = record.UserID
		attributes["authenticated_at"] = record.AuthDate.UTC().Format(time.RFC3339)
	}
	logOutcome := observability.OutcomeFailed
	if record.Outcome == AuthAuditAccepted {
		logOutcome = observability.OutcomeSucceeded
	}
	return auditor.logger.Log(ctx, observability.Event{
		Level:      observability.LevelInfo,
		Name:       "mobile.telegram_auth",
		Outcome:    logOutcome,
		Attributes: attributes,
	})
}
