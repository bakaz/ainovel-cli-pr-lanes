package litellm

import (
	"bytes"
	"encoding/json"
)

func IntPtr(v int) *int {
	return &v
}

func Float64Ptr(v float64) *float64 {
	return &v
}

func BoolPtr(v bool) *bool {
	return &v
}

func Bool(v bool) *bool {
	return &v
}

func StringPtr(v string) *string {
	return &v
}

func Text(text string) TextBlock {
	return TextBlock{Text: text}
}

func ImageURL(url string) ImageBlock {
	return ImageBlock{URL: url}
}

func User(blocks ...Block) Message {
	return Message{Role: RoleUser, Blocks: blocks}
}

func UserText(text string) Message {
	return User(Text(text))
}

func System(text string) Message {
	return Message{Role: RoleSystem, Blocks: []Block{Text(text)}}
}

func Assistant(blocks ...Block) Message {
	return Message{Role: RoleAssistant, Blocks: blocks}
}

func AssistantText(text string) Message {
	return Assistant(Text(text))
}

func ToolResult(toolUseID string, blocks ...Block) Message {
	return Message{
		Role: RoleTool,
		Blocks: []Block{
			ToolResultBlock{ToolUseID: toolUseID, Content: blocks},
		},
	}
}

func ToolResultText(toolUseID, text string) Message {
	return ToolResult(toolUseID, Text(text))
}

func NewResponseFormatText() *ResponseFormat {
	return &ResponseFormat{Type: ResponseFormatText}
}

func NewResponseFormatJSONObject() *ResponseFormat {
	return &ResponseFormat{Type: ResponseFormatJSONObject}
}

func NewResponseFormatJSONSchema(name, description string, schema any, strict StrictMode) (*ResponseFormat, error) {
	s, err := SchemaFrom(schema)
	if err != nil {
		return nil, err
	}
	return &ResponseFormat{
		Type: ResponseFormatJSONSchema,
		JSONSchema: &JSONSchema{
			Name:        name,
			Description: description,
			Schema:      s,
			Strict:      strict,
		},
	}, nil
}

func JSONRaw(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func MustJSONRaw(v any) json.RawMessage {
	b, err := JSONRaw(v)
	if err != nil {
		panic(err)
	}
	return b
}

// repairConcatenatedJSONObjects attempts to repair tool-call arguments that
// contain two or more concatenated top-level JSON objects (e.g. "{}" followed
// by "{\"chapter\":40}"), a shape observed from some OpenAI-compatible relays
// when converting upstream native function calls. All objects are
// shallow-merged in order of appearance; later keys win. ok is false when the
// input does not decode into at least two objects — callers should keep their
// existing malformed-argument handling in that case.
func repairConcatenatedJSONObjects(raw []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var merged map[string]any
	count := 0
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			break
		}
		count++
		if merged == nil {
			merged = make(map[string]any, len(obj))
		}
		for k, v := range obj {
			merged[k] = v
		}
	}
	if count < 2 || merged == nil {
		return nil, false
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, false
	}
	return out, true
}
