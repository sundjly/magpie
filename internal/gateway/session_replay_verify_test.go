package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestSessionReplayKimiValid replays a real Claude Code session file
// (SESSION_JSONL) through the Anthropic parse and the Chat build, the way
// a request relayed to Kimi is, and checks the result against Kimi's tool
// exchange validation. Run: SESSION_JSONL=... go test -run TestSessionReplay
func TestSessionReplayKimiValid(t *testing.T) {
	path := os.Getenv("SESSION_JSONL")
	if path == "" {
		t.Skip("SESSION_JSONL unset")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type block struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		IsError   bool            `json:"is_error"`
	}
	type entry struct {
		Type      string `json:"type"`
		Sidechain bool   `json:"isSidechain"`
		Message   struct {
			ID      string          `json:"id"`
			Model   string          `json:"model"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	// Rebuild the Anthropic request the client sends: assistant blocks of
	// one API message (same message id) merge into one message; tool
	// results of consecutive user messages gather into one.
	var messages []map[string]any
	var lastRole string
	var lastAssistantID string
	push := func(role, assistantID string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if role == lastRole && (role != "assistant" || assistantID == lastAssistantID) {
			c, _ := messages[len(messages)-1]["content"].([]map[string]any)
			messages[len(messages)-1]["content"] = append(c, blocks...)
			return
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
		lastRole, lastAssistantID = role, assistantID
	}
	for _, line := range strings.Split(string(data), "\n") {
		var e entry
		if json.Unmarshal([]byte(line), &e) != nil || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		if e.Sidechain || e.Message.Model == "<synthetic>" {
			continue
		}
		var raw []block
		if json.Unmarshal(e.Message.Content, &raw) != nil {
			continue // string content: skip for this replay
		}
		var blocks []map[string]any
		for _, b := range raw {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
				}
			case "tool_use":
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": b.Input})
			case "tool_result":
				blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": b.ToolUseID,
					"content": b.Content, "is_error": b.IsError})
			case "thinking":
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": b.Signature})
			}
		}
		push(e.Message.Role, e.Message.ID, blocks)
	}
	body, _ := json.Marshal(map[string]any{"model": "claude-test", "messages": messages, "max_tokens": 1024})
	r, err := parseAnthropic(body)
	if err != nil {
		t.Fatal(err)
	}
	out := buildChat(r, "kimi-k3", "api.moonshot.cn", false)
	var q struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &q); err != nil {
		t.Fatal(err)
	}
	// Report the exchange's shape: how many calls, answers, repairs.
	calls, answers, synthetic := 0, 0, 0
	for _, m := range q.Messages {
		if cs, ok := m["tool_calls"].([]any); ok {
			calls += len(cs)
			for _, c := range cs {
				if c.(map[string]any)["function"].(map[string]any)["name"] == "unknown_tool" {
					synthetic++
				}
			}
		}
		if m["role"] == "tool" {
			answers++
		}
	}
	t.Logf("anthropic messages: %d -> chat messages: %d; calls: %d, tool answers: %d, synthetic calls: %d",
		len(messages), len(q.Messages), calls, answers, synthetic)
	if calls != answers {
		t.Errorf("calls (%d) != tool answers (%d)", calls, answers)
	}
	if !kimiValid(q.Messages) {
		t.Error("replayed session fails Kimi's tool exchange validation")
	}
}
