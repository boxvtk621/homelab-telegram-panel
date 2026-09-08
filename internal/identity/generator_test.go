package identity

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("нет энтропии") }

func TestGeneratorProducesStableShape(t *testing.T) {
	t.Parallel()

	generator, err := NewGeneratorFrom(bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatalf("NewGeneratorFrom(): %v", err)
	}
	value, err := generator.New("trace")
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if value != "trace-00000000000000000000000000000000" {
		t.Fatalf("неожиданный идентификатор: %s", value)
	}
}

func TestGeneratorRejectsInvalidPrefix(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"", "Trace", "../trace", strings.Repeat("a", 33)} {
		if _, err := NewGenerator().New(prefix); err == nil {
			t.Errorf("префикс %q должен быть отклонён", prefix)
		}
	}
}

func TestGeneratorPropagatesEntropyFailure(t *testing.T) {
	t.Parallel()

	generator, err := NewGeneratorFrom(failingReader{})
	if err != nil {
		t.Fatalf("NewGeneratorFrom(): %v", err)
	}
	if _, err := generator.New("trace"); err == nil {
		t.Fatal("ожидалась ошибка источника случайности")
	}
}
