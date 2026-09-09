package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

// CanonicalCommand validates the C1 wire shape and serializes its semantic
// value using the bounded JCS profile defined by docs/harness-v1.md.
func CanonicalCommand(raw []byte) ([]byte, string, error) {
	if err := harnessprotocol.Validate("command", raw); err != nil {
		return nil, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", err
	}
	canonical := make([]byte, 0, len(raw))
	canonical, err := appendCanonical(canonical, value)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(digest[:]), nil
}

func appendCanonical(destination []byte, value any) ([]byte, error) {
	switch typed := value.(type) {
	case nil:
		return append(destination, "null"...), nil
	case bool:
		return strconv.AppendBool(destination, typed), nil
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil || parsed < 0 || parsed > harnessprotocol.MaximumSafeInteger {
			return nil, errors.New("canonical number is not a safe unsigned integer")
		}
		return append(destination, typed...), nil
	case string:
		return appendJSONString(destination, typed), nil
	case []any:
		destination = append(destination, '[')
		for index, item := range typed {
			if index > 0 {
				destination = append(destination, ',')
			}
			var err error
			destination, err = appendCanonical(destination, item)
			if err != nil {
				return nil, err
			}
		}
		return append(destination, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		destination = append(destination, '{')
		for index, key := range keys {
			if index > 0 {
				destination = append(destination, ',')
			}
			destination = appendJSONString(destination, key)
			destination = append(destination, ':')
			var err error
			destination, err = appendCanonical(destination, typed[key])
			if err != nil {
				return nil, err
			}
		}
		return append(destination, '}'), nil
	default:
		return nil, fmt.Errorf("unsupported canonical value %T", value)
	}
}

func appendJSONString(destination []byte, value string) []byte {
	destination = append(destination, '"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			destination = append(destination, '\\', byte(character))
		case '\b':
			destination = append(destination, `\b`...)
		case '\f':
			destination = append(destination, `\f`...)
		case '\n':
			destination = append(destination, `\n`...)
		case '\r':
			destination = append(destination, `\r`...)
		case '\t':
			destination = append(destination, `\t`...)
		default:
			if character < 0x20 {
				destination = append(destination, `\u00`...)
				destination = strconv.AppendInt(destination, int64(character>>4), 16)
				destination = strconv.AppendInt(destination, int64(character&0x0f), 16)
			} else {
				destination = utf8.AppendRune(destination, character)
			}
		}
	}
	return append(destination, '"')
}
