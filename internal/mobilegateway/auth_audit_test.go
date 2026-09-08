package mobilegateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
	"github.com/boxvtk621/homelab-telegram-panel/internal/observability"
)

func TestObservabilityAuthAuditorEmitsOnlyPermittedFields(t *testing.T) {
	t.Parallel()
	buffer := bytes.Buffer{}
	logger, err := observability.New(&buffer, observability.WithClock(observability.ClockFunc(func() time.Time {
		return time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)
	})))
	if err != nil {
		t.Fatalf("observability.New(): %v", err)
	}
	auditor, err := NewObservabilityAuthAuditor(logger)
	if err != nil {
		t.Fatalf("NewObservabilityAuthAuditor(): %v", err)
	}
	authDate := time.Date(2026, 8, 29, 2, 59, 0, 0, time.UTC)
	if err := auditor.Record(context.Background(), AuthAuditRecord{
		Outcome: AuthAuditAccepted, Environment: mobileauth.EnvironmentProduction,
		UserID: 900000000001, AuthDate: authDate,
	}); err != nil {
		t.Fatalf("Record(): %v", err)
	}

	var event struct {
		Name       string         `json:"event"`
		Outcome    string         `json:"outcome"`
		Message    string         `json:"message"`
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &event); err != nil {
		t.Fatalf("decode audit event: %v", err)
	}
	if event.Name != "mobile.telegram_auth" || event.Outcome != string(observability.OutcomeSucceeded) || event.Message != "" {
		t.Fatalf("unexpected audit envelope: %+v", event)
	}
	if len(event.Attributes) != 4 || event.Attributes["result_class"] != string(AuthAuditAccepted) ||
		event.Attributes["telegram_environment"] != string(mobileauth.EnvironmentProduction) ||
		event.Attributes["actor_id"] != float64(900000000001) ||
		event.Attributes["authenticated_at"] != authDate.Format(time.RFC3339) {
		t.Fatalf("unexpected bounded audit attributes: %#v", event.Attributes)
	}
	for _, forbidden := range []string{"init_data", "signature", "fingerprint", "csrf", "cookie", "token"} {
		if strings.Contains(strings.ToLower(buffer.String()), forbidden) {
			t.Fatalf("audit output contains forbidden field %q: %s", forbidden, buffer.String())
		}
	}
}

func TestAuthExchangeAuditIsBoundedAndFailsClosed(t *testing.T) {
	t.Parallel()
	clock := &gatewayClock{now: time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)}
	identity := mobileauth.TelegramIdentity{
		UserID: 900000000001, AuthDate: clock.Now().Add(time.Second),
		Fingerprint: sha256.Sum256([]byte("audited-exchange")),
	}
	auditor := &recordingAuthAuditor{}
	handler, sessions := mustAuthHandlerWithAudit(t, clock, &fakeTelegramVerifier{identity: identity}, auditor)
	mux := http.NewServeMux()
	_ = handler.Register(mux)
	bootstrap := responseCookie(t, serveBootstrap(mux, "audit-client"), BootstrapCookieName)
	response := serve(mux, http.MethodPost, "/api/v1/auth/telegram", "signed", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin,
		"Cookie": cookieHeader(bootstrap), clientInstanceHeader: "audit-client",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("exchange status=%d body=%s", response.Code, response.Body.String())
	}
	records := auditor.snapshot()
	if len(records) != 1 || records[0].Outcome != AuthAuditAccepted || records[0].Environment != mobileauth.EnvironmentProduction || records[0].UserID != identity.UserID || !records[0].AuthDate.Equal(identity.AuthDate) {
		t.Fatalf("audit records = %+v", records)
	}
	if sessions.ActiveCount() != 1 {
		t.Fatalf("active sessions = %d", sessions.ActiveCount())
	}

	failing := &recordingAuthAuditor{failOutcome: AuthAuditAccepted}
	failedHandler, failedSessions := mustAuthHandlerWithAudit(t, clock, &fakeTelegramVerifier{identity: mobileauth.TelegramIdentity{
		UserID: identity.UserID, AuthDate: identity.AuthDate,
		Fingerprint: sha256.Sum256([]byte("audit-failure")),
	}}, failing)
	failedMux := http.NewServeMux()
	_ = failedHandler.Register(failedMux)
	failedBootstrap := responseCookie(t, serveBootstrap(failedMux, "failed-audit-client"), BootstrapCookieName)
	failedResponse := serve(failedMux, http.MethodPost, "/api/v1/auth/telegram", "signed", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Origin": testPublicOrigin,
		"Cookie": cookieHeader(failedBootstrap), clientInstanceHeader: "failed-audit-client",
	})
	if failedResponse.Code != http.StatusServiceUnavailable || len(failedResponse.Result().Cookies()) != 1 || failedSessions.ActiveCount() != 0 {
		t.Fatalf("audit failure status=%d cookies=%v active=%d body=%s", failedResponse.Code, failedResponse.Result().Cookies(), failedSessions.ActiveCount(), failedResponse.Body.String())
	}
}

type recordingAuthAuditor struct {
	mu          sync.Mutex
	records     []AuthAuditRecord
	failOutcome AuthAuditOutcome
}

func (auditor *recordingAuthAuditor) Record(_ context.Context, record AuthAuditRecord) error {
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	auditor.records = append(auditor.records, record)
	if record.Outcome == auditor.failOutcome {
		return errors.New("synthetic auth audit failure")
	}
	return nil
}

func (auditor *recordingAuthAuditor) snapshot() []AuthAuditRecord {
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	return append([]AuthAuditRecord(nil), auditor.records...)
}

func mustAuthHandlerWithAudit(t *testing.T, clock mobileauth.Clock, verifier TelegramVerifier, auditor AuthAuditor) (*AuthHandler, *mobileauth.SessionManager) {
	t.Helper()
	sessions, err := mobileauth.NewSessionManager(mobileauth.SessionConfig{CapabilityVersion: 1, Clock: clock})
	if err != nil {
		t.Fatalf("NewSessionManager(): %v", err)
	}
	handler, err := NewAuthHandler(AuthConfig{
		PublicOrigin: testPublicOrigin, Capacity: NewCapacityGate(), Audit: auditor,
	}, verifier, sessions, &sequenceIDs{}, clock)
	if err != nil {
		t.Fatalf("NewAuthHandler(): %v", err)
	}
	return handler, sessions
}
