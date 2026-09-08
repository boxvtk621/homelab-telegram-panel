// Package mobileauth validates Telegram Mini App bootstrap identity without
// giving the public Gateway access to the bot token.
package mobileauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxInitDataBytes = 16 * 1024
	maxTelegramID    = int64(1<<52 - 1)

	// TelegramMaximumAge and TelegramFutureSkew are security policy, not
	// deployment tuning. Keeping them out of caller-provided configuration
	// prevents a permissive runtime flag from weakening bootstrap validation.
	TelegramMaximumAge = 300 * time.Second
	TelegramFutureSkew = 30 * time.Second

	telegramTestPublicKeyHex       = "40055058a4ee38156a06562e52eece92a771bcd8346a8c4615cb7376eddf72ec"
	telegramProductionPublicKeyHex = "e7bf03a2fa4602af4580703d88dda5bb59f32ed8b02a56c187fe7d34caed242d"
)

// Environment pins the Telegram signature-verification key.
type Environment string

const (
	EnvironmentTest       Environment = "test"
	EnvironmentProduction Environment = "production"
)

// Reason is safe low-cardinality diagnostic metadata. It must not be returned
// to an unauthenticated client as a distinguishable response.
type Reason string

const (
	ReasonMalformed        Reason = "malformed"
	ReasonSignatureInvalid Reason = "signature_invalid"
	ReasonExpired          Reason = "expired"
	ReasonFuture           Reason = "future"
	ReasonUserInvalid      Reason = "user_invalid"
	ReasonUserDenied       Reason = "user_denied"
)

// VerificationError deliberately carries no raw initData or decoded user data.
type VerificationError struct {
	reason Reason
}

func (err *VerificationError) Error() string { return "telegram init data rejected" }

// Reason returns a bounded class suitable for protected audit and metrics.
func (err *VerificationError) Reason() Reason {
	if err == nil {
		return ""
	}
	return err.reason
}

// ReasonOf returns a bounded reason without exposing input data.
func ReasonOf(err error) Reason {
	var verificationError *VerificationError
	if errors.As(err, &verificationError) {
		return verificationError.Reason()
	}
	return ""
}

// Clock makes age validation deterministic in tests.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

func (clock ClockFunc) Now() time.Time { return clock() }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// TelegramIdentity is the minimum authenticated bootstrap result. Fingerprint
// is an opaque replay-cache key and must not be logged or sent to the client.
type TelegramIdentity struct {
	UserID      int64
	AuthDate    time.Time
	QueryID     string
	Fingerprint [sha256.Size]byte
}

// TelegramVerifier verifies the Ed25519 signature defined by Telegram for
// third-party Mini App validation.
type TelegramVerifier struct {
	environment Environment
	botID       int64
	publicKey   ed25519.PublicKey
	allowed     map[int64]struct{}
	maxAge      time.Duration
	futureSkew  time.Duration
	clock       Clock
}

// NewTelegramVerifier pins one of Telegram's published environment keys.
func NewTelegramVerifier(
	botID int64,
	environment Environment,
	allowedUserIDs []int64,
	clock Clock,
) (*TelegramVerifier, error) {
	publicKeyHex := ""
	switch environment {
	case EnvironmentTest:
		publicKeyHex = telegramTestPublicKeyHex
	case EnvironmentProduction:
		publicKeyHex = telegramProductionPublicKeyHex
	default:
		return nil, fmt.Errorf("unsupported Telegram environment %q", environment)
	}
	publicKey, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("decode pinned Telegram public key: %w", err)
	}
	return newTelegramVerifierForEnvironment(environment, botID, publicKey, allowedUserIDs, TelegramMaximumAge, TelegramFutureSkew, clock)
}

func newTelegramVerifier(
	botID int64,
	publicKey ed25519.PublicKey,
	allowedUserIDs []int64,
	maxAge time.Duration,
	futureSkew time.Duration,
	clock Clock,
) (*TelegramVerifier, error) {
	return newTelegramVerifierForEnvironment(EnvironmentTest, botID, publicKey, allowedUserIDs, maxAge, futureSkew, clock)
}

func newTelegramVerifierForEnvironment(
	environment Environment,
	botID int64,
	publicKey ed25519.PublicKey,
	allowedUserIDs []int64,
	maxAge time.Duration,
	futureSkew time.Duration,
	clock Clock,
) (*TelegramVerifier, error) {
	if environment != EnvironmentTest && environment != EnvironmentProduction {
		return nil, errors.New("Telegram signature environment is invalid")
	}
	if botID <= 0 || botID > maxTelegramID {
		return nil, errors.New("Telegram bot ID must be a positive 52-bit integer")
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("Telegram public key must contain 32 bytes")
	}
	if maxAge <= 0 {
		return nil, errors.New("Telegram init data maximum age must be positive")
	}
	if futureSkew < 0 || futureSkew > maxAge {
		return nil, errors.New("Telegram future skew must be non-negative and not exceed maximum age")
	}
	if clock == nil {
		clock = systemClock{}
	}
	if len(allowedUserIDs) != 1 {
		return nil, errors.New("Telegram single-owner allowlist must contain exactly one ID")
	}
	allowed := make(map[int64]struct{}, len(allowedUserIDs))
	for _, userID := range allowedUserIDs {
		if userID <= 0 || userID > maxTelegramID {
			return nil, errors.New("Telegram user allowlist contains an invalid ID")
		}
		if _, duplicate := allowed[userID]; duplicate {
			return nil, errors.New("Telegram user allowlist contains a duplicate ID")
		}
		allowed[userID] = struct{}{}
	}

	keyCopy := append(ed25519.PublicKey(nil), publicKey...)
	return &TelegramVerifier{
		environment: environment,
		botID:       botID,
		publicKey:   keyCopy,
		allowed:     allowed,
		maxAge:      maxAge,
		futureSkew:  futureSkew,
		clock:       clock,
	}, nil
}

// SignatureEnvironment returns the immutable Telegram public-key environment
// used by this verifier. It is safe low-cardinality audit metadata.
func (verifier *TelegramVerifier) SignatureEnvironment() Environment {
	if verifier == nil {
		return ""
	}
	return verifier.environment
}

// Verify validates signature, age and the exact numeric user allowlist. Raw
// initData is not retained by the verifier.
func (verifier *TelegramVerifier) Verify(raw string) (TelegramIdentity, error) {
	if verifier == nil || verifier.clock == nil || len(verifier.publicKey) != ed25519.PublicKeySize {
		return TelegramIdentity{}, errors.New("Telegram verifier is not initialized")
	}
	values, err := parseInitData(raw)
	if err != nil {
		return TelegramIdentity{}, reject(ReasonMalformed)
	}

	signature, err := decodeSignature(values["signature"])
	if err != nil {
		return TelegramIdentity{}, reject(ReasonMalformed)
	}
	dataCheckString := canonicalDataCheckString(verifier.botID, values)
	if !ed25519.Verify(verifier.publicKey, dataCheckString, signature) {
		return TelegramIdentity{}, reject(ReasonSignatureInvalid)
	}

	authUnix, err := parsePositiveDecimal(values["auth_date"])
	if err != nil {
		return TelegramIdentity{}, reject(ReasonMalformed)
	}
	authDate := time.Unix(authUnix, 0).UTC()
	now := verifier.clock.Now().UTC()
	if authDate.After(now.Add(verifier.futureSkew)) {
		return TelegramIdentity{}, reject(ReasonFuture)
	}
	if now.Sub(authDate) > verifier.maxAge {
		return TelegramIdentity{}, reject(ReasonExpired)
	}

	userID, err := parseTelegramUserID(values["user"])
	if err != nil {
		return TelegramIdentity{}, reject(ReasonUserInvalid)
	}
	if _, ok := verifier.allowed[userID]; !ok {
		return TelegramIdentity{}, reject(ReasonUserDenied)
	}

	return TelegramIdentity{
		UserID:   userID,
		AuthDate: authDate,
		QueryID:  values["query_id"],
		// Fingerprint the authenticated canonical form, not the raw query. Raw
		// field order and percent-encoding are malleable without changing the
		// Telegram signature and therefore cannot define replay identity.
		Fingerprint: sha256.Sum256(dataCheckString),
	}, nil
}

func reject(reason Reason) error { return &VerificationError{reason: reason} }

func parseInitData(raw string) (map[string]string, error) {
	if raw == "" || len(raw) > maxInitDataBytes || !utf8.ValidString(raw) {
		return nil, errors.New("invalid init data size or encoding")
	}

	values := make(map[string]string)
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			return nil, errors.New("empty init data pair")
		}
		rawKey, rawValue, found := strings.Cut(pair, "=")
		if !found || rawKey == "" {
			return nil, errors.New("invalid init data pair")
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil || !validInitDataKey(key) {
			return nil, errors.New("invalid init data key")
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("invalid init data value")
		}
		if _, duplicate := values[key]; duplicate {
			return nil, errors.New("duplicate init data key")
		}
		values[key] = value
	}
	for _, required := range []string{"auth_date", "signature", "user"} {
		if values[required] == "" {
			return nil, fmt.Errorf("missing init data field %s", required)
		}
	}
	return values, nil
}

func validInitDataKey(key string) bool {
	if key == "" || len(key) > 64 || !utf8.ValidString(key) {
		return false
	}
	for _, character := range key {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func canonicalDataCheckString(botID int64, values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "hash" && key != "signature" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	var builder strings.Builder
	builder.WriteString(strconv.FormatInt(botID, 10))
	builder.WriteString(":WebAppData\n")
	for index, key := range keys {
		if index > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(values[key])
	}
	return []byte(builder.String())
}

func decodeSignature(value string) ([]byte, error) {
	if value == "" || len(value) > 128 {
		return nil, errors.New("invalid signature size")
	}
	trimmed := strings.TrimRight(value, "=")
	padding := len(value) - len(trimmed)
	if strings.Contains(trimmed, "=") || (padding != 0 && padding != 2) {
		return nil, errors.New("invalid signature encoding")
	}
	signature, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, errors.New("invalid signature")
	}
	if base64.RawURLEncoding.EncodeToString(signature) != trimmed {
		return nil, errors.New("non-canonical signature")
	}
	return signature, nil
}

func parsePositiveDecimal(value string) (int64, error) {
	if value == "" || len(value) > 20 {
		return 0, errors.New("invalid decimal")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("invalid decimal")
		}
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number <= 0 {
		return 0, errors.New("invalid decimal")
	}
	return number, nil
}

func parseTelegramUserID(raw string) (int64, error) {
	if raw == "" || len(raw) > 8*1024 || !utf8.ValidString(raw) {
		return 0, errors.New("invalid Telegram user")
	}
	if err := rejectDuplicateJSONKeys([]byte(raw)); err != nil {
		return 0, err
	}
	var user struct {
		ID int64 `json:"id"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&user); err != nil {
		return 0, err
	}
	if user.ID <= 0 || user.ID > maxTelegramID {
		return 0, errors.New("invalid Telegram user ID")
	}
	return user.ID, nil
}

func rejectDuplicateJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return errors.New("trailing Telegram user JSON")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid Telegram user JSON key")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate Telegram user JSON key")
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid Telegram user JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid Telegram user JSON array")
		}
	default:
		return errors.New("invalid Telegram user JSON delimiter")
	}
	return nil
}
