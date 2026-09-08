package observability

import "context"

// CausalContext связывает событие с исходной задачей и конкретной попыткой работы.
type CausalContext struct {
	TraceID              string
	TaskID               string
	StageID              string
	AttemptID            string
	ToolCallID           string
	InvocationID         string
	SkillID              string
	SkillDigest          string
	ActivationGeneration int64
	FenceEpoch           int64
	Tool                 string
	EffectID             string
	IdempotencyKey       string
	GrantSetDigest       string
}

// Child возвращает дочерний контекст: пустые поля наследуются от родителя.
func (parent CausalContext) Child(child CausalContext) CausalContext {
	result := parent
	if child.TraceID != "" {
		result.TraceID = child.TraceID
	}
	if child.TaskID != "" {
		result.TaskID = child.TaskID
	}
	if child.StageID != "" {
		result.StageID = child.StageID
	}
	if child.ToolCallID != "" {
		result.ToolCallID = child.ToolCallID
	}
	if child.AttemptID != "" {
		result.AttemptID = child.AttemptID
	}
	if child.InvocationID != "" {
		result.InvocationID = child.InvocationID
	}
	if child.SkillID != "" {
		result.SkillID = child.SkillID
	}
	if child.SkillDigest != "" {
		result.SkillDigest = child.SkillDigest
	}
	if child.ActivationGeneration != 0 {
		result.ActivationGeneration = child.ActivationGeneration
	}
	if child.FenceEpoch != 0 {
		result.FenceEpoch = child.FenceEpoch
	}
	if child.Tool != "" {
		result.Tool = child.Tool
	}
	if child.EffectID != "" {
		result.EffectID = child.EffectID
	}
	if child.IdempotencyKey != "" {
		result.IdempotencyKey = child.IdempotencyKey
	}
	if child.GrantSetDigest != "" {
		result.GrantSetDigest = child.GrantSetDigest
	}
	return result
}

type causalContextKey struct{}

// WithCausalContext добавляет причинный контекст, наследуя незаданные поля.
func WithCausalContext(parent context.Context, child CausalContext) context.Context {
	if parent == nil {
		parent = context.Background()
	}

	if current, ok := CausalContextFromContext(parent); ok {
		child = current.Child(child)
	}
	return context.WithValue(parent, causalContextKey{}, child)
}

// CausalContextFromContext извлекает причинный контекст без изменения исходного context.Context.
func CausalContextFromContext(ctx context.Context) (CausalContext, bool) {
	if ctx == nil {
		return CausalContext{}, false
	}
	causal, ok := ctx.Value(causalContextKey{}).(CausalContext)
	return causal, ok
}
