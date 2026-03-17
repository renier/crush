package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
