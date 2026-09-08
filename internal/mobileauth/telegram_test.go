package mobileauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testBotID  = int64(424242)
	testUserID = int64(900000000001)
)

func TestTelegramVerifierAcceptsSignedAllowedUser(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 0, 30, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier := mustTestVerifier(t, publicKey, []int64{testUserID}, now)
	values := map[string]string{
		"auth_date": strconv.FormatInt(now.Add(-time.Minute).Unix(), 10),
		"query_id":  "AAH-safe-query",
		"user":      `{"id":900000000001,"first_name":"Kondor","username":"owner","language_code":"ru"}`,
		"hash":      strings.Repeat("a", 64),
	}
	raw := signedInitData(values, privateKey)

	identity, err := verifier.Verify(raw)
	if err != nil {
		t.Fatalf("Verify() error = %v, reason=%s", err, ReasonOf(err))
	}
	if identity.UserID != testUserID || identity.QueryID != values["query_id"] {
		t.Fatalf("identity = %+v", identity)
	}
	if !identity.AuthDate.Equal(now.Add(-time.Minute)) {
		t.Fatalf("auth date = %s", identity.AuthDate)
	}
	if identity.Fingerprint == ([32]byte{}) {
		t.Fatal("fingerprint is empty")
	}
}

func TestTelegramVerifierRejectsHostileInputs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 0, 30, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier := mustTestVerifier(t, publicKey, []int64{testUserID}, now)

	validValues := func() map[string]string {
		return map[string]string{
			"auth_date": strconv.FormatInt(now.Add(-time.Minute).Unix(), 10),
			"query_id":  "query-1",
			"user":      `{"id":900000000001,"first_name":"Kondor"}`,
		}
	}
	tests := []struct {
		name   string
		build  func() string
		reason Reason
	}{
		{
			name: "tampered signed field",
			build: func() string {
				raw := signedInitData(validValues(), privateKey)
				return strings.Replace(raw, "query-1", "query-2", 1)
			},
			reason: ReasonSignatureInvalid,
		},
		{
			name: "expired",
			build: func() string {
				values := validValues()
				values["auth_date"] = strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10)
				return signedInitData(values, privateKey)
			},
			reason: ReasonExpired,
		},
		{
			name: "future",
			build: func() string {
				values := validValues()
				values["auth_date"] = strconv.FormatInt(now.Add(31*time.Second).Unix(), 10)
				return signedInitData(values, privateKey)
			},
			reason: ReasonFuture,
		},
		{
			name: "denied user",
			build: func() string {
				values := validValues()
				values["user"] = `{"id":900000000002,"first_name":"Other"}`
				return signedInitData(values, privateKey)
			},
			reason: ReasonUserDenied,
		},
		{
			name: "duplicate query key",
			build: func() string {
				return signedInitData(validValues(), privateKey) + "&user=%7B%22id%22%3A900000000001%7D"
			},
			reason: ReasonMalformed,
		},
		{
			name: "percent encoded duplicate signature",
			build: func() string {
				return signedInitData(validValues(), privateKey) + "&%73ignature=x"
			},
			reason: ReasonMalformed,
		},
		{
			name: "percent encoded duplicate auth date",
			build: func() string {
				return signedInitData(validValues(), privateKey) + "&%61uth_date=1"
			},
			reason: ReasonMalformed,
		},
		{
			name: "invalid UTF-8 key",
			build: func() string {
				return signedInitData(validValues(), privateKey) + "&%FF=x"
			},
			reason: ReasonMalformed,
		},
		{
			name: "newline in value",
			build: func() string {
				return signedInitData(validValues(), privateKey) + "&extra=line%0Abreak"
			},
			reason: ReasonMalformed,
		},
		{
			name: "non canonical key grammar",
			build: func() string {
				return signedInitData(validValues(), privateKey) + "&Upper=x"
			},
			reason: ReasonMalformed,
		},
		{
			name: "duplicate user id",
			build: func() string {
				values := validValues()
				values["user"] = `{"id":900000000001,"id":900000000002}`
				return signedInitData(values, privateKey)
			},
			reason: ReasonUserInvalid,
		},
		{
			name: "missing signature",
			build: func() string {
				return url.Values{
					"auth_date": {validValues()["auth_date"]},
					"user":      {validValues()["user"]},
				}.Encode()
			},
			reason: ReasonMalformed,
		},
		{
			name:   "malformed percent encoding",
			build:  func() string { return "auth_date=%zz&signature=x&user=x" },
			reason: ReasonMalformed,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := verifier.Verify(test.build())
			if err == nil {
				t.Fatal("Verify() succeeded")
			}
			if got := ReasonOf(err); got != test.reason {
				t.Fatalf("reason = %q, want %q; err=%v", got, test.reason, err)
			}
			if strings.Contains(err.Error(), "Kondor") || strings.Contains(err.Error(), "query-1") {
				t.Fatalf("error leaked input: %v", err)
			}
		})
	}
}

func TestTelegramVerifierAcceptsPaddedBase64URLSignature(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 0, 30, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier := mustTestVerifier(t, publicKey, []int64{testUserID}, now)
	values := map[string]string{
		"auth_date": strconv.FormatInt(now.Unix(), 10),
		"user":      `{"id":900000000001}`,
	}
	raw := signedInitData(values, privateKey)
	parsed, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	parsed.Set("signature", parsed.Get("signature")+"==")
	if _, err := verifier.Verify(parsed.Encode()); err != nil {
		t.Fatalf("Verify() padded signature error = %v", err)
	}
	parsed.Set("signature", strings.TrimSuffix(parsed.Get("signature"), "="))
	if _, err := verifier.Verify(parsed.Encode()); ReasonOf(err) != ReasonMalformed {
		t.Fatalf("single-padding signature reason = %q, err=%v", ReasonOf(err), err)
	}
}

func TestCanonicalDataCheckStringMatchesTelegramThirdPartyAlgorithm(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"user":      `{"id":7}`,
		"signature": "excluded",
		"query_id":  "A+B",
		"hash":      "excluded",
		"auth_date": "1",
	}
	want := "42:WebAppData\nauth_date=1\nquery_id=A+B\nuser={\"id\":7}"
	if got := string(canonicalDataCheckString(42, values)); got != want {
		t.Fatalf("data-check-string = %q, want %q", got, want)
	}
}

func TestReplayFingerprintUsesCanonicalAuthenticatedData(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 0, 30, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier := mustTestVerifier(t, publicKey, []int64{testUserID}, now)
	values := map[string]string{
		"auth_date": strconv.FormatInt(now.Unix(), 10),
		"query_id":  "query with spaces",
		"user":      `{"id":900000000001,"first_name":"Kondor"}`,
		"hash":      strings.Repeat("a", 64),
	}
	raw := signedInitData(values, privateKey)
	parsed, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(): %v", err)
	}
	reordered := "user=" + url.QueryEscape(parsed.Get("user")) +
		"&signature=" + url.QueryEscape(parsed.Get("signature")) +
		"&query_id=query%20with%20spaces" +
		"&hash=" + strings.Repeat("b", 64) +
		"&auth_date=" + parsed.Get("auth_date")
	first, err := verifier.Verify(raw)
	if err != nil {
		t.Fatalf("Verify(raw): %v", err)
	}
	second, err := verifier.Verify(reordered)
	if err != nil {
		t.Fatalf("Verify(reordered): %v", err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("malleable raw query changed replay fingerprint")
	}
}

func TestPinnedTelegramKeysAndConfiguration(t *testing.T) {
	t.Parallel()
	clock := ClockFunc(func() time.Time { return time.Unix(1, 0) })
	for _, environment := range []Environment{EnvironmentTest, EnvironmentProduction} {
		verifier, err := NewTelegramVerifier(testBotID, environment, []int64{testUserID}, clock)
		if err != nil {
			t.Fatalf("NewTelegramVerifier(%s): %v", environment, err)
		}
		if len(verifier.publicKey) != ed25519.PublicKeySize {
			t.Fatalf("%s key size = %d", environment, len(verifier.publicKey))
		}
		if verifier.SignatureEnvironment() != environment {
			t.Fatalf("signature environment = %q want %q", verifier.SignatureEnvironment(), environment)
		}
		wantHex := telegramTestPublicKeyHex
		if environment == EnvironmentProduction {
			wantHex = telegramProductionPublicKeyHex
		}
		if got := hex.EncodeToString(verifier.publicKey); got != wantHex {
			t.Fatalf("%s public key = %s want %s", environment, got, wantHex)
		}
	}
	if TelegramMaximumAge != 300*time.Second || TelegramFutureSkew != 30*time.Second {
		t.Fatalf("Telegram security policy drifted: max_age=%s future_skew=%s", TelegramMaximumAge, TelegramFutureSkew)
	}
	if _, err := NewTelegramVerifier(testBotID, "unknown", []int64{testUserID}, clock); err == nil {
		t.Fatal("unknown environment accepted")
	}
	if _, err := newTelegramVerifier(testBotID, make([]byte, ed25519.PublicKeySize), []int64{testUserID, testUserID}, 5*time.Minute, 30*time.Second, clock); err == nil {
		t.Fatal("duplicate allowlist accepted")
	}
	if _, err := newTelegramVerifier(testBotID, make([]byte, ed25519.PublicKeySize), []int64{testUserID, testUserID + 1}, 5*time.Minute, 30*time.Second, clock); err == nil {
		t.Fatal("multi-owner allowlist accepted without ADR revisit")
	}
}

func TestReasonOfDoesNotClassifyUnrelatedErrors(t *testing.T) {
	t.Parallel()
	if got := ReasonOf(errors.New("unrelated")); got != "" {
		t.Fatalf("ReasonOf() = %q", got)
	}
}

func mustTestVerifier(t *testing.T, publicKey ed25519.PublicKey, allowed []int64, now time.Time) *TelegramVerifier {
	t.Helper()
	verifier, err := newTelegramVerifier(
		testBotID,
		publicKey,
		allowed,
		5*time.Minute,
		30*time.Second,
		ClockFunc(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("newTelegramVerifier: %v", err)
	}
	return verifier
}

func signedInitData(values map[string]string, privateKey ed25519.PrivateKey) string {
	copyValues := make(map[string]string, len(values)+1)
	for key, value := range values {
		copyValues[key] = value
	}
	signature := ed25519.Sign(privateKey, canonicalDataCheckString(testBotID, copyValues))
	copyValues["signature"] = base64.RawURLEncoding.EncodeToString(signature)
	query := make(url.Values, len(copyValues))
	for key, value := range copyValues {
		query.Set(key, value)
	}
	return query.Encode()
}

func FuzzTelegramVerifier(f *testing.F) {
	now := time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC)
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	verifier, err := newTelegramVerifier(testBotID, publicKey, []int64{testUserID}, TelegramMaximumAge, TelegramFutureSkew, ClockFunc(func() time.Time { return now }))
	if err != nil {
		f.Fatalf("newTelegramVerifier(): %v", err)
	}
	valid := signedInitData(map[string]string{
		"auth_date": strconv.FormatInt(now.Unix(), 10),
		"query_id":  "fuzz-seed",
		"user":      `{"id":900000000001,"first_name":"Kondor"}`,
	}, privateKey)
	encodedKeys := strings.NewReplacer(
		"auth_date=", "%61uth_date=",
		"signature=", "%73ignature=",
		"user=", "%75ser=",
	).Replace(valid)
	for _, corpus := range []string{
		valid,
		encodedKeys,
		"",
		"auth_date=1&signature=x&user=x",
		"auth_date=%zz&signature=x&user=x",
		strings.Repeat("x", maxInitDataBytes+1),
	} {
		f.Add(corpus)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		identity, err := verifier.Verify(raw)
		if err != nil {
			if err.Error() != "telegram init data rejected" || ReasonOf(err) == "" {
				t.Fatalf("unbounded verifier error: %T %q", err, err.Error())
			}
			return
		}
		if identity.UserID != testUserID || identity.AuthDate.IsZero() || identity.Fingerprint == ([32]byte{}) {
			t.Fatalf("invalid accepted identity: %+v", identity)
		}
	})
}
