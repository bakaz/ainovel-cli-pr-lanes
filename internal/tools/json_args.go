package tools

import (
	"encoding/json"
	"strconv"
	"strings"
)

func normalizeIntegerStringFields(args json.RawMessage, fields ...string) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return args
	}
	changed := false
	for _, field := range fields {
		raw, ok := obj[field]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		n, err := strconv.Atoi(text)
		if err != nil {
			continue
		}
		obj[field] = json.RawMessage(strconv.Itoa(n))
		changed = true
	}
	if !changed {
		return args
	}
	normalized, err := json.Marshal(obj)
	if err != nil {
		return args
	}
	return normalized
}
