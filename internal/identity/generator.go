// Package identity создаёт непрозрачные идентификаторы причинных цепочек.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
)

var prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Generator позволяет заменить источник случайности в тестах.
type Generator struct {
	reader io.Reader
}

// NewGenerator создаёт генератор на криптографическом источнике случайности.
func NewGenerator() Generator {
	return Generator{reader: rand.Reader}
}

// NewGeneratorFrom создаёт генератор с явно заданным источником.
func NewGeneratorFrom(reader io.Reader) (Generator, error) {
	if reader == nil {
		return Generator{}, fmt.Errorf("источник случайности не задан")
	}
	return Generator{reader: reader}, nil
}

// New возвращает идентификатор вида prefix-<128 бит в hex>.
func (g Generator) New(prefix string) (string, error) {
	if !prefixPattern.MatchString(prefix) {
		return "", fmt.Errorf("некорректный префикс идентификатора %q", prefix)
	}
	if g.reader == nil {
		return "", fmt.Errorf("генератор идентификаторов не инициализирован")
	}

	value := make([]byte, 16)
	if _, err := io.ReadFull(g.reader, value); err != nil {
		return "", fmt.Errorf("чтение случайного идентификатора: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(value), nil
}
