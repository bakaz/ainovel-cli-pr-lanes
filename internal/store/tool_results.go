package store

import (
	"encoding/json"
	"fmt"
)

const toolResultDir = "meta/sessions/tool-results"

type storedToolResult struct {
	Tool    string          `json:"tool"`
	Content json.RawMessage `json:"content"`
}

// SaveToolResult 把工具结果全文旁路落盘（不进模型 prompt）。
func (s *SessionStore) SaveToolResult(id, tool string, content json.RawMessage) error {
	if s == nil || s.io == nil || id == "" {
		return fmt.Errorf("save tool result: missing session store or id")
	}
	rec := storedToolResult{Tool: tool, Content: content}
	return s.io.WriteJSON(toolResultDir+"/"+id+".json", rec)
}

// LoadToolResult 读取旁路全文。
func (s *SessionStore) LoadToolResult(id string) (json.RawMessage, error) {
	var rec storedToolResult
	if err := s.io.ReadJSON(toolResultDir+"/"+id+".json", &rec); err != nil {
		return nil, err
	}
	return rec.Content, nil
}
