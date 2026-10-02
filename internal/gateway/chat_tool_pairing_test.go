package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// chatTrace renders the message sequence of a Chat request built for an
// upstream: roles with tool call ids, e.g.
// "user assistant(A,B) tool(A) tool(B) user".
func chatTrace(t *testing.T, r *Request) (string, []map[string]any) {
	t.Helper()
	body := buildChat(r, "kimi-k3", "api.moonshot.cn", false)
	var q struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &q); err != nil {
		t.Fatalf("built request does not parse: %v", err)
	}
	var b strings.Builder
	for _, m := range q.Messages {
		role, _ := m["role"].(string)
		b.WriteString(" " + role)
		if calls, ok := m["tool_calls"].([]any); ok {
			var ids []string
			for _, c := range calls {
				id, _ := c.(map[string]any)["id"].(string)
				ids = append(ids, id)
			}
			b.WriteString("(" + strings.Join(ids, ",") + ")")
		}
		if id, ok := m["tool_call_id"].(string); ok {
			b.WriteString("(" + id + ")")
		}
	}
	return strings.TrimSpace(b.String()), q.Messages
}

// kimiValid reports whether a built message sequence passes what Kimi
// validates: every tool message answers a call of the assistant message
// it follows, with only tool messages between.
func kimiValid(msgs []map[string]any) bool {
	var pending map[string]bool
	for _, m := range msgs {
		switch m["role"] {
		case "assistant":
			pending = nil
			if calls, ok := m["tool_calls"].([]any); ok {
				pending = map[string]bool{}
				for _, c := range calls {
					id, _ := c.(map[string]any)["id"].(string)
					pending[id] = true
				}
			}
		case "tool":
			id, _ := m["tool_call_id"].(string)
			if !pending[id] {
				return false
			}
		default:
			pending = nil
		}
	}
	return true
}

func call(id string) Part {
	return Part{Kind: ToolCall, ID: id, Name: "run", Args: json.RawMessage(`{}`)}
}
func result(id, out string) Part {
	return Part{Kind: ToolResult, CallID: id, Text: out}
}

// Claude Code makes parallel calls in one turn and the results come back
// out of order, beside the turn's own text (an attachment's reminder):
// the tool messages must lead, in the calls' order, the text after.
func TestChatToolResultsLeadInCallOrder(t *testing.T) {
	r := &Request{Messages: []Message{
		{Role: "user", Parts: []Part{{Kind: Text, Text: "run them"}}},
		{Role: "assistant", Parts: []Part{call("A"), call("B"), call("C"), call("D")}},
		{Role: "user", Parts: []Part{
			{Kind: Text, Text: "note from an attachment"},
			result("C", "3"), result("A", "1"), result("D", "4"), result("B", "2"),
		}},
	}}
	trace, msgs := chatTrace(t, r)
	want := "user assistant(A,B,C,D) tool(A) tool(B) tool(C) tool(D) user"
	if trace != want {
		t.Fatalf("trace = %q, want %q", trace, want)
	}
	if !kimiValid(msgs) {
		t.Fatal("sequence fails Kimi's tool exchange validation")
	}
}

// Results split over consecutive user messages (Claude Code logs one per
// message) still gather behind their assistant message, in call order.
func TestChatToolResultsGatherAcrossUserMessages(t *testing.T) {
	r := &Request{Messages: []Message{
		{Role: "assistant", Parts: []Part{call("A"), call("B")}},
		{Role: "user", Parts: []Part{result("B", "2")}},
		{Role: "user", Parts: []Part{result("A", "1")}},
	}}
	trace, msgs := chatTrace(t, r)
	want := "assistant(A,B) tool(A) tool(B)"
	if trace != want {
		t.Fatalf("trace = %q, want %q", trace, want)
	}
	if !kimiValid(msgs) {
		t.Fatal("sequence fails Kimi's tool exchange validation")
	}
}

// A result whose call is gone (compaction cut it, or a client sends
// results alone) gets a synthetic call so the exchange stays valid and
// the result survives.
func TestChatOrphanToolResultGetsSyntheticCall(t *testing.T) {
	r := &Request{Messages: []Message{
		{Role: "user", Parts: []Part{result("X", "kept")}},
		{Role: "user", Parts: []Part{{Kind: Text, Text: "go on"}}},
	}}
	trace, msgs := chatTrace(t, r)
	want := "assistant(X) tool(X) user"
	if trace != want {
		t.Fatalf("trace = %q, want %q", trace, want)
	}
	if !kimiValid(msgs) {
		t.Fatal("sequence fails Kimi's tool exchange validation")
	}
	if msgs[0]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] != "unknown_tool" {
		t.Fatalf("synthetic call = %v", msgs[0])
	}
}

// An interrupted turn leaves a call unanswered: it gets a synthetic error
// result so the next turn can start.
func TestChatUnansweredCallGetsSyntheticResult(t *testing.T) {
	r := &Request{Messages: []Message{
		{Role: "assistant", Parts: []Part{call("A")}},
		{Role: "user", Parts: []Part{{Kind: Text, Text: "继续"}}},
	}}
	trace, msgs := chatTrace(t, r)
	want := "assistant(A) tool(A) user"
	if trace != want {
		t.Fatalf("trace = %q, want %q", trace, want)
	}
	if c, _ := msgs[1]["content"].(string); !strings.Contains(c, "unavailable") {
		t.Fatalf("synthetic result content = %q", c)
	}
	if !kimiValid(msgs) {
		t.Fatal("sequence fails Kimi's tool exchange validation")
	}
}

// A thinking-only assistant turn (what an aborted stream leaves behind)
// carries nothing and must not sit between calls and their answers.
func TestChatThinkingOnlyAssistantDropped(t *testing.T) {
	r := &Request{Messages: []Message{
		{Role: "assistant", Parts: []Part{{Kind: Thinking, Text: "hmm", Signature: "sig"}}},
		{Role: "user", Parts: []Part{{Kind: Text, Text: "继续"}}},
	}}
	trace, _ := chatTrace(t, r)
	if trace != "user" {
		t.Fatalf("trace = %q, want %q", trace, "user")
	}
}

// A clean exchange goes through untouched.
func TestChatCleanExchangeUntouched(t *testing.T) {
	r := &Request{Messages: []Message{
		{Role: "user", Parts: []Part{{Kind: Text, Text: "hi"}}},
		{Role: "assistant", Parts: []Part{{Kind: Text, Text: "hello"}}},
		{Role: "assistant", Parts: []Part{call("A"), call("B")}},
		{Role: "user", Parts: []Part{result("A", "1"), result("B", "2")}},
		{Role: "user", Parts: []Part{{Kind: Text, Text: "thanks"}}},
	}}
	trace, msgs := chatTrace(t, r)
	want := "user assistant assistant(A,B) tool(A) tool(B) user"
	if trace != want {
		t.Fatalf("trace = %q, want %q", trace, want)
	}
	if !kimiValid(msgs) {
		t.Fatal("sequence fails Kimi's tool exchange validation")
	}
}
