package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// SchemaVersion однозначно задаёт формат каждой строки журнала.
const SchemaVersion = "fixik.observability/v1"

// RedactedValue означает, что значение было очищено как потенциально чувствительное.
const RedactedValue = "[REDACTED]"

// Level задаёт важность события.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Outcome задаёт результат операции, к которой относится событие.
type Outcome string

const (
	OutcomeStarted   Outcome = "started"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeCancelled Outcome = "cancelled"
	OutcomeSkipped   Outcome = "skipped"
	OutcomeUnknown   Outcome = "unknown"
)

// Event описывает одно структурированное событие до безопасной сериализации.
type Event struct {
	Level      Level
	Name       string
	Message    string
	Outcome    Outcome
	ErrorCode  string
	Duration   time.Duration
	Err        error
	Attributes map[string]any
}

// Clock позволяет воспроизводимо проверять время событий.
type Clock interface {
	Now() time.Time
}

// ClockFunc адаптирует функцию к Clock.
type ClockFunc func() time.Time

// Now возвращает текущее время из функции.
func (clock ClockFunc) Now() time.Time {
	return clock()
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

type config struct {
	clock Clock
}

// Option изменяет безопасную конфигурацию Logger.
type Option func(*config) error

// WithClock внедряет источник времени.
func WithClock(clock Clock) Option {
	return func(cfg *config) error {
		if isNil(clock) {
			return errors.New("источник времени не задан")
		}
		cfg.clock = clock
		return nil
	}
}

// Logger пишет по одному JSON-событию в строку и не допускает смешивания конкурентных записей.
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
	clock  Clock
}

// New создаёт Logger с обязательным назначением записи.
func New(writer io.Writer, options ...Option) (*Logger, error) {
	if isNil(writer) {
		return nil, errors.New("назначение для журнала не задано")
	}

	cfg := config{clock: systemClock{}}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("параметр журнала %d не задан", index)
		}
		if err := option(&cfg); err != nil {
			return nil, fmt.Errorf("параметр журнала %d: %w", index, err)
		}
	}

	return &Logger{writer: writer, clock: cfg.clock}, nil
}

type wireEvent struct {
	SchemaVersion        string         `json:"schema_version"`
	Timestamp            string         `json:"timestamp"`
	Level                Level          `json:"level"`
	Name                 string         `json:"event"`
	Message              string         `json:"message"`
	TraceID              string         `json:"trace_id"`
	TaskID               string         `json:"task_id"`
	StageID              string         `json:"stage_id"`
	AttemptID            string         `json:"attempt_id"`
	ToolCallID           string         `json:"tool_call_id"`
	InvocationID         string         `json:"invocation_id"`
	SkillID              string         `json:"skill_id"`
	SkillDigest          string         `json:"skill_digest"`
	ActivationGeneration int64          `json:"activation_generation"`
	FenceEpoch           int64          `json:"fence_epoch"`
	Tool                 string         `json:"tool"`
	EffectID             string         `json:"effect_id"`
	IdempotencyKey       string         `json:"idempotency_key"`
	GrantSetDigest       string         `json:"grant_set_digest"`
	Outcome              Outcome        `json:"outcome"`
	ErrorCode            string         `json:"error_code"`
	ErrorMessage         string         `json:"error_message"`
	DurationMS           int64          `json:"duration_ms"`
	Attributes           map[string]any `json:"attributes"`
}

// Log проверяет, очищает и атомарно записывает одно событие.
func (logger *Logger) Log(ctx context.Context, event Event) error {
	if logger == nil {
		return errors.New("журнал не инициализирован")
	}
	if err := validateEvent(event); err != nil {
		return err
	}

	causal, _ := CausalContextFromContext(ctx)

	logger.mu.Lock()
	defer logger.mu.Unlock()

	wire := wireEvent{
		SchemaVersion:        SchemaVersion,
		Timestamp:            logger.clock.Now().UTC().Format(time.RFC3339Nano),
		Level:                event.Level,
		Name:                 redactText(event.Name),
		Message:              redactText(event.Message),
		TraceID:              redactText(causal.TraceID),
		TaskID:               redactText(causal.TaskID),
		StageID:              redactText(causal.StageID),
		AttemptID:            redactText(causal.AttemptID),
		ToolCallID:           redactText(causal.ToolCallID),
		InvocationID:         redactText(causal.InvocationID),
		SkillID:              redactText(causal.SkillID),
		SkillDigest:          redactText(causal.SkillDigest),
		ActivationGeneration: causal.ActivationGeneration,
		FenceEpoch:           causal.FenceEpoch,
		Tool:                 redactText(causal.Tool),
		EffectID:             redactText(causal.EffectID),
		IdempotencyKey:       redactText(causal.IdempotencyKey),
		GrantSetDigest:       redactText(causal.GrantSetDigest),
		Outcome:              event.Outcome,
		ErrorCode:            redactText(event.ErrorCode),
		ErrorMessage:         safeErrorText(event.Err),
		DurationMS:           event.Duration.Milliseconds(),
		Attributes:           sanitizeAttributes(event.Attributes),
	}

	payload, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("не удалось сериализовать событие журнала: %w", err)
	}
	payload = append(payload, '\n')
	if err := writeAll(logger.writer, payload); err != nil {
		return fmt.Errorf("не удалось записать событие журнала: %w", err)
	}
	return nil
}

func validateEvent(event Event) error {
	if strings.TrimSpace(event.Name) == "" {
		return errors.New("имя события не задано")
	}
	if !event.Level.valid() {
		return fmt.Errorf("неподдерживаемый уровень журнала %q", event.Level)
	}
	if !event.Outcome.valid() {
		return fmt.Errorf("неподдерживаемый результат операции %q", event.Outcome)
	}
	if event.Duration < 0 {
		return errors.New("длительность события не может быть отрицательной")
	}
	return nil
}

func (level Level) valid() bool {
	switch level {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
		return true
	default:
		return false
	}
}

func (outcome Outcome) valid() bool {
	switch outcome {
	case "", OutcomeStarted, OutcomeSucceeded, OutcomeFailed, OutcomeCancelled, OutcomeSkipped, OutcomeUnknown:
		return true
	default:
		return false
	}
}

func safeErrorText(err error) (message string) {
	if err == nil {
		return ""
	}
	defer func() {
		if recover() != nil {
			message = RedactedValue
		}
	}()
	return redactText(err.Error())
}

func sanitizeAttributes(attributes map[string]any) map[string]any {
	if attributes == nil {
		return nil
	}
	sanitized := make(map[string]any, len(attributes))
	seen := make(map[uintptr]struct{}, len(attributes))
	for key, value := range attributes {
		if isSensitiveKey(key) {
			sanitized[key] = RedactedValue
			continue
		}
		sanitized[key] = sanitizeValue(value, 0, seen)
	}
	return sanitized
}

func sanitizeValue(value any, depth int, seen map[uintptr]struct{}) (sanitized any) {
	defer func() {
		if recover() != nil {
			sanitized = RedactedValue
		}
	}()

	if depth > 64 {
		return RedactedValue
	}
	if isNil(value) {
		return nil
	}
	if isSensitiveValue(value) {
		return RedactedValue
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.String:
		return redactText(reflected.String())
	case reflect.Bool:
		return reflected.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflected.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return reflected.Uint()
	case reflect.Float32, reflect.Float64:
		return reflected.Float()
	case reflect.Interface:
		return sanitizeValue(reflected.Interface(), depth+1, seen)
	case reflect.Pointer:
		if reflected.IsNil() {
			return nil
		}
		ptr := reflected.Pointer()
		if _, ok := seen[ptr]; ok {
			return RedactedValue
		}
		seen[ptr] = struct{}{}
		defer delete(seen, ptr)
		return sanitizeValue(reflected.Elem().Interface(), depth+1, seen)
	case reflect.Array, reflect.Slice:
		if reflected.Type().Elem().Kind() == reflect.Uint8 {
			return RedactedValue
		}
		length := reflected.Len()
		output := make([]any, 0, length)
		for i := 0; i < length; i++ {
			item := reflected.Index(i)
			if !item.IsValid() {
				output = append(output, nil)
				continue
			}
			output = append(output, sanitizeValue(item.Interface(), depth+1, seen))
		}
		return output
	case reflect.Map:
		if reflected.IsNil() {
			return nil
		}
		ptr := reflected.Pointer()
		if _, ok := seen[ptr]; ok {
			return RedactedValue
		}
		seen[ptr] = struct{}{}
		defer delete(seen, ptr)

		output := make(map[string]any, reflected.Len())
		for _, key := range reflected.MapKeys() {
			keyText := sanitizeMapKey(key)
			if isSensitiveKey(keyText) {
				output[keyText] = RedactedValue
				continue
			}
			entry := reflected.MapIndex(key)
			if !entry.IsValid() {
				output[keyText] = nil
				continue
			}
			output[keyText] = sanitizeValue(entry.Interface(), depth+1, seen)
		}
		return output
	case reflect.Struct:
		output := make(map[string]any, reflected.NumField())
		refType := reflected.Type()
		for i := 0; i < reflected.NumField(); i++ {
			field := refType.Field(i)
			value := reflected.Field(i)
			if !value.IsValid() || !value.CanInterface() {
				continue
			}
			fieldKey := field.Name
			if jsonTag := field.Tag.Get("json"); jsonTag != "" && jsonTag != "-" {
				if parsed, _, _ := strings.Cut(jsonTag, ","); parsed != "" {
					fieldKey = parsed
				}
			}
			if isSensitiveKey(fieldKey) {
				output[fieldKey] = RedactedValue
				continue
			}
			output[fieldKey] = sanitizeValue(value.Interface(), depth+1, seen)
		}
		return output
	default:
		return value
	}
}

func sanitizeMapKey(value reflect.Value) string {
	if value.Kind() == reflect.String {
		return strings.TrimSpace(value.String())
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value.Interface()))
}

func redactText(value string) string {
	redacted := strings.TrimSpace(value)
	if redacted == "" {
		return ""
	}
	// A raw Telegram initData query contains protected user JSON before its
	// signature field. Redacting only signature= would therefore leak identity.
	// Collapse the entire free-form value when the authenticated field set is
	// recognizable, independent of field order.
	if looksLikeTelegramInitData(redacted) {
		return RedactedValue
	}
	redacted = redactExplicitPairs(redacted)
	redacted = reAuthorizationHeader.ReplaceAllString(redacted, "${1}${2}"+RedactedValue)
	redacted = reCookieHeader.ReplaceAllString(redacted, "${1}"+RedactedValue)
	redacted = reBearerToken.ReplaceAllString(redacted, "${1}"+RedactedValue)
	redacted = reTelegramBotToken.ReplaceAllString(redacted, RedactedValue)
	redacted = reOpenAI.ReplaceAllString(redacted, RedactedValue)
	redacted = reGitHubToken.ReplaceAllString(redacted, RedactedValue)
	redacted = rePEMBlock.ReplaceAllString(redacted, RedactedValue)
	return redacted
}

func looksLikeTelegramInitData(text string) bool {
	var fields uint8
	for _, pair := range strings.Split(text, "&") {
		rawKey, _, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		key, err := url.QueryUnescape(strings.TrimSpace(rawKey))
		if err != nil {
			continue
		}
		key = strings.ToLower(key)
		switch {
		case hasFieldKeySuffix(key, "auth_date"):
			fields |= 1
		case hasFieldKeySuffix(key, "signature"):
			fields |= 2
		case hasFieldKeySuffix(key, "user"):
			fields |= 4
		}
	}
	return fields == 7
}

func hasFieldKeySuffix(value, key string) bool {
	if value == key {
		return true
	}
	if !strings.HasSuffix(value, key) {
		return false
	}
	prefix := strings.TrimSuffix(value, key)
	var boundary rune
	for _, current := range prefix {
		boundary = current
	}
	return !unicode.IsLetter(boundary) && !unicode.IsDigit(boundary) && boundary != '_'
}

var reAuthorizationHeader = regexp.MustCompile(`(?i)(\bauthorization\s*[:=]\s*)([A-Za-z][A-Za-z0-9_-]*\s+)?[^\s,;]+`)
var reCookieHeader = regexp.MustCompile(`(?i)(\b(?:set-cookie|cookie)\s*[:=]\s*)[^\r\n]+`)
var reBearerToken = regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+\/=-]{8,}`)
var reTelegramBotToken = regexp.MustCompile(`\b\d{8,}:[A-Za-z0-9_-]{35,}\b`)
var reOpenAI = regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{20,}\b`)
var reGitHubToken = regexp.MustCompile(`(?i)\b(?:github_pat_[A-Za-z0-9_]{35,}|gh[pousr]_[A-Za-z0-9]{35,}|gh[or]_[A-Za-z0-9]{35,}|ghs_[A-Za-z0-9]{35,}|ghr_[A-Za-z0-9]{35,}|ghu_[A-Za-z0-9]{35,}|ghl_[A-Za-z0-9]{35,})\b`)
var rePEMBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+ PRIVATE KEY-----.*?-----END [A-Z0-9 ]+ PRIVATE KEY-----`)
var reMarkedPairs = regexp.MustCompile(`(?i)["']?\b(token|api[_-]?key|password|secret|init[_-]?data|signature|csrf(?:[_-]?token)?|bootstrap|fingerprint)\b["']?\s*[:=]\s*`)

func redactExplicitPairs(text string) string {
	matches := reMarkedPairs.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var replaced strings.Builder
	last := 0
	for _, match := range matches {
		keyStart := match[0]
		valueStart := match[1]
		if keyStart < last {
			continue
		}

		valueOffset, replacement, ok := redactMarkedPairValue(text[valueStart:])
		replaced.WriteString(text[last:keyStart])
		replaced.WriteString(text[keyStart:valueStart])
		replaced.WriteString(replacement)

		if !ok {
			return replaced.String()
		}
		last = valueStart + valueOffset
	}
	replaced.WriteString(text[last:])
	return replaced.String()
}

func redactMarkedPairValue(value string) (int, string, bool) {
	if value == "" {
		return 0, RedactedValue, false
	}
	offset := 0
	for offset < len(value) && isWhitespace(value[offset]) {
		offset++
	}
	if offset >= len(value) {
		return 0, RedactedValue, false
	}

	first := value[offset]
	if first == '"' || first == '\'' {
		closeOffset := offset + 1
		for closeOffset < len(value) && value[closeOffset] != first {
			closeOffset++
		}
		if closeOffset >= len(value) {
			return len(value), RedactedValue, false
		}
		return closeOffset + 1, string(first) + RedactedValue + string(first), true
	}

	end := offset
	for end < len(value) && !isMarkedPairValueDelimiter(value[end]) {
		end++
	}
	if end == offset {
		return 0, RedactedValue, false
	}
	return end, RedactedValue, true
}

func isWhitespace(value byte) bool {
	return unicode.IsSpace(rune(value))
}

func isMarkedPairValueDelimiter(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n', ',', ';', ')', ']', '}', '{':
		return true
	default:
		return false
	}
}

var sensitiveKeys = []string{
	"token",
	"apikey",
	"api_key",
	"secret",
	"password",
	"passphrase",
	"passwd",
	"credentials",
	"credential",
	"authorization",
	"auth",
	"private_key",
	"privatekey",
	"access_token",
	"refresh_token",
	"session_id",
	"session",
	"cookie",
	"init_data",
	"initdata",
	"signature",
	"csrf",
	"csrf_token",
	"bootstrap",
	"fingerprint",
}

func isSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(normalized)
	padded := "_" + strings.Trim(normalized, "_") + "_"
	for _, marker := range sensitiveKeys {
		marker = "_" + marker + "_"
		if strings.Contains(padded, marker) {
			return true
		}
	}

	compact := strings.ReplaceAll(normalized, "_", "")
	for _, suffix := range []string{
		"token", "apikey", "secret", "password", "passphrase", "passwd",
		"credential", "credentials", "authorization", "privatekey",
		"accesstoken", "refreshtoken", "sessionid", "cookie",
		"initdata", "signature", "csrf", "csrftoken", "bootstrap", "fingerprint",
	} {
		if compact == suffix || strings.HasSuffix(compact, suffix) {
			return true
		}
	}
	return false
}

func isSensitiveValue(value any) bool {
	switch value.(type) {
	case []byte:
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Invalid:
		return true
	}
	return false
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if written < 0 || written > len(payload) {
			return errors.New("назначение журнала вернуло неверный размер записи")
		}
		payload = payload[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
