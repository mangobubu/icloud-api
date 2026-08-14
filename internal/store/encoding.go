package store

import (
	"encoding/json"
	"strings"
)

func sanitizePostgresText(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	return strings.ReplaceAll(value, "\x00", "\uFFFD")
}

func marshalJSONList[T any](value []T) (string, error) {
	if value == nil {
		value = []T{}
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}
