package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// OutputFormat validation
// ---------------------------------------------------------------------------

func TestOutputFormat_IsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		format OutputFormat
		valid  bool
	}{
		{OutputFormatText, true},
		{OutputFormatJSON, true},
		{OutputFormatStreamJSON, true},
		{"yaml", false},
		{"xml", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.valid, tt.format.IsValid())
		})
	}
}

func TestValidOutputFormats(t *testing.T) {
	t.Parallel()
	formats := ValidOutputFormats()
	require.Len(t, formats, 3)
	require.Contains(t, formats, OutputFormatText)
	require.Contains(t, formats, OutputFormatJSON)
	require.Contains(t, formats, OutputFormatStreamJSON)
}

// ---------------------------------------------------------------------------
// RunResult JSON serialisation
// ---------------------------------------------------------------------------

func TestRunResult_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	result := RunResult{
		SessionID: "sess-123",
		Model: RunResultModel{
			Name:          "Claude Sonnet 4",
			Provider:      "anthropic",
			ContextWindow: 200000,
		},
		Execution: RunResultExec{
			DurationMS: 1500,
			IsError:    false,
			Status:     "success",
		},
		Result: RunResultOutput{
			Content: "Hello from the assistant",
		},
		Usage: RunResultUsage{
			InputTokens:  50,
			OutputTokens: 100,
			CostEstimate: 0.002,
		},
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	var decoded RunResult
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, result, decoded)
}

func TestRunResult_JSONFieldNames(t *testing.T) {
	t.Parallel()

	result := RunResult{
		SessionID: "s1",
		Model:     RunResultModel{Name: "m", Provider: "p", ContextWindow: 128000},
		Execution: RunResultExec{DurationMS: 10, IsError: true, Status: "error", Error: "something broke"},
		Result:    RunResultOutput{Content: "c"},
		Usage:     RunResultUsage{InputTokens: 1, OutputTokens: 2, CostEstimate: 0.5},
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	raw := make(map[string]json.RawMessage)
	require.NoError(t, json.Unmarshal(data, &raw))

	for _, key := range []string{"session_id", "model", "execution", "result", "usage"} {
		_, ok := raw[key]
		require.True(t, ok, "expected top-level JSON key %q", key)
	}
}

func TestRunResult_ErrorFieldOmittedOnSuccess(t *testing.T) {
	t.Parallel()

	result := RunResult{
		SessionID: "s1",
		Execution: RunResultExec{DurationMS: 10, IsError: false, Status: "success"},
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(data), `"error"`)
}

func TestRunResult_ErrorFieldPresentOnFailure(t *testing.T) {
	t.Parallel()

	result := RunResult{
		SessionID: "s1",
		Execution: RunResultExec{DurationMS: 10, IsError: true, Status: "error", Error: "timeout"},
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)
	require.Contains(t, string(data), `"error":"timeout"`)
}

func TestRunResult_ZeroValues(t *testing.T) {
	t.Parallel()

	result := RunResult{}
	data, err := json.Marshal(result)
	require.NoError(t, err)

	var decoded RunResult
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, result, decoded)
}

// ---------------------------------------------------------------------------
// writeJSON
// ---------------------------------------------------------------------------

func TestWriteJSON_Compact(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, writeJSON(&buf, map[string]string{"a": "b"}, false))

	out := strings.TrimSpace(buf.String())
	require.Equal(t, `{"a":"b"}`, out)
}

func TestWriteJSON_Pretty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, writeJSON(&buf, map[string]string{"a": "b"}, true))

	out := buf.String()
	require.Contains(t, out, "\n")
	require.Contains(t, out, "  ")
}

func TestWriteJSON_NoHTMLEscape(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, writeJSON(&buf, map[string]string{"url": "https://example.com?a=1&b=2"}, false))

	require.NotContains(t, buf.String(), `\u0026`)
	require.Contains(t, buf.String(), "&")
}

// ---------------------------------------------------------------------------
// StreamEvent JSON serialisation
// ---------------------------------------------------------------------------

func TestStreamEvent_InitEvent(t *testing.T) {
	t.Parallel()

	model := &RunResultModel{Name: "GPT-4", Provider: "openai", ContextWindow: 128000}
	ev := StreamEvent{
		Type:      "init",
		SessionID: "s-1",
		Model:     model,
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	raw := make(map[string]json.RawMessage)
	require.NoError(t, json.Unmarshal(data, &raw))

	require.Contains(t, raw, "type")
	require.Contains(t, raw, "session_id")
	require.Contains(t, raw, "model")
	// content should be omitted (empty).
	_, hasContent := raw["content"]
	require.False(t, hasContent, "init event should omit empty content")
}

func TestStreamEvent_ContentEvent(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:      "content",
		SessionID: "s-1",
		Content:   "hello world",
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	raw := make(map[string]json.RawMessage)
	require.NoError(t, json.Unmarshal(data, &raw))

	require.Contains(t, raw, "content")
	// model should be omitted.
	_, hasModel := raw["model"]
	require.False(t, hasModel, "content event should omit nil model")
}

func TestStreamEvent_ResultEvent(t *testing.T) {
	t.Parallel()

	dur := int64(5000)
	isErr := false
	usage := &RunResultUsage{InputTokens: 10, OutputTokens: 20, CostEstimate: 0.001}
	ev := StreamEvent{
		Type:       "result",
		SessionID:  "s-1",
		DurationMS: &dur,
		IsError:    &isErr,
		Usage:      usage,
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	raw := make(map[string]json.RawMessage)
	require.NoError(t, json.Unmarshal(data, &raw))

	require.Contains(t, raw, "duration_ms")
	require.Contains(t, raw, "is_error")
	require.Contains(t, raw, "usage")
}

func TestStreamEvent_ResultEventIncludesError(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:      "result",
		SessionID: "s-1",
		Error:     "timeout",
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)
	require.Contains(t, string(data), `"error":"timeout"`)
}

func TestNewStreamContentEvent(t *testing.T) {
	t.Parallel()

	t.Run("empty chunk is ignored", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, newStreamContentEvent("s-1", ""))
	})

	t.Run("non-empty chunk is emitted", func(t *testing.T) {
		t.Parallel()
		ev := newStreamContentEvent("s-1", "hello")
		require.NotNil(t, ev)
		require.Equal(t, "content", ev.Type)
		require.Equal(t, "s-1", ev.SessionID)
		require.Equal(t, "hello", ev.Content)
	})
}

// ---------------------------------------------------------------------------
// writeStreamEvent
// ---------------------------------------------------------------------------

func TestWriteStreamEvent_NDJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ev := StreamEvent{Type: "content", SessionID: "s-1", Content: "hi"}
	require.NoError(t, writeStreamEvent(&buf, ev))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, "should produce exactly one line")

	var decoded StreamEvent
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &decoded))
	require.Equal(t, "content", decoded.Type)
	require.Equal(t, "hi", decoded.Content)
}

func TestWriteStreamEvent_MultipleEvents(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	events := []StreamEvent{
		{Type: "init", SessionID: "s-1", Model: &RunResultModel{Name: "m", Provider: "p"}},
		{Type: "content", SessionID: "s-1", Content: "chunk1"},
		{Type: "content", SessionID: "s-1", Content: "chunk2"},
	}
	for _, ev := range events {
		require.NoError(t, writeStreamEvent(&buf, ev))
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 3)

	for i, line := range lines {
		var decoded StreamEvent
		require.NoError(t, json.Unmarshal([]byte(line), &decoded), "line %d is not valid JSON", i)
		require.Equal(t, events[i].Type, decoded.Type)
	}
}

// ---------------------------------------------------------------------------
// durationMS
// ---------------------------------------------------------------------------

func TestDurationMS(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-100 * time.Millisecond)
	ms := durationMS(start)
	require.GreaterOrEqual(t, ms, int64(90), "expected at least ~90ms")
	require.Less(t, ms, int64(1000), "expected less than 1s")
}

// ---------------------------------------------------------------------------
// NonInteractiveOptions defaults
// ---------------------------------------------------------------------------

func TestNonInteractiveOptions_Defaults(t *testing.T) {
	t.Parallel()

	opts := NonInteractiveOptions{}
	require.Equal(t, OutputFormat(""), opts.Format)
	require.False(t, opts.Pretty)
	require.False(t, opts.HideSpinner)
	require.False(t, opts.UseLast)
}

// ---------------------------------------------------------------------------
// StreamEvent — new event types serialisation
// ---------------------------------------------------------------------------

func TestStreamEvent_ThinkingEvent(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:      "thinking",
		SessionID: "s-1",
		Thinking:  "Let me consider the options...",
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	raw := make(map[string]json.RawMessage)
	require.NoError(t, json.Unmarshal(data, &raw))

	require.Contains(t, raw, "thinking")
	_, hasContent := raw["content"]
	require.False(t, hasContent, "thinking event should omit empty content")
	_, hasModel := raw["model"]
	require.False(t, hasModel, "thinking event should omit nil model")
}

func TestStreamEvent_ToolCallEvent(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:      "tool_call",
		SessionID: "s-1",
		ToolCall: &StreamToolCall{
			ID:       "tc-1",
			Name:     "grep",
			Input:    `{"pattern":"foo","path":"src/"}`,
			Finished: false,
		},
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	var decoded StreamEvent
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, "tool_call", decoded.Type)
	require.NotNil(t, decoded.ToolCall)
	require.Equal(t, "tc-1", decoded.ToolCall.ID)
	require.Equal(t, "grep", decoded.ToolCall.Name)
	require.False(t, decoded.ToolCall.Finished)
}

func TestStreamEvent_ToolResultEvent(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:      "tool_result",
		SessionID: "s-1",
		ToolResult: &StreamToolResult{
			ToolCallID: "tc-1",
			Name:       "grep",
			Content:    "src/foo.go:42: func Foo() {",
			IsError:    false,
		},
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	var decoded StreamEvent
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, "tool_result", decoded.Type)
	require.NotNil(t, decoded.ToolResult)
	require.Equal(t, "tc-1", decoded.ToolResult.ToolCallID)
	require.False(t, decoded.ToolResult.IsError)
}

func TestStreamEvent_ToolResultErrorEvent(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:      "tool_result",
		SessionID: "s-1",
		ToolResult: &StreamToolResult{
			ToolCallID: "tc-2",
			Name:       "bash",
			Content:    "exit status 1",
			IsError:    true,
		},
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	var decoded StreamEvent
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.True(t, decoded.ToolResult.IsError)
}

func TestStreamEvent_MessageStartEvent(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:      "message_start",
		SessionID: "s-1",
		MessageID: "msg-1",
		Role:      "assistant",
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	raw := make(map[string]json.RawMessage)
	require.NoError(t, json.Unmarshal(data, &raw))

	require.Contains(t, raw, "message_id")
	require.Contains(t, raw, "role")
}

func TestStreamEvent_MessageFinishEvent(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:         "message_finish",
		SessionID:    "s-1",
		MessageID:    "msg-1",
		FinishReason: "end_turn",
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	var decoded StreamEvent
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, "message_finish", decoded.Type)
	require.Equal(t, "end_turn", decoded.FinishReason)
}

func TestStreamEvent_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	ev := StreamEvent{
		Type:      "content",
		SessionID: "s-1",
		Content:   "hello",
	}

	data, err := json.Marshal(ev)
	require.NoError(t, err)

	raw := make(map[string]json.RawMessage)
	require.NoError(t, json.Unmarshal(data, &raw))

	for _, key := range []string{"thinking", "tool_call", "tool_result", "message_id", "role", "finish_reason", "duration_ms", "is_error", "usage", "model"} {
		_, present := raw[key]
		require.False(t, present, "content event should omit empty %q", key)
	}
}

// ---------------------------------------------------------------------------
// emitStreamEvents
// ---------------------------------------------------------------------------

func TestEmitStreamEvents_MessageStart(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	require.True(t, len(events) >= 1)
	require.Equal(t, "message_start", events[0].Type)
	require.Equal(t, "msg-1", events[0].MessageID)
	require.Equal(t, "assistant", events[0].Role)
	require.True(t, state.started)
}

func TestEmitStreamEvents_MessageStartOnlyOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))
	buf.Reset()

	msg.Parts = []message.ContentPart{
		message.TextContent{Text: "hello world"},
	}
	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	for _, ev := range events {
		require.NotEqual(t, "message_start", ev.Type, "message_start should not be emitted twice")
	}
}

func TestEmitStreamEvents_ThinkingDelta(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "Step 1: "},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	thinkingEvents := filterEvents(events, "thinking")
	require.Len(t, thinkingEvents, 1)
	require.Equal(t, "Step 1: ", thinkingEvents[0].Thinking)

	buf.Reset()
	msg.Parts = []message.ContentPart{
		message.ReasoningContent{Thinking: "Step 1: Analyze the code"},
	}
	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events = parseNDJSON(t, buf.String())
	thinkingEvents = filterEvents(events, "thinking")
	require.Len(t, thinkingEvents, 1)
	require.Equal(t, "Analyze the code", thinkingEvents[0].Thinking)
}

func TestEmitStreamEvents_ToolCall(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:    "tc-1",
				Name:  "view",
				Input: `{"file_path":"src/main.go"}`,
			},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	tcEvents := filterEvents(events, "tool_call")
	require.Len(t, tcEvents, 1)
	require.NotNil(t, tcEvents[0].ToolCall)
	require.Equal(t, "tc-1", tcEvents[0].ToolCall.ID)
	require.Equal(t, "view", tcEvents[0].ToolCall.Name)
	require.False(t, tcEvents[0].ToolCall.Finished)
}

func TestEmitStreamEvents_ToolCallFinished(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       "tc-1",
				Name:     "view",
				Input:    `{"file_path":"src/main.go"}`,
				Finished: false,
			},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))
	buf.Reset()

	msg.Parts = []message.ContentPart{
		message.ToolCall{
			ID:       "tc-1",
			Name:     "view",
			Input:    `{"file_path":"src/main.go"}`,
			Finished: true,
		},
	}
	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	tcEvents := filterEvents(events, "tool_call")
	require.Len(t, tcEvents, 1)
	require.True(t, tcEvents[0].ToolCall.Finished)
}

func TestEmitStreamEvents_ToolResult(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-2",
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "tc-1",
				Name:       "view",
				Content:    "package main\n\nfunc main() {}",
			},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	trEvents := filterEvents(events, "tool_result")
	require.Len(t, trEvents, 1)
	require.NotNil(t, trEvents[0].ToolResult)
	require.Equal(t, "tc-1", trEvents[0].ToolResult.ToolCallID)
	require.Equal(t, "view", trEvents[0].ToolResult.Name)
	require.False(t, trEvents[0].ToolResult.IsError)
}

func TestEmitStreamEvents_ToolResultNotDuplicated(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-2",
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "tc-1",
				Name:       "view",
				Content:    "file contents",
			},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))
	buf.Reset()
	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	trEvents := filterEvents(events, "tool_result")
	require.Empty(t, trEvents, "tool_result should not be emitted twice for the same tool call")
}

func TestEmitStreamEvents_ToolResultTruncated(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	longContent := strings.Repeat("x", 5000)
	msg := &message.Message{
		ID:   "msg-2",
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "tc-1",
				Name:       "bash",
				Content:    longContent,
			},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	trEvents := filterEvents(events, "tool_result")
	require.Len(t, trEvents, 1)
	require.True(t, len(trEvents[0].ToolResult.Content) < len(longContent))
	require.Contains(t, trEvents[0].ToolResult.Content, "… (truncated)")
}

func TestEmitStreamEvents_MessageFinish(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Done."},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	finishEvents := filterEvents(events, "message_finish")
	require.Len(t, finishEvents, 1)
	require.Equal(t, "end_turn", finishEvents[0].FinishReason)
	require.Equal(t, "msg-1", finishEvents[0].MessageID)
}

func TestEmitStreamEvents_MessageFinishOnlyOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	msg := &message.Message{
		ID:   "msg-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
		},
	}
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))
	buf.Reset()
	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())
	finishEvents := filterEvents(events, "message_finish")
	require.Empty(t, finishEvents, "message_finish should not be emitted twice")
}

func TestEmitStreamEvents_FullLifecycle(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	state := &streamMessageState{
		toolCallsSeen:   make(map[string]bool),
		toolResultsSeen: make(map[string]bool),
	}

	// Turn 1: assistant thinks, then calls a tool.
	msg := &message.Message{
		ID:   "msg-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "I need to search."},
			message.ToolCall{
				ID:       "tc-1",
				Name:     "grep",
				Input:    `{"pattern":"TODO"}`,
				Finished: true,
			},
			message.Finish{Reason: message.FinishReasonToolUse, Time: time.Now().Unix()},
		},
	}
	require.NoError(t, emitStreamEvents(&buf, "s-1", msg, state))

	events := parseNDJSON(t, buf.String())

	typeSequence := make([]string, len(events))
	for i, ev := range events {
		typeSequence[i] = ev.Type
	}
	require.Equal(t, []string{
		"message_start",
		"thinking",
		"tool_call",
		"message_finish",
	}, typeSequence)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseNDJSON(t *testing.T, s string) []StreamEvent {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	events := make([]StreamEvent, 0, len(lines))
	for i, line := range lines {
		var ev StreamEvent
		require.NoError(t, json.Unmarshal([]byte(line), &ev), "line %d is not valid JSON: %s", i, line)
		events = append(events, ev)
	}
	return events
}

func filterEvents(events []StreamEvent, eventType string) []StreamEvent {
	var out []StreamEvent
	for _, ev := range events {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}
