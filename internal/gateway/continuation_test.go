package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// cutChat is a Chat upstream that severs the reply mid-stream on the calls
// cut names: the events given go out with a Content-Length promising more,
// then the connection closes. Other calls answer whole. Bodies journals
// what each call was asked.
type cutChat struct {
	mu     sync.Mutex
	bodies []map[string]any
	cuts   map[int][]string
	whole  []string
}

func (f *cutChat) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &body); err != nil {
		t.Errorf("upstream request isn't JSON: %s", b)
	}
	f.mu.Lock()
	call := len(f.bodies)
	f.bodies = append(f.bodies, body)
	events, cut := f.cuts[call]
	whole := f.whole
	f.mu.Unlock()
	if !cut {
		events = whole
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if !cut {
		for _, ev := range events {
			io.WriteString(w, "data: "+ev+"\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("no hijacker")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	var sb strings.Builder
	for _, ev := range events {
		sb.WriteString("data: " + ev + "\n\n")
	}
	fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n", sb.Len()+100)
	io.WriteString(buf, sb.String())
	buf.Flush()
}

func (f *cutChat) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// lastAssistant is the last message a call was asked with, when it's the
// assistant's: the reply so far, sent back to go on from.
func (f *cutChat) lastAssistant(t *testing.T, call int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs, _ := f.bodies[call]["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "assistant" {
		return nil
	}
	return last
}

// chatThink and chatText are one Chat chunk of reasoning and of text.
func chatThink(model, s string) string {
	return fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,"delta":{"reasoning_content":%s}}]}`, model, jsonStr(s))
}

func chatText(model, s string) string {
	return fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,"delta":{"content":%s}}]}`, model, jsonStr(s))
}

func chatStop(model string) string {
	return fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`, model)
}

// continuationServer points a provider whose only API is Chat at f, and
// answers a Messages request for its model: the reply is translated.
func continuationServer(t *testing.T, model string, f *cutChat) string {
	t.Helper()
	fresh(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(up.Close)
	p := provider.Provider{ID: "up", Name: "UP", Key: "k", Models: []string{model}, Chat: up.URL + "/v1"}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	code, body := post(t, "/v1/messages", `{"model":"up/`+model+`","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	return body
}

// textOf concatenates a Messages stream's text deltas, thinkOf its
// thinking deltas.
func textOf(body string) string {
	var sb strings.Builder
	for _, ev := range events(body) {
		if ev["type"] == "content_block_delta" {
			if d, ok := ev["delta"].(map[string]any); ok && d["type"] == "text_delta" {
				sb.WriteString(d["text"].(string))
			}
		}
	}
	return sb.String()
}

func thinkOf(body string) string {
	var sb strings.Builder
	for _, ev := range events(body) {
		if ev["type"] == "content_block_delta" {
			if d, ok := ev["delta"].(map[string]any); ok && d["type"] == "thinking_delta" {
				sb.WriteString(d["thinking"].(string))
			}
		}
	}
	return sb.String()
}

func messageStops(body string) (starts, stops, errs int) {
	for _, ev := range events(body) {
		switch ev["type"] {
		case "message_start":
			starts++
		case "message_stop":
			stops++
		case "error":
			errs++
		}
	}
	return
}

// A reply the upstream cut after some text goes on: the same conversation
// is asked again with what the client has sent back, and the client reads
// one whole reply, its thinking with it.
func TestStreamCutMidReplyContinues(t *testing.T) {
	f := &cutChat{
		cuts:  map[int][]string{0: {chatThink("kimi-k3", "think-1"), chatText("kimi-k3", "hel")}},
		whole: []string{chatThink("kimi-k3", "think-2"), chatText("kimi-k3", "lo"), chatStop("kimi-k3")},
	}
	body := continuationServer(t, "kimi-k3", f)
	if f.calls() != 2 {
		t.Fatalf("the cut reply wasn't asked of the same conversation again: %d calls", f.calls())
	}
	m := f.lastAssistant(t, 1)
	if m == nil || m["content"] != "hel" || m["reasoning_content"] != "think-1" {
		t.Fatalf("the second ask didn't carry what the client has to go on from: %v", m)
	}
	if got := textOf(body); got != "hello" {
		t.Fatalf("client's text = %q, want %q", got, "hello")
	}
	if got := thinkOf(body); got != "think-1" {
		// a continuation's fresh thinking isn't shown a second time
		t.Fatalf("client's thinking = %q, want %q", got, "think-1")
	}
	if starts, stops, errs := messageStops(body); starts != 1 || stops != 1 || errs != 0 {
		t.Fatalf("starts %d, stops %d, errors %d, want 1/1/0: %s", starts, stops, errs, body)
	}
}

// A model that says what it was prefilled with again before going on has
// the echo dropped: the client reads the text once.
func TestStreamCutContinuationEchoDropped(t *testing.T) {
	f := &cutChat{
		cuts:  map[int][]string{0: {chatText("m", "hel")}},
		whole: []string{chatText("m", "hel"), chatText("m", "lo"), chatStop("m")},
	}
	body := continuationServer(t, "m", f)
	if f.calls() != 2 {
		t.Fatalf("calls = %d, want 2", f.calls())
	}
	if got := textOf(body); got != "hello" {
		t.Fatalf("client's text = %q, want %q (echo not dropped)", got, "hello")
	}
	if _, _, errs := messageStops(body); errs != 0 {
		t.Fatalf("error events in the stream: %s", body)
	}
}

// A reply a tool call of has begun can't be prefilled and go on: it ends
// with the error in the stream, as it used to.
func TestStreamCutAfterToolCallEndsAsBefore(t *testing.T) {
	f := &cutChat{
		cuts: map[int][]string{0: {
			chatText("m", "hel"),
			`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
		}},
	}
	body := continuationServer(t, "m", f)
	if f.calls() != 1 {
		t.Fatalf("a reply with a tool call begun was asked again: %d calls", f.calls())
	}
	if _, _, errs := messageStops(body); errs != 1 {
		t.Fatalf("no error event in the stream: %s", body)
	}
	if !strings.Contains(body, "connection lost mid-reply") {
		t.Fatalf("the error doesn't say the connection was lost: %s", body)
	}
}

// A reply whose every ask is cut ends with the error after the tries are
// up, as it used to after one.
func TestStreamCutContinuesOnlySoLong(t *testing.T) {
	f := &cutChat{
		cuts: map[int][]string{0: {chatText("m", "hel")}, 1: {chatText("m", "hel")}, 2: {chatText("m", "hel")}},
	}
	body := continuationServer(t, "m", f)
	if f.calls() != 1+streamRetries {
		t.Fatalf("calls = %d, want %d", f.calls(), 1+streamRetries)
	}
	if _, _, errs := messageStops(body); errs != 1 {
		t.Fatalf("no error event in the stream: %s", body)
	}
	if got := textOf(body); got != "hel" {
		t.Fatalf("client's text = %q, want %q", got, "hel")
	}
}

// A reply cut while it was only thinking goes on from the thinking the
// client has, a model that reads it back asked with it; the client reads
// its thinking once, then the answer.
func TestStreamCutMidThinkingContinues(t *testing.T) {
	f := &cutChat{
		cuts:  map[int][]string{0: {chatThink("kimi-k3", "think-1")}},
		whole: []string{chatThink("kimi-k3", "rethink"), chatText("kimi-k3", "done"), chatStop("kimi-k3")},
	}
	body := continuationServer(t, "kimi-k3", f)
	if f.calls() != 2 {
		t.Fatalf("calls = %d, want 2", f.calls())
	}
	m := f.lastAssistant(t, 1)
	if m == nil || m["reasoning_content"] != "think-1" {
		t.Fatalf("the second ask didn't carry the thinking to go on from: %v", m)
	}
	if got := thinkOf(body); got != "think-1" {
		t.Fatalf("client's thinking = %q, want %q", got, "think-1")
	}
	if got := textOf(body); got != "done" {
		t.Fatalf("client's text = %q, want %q", got, "done")
	}
	if _, _, errs := messageStops(body); errs != 0 {
		t.Fatalf("error events in the stream: %s", body)
	}
}

// A model that doesn't read reasoning back (no reasoning_content for it)
// is asked again with no prefill to go on from, and answers anew: the
// client reads the thinking the cut reply had, then the fresh answer.
func TestStreamCutMidThinkingContinuesWithoutReasoning(t *testing.T) {
	f := &cutChat{
		cuts:  map[int][]string{0: {chatThink("m", "think-1")}},
		whole: []string{chatThink("m", "rethink"), chatText("m", "done"), chatStop("m")},
	}
	body := continuationServer(t, "m", f)
	if f.calls() != 2 {
		t.Fatalf("calls = %d, want 2", f.calls())
	}
	if m := f.lastAssistant(t, 1); m != nil {
		t.Fatalf("a model that doesn't read reasoning back was sent a prefill: %v", m)
	}
	if got := thinkOf(body); got != "think-1" {
		t.Fatalf("client's thinking = %q, want %q", got, "think-1")
	}
	if got := textOf(body); got != "done" {
		t.Fatalf("client's text = %q, want %q", got, "done")
	}
}

// A stream's own error event mid-reply, the connection kept open after
// it, ends the read at once and the reply goes on: the client isn't made
// to wait on the vendor hanging up.
func TestStreamErrorMidReplyContinuesAtOnce(t *testing.T) {
	fresh(t)
	var mu sync.Mutex
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if !first {
			io.WriteString(w, sse("data: "+chatText("m", "lo"), "data: "+chatStop("m"), "data: [DONE]"))
			return
		}
		io.WriteString(w, sse("data: "+chatText("m", "hel"), `data: {"error":{"message":"boom"}}`))
		w.(http.Flusher).Flush()
		select { // said, and not hung up
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
			t.Error("the error event's read wasn't ended at once")
		}
	}))
	t.Cleanup(up.Close)
	p := provider.Provider{ID: "up", Name: "UP", Key: "k", Models: []string{"m"}, Chat: up.URL + "/v1"}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	body := continuationServerBody(t)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
	if got := textOf(body); got != "hello" {
		t.Fatalf("client's text = %q, want %q", got, "hello")
	}
}

func continuationServerBody(t *testing.T) string {
	t.Helper()
	code, body := post(t, "/v1/messages", `{"model":"up/m","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	return body
}

// unecho drops a full echo of what a continuation was prefilled with,
// however it comes in pieces: the client already has the prefill, so a
// stream that starts with it is one, and what follows goes on; anything
// else goes whole.
func TestContinuationUnecho(t *testing.T) {
	for _, c := range []struct {
		echo   string
		chunks []string
		want   string
	}{
		{"hel", []string{"lo"}, "lo"},            // a plain continuation
		{"hel", []string{"hel", "lo"}, "lo"},     // the echo, then the continuation
		{"hel", []string{"h", "e", "llo"}, "lo"}, // the echo in pieces
		{"hel", []string{"he", "lp"}, "p"},       // starts with the prefill: an echo
		{"hel", []string{"x", "yz"}, "xyz"},      // nothing alike goes whole
		{"hel", []string{"hel"}, ""},             // the echo alone
		{"hel", []string{"he"}, ""},              // what could still be one is held
		{"", []string{"hel"}, "hel"},             // nothing prefilled: nothing dropped
	} {
		con := &continuation{echo: c.echo}
		var sb strings.Builder
		for _, ch := range c.chunks {
			sb.WriteString(con.unecho(ch))
		}
		if sb.String() != c.want {
			t.Errorf("unecho(%q, %v) = %q, want %q", c.echo, c.chunks, sb.String(), c.want)
		}
	}
}
