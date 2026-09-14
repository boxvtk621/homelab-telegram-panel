// Package exactjson validates that JSON object keys match Go JSON tags exactly.
// encoding/json intentionally matches struct fields case-insensitively, which is
// unsafe at signed and migration contract boundaries.
package exactjson

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
)

// Shape reports whether every object key in raw exactly matches the complete
// JSON field set of target, recursively. Scalar values remain the responsibility
// of the typed decoder and domain validator.
func Shape(raw []byte, target any) bool {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	typeOfTarget := reflect.TypeOf(target)
	if typeOfTarget == nil {
		return false
	}
	return matches(value, typeOfTarget)
}

func matches(value any, target reflect.Type) bool {
	for target.Kind() == reflect.Pointer {
		if value == nil {
			return true
		}
		target = target.Elem()
	}
	if value == nil {
		switch target.Kind() {
		case reflect.Interface, reflect.Map, reflect.Slice:
			return true
		default:
			return false
		}
	}

	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		fields := make(map[string]reflect.Type, target.NumField())
		for index := 0; index < target.NumField(); index++ {
			field := target.Field(index)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field.Type
		}
		if len(object) != len(fields) {
			return false
		}
		for name, child := range object {
			fieldType, exists := fields[name]
			if !exists || !matches(child, fieldType) {
				return false
			}
		}
		return true
	case reflect.Array, reflect.Slice:
		array, ok := value.([]any)
		if !ok || (target.Kind() == reflect.Array && len(array) != target.Len()) {
			return false
		}
		for _, child := range array {
			if !matches(child, target.Elem()) {
				return false
			}
		}
		return true
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok || target.Key().Kind() != reflect.String {
			return false
		}
		for _, child := range object {
			if !matches(child, target.Elem()) {
				return false
			}
		}
		return true
	default:
		return true
	}
}
