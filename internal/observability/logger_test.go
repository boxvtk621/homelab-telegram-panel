package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type eventRecord struct {
	SchemaVersion  string         `json:"schema_version"`
	Timestamp      string         `json:"timestamp"`
	Level          string         `json:"level"`
	Name           string         `json:"event"`
	Message        string         `json:"message"`
	TraceID        string         `json:"trace_id"`
	TaskID         string         `json:"task_id"`
	StageID        string         `json:"stage_id"`
	AttemptID      string         `json:"attempt_id"`
	ToolCallID     string         `json:"tool_call_id"`
	IdempotencyKey string         `json:"idempotency_key"`
	GrantSetDigest string         `json:"grant_set_digest"`
	Outcome        string         `json:"outcome"`
	ErrorCode      string         `json:"error_code"`
	ErrorMessage   string         `json:"error_message"`
	DurationMS     int64          `json:"duration_ms"`
	Attributes     map[string]any `json:"attributes"`
}

func TestLoggerWritesStructuredEventAndRedactsSecrets(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, time.August, 26, 10, 0, 0, 0, time.UTC)
	buffer := bytes.Buffer{}
	logger, err := New(&buffer, WithClock(ClockFunc(func() time.Time { return fixedTime })))
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}

	ctx := WithCausalContext(
		context.Background(),
		CausalContext{
			TraceID:        "trace-123",
			TaskID:         "task-8",
			StageID:        "stage-bootstrap",
			AttemptID:      "attempt-3",
			ToolCallID:     "tool-call-1",
			IdempotencyKey: "invoke-once-1",
			GrantSetDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	)

	err = logger.Log(
		ctx,
		Event{
			Level:     LevelInfo,
			Name:      "skills.load.started",
			Message:   "загрузка skills",
			Outcome:   OutcomeStarted,
			ErrorCode: "SKILLS_START",
			Duration:  123 * time.Millisecond,
			Attributes: map[string]any{
				"user": map[string]any{
					"username":  "alice",
					"api_token": "should-be-redacted",
					"nested": map[string]any{
						"password": "secret",
					},
					"raw":      []byte{1, 2, 3},
					"attempts": []any{"value", map[string]any{"Auth": "bearer"}},
				},
			},
			Err: errors.New("тестовая ошибка"),
		},
	)
	if err != nil {
		t.Fatalf("Log() вернул ошибку: %v", err)
	}

	line := strings.TrimSpace(buffer.String())
	if line == "" {
		t.Fatal("лог не содержит строку")
	}

	var record eventRecord
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("не удалось декодировать JSONL: %v", err)
	}
	if record.SchemaVersion != SchemaVersion {
		t.Fatalf("неверная схема: %q", record.SchemaVersion)
	}
	if record.Timestamp != fixedTime.Format(time.RFC3339Nano) {
		t.Fatalf("неверный timestamp: %q", record.Timestamp)
	}

	if record.TraceID != "trace-123" || record.TaskID != "task-8" || record.StageID != "stage-bootstrap" {
		t.Fatalf("неверный causal context: %v", record)
	}
	if record.AttemptID != "attempt-3" || record.IdempotencyKey != "invoke-once-1" ||
		record.GrantSetDigest != "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("неверные causal pin: %v", record)
	}
	if record.ToolCallID != "tool-call-1" {
		t.Fatalf("неверный ToolCallID: %q", record.ToolCallID)
	}
	if record.DurationMS != 123 {
		t.Fatalf("неверная длительность в мс: %d", record.DurationMS)
	}
	if record.ErrorMessage != "тестовая ошибка" {
		t.Fatalf("неверное сообщение ошибки: %q", record.ErrorMessage)
	}

	user, ok := record.Attributes["user"].(map[string]any)
	if !ok {
		t.Fatalf("атрибут user имеет неожиданный тип: %T", record.Attributes["user"])
	}
	if user["api_token"] != RedactedValue {
		t.Fatalf("api_token должен быть редактирован: %v", user["api_token"])
	}
	if user["raw"] != RedactedValue {
		t.Fatalf("raw секретные байты не редактируются: %v", user["raw"])
	}
	nested, ok := user["nested"].(map[string]any)
	if !ok || nested["password"] != RedactedValue {
		t.Fatalf("вложенный password должен быть редактирован: %v", nested["password"])
	}
	attempts, ok := user["attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("attempts должен быть массивом с двумя элементами: %#v", user["attempts"])
	}
	attemptMap, ok := attempts[1].(map[string]any)
	if !ok || attemptMap["Auth"] != RedactedValue {
		t.Fatalf("чувствительный ключ в массиве не отредактирован: %v", attempts[1])
	}
}

func TestLoggerRedactsSensitivePatternsInMessageErrorAndNestedString(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, time.August, 26, 11, 0, 0, 0, time.UTC)
	testCases := []struct {
		name        string
		message     string
		err         error
		attributes  map[string]any
		wantMessage string
		wantError   string
	}{
		{
			name:        "в сообщении есть Authorization Bearer",
			message:     "вызов endpoints с Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890",
			wantMessage: "вызов endpoints с Authorization: Bearer [REDACTED]",
		},
		{
			name:      "в ошибке есть token=...",
			message:   "ошибка верификации",
			err:       errors.New("получен token=super-long-secret-value"),
			wantError: "получен token=[REDACTED]",
		},
		{
			name:    "вложенный текстовый атрибут содержит Telegram token",
			message: "выполнение внутреннего этапа",
			attributes: map[string]any{
				"payload": map[string]any{
					"comment": "telegram 12345678:ABCDEFGHIJKLMNOPQRSTUVWXYZ12345678901234567890",
					"note":    "только текст",
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buffer := bytes.Buffer{}
			logger, err := New(&buffer, WithClock(ClockFunc(func() time.Time { return fixedTime })))
			if err != nil {
				t.Fatalf("не удалось создать логгер: %v", err)
			}

			err = logger.Log(
				context.Background(),
				Event{
					Level:      LevelInfo,
					Name:       "sensitive.texts",
					Message:    tc.message,
					Outcome:    OutcomeFailed,
					ErrorCode:  "CHECK",
					Duration:   time.Millisecond,
					Err:        tc.err,
					Attributes: tc.attributes,
				},
			)
			if err != nil {
				t.Fatalf("Log() вернул ошибку: %v", err)
			}

			line := strings.TrimSpace(buffer.String())
			if line == "" {
				t.Fatal("журнал пуст")
			}
			var record eventRecord
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("невалидный JSON: %v", err)
			}

			if tc.wantMessage != "" && record.Message != tc.wantMessage {
				t.Fatalf("неверное message: %q", record.Message)
			}
			if tc.wantError != "" && record.ErrorMessage != tc.wantError {
				t.Fatalf("неверный error message: %q", record.ErrorMessage)
			}
			if tc.attributes != nil {
				payload, ok := record.Attributes["payload"].(map[string]any)
				if !ok {
					t.Fatalf("payload должен быть map[string]any: %T", record.Attributes["payload"])
				}
				comment, ok := payload["comment"].(string)
				if !ok {
					t.Fatalf("comment должен быть строкой: %T", payload["comment"])
				}
				if strings.Contains(comment, "12345678:") {
					t.Fatalf("вложенный comment не редактирован")
				}
				if !strings.Contains(comment, RedactedValue) {
					t.Fatalf("comment должен содержать редактирование: %v", comment)
				}
				if payload["note"] != "только текст" {
					t.Fatalf("обычный текст должен остаться: %v", payload["note"])
				}
			}
		})
	}
}

func TestLoggerDeterminismWithFixedClock(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, time.August, 26, 10, 0, 1, 111_000_000, time.UTC)
	buffer := bytes.Buffer{}
	logger, err := New(&buffer, WithClock(ClockFunc(func() time.Time { return fixedTime })))
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}

	event := Event{
		Level:      LevelInfo,
		Name:       "determinism.check",
		Message:    "same message",
		Outcome:    OutcomeSucceeded,
		Duration:   50 * time.Millisecond,
		Attributes: map[string]any{"stage": "same"},
	}

	if err := logger.Log(context.Background(), event); err != nil {
		t.Fatalf("первая запись: %v", err)
	}
	if err := logger.Log(context.Background(), event); err != nil {
		t.Fatalf("вторая запись: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("ожидалось 2 строки, получено %d", len(lines))
	}
	if lines[0] != lines[1] {
		t.Fatalf("записи должны быть идентичны при фиксированном времени:\n%q\n%q", lines[0], lines[1])
	}
}

func TestRedactTextKeepsSafeTexts(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "обычный русский текст без секретов",
			input: "Это обычный русскоязычный текст для проверки",
			want:  "Это обычный русскоязычный текст для проверки",
		},
		{
			name:  "токен без маркера",
			input: "У нас tokenomics это новая метрика",
			want:  "У нас tokenomics это новая метрика",
		},
		{
			name:  "идентификатор не похож на секрет",
			input: "task-2026-08-26",
			want:  "task-2026-08-26",
		},
		{
			name:  "слово secret без пары ключ=значение",
			input: "секретное поле secret code не является ключом",
			want:  "секретное поле secret code не является ключом",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactText(tc.input); got != tc.want {
				t.Fatalf("ожидалось %q, получено %q", tc.want, got)
			}
		})
	}
}

func TestRedactTextCoversExplicitShortAndQuotedSecrets(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		input string
		want  string
	}{
		{
			input: `ответ {"token":"abc"}`,
			want:  `ответ {"token":"[REDACTED]"}`,
		},
		{
			input: "Authorization: Bearer abc",
			want:  "Authorization: Bearer [REDACTED]",
		},
		{
			input: "Authorization=Bearer abc",
			want:  "Authorization=Bearer [REDACTED]",
		},
		{
			input: "Cookie: session=abc",
			want:  "Cookie: [REDACTED]",
		},
		{
			input: "initData=auth_date%3D1%26signature%3Dabc",
			want:  "initData=[REDACTED]",
		},
		{
			input: "%61uth_date=1&%75ser=%7B%22id%22%3A42%7D&%73ignature=abc",
			want:  RedactedValue,
		},
		{
			input: `payload {"signature":"short","csrf_token":"nonce"}`,
			want:  `payload {"signature":"[REDACTED]","csrf_token":"[REDACTED]"}`,
		},
		{
			input: "ошибка sk-proj-abcdefghijklmnopqrstuvwxyz012345",
			want:  "ошибка [REDACTED]",
		},
	}

	for _, testCase := range testCases {
		if got := redactText(testCase.input); got != testCase.want {
			t.Errorf("redactText(%q) = %q, ожидалось %q", testCase.input, got, testCase.want)
		}
	}
}

func TestLoggerRedactsHeadersInCausalAndNestedTextWithoutHidingAuthor(t *testing.T) {
	t.Parallel()

	buffer := bytes.Buffer{}
	logger, err := New(&buffer)
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}
	ctx := WithCausalContext(context.Background(), CausalContext{
		TraceID: "Authorization=Bearer abc",
		TaskID:  "Cookie: session=abc",
	})
	err = logger.Log(ctx, Event{
		Level:   LevelError,
		Name:    "redaction.paths",
		Message: "Authorization: Basic Zm9vOmJhcg==",
		Outcome: OutcomeFailed,
		Err:     errors.New("Set-Cookie: sid=abc; HttpOnly"),
		Attributes: map[string]any{
			"metadata": map[string]any{
				"author": "Kondor",
				"auth":   "секрет",
				"note":   "Cookie=session=abc",
			},
		},
	})
	if err != nil {
		t.Fatalf("Log(): %v", err)
	}

	var record eventRecord
	if err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &record); err != nil {
		t.Fatalf("не удалось прочитать событие: %v", err)
	}
	if record.TraceID != "Authorization=Bearer [REDACTED]" || record.TaskID != "Cookie: [REDACTED]" {
		t.Fatalf("causal поля не очищены: trace=%q task=%q", record.TraceID, record.TaskID)
	}
	if record.Message != "Authorization: Basic [REDACTED]" || record.ErrorMessage != "Set-Cookie: [REDACTED]" {
		t.Fatalf("message/error не очищены: message=%q error=%q", record.Message, record.ErrorMessage)
	}
	metadata := record.Attributes["metadata"].(map[string]any)
	if metadata["author"] != "Kondor" {
		t.Fatalf("обычное поле author скрыто: %#v", metadata)
	}
	if metadata["auth"] != RedactedValue || metadata["note"] != "Cookie="+RedactedValue {
		t.Fatalf("чувствительные поля не очищены: %#v", metadata)
	}
}

func TestLoggerNeverEmitsRawMobileAuthenticationMaterial(t *testing.T) {
	t.Parallel()

	const rawInitData = `auth_date=1787968800&query_id=AAE&user=%7B%22id%22%3A900000000001%7D&signature=secret-signature`
	buffer := bytes.Buffer{}
	logger, err := New(&buffer)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	err = logger.Log(context.Background(), Event{
		Level:   LevelWarn,
		Name:    "mobile.auth.rejected",
		Message: rawInitData,
		Outcome: OutcomeFailed,
		Err:     errors.New("request body: " + rawInitData),
		Attributes: map[string]any{
			"nested":           map[string]any{"body": rawInitData},
			"bootstrap":        "bootstrap-secret",
			"auth_fingerprint": "fingerprint-secret",
		},
	})
	if err != nil {
		t.Fatalf("Log(): %v", err)
	}
	logged := buffer.String()
	for _, forbidden := range []string{"900000000001", "secret-signature", "bootstrap-secret", "fingerprint-secret", "auth_date=", "user=%7B"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("mobile auth material %q escaped into log: %s", forbidden, logged)
		}
	}
}

func TestLoggerConcurrentWrites(t *testing.T) {
	const events = 80

	ctx := context.Background()
	var (
		wg     sync.WaitGroup
		lines  = make(chan error, events)
		buffer = &bytes.Buffer{}
	)
	logger := &Logger{
		mu:     sync.Mutex{},
		writer: buffer,
		clock:  systemClock{},
	}

	for i := 0; i < events; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			err := logger.Log(
				ctx,
				Event{
					Level:      LevelInfo,
					Name:       "parallel.event",
					Message:    "параллельная проверка",
					Outcome:    OutcomeSucceeded,
					Duration:   time.Duration(index) * time.Millisecond,
					Attributes: map[string]any{"index": index},
				},
			)
			lines <- err
		}(i)
	}
	wg.Wait()
	close(lines)

	for err := range lines {
		if err != nil {
			t.Fatalf("ошибка при параллельной записи: %v", err)
		}
	}

	written := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(written) != events {
		t.Fatalf("ожидалось %d строк: получено %d", events, len(written))
	}
	for _, line := range written {
		var record eventRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("невалидный JSON в строке журнала: %v", err)
		}
		if record.SchemaVersion != SchemaVersion {
			t.Fatalf("неверная схема в одной из строк: %q", record.SchemaVersion)
		}
	}
}

func TestSanitizeAttributesRedactsRecursively(t *testing.T) {
	t.Parallel()

	attributes := map[string]any{
		"public":    "ok",
		"authToken": "should-redact",
		"nested": map[string]any{
			"password": "secret",
			"payload": []any{
				map[string]any{
					"api_key": "kek",
				},
				"safe",
			},
		},
		"raw_secret": []byte("abc"),
		"list":       []any{"a", map[string]any{"cookie": "c"}},
		"init_data":  "auth_date=1&signature=abc",
		"csrfToken":  "nonce",
	}

	sanitized := sanitizeAttributes(attributes)
	if sanitized["authToken"] != RedactedValue {
		t.Fatalf("authToken должен быть редактирован: %v", sanitized["authToken"])
	}
	if sanitized["raw_secret"] != RedactedValue {
		t.Fatalf("raw_secret должен быть редактирован: %v", sanitized["raw_secret"])
	}
	if sanitized["init_data"] != RedactedValue || sanitized["csrfToken"] != RedactedValue {
		t.Fatalf("mobile auth material must be redacted: %#v", sanitized)
	}
	nested, ok := sanitized["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested должен быть map[string]any: %T", sanitized["nested"])
	}
	if nested["password"] != RedactedValue {
		t.Fatalf("nested.password должен быть редактирован: %v", nested["password"])
	}
	payload, ok := nested["payload"].([]any)
	if !ok || len(payload) != 2 {
		t.Fatalf("payload должен быть массивом из 2 элементов: %#v", nested["payload"])
	}
	item0, ok := payload[0].(map[string]any)
	if !ok || item0["api_key"] != RedactedValue {
		t.Fatalf("вложенный api_key должен быть редактирован: %v", payload[0])
	}
	list, ok := sanitized["list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("list должен быть массивом из 2 элементов: %#v", sanitized["list"])
	}
	item1, ok := list[1].(map[string]any)
	if !ok {
		t.Fatalf("list[1] должен быть map: %#v", list[1])
	}
	if item1["cookie"] != RedactedValue {
		t.Fatalf("чувствительный ключ cookie внутри list[1] должен быть редактирован: %#v", item1)
	}
}
