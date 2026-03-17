package app

import (
	"encoding/json"
	"io"
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
type StreamEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`

	// Fields present only on "init" events.
	Model *RunResultModel `json:"model,omitempty"`

	// Fields present only on "content" events.
	Content string `json:"content,omitempty"`

	// Fields present only on "result" events.
	DurationMS *int64          `json:"duration_ms,omitempty"`
	IsError    *bool           `json:"is_error,omitempty"`
	Usage      *RunResultUsage `json:"usage,omitempty"`
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
