package mobileauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"regexp"
	"sort"
	"sync"
	"time"
)

const (
	sessionTokenBytes   = 32
	bootstrapTokenBytes = 32
	csrfTokenBytes      = 32

	// These values are the frozen ADR-MW-002 security profile. They are not
	// configurable at runtime because longer lifetimes or larger pools would
	// silently weaken the reviewed boundary.
	SessionIdleTTL              = 30 * time.Minute
	SessionAbsoluteTTL          = 8 * time.Hour
	SessionReplayTTL            = 10 * time.Minute
	BootstrapTTL                = 5 * time.Minute
	MaximumSessionsPerUser      = 4
	MaximumOutstandingBootstrap = 64
)

var clientInstancePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// ValidClientInstanceID validates the bounded non-authoritative browser
// instance identifier used only for session/replay isolation.
func ValidClientInstanceID(value string) bool { return clientInstancePattern.MatchString(value) }

// SessionReason is safe low-cardinality diagnostic metadata. Public handlers
// must collapse these reasons into one unauthenticated response.
type SessionReason string

const (
	SessionReasonMalformed SessionReason = "malformed"
	SessionReasonUnknown   SessionReason = "unknown"
	SessionReasonExpired   SessionReason = "expired"
	SessionReasonRevoked   SessionReason = "revoked"
	SessionReasonReplay    SessionReason = "replay"
	SessionReasonEntropy   SessionReason = "entropy_unavailable"
)

// SessionError deliberately excludes tokens, digests and actor data.
type SessionError struct {
	reason SessionReason
}

func (err *SessionError) Error() string { return "mobile session rejected" }

// Reason returns the bounded internal reason class.
func (err *SessionError) Reason() SessionReason {
	if err == nil {
		return ""
	}
	return err.reason
}

// SessionReasonOf extracts a safe bounded reason.
func SessionReasonOf(err error) SessionReason {
	var sessionError *SessionError
	if errors.As(err, &sessionError) {
		return sessionError.Reason()
	}
	return ""
}

// Session is server-derived authorization context. It never contains the raw
// cookie token or its digest.
type Session struct {
	UserID            int64
	Role              string
	CapabilityVersion uint64
	ClientInstanceID  string
	CreatedAt         time.Time
	LastSeenAt        time.Time
	ExpiresAt         time.Time
}

type storedSession struct {
	Session
	generation uint64
	csrfDigest [sha256.Size]byte
}

type storedBootstrap struct {
	expiresAt        time.Time
	clientInstanceID string
}

// SessionConfig freezes bounded, fail-closed session behavior.
type SessionConfig struct {
	CapabilityVersion uint64
	Clock             Clock
}

// SessionManager owns rebuildable in-memory sessions, one-shot initData
// fingerprints and one-shot bootstrap cookies.
type SessionManager struct {
	mu       sync.Mutex
	randomMu sync.Mutex

	idleTTL           time.Duration
	absoluteTTL       time.Duration
	replayTTL         time.Duration
	bootstrapTTL      time.Duration
	maximumPerUser    int
	maximumBootstraps int
	capabilityVersion uint64
	clock             Clock
	random            io.Reader
	generation        uint64
	acceptedAfter     time.Time

	sessions        map[[sha256.Size]byte]storedSession
	replays         map[[sha256.Size]byte]time.Time
	bootstraps      map[[sha256.Size]byte]storedBootstrap
	bootstrapClient map[string][sha256.Size]byte
}

// NewSessionManager creates an empty, restart-revoked session boundary.
func NewSessionManager(config SessionConfig) (*SessionManager, error) {
	return newSessionManager(config, rand.Reader)
}

func newSessionManager(config SessionConfig, random io.Reader) (*SessionManager, error) {
	if config.CapabilityVersion == 0 {
		return nil, errors.New("capability version must be positive")
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if random == nil {
		return nil, errors.New("session entropy source is not configured")
	}
	startedAt := config.Clock.Now().Truncate(time.Second)
	if startedAt.IsZero() {
		return nil, errors.New("session clock returned an invalid start time")
	}
	return &SessionManager{
		idleTTL:           SessionIdleTTL,
		absoluteTTL:       SessionAbsoluteTTL,
		replayTTL:         SessionReplayTTL,
		bootstrapTTL:      BootstrapTTL,
		maximumPerUser:    MaximumSessionsPerUser,
		maximumBootstraps: MaximumOutstandingBootstrap,
		capabilityVersion: config.CapabilityVersion,
		clock:             config.Clock,
		random:            random,
		generation:        1,
		// Telegram auth_date has one-second precision. Requiring the first
		// second after process start prevents a pre-restart initData blob from
		// bypassing the in-memory replay fence after a restart.
		acceptedAfter:   startedAt.Add(time.Second),
		sessions:        make(map[[sha256.Size]byte]storedSession),
		replays:         make(map[[sha256.Size]byte]time.Time),
		bootstraps:      make(map[[sha256.Size]byte]storedBootstrap),
		bootstrapClient: make(map[string][sha256.Size]byte),
	}, nil
}

// IssueBootstrap creates a one-shot cookie token for the Telegram exchange.
func (manager *SessionManager) IssueBootstrap(clientInstanceID string) (string, time.Time, error) {
	if manager == nil {
		return "", time.Time{}, errors.New("session manager is not initialized")
	}
	if !ValidClientInstanceID(clientInstanceID) {
		return "", time.Time{}, sessionReject(SessionReasonMalformed)
	}
	token, digest, err := manager.newToken(bootstrapTokenBytes)
	if err != nil {
		return "", time.Time{}, sessionReject(SessionReasonEntropy)
	}
	now := manager.clock.Now()
	expiresAt := now.Add(manager.bootstrapTTL)

	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pruneLocked(now)
	if _, collision := manager.bootstraps[digest]; collision {
		return "", time.Time{}, sessionReject(SessionReasonEntropy)
	}
	if previous, exists := manager.bootstrapClient[clientInstanceID]; exists {
		delete(manager.bootstraps, previous)
	}
	if len(manager.bootstraps) >= manager.maximumBootstraps {
		manager.evictOldestBootstrapLocked()
	}
	manager.bootstraps[digest] = storedBootstrap{expiresAt: expiresAt, clientInstanceID: clientInstanceID}
	manager.bootstrapClient[clientInstanceID] = digest
	return token, expiresAt, nil
}

// ConsumeBootstrap atomically consumes a one-shot bootstrap cookie.
func (manager *SessionManager) ConsumeBootstrap(token, clientInstanceID string) error {
	if manager == nil {
		return errors.New("session manager is not initialized")
	}
	if !ValidClientInstanceID(clientInstanceID) {
		return sessionReject(SessionReasonMalformed)
	}
	digest, err := tokenDigest(token, bootstrapTokenBytes)
	if err != nil {
		return sessionReject(SessionReasonMalformed)
	}
	now := manager.clock.Now()

	manager.mu.Lock()
	defer manager.mu.Unlock()
	bootstrap, exists := manager.bootstraps[digest]
	if !exists {
		return sessionReject(SessionReasonUnknown)
	}
	if bootstrap.clientInstanceID != clientInstanceID {
		return sessionReject(SessionReasonUnknown)
	}
	delete(manager.bootstraps, digest)
	if manager.bootstrapClient[bootstrap.clientInstanceID] == digest {
		delete(manager.bootstrapClient, bootstrap.clientInstanceID)
	}
	if !now.Before(bootstrap.expiresAt) {
		return sessionReject(SessionReasonExpired)
	}
	return nil
}

// Exchange consumes an authenticated initData fingerprint and creates one
// opaque cookie session. Exactly one concurrent exchange of a fingerprint can
// succeed.
func (manager *SessionManager) Exchange(identity TelegramIdentity, clientInstanceID string) (string, string, Session, error) {
	if manager == nil {
		return "", "", Session{}, errors.New("session manager is not initialized")
	}
	now := manager.clock.Now()
	if identity.UserID <= 0 || identity.UserID > maxTelegramID || identity.Fingerprint == ([sha256.Size]byte{}) ||
		identity.AuthDate.IsZero() || identity.AuthDate.Before(manager.acceptedAfter) ||
		identity.AuthDate.After(now.Add(TelegramFutureSkew)) || now.Sub(identity.AuthDate) > TelegramMaximumAge ||
		!ValidClientInstanceID(clientInstanceID) {
		return "", "", Session{}, sessionReject(SessionReasonMalformed)
	}
	token, digest, err := manager.newToken(sessionTokenBytes)
	if err != nil {
		return "", "", Session{}, sessionReject(SessionReasonEntropy)
	}
	csrfToken, csrfDigest, err := manager.newToken(csrfTokenBytes)
	if err != nil {
		return "", "", Session{}, sessionReject(SessionReasonEntropy)
	}
	session := Session{
		UserID:            identity.UserID,
		Role:              "owner",
		CapabilityVersion: manager.capabilityVersion,
		ClientInstanceID:  clientInstanceID,
		CreatedAt:         now,
		LastSeenAt:        now,
		ExpiresAt:         now.Add(manager.absoluteTTL),
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pruneLocked(now)
	if replayExpiry, replayed := manager.replays[identity.Fingerprint]; replayed && now.Before(replayExpiry) {
		return "", "", Session{}, sessionReject(SessionReasonReplay)
	}
	if _, collision := manager.sessions[digest]; collision {
		return "", "", Session{}, sessionReject(SessionReasonEntropy)
	}

	manager.evictOldestLocked(identity.UserID)
	manager.replays[identity.Fingerprint] = now.Add(manager.replayTTL)
	manager.sessions[digest] = storedSession{Session: session, generation: manager.generation, csrfDigest: csrfDigest}
	return token, csrfToken, session, nil
}

// Authenticate validates and refreshes idle activity for an opaque cookie.
func (manager *SessionManager) Authenticate(token string) (Session, error) {
	return manager.authenticate(token, "", false)
}

// Resume rotates only the CSRF nonce for an existing, client-bound session.
// It never extends absolute expiry or bypasses the Telegram replay fence.
// The HTTP caller must enforce exact same-origin POST before this method.
func (manager *SessionManager) Resume(token, clientInstanceID string) (string, Session, error) {
	if manager == nil || !ValidClientInstanceID(clientInstanceID) {
		return "", Session{}, sessionReject(SessionReasonMalformed)
	}
	digest, err := tokenDigest(token, sessionTokenBytes)
	if err != nil {
		return "", Session{}, sessionReject(SessionReasonMalformed)
	}
	csrf, csrfDigest, err := manager.newToken(csrfTokenBytes)
	if err != nil {
		return "", Session{}, sessionReject(SessionReasonEntropy)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.clock.Now()
	stored, exists := manager.sessions[digest]
	if !exists || stored.generation != manager.generation || stored.ClientInstanceID != clientInstanceID {
		return "", Session{}, sessionReject(SessionReasonUnknown)
	}
	if !now.Before(stored.ExpiresAt) || now.Sub(stored.LastSeenAt) >= manager.idleTTL {
		delete(manager.sessions, digest)
		return "", Session{}, sessionReject(SessionReasonExpired)
	}
	stored.LastSeenAt = now
	stored.csrfDigest = csrfDigest
	manager.sessions[digest] = stored
	return csrf, stored.Session, nil
}

// AuthenticateMutation requires the session-bound CSRF nonce in addition to
// the HttpOnly session cookie. The nonce is stored only as a digest.
func (manager *SessionManager) AuthenticateMutation(token, csrfToken string) (Session, error) {
	return manager.authenticate(token, csrfToken, true)
}

func (manager *SessionManager) authenticate(token, csrfToken string, requireCSRF bool) (Session, error) {
	if manager == nil {
		return Session{}, errors.New("session manager is not initialized")
	}
	digest, err := tokenDigest(token, sessionTokenBytes)
	if err != nil {
		return Session{}, sessionReject(SessionReasonMalformed)
	}
	var csrfDigest [sha256.Size]byte
	if requireCSRF {
		csrfDigest, err = tokenDigest(csrfToken, csrfTokenBytes)
		if err != nil {
			return Session{}, sessionReject(SessionReasonMalformed)
		}
	}
	now := manager.clock.Now()

	manager.mu.Lock()
	defer manager.mu.Unlock()
	stored, exists := manager.sessions[digest]
	if !exists {
		return Session{}, sessionReject(SessionReasonUnknown)
	}
	if stored.generation != manager.generation {
		delete(manager.sessions, digest)
		return Session{}, sessionReject(SessionReasonRevoked)
	}
	if !now.Before(stored.ExpiresAt) || now.Sub(stored.LastSeenAt) >= manager.idleTTL {
		delete(manager.sessions, digest)
		return Session{}, sessionReject(SessionReasonExpired)
	}
	if requireCSRF && subtle.ConstantTimeCompare(stored.csrfDigest[:], csrfDigest[:]) != 1 {
		return Session{}, sessionReject(SessionReasonUnknown)
	}
	stored.LastSeenAt = now
	manager.sessions[digest] = stored
	return stored.Session, nil
}

// Revoke removes one cookie session. Unknown tokens are idempotent after their
// shape is validated.
func (manager *SessionManager) Revoke(token string) error {
	if manager == nil {
		return errors.New("session manager is not initialized")
	}
	digest, err := tokenDigest(token, sessionTokenBytes)
	if err != nil {
		return sessionReject(SessionReasonMalformed)
	}
	manager.mu.Lock()
	delete(manager.sessions, digest)
	manager.mu.Unlock()
	return nil
}

// RevokeAll is the rollback/security kill-switch. Replay fingerprints remain
// so previously exchanged initData cannot mint a replacement session.
func (manager *SessionManager) RevokeAll() {
	if manager == nil {
		return
	}
	manager.mu.Lock()
	manager.generation++
	manager.sessions = make(map[[sha256.Size]byte]storedSession)
	manager.bootstraps = make(map[[sha256.Size]byte]storedBootstrap)
	manager.bootstrapClient = make(map[string][sha256.Size]byte)
	manager.mu.Unlock()
}

// ActiveCount is a test/health projection and reveals no identities.
func (manager *SessionManager) ActiveCount() int {
	if manager == nil {
		return 0
	}
	now := manager.clock.Now()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pruneLocked(now)
	return len(manager.sessions)
}

func (manager *SessionManager) newToken(size int) (string, [sha256.Size]byte, error) {
	value := make([]byte, size)
	manager.randomMu.Lock()
	defer manager.randomMu.Unlock()
	if _, err := io.ReadFull(manager.random, value); err != nil {
		return "", [sha256.Size]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(value)
	return token, sha256.Sum256([]byte(token)), nil
}

func tokenDigest(token string, expectedBytes int) ([sha256.Size]byte, error) {
	if token == "" || len(token) > 128 {
		return [sha256.Size]byte{}, errors.New("invalid token")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != expectedBytes || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return [sha256.Size]byte{}, errors.New("invalid token")
	}
	return sha256.Sum256([]byte(token)), nil
}

func (manager *SessionManager) evictOldestLocked(userID int64) {
	type candidate struct {
		digest    [sha256.Size]byte
		createdAt time.Time
	}
	candidates := make([]candidate, 0, manager.maximumPerUser)
	for digest, session := range manager.sessions {
		if session.UserID == userID {
			candidates = append(candidates, candidate{digest: digest, createdAt: session.CreatedAt})
		}
	}
	if len(candidates) < manager.maximumPerUser {
		return
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].createdAt.Equal(candidates[j].createdAt) {
			return string(candidates[i].digest[:]) < string(candidates[j].digest[:])
		}
		return candidates[i].createdAt.Before(candidates[j].createdAt)
	})
	delete(manager.sessions, candidates[0].digest)
}

func (manager *SessionManager) evictOldestBootstrapLocked() {
	var selected [sha256.Size]byte
	var selectedBootstrap storedBootstrap
	found := false
	for digest, bootstrap := range manager.bootstraps {
		if !found || bootstrap.expiresAt.Before(selectedBootstrap.expiresAt) ||
			(bootstrap.expiresAt.Equal(selectedBootstrap.expiresAt) && string(digest[:]) < string(selected[:])) {
			selected = digest
			selectedBootstrap = bootstrap
			found = true
		}
	}
	if !found {
		return
	}
	delete(manager.bootstraps, selected)
	if manager.bootstrapClient[selectedBootstrap.clientInstanceID] == selected {
		delete(manager.bootstrapClient, selectedBootstrap.clientInstanceID)
	}
}

func (manager *SessionManager) pruneLocked(now time.Time) {
	for digest, session := range manager.sessions {
		if session.generation != manager.generation || !now.Before(session.ExpiresAt) || now.Sub(session.LastSeenAt) >= manager.idleTTL {
			delete(manager.sessions, digest)
		}
	}
	for digest, expiresAt := range manager.replays {
		if !now.Before(expiresAt) {
			delete(manager.replays, digest)
		}
	}
	for digest, bootstrap := range manager.bootstraps {
		if !now.Before(bootstrap.expiresAt) {
			delete(manager.bootstraps, digest)
			if manager.bootstrapClient[bootstrap.clientInstanceID] == digest {
				delete(manager.bootstrapClient, bootstrap.clientInstanceID)
			}
		}
	}
}

func sessionReject(reason SessionReason) error { return &SessionError{reason: reason} }
