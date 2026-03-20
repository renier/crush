package app

import (
	"encoding/json"
	"io"
	"strings"
	"time"
)

// OutputFormat specifies how non-interactive output is rendered.
type OutputFormat string

const (
	OutputFormatText       OutputFormat = "text"
	OutputFormatJSON       OutputFormat = "json"
	OutputFormatStreamJSON OutputFormat = "stream-json"
)

// ValidOutputFormats returns the set of supported output format values.
func ValidOutputFormats() []OutputFormat {
	return []OutputFormat{
		OutputFormatText,
		OutputFormatJSON,
		OutputFormatStreamJSON,
	}
}

// IsValid reports whether the format is a recognised output format.
func (f OutputFormat) IsValid() bool {
	for _, v := range ValidOutputFormats() {
		if f == v {
			return true
		}
	}
	return false
}

// RunResult is the structured output emitted by crush run --output-format json.
type RunResult struct {
	SessionID string          `json:"session_id"`
	Model     RunResultModel  `json:"model"`
	Execution RunResultExec   `json:"execution"`
	Result    RunResultOutput `json:"result"`
	Usage     RunResultUsage  `json:"usage"`
}

// RunResultModel describes the model used for the run.
type RunResultModel struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	ContextWindow int64  `json:"context_window,omitempty"`
}

// RunResultExec describes execution metadata.
type RunResultExec struct {
	DurationMS int64  `json:"duration_ms"`
	IsError    bool   `json:"is_error"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// RunResultOutput holds the response content.
type RunResultOutput struct {
	Content string `json:"content"`
}

// RunResultUsage holds token and cost information.
type RunResultUsage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostEstimate float64 `json:"cost_estimate"`
}

// StreamEvent is a single line of newline-delimited JSON emitted by
// crush run --output-format stream-json.
//
// Event types:
//   - "init"           — emitted once at the start with model metadata.
//   - "content"        — assistant text content delta.
//   - "thinking"       — model reasoning/chain-of-thought delta.
//   - "tool_call"      — a tool invocation by the assistant.
//   - "tool_result"    — the result returned by a tool.
//   - "message_start"  — marks the beginning of a new assistant turn.
//   - "message_finish" — marks the end of an assistant turn with the
//     finish reason.
//   - "result"         — emitted once at the end with usage/duration.
type StreamEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`

	// Fields present only on "init" events.
	Model *RunResultModel `json:"model,omitempty"`

	// Fields present only on "content" events.
	Content string `json:"content,omitempty"`

	// Fields present only on "thinking" events.
	Thinking string `json:"thinking,omitempty"`

	// Fields present only on "tool_call" events.
	ToolCall *StreamToolCall `json:"tool_call,omitempty"`

	// Fields present only on "tool_result" events.
	ToolResult *StreamToolResult `json:"tool_result,omitempty"`

	// Fields present only on "message_start" events.
	MessageID string `json:"message_id,omitempty"`
	Role      string `json:"role,omitempty"`

	// Fields present only on "message_finish" events.
	FinishReason string `json:"finish_reason,omitempty"`

	// Fields present only on "result" events.
	DurationMS *int64          `json:"duration_ms,omitempty"`
	IsError    *bool           `json:"is_error,omitempty"`
	Error      string          `json:"error,omitempty"`
	Usage      *RunResultUsage `json:"usage,omitempty"`
}

// StreamToolCall describes a tool invocation in a stream event.
type StreamToolCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Input    string `json:"input,omitempty"`
	Finished bool   `json:"finished"`
}

// StreamToolResult describes the result of a tool invocation.
type StreamToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content,omitempty"`
	IsError    bool   `json:"is_error"`
}

// streamMessageState tracks what has already been emitted for a single
// message so that incremental updates produce only deltas.
type streamMessageState struct {
	started         bool
	thinkingBytes   int
	toolCallsSeen   map[string]bool
	toolResultsSeen map[string]bool
	finished        bool
}

// streamContentFlushInterval is how long the buffer waits before flushing
// content that has not yet reached a sentence boundary. 300ms balances
// coherence (~10-25 tokens at typical LLM speeds) with responsiveness.
const streamContentFlushInterval = 300 * time.Millisecond

// streamContentBuffer accumulates content deltas and flushes them as
// complete sentences (or after a short timeout) so that stream-json
// consumers receive coherent text rather than per-token fragments.
type streamContentBuffer struct {
	w         io.Writer
	sessionID string
	buf       strings.Builder
	lastFlush time.Time
	total     strings.Builder
	timer     *time.Timer
}

func newStreamContentBuffer(w io.Writer, sessionID string) *streamContentBuffer {
	return &streamContentBuffer{
		w:         w,
		sessionID: sessionID,
		lastFlush: time.Now(),
	}
}

// Write adds a text delta to the buffer and flushes if the accumulated
// content ends at a natural boundary or the time threshold has elapsed.
func (b *streamContentBuffer) Write(delta string) error {
	if delta == "" {
		return nil
	}
	b.buf.WriteString(delta)

	if b.shouldFlush() {
		return b.Flush()
	}
	return nil
}

// shouldFlush reports whether the buffer should be flushed now.
func (b *streamContentBuffer) shouldFlush() bool {
	s := b.buf.String()
	if s == "" {
		return false
	}

	if time.Since(b.lastFlush) >= streamContentFlushInterval {
		return true
	}

	// Flush on newlines — paragraphs and line breaks are natural
	// reading boundaries.
	if strings.HasSuffix(s, "\n") {
		return true
	}

	// Flush on sentence-ending punctuation followed by a space or at
	// the end of the buffer, covering ". ", "! ", "? " patterns that
	// signal a complete sentence.
	n := len(s)
	if n >= 2 {
		prev := s[n-2]
		cur := s[n-1]
		if (prev == '.' || prev == '!' || prev == '?') && cur == ' ' {
			return true
		}
	}

	return false
}

// Flush writes any buffered content as a stream event immediately.
func (b *streamContentBuffer) Flush() error {
	s := b.buf.String()
	if s == "" {
		return nil
	}
	b.buf.Reset()
	b.lastFlush = time.Now()
	b.stopTimer()
	b.total.WriteString(s)
	return writeStreamEvent(b.w, StreamEvent{
		Type:      "content",
		SessionID: b.sessionID,
		Content:   s,
	})
}

// Total returns all content written through the buffer.
func (b *streamContentBuffer) Total() string {
	return b.total.String() + b.buf.String()
}

// Timer returns a channel that fires when buffered content should be
// flushed due to the time threshold. Returns nil when the buffer is
// empty. The timer is reused across calls to avoid goroutine/channel
// churn in the select loop.
func (b *streamContentBuffer) Timer() <-chan time.Time {
	if b.buf.Len() == 0 {
		b.stopTimer()
		return nil
	}
	remaining := streamContentFlushInterval - time.Since(b.lastFlush)
	if remaining <= 0 {
		b.stopTimer()
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	if b.timer == nil {
		b.timer = time.NewTimer(remaining)
	} else {
		b.timer.Reset(remaining)
	}
	return b.timer.C
}

// stopTimer stops and drains the reusable timer if it is active.
func (b *streamContentBuffer) stopTimer() {
	if b.timer != nil {
		if !b.timer.Stop() {
			select {
			case <-b.timer.C:
			default:
			}
		}
	}
}

// writeJSON serialises v as JSON to w. When pretty is true the output is
// indented for human readability.
func writeJSON(w io.Writer, v any, pretty bool) error {
	enc := json.NewEncoder(w)
	if pretty {
		enc.SetIndent("", "  ")
	}
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// writeStreamEvent writes a single NDJSON line.
func writeStreamEvent(w io.Writer, ev StreamEvent) error {
	return writeJSON(w, ev, false)
}

// NonInteractiveOptions bundles options for RunNonInteractive.
type NonInteractiveOptions struct {
	Prompt            string
	LargeModel        string
	SmallModel        string
	HideSpinner       bool
	ContinueSessionID string
	UseLast           bool
	Format            OutputFormat
	Pretty            bool
}

// durationMS returns milliseconds elapsed since start.
func durationMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
