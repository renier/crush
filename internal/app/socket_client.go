package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// SocketClientOptions bundles options for RunViaSocket.
type SocketClientOptions struct {
	SocketPath string
	Prompt     string
	Format     OutputFormat
	Pretty     bool
}

// RunViaSocket connects to a running Crush instance via a Unix socket,
// sends the prompt, and writes the response to output. It acts as a thin
// client — no App, DB, or provider setup is needed.
func RunViaSocket(ctx context.Context, output io.Writer, opts SocketClientOptions) error {
	conn, err := net.DialTimeout("unix", opts.SocketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect to socket %s: %w", opts.SocketPath, err)
	}
	defer conn.Close()

	// Send request.
	req := SocketRequest{Prompt: opts.Prompt}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}

	// Read NDJSON events.
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var (
		contentBuilder strings.Builder
		sessionID      string
		runErr         error
	)

	// Cancel reads when the context is done. Closing the connection
	// unblocks scanner.Scan immediately and ensures the goroutine
	// exits once RunViaSocket returns.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.SetReadDeadline(time.Now())
	}()

	for scanner.Scan() {
		var ev StreamEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}

		if sessionID == "" {
			sessionID = ev.SessionID
		}

		switch opts.Format {
		case OutputFormatText:
			if ev.Type == "content" {
				fmt.Fprint(output, ev.Content)
			}
			if ev.Type == "error" {
				return fmt.Errorf("socket error: %s", ev.Error)
			}

		case OutputFormatStreamJSON:
			if err := writeJSON(output, ev, opts.Pretty); err != nil {
				return fmt.Errorf("failed to write event: %w", err)
			}

		case OutputFormatJSON:
			if ev.Type == "content" {
				contentBuilder.WriteString(ev.Content)
			}
		}

		if ev.Type == "result" {
			if ev.IsError != nil && *ev.IsError {
				runErr = fmt.Errorf("agent error: %s", ev.Error)
			}

			if opts.Format == OutputFormatJSON {
				result := RunResult{
					SessionID: sessionID,
					Result:    RunResultOutput{Content: contentBuilder.String()},
				}
				if ev.DurationMS != nil {
					result.Execution.DurationMS = *ev.DurationMS
				}
				if ev.IsError != nil {
					result.Execution.IsError = *ev.IsError
					if *ev.IsError {
						result.Execution.Status = "error"
						result.Execution.Error = ev.Error
					} else {
						result.Execution.Status = "success"
					}
				}
				if ev.Usage != nil {
					result.Usage = *ev.Usage
				}
				if err := writeJSON(output, result, opts.Pretty); err != nil {
					return fmt.Errorf("failed to write JSON result: %w", err)
				}
			}
			break
		}
	}

	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("failed to read from socket: %w", err)
	}

	if opts.Format == OutputFormatText {
		fmt.Fprintln(output)
	}

	return runErr
}
