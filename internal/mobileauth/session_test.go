package mobileauth

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBootstrapIsOneShotAndExpires(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, nil)
	token, expiresAt, err := manager.IssueBootstrap("client-1")
	if err != nil {
		t.Fatalf("IssueBootstrap(): %v", err)
	}
	if err := manager.ConsumeBootstrap(token, "other-client"); SessionReasonOf(err) != SessionReasonUnknown {
		t.Fatalf("wrong-client consume reason = %q, err=%v", SessionReasonOf(err), err)
	}
	if err := manager.ConsumeBootstrap(token, "client-1"); err != nil {
		t.Fatalf("ConsumeBootstrap(): %v", err)
	}
	if !expiresAt.Equal(clock.Now().Add(5 * time.Minute)) {
		t.Fatalf("bootstrap expiry = %s", expiresAt)
	}
	if err := manager.ConsumeBootstrap(token, "client-1"); SessionReasonOf(err) != SessionReasonUnknown {
		t.Fatalf("second consume reason = %q, err=%v", SessionReasonOf(err), err)
	}

	expiring, _, err := manager.IssueBootstrap("client-2")
	if err != nil {
		t.Fatalf("IssueBootstrap() expiring: %v", err)
	}
	clock.Advance(5 * time.Minute)
	if err := manager.ConsumeBootstrap(expiring, "client-2"); SessionReasonOf(err) != SessionReasonExpired {
		t.Fatalf("expired consume reason = %q, err=%v", SessionReasonOf(err), err)
	}
}

func TestBootstrapReplacementAndBoundedEvictionKeepLoginAvailable(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, nil)
	first, _, err := manager.IssueBootstrap("same-client")
	if err != nil {
		t.Fatalf("first IssueBootstrap(): %v", err)
	}
	replacement, _, err := manager.IssueBootstrap("same-client")
	if err != nil {
		t.Fatalf("replacement IssueBootstrap(): %v", err)
	}
	if err := manager.ConsumeBootstrap(first, "same-client"); SessionReasonOf(err) != SessionReasonUnknown {
		t.Fatalf("replaced bootstrap reason=%q err=%v", SessionReasonOf(err), err)
	}
	if err := manager.ConsumeBootstrap(replacement, "same-client"); err != nil {
		t.Fatalf("replacement ConsumeBootstrap(): %v", err)
	}

	var oldest string
	var newest string
	for index := 0; index <= MaximumOutstandingBootstrap; index++ {
		token, _, err := manager.IssueBootstrap("pool-" + twoDigit(index))
		if err != nil {
			t.Fatalf("IssueBootstrap(%d): %v", index, err)
		}
		if index == 0 {
			oldest = token
		}
		newest = token
		clock.Advance(time.Millisecond)
	}
	if err := manager.ConsumeBootstrap(oldest, "pool-00"); SessionReasonOf(err) != SessionReasonUnknown {
		t.Fatalf("evicted bootstrap reason=%q err=%v", SessionReasonOf(err), err)
	}
	if err := manager.ConsumeBootstrap(newest, "pool-64"); err != nil {
		t.Fatalf("newest bootstrap rejected: %v", err)
	}
}

func TestSessionLifecycleIdleAbsoluteAndRevoke(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, nil)
	identity := testIdentity("lifecycle")
	token, csrfToken, created, err := manager.Exchange(identity, "client-1")
	if err != nil {
		t.Fatalf("Exchange(): %v", err)
	}
	if created.Role != "owner" || created.CapabilityVersion != 7 || manager.ActiveCount() != 1 {
		t.Fatalf("created=%+v active=%d", created, manager.ActiveCount())
	}
	if _, err := manager.AuthenticateMutation(token, "wrong"); SessionReasonOf(err) != SessionReasonMalformed {
		t.Fatalf("malformed CSRF reason = %q, err=%v", SessionReasonOf(err), err)
	}
	wrongCSRF := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := manager.AuthenticateMutation(token, wrongCSRF); SessionReasonOf(err) != SessionReasonUnknown {
		t.Fatalf("wrong CSRF reason = %q, err=%v", SessionReasonOf(err), err)
	}
	if _, err := manager.AuthenticateMutation(token, csrfToken); err != nil {
		t.Fatalf("AuthenticateMutation(): %v", err)
	}
	clock.Advance(29 * time.Minute)
	authenticated, err := manager.Authenticate(token)
	if err != nil {
		t.Fatalf("Authenticate(): %v", err)
	}
	if !authenticated.LastSeenAt.Equal(clock.Now()) {
		t.Fatalf("last seen = %s, now=%s", authenticated.LastSeenAt, clock.Now())
	}
	clock.Advance(30 * time.Minute)
	if _, err := manager.Authenticate(token); SessionReasonOf(err) != SessionReasonExpired {
		t.Fatalf("idle expiry reason = %q, err=%v", SessionReasonOf(err), err)
	}

	absoluteIdentity := testIdentity("absolute")
	absoluteIdentity.AuthDate = clock.Now().Add(time.Second)
	token, _, _, err = manager.Exchange(absoluteIdentity, "client-2")
	if err != nil {
		t.Fatalf("Exchange() absolute: %v", err)
	}
	for range 16 {
		clock.Advance(29 * time.Minute)
		if _, err := manager.Authenticate(token); err != nil {
			t.Fatalf("periodic Authenticate(): %v", err)
		}
	}
	clock.Advance(16 * time.Minute)
	if _, err := manager.Authenticate(token); SessionReasonOf(err) != SessionReasonExpired {
		t.Fatalf("absolute expiry reason = %q, err=%v", SessionReasonOf(err), err)
	}

	revokeIdentity := testIdentity("revoke")
	revokeIdentity.AuthDate = clock.Now().Add(time.Second)
	token, _, _, err = manager.Exchange(revokeIdentity, "client-3")
	if err != nil {
		t.Fatalf("Exchange() revoke: %v", err)
	}
	if err := manager.Revoke(token); err != nil {
		t.Fatalf("Revoke(): %v", err)
	}
	if err := manager.Revoke(token); err != nil {
		t.Fatalf("idempotent Revoke(): %v", err)
	}
	if _, err := manager.Authenticate(token); SessionReasonOf(err) != SessionReasonUnknown {
		t.Fatalf("revoked auth reason = %q, err=%v", SessionReasonOf(err), err)
	}
}

func TestConcurrentReplayAllowsExactlyOneSession(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, nil)
	identity := testIdentity("same-init-data")
	var successes atomic.Int32
	var replays atomic.Int32
	var unexpected atomic.Int32
	var wait sync.WaitGroup
	for index := range 32 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, _, _, err := manager.Exchange(identity, "client-"+twoDigit(index))
			switch SessionReasonOf(err) {
			case "":
				if err == nil {
					successes.Add(1)
				} else {
					unexpected.Add(1)
				}
			case SessionReasonReplay:
				replays.Add(1)
			default:
				unexpected.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if successes.Load() != 1 || replays.Load() != 31 || unexpected.Load() != 0 || manager.ActiveCount() != 1 {
		t.Fatalf("success=%d replay=%d unexpected=%d active=%d", successes.Load(), replays.Load(), unexpected.Load(), manager.ActiveCount())
	}
}

func TestFifthSessionEvictsOldestForUser(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, nil)
	tokens := make([]string, 0, 5)
	for index := range 5 {
		token, _, _, err := manager.Exchange(testIdentity("session-"+twoDigit(index)), "client-"+twoDigit(index))
		if err != nil {
			t.Fatalf("Exchange(%d): %v", index, err)
		}
		tokens = append(tokens, token)
		clock.Advance(time.Second)
	}
	if manager.ActiveCount() != 4 {
		t.Fatalf("active sessions = %d", manager.ActiveCount())
	}
	if _, err := manager.Authenticate(tokens[0]); SessionReasonOf(err) != SessionReasonUnknown {
		t.Fatalf("oldest reason = %q, err=%v", SessionReasonOf(err), err)
	}
	for _, token := range tokens[1:] {
		if _, err := manager.Authenticate(token); err != nil {
			t.Fatalf("newer session rejected: %v", err)
		}
	}
}

func TestRevokeAllKeepsReplayFence(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, nil)
	identity := testIdentity("rollback")
	token, _, _, err := manager.Exchange(identity, "client-1")
	if err != nil {
		t.Fatalf("Exchange(): %v", err)
	}
	manager.RevokeAll()
	if manager.ActiveCount() != 0 {
		t.Fatalf("active after revoke all = %d", manager.ActiveCount())
	}
	if _, err := manager.Authenticate(token); SessionReasonOf(err) != SessionReasonUnknown {
		t.Fatalf("old token reason = %q, err=%v", SessionReasonOf(err), err)
	}
	if _, _, _, err := manager.Exchange(identity, "client-2"); SessionReasonOf(err) != SessionReasonReplay {
		t.Fatalf("replay after revoke all reason = %q, err=%v", SessionReasonOf(err), err)
	}
}

func TestRestartEpochRejectsPreviouslyIssuedInitData(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	beforeRestart := testIdentity("before-restart")
	beforeRestart.AuthDate = clock.Now().Add(time.Second)
	first := mustSessionManager(t, clock, nil)
	if _, _, _, err := first.Exchange(beforeRestart, "client-1"); err != nil {
		t.Fatalf("first Exchange(): %v", err)
	}

	clock.Advance(time.Second)
	restarted := mustSessionManager(t, clock, nil)
	if _, _, _, err := restarted.Exchange(beforeRestart, "client-2"); SessionReasonOf(err) != SessionReasonMalformed {
		t.Fatalf("pre-restart initData reason=%q err=%v", SessionReasonOf(err), err)
	}
	afterRestart := testIdentity("after-restart")
	afterRestart.AuthDate = clock.Now().Add(time.Second)
	if _, _, _, err := restarted.Exchange(afterRestart, "client-2"); err != nil {
		t.Fatalf("post-restart Exchange(): %v", err)
	}
}

func TestSessionEntropyAndTokenShapeFailClosed(t *testing.T) {
	t.Parallel()
	clock := newMutableClock(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
	manager := mustSessionManager(t, clock, failingReader{})
	if _, _, err := manager.IssueBootstrap("client-1"); SessionReasonOf(err) != SessionReasonEntropy {
		t.Fatalf("bootstrap entropy reason = %q, err=%v", SessionReasonOf(err), err)
	}
	if _, _, _, err := manager.Exchange(testIdentity("entropy"), "client-1"); SessionReasonOf(err) != SessionReasonEntropy {
		t.Fatalf("session entropy reason = %q, err=%v", SessionReasonOf(err), err)
	}
	if _, err := manager.Authenticate("not-base64!"); SessionReasonOf(err) != SessionReasonMalformed {
		t.Fatalf("malformed reason = %q, err=%v", SessionReasonOf(err), err)
	}
	if got := SessionReasonOf(errors.New("unrelated")); got != "" {
		t.Fatalf("SessionReasonOf unrelated = %q", got)
	}
}

func TestFrozenSessionSecurityPolicy(t *testing.T) {
	t.Parallel()
	if SessionIdleTTL != 30*time.Minute || SessionAbsoluteTTL != 8*time.Hour || SessionReplayTTL != 10*time.Minute || BootstrapTTL != 5*time.Minute || MaximumSessionsPerUser != 4 || MaximumOutstandingBootstrap != 64 {
		t.Fatalf("session security policy drifted: idle=%s absolute=%s replay=%s bootstrap=%s sessions=%d bootstraps=%d", SessionIdleTTL, SessionAbsoluteTTL, SessionReplayTTL, BootstrapTTL, MaximumSessionsPerUser, MaximumOutstandingBootstrap)
	}
}

type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMutableClock(now time.Time) *mutableClock { return &mutableClock{now: now} }

func (clock *mutableClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *mutableClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func mustSessionManager(t *testing.T, clock Clock, random io.Reader) *SessionManager {
	t.Helper()
	manager, err := newSessionManager(SessionConfig{
		CapabilityVersion: 7,
		Clock:             clock,
	}, randomOrCrypto(random))
	if err != nil {
		t.Fatalf("NewSessionManager(): %v", err)
	}
	return manager
}

func randomOrCrypto(reader io.Reader) io.Reader {
	if reader == nil {
		return rand.Reader
	}
	return reader
}

func testIdentity(label string) TelegramIdentity {
	return TelegramIdentity{
		UserID:      testUserID,
		AuthDate:    time.Date(2026, 8, 29, 1, 0, 1, 0, time.UTC),
		Fingerprint: sha256.Sum256([]byte(label)),
	}
}

func twoDigit(value int) string {
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)})
}
