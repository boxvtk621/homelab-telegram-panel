package observability

import (
	"context"
	"reflect"
	"testing"
)

func TestWithCausalContextInheritsFields(t *testing.T) {
	t.Parallel()

	parent := WithCausalContext(
		context.Background(),
		CausalContext{
			TraceID:        "trace-1",
			TaskID:         "task-1",
			StageID:        "stage-init",
			AttemptID:      "attempt-2",
			IdempotencyKey: "operation-1",
		},
	)

	withChild := WithCausalContext(
		parent,
		CausalContext{
			StageID:        "stage-run",
			ToolCallID:     "tool-7",
			GrantSetDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	)
	causal, ok := CausalContextFromContext(withChild)
	if !ok {
		t.Fatal("ожидался извлечённый контекст, но он отсутствует")
	}
	expected := CausalContext{
		TraceID:        "trace-1",
		TaskID:         "task-1",
		StageID:        "stage-run",
		AttemptID:      "attempt-2",
		ToolCallID:     "tool-7",
		IdempotencyKey: "operation-1",
		GrantSetDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	if !reflect.DeepEqual(causal, expected) {
		t.Fatalf("неверный объединённый контекст: %+v", causal)
	}
}
