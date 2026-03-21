package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

// maxSocketRequestSize is the maximum size of a JSON request read from the
// socket. Requests larger than this are rejected.
const maxSocketRequestSize = 1 << 20 // 1 MiB

// agentResponse holds the result of a coordinator.Run call.
type agentResponse struct {
	result *fantasy.AgentResult
	err    error
}

// maxSocketConnections caps the number of concurrent socket connections.
const maxSocketConnections = 8

// socketReadTimeout is how long the server waits for the client to send
// the full request JSON before giving up.
const socketReadTimeout = 5 * time.Second

// SocketRequest is the JSON payload sent by a client over the Unix socket.
type SocketRequest struct {
	Prompt      string               `json:"prompt"`
	SessionID   string               `json:"session_id,omitempty"`
	Attachments []message.Attachment `json:"attachments,omitempty"`
}

// Listener accepts connections on a Unix domain socket and dispatches
// prompts to the agent coordinator, streaming NDJSON events back to
// each client.
type Listener struct {
	app       *App
	sockPath  string
	ln        net.Listener
	connSem   chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	done      chan struct{}
}

// NewListener creates a Listener that will bind to sockPath. Call Listen
// to start accepting connections.
func NewListener(app *App, sockPath string) *Listener {
	return &Listener{
		app:      app,
		sockPath: sockPath,
		connSem:  make(chan struct{}, maxSocketConnections),
		done:     make(chan struct{}),
	}
}

// Listen starts accepting connections on the Unix domain socket. It blocks
// until ctx is cancelled, Close is called, or an unrecoverable error occurs.
func (l *Listener) Listen(ctx context.Context) error {
	// Remove stale socket file if it exists.
	if err := os.Remove(l.sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	// Ensure the parent directory exists.
	if err := os.MkdirAll(filepath.Dir(l.sockPath), 0o700); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", l.sockPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", l.sockPath, err)
	}
	l.ln = ln

	// Restrict socket to owner only.
	if err := os.Chmod(l.sockPath, 0o600); err != nil {
		slog.Warn("Failed to set socket permissions", "error", err)
	}

	slog.Info("Socket listener started", "path", l.sockPath)
	_, _ = fmt.Fprintf(os.Stderr, "Listening on %s\n", l.sockPath)

	// Close the listener when the context is cancelled or Close is called,
	// whichever comes first.
	go func() {
		select {
		case <-ctx.Done():
		case <-l.done:
		}
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Exit the loop on any accept error once we know we are
			// shutting down (context cancelled or Close called).
			select {
			case <-l.done:
				break
			case <-ctx.Done():
				break
			default:
				slog.Error("Socket accept error", "error", err)
				continue
			}
			break
		}

		select {
		case l.connSem <- struct{}{}:
		default:
			slog.Warn("Socket connection limit reached, rejecting")
			writeSocketError(conn, "connection limit reached")
			conn.Close()
			continue
		}

		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			defer func() { <-l.connSem }()
			l.handleConn(ctx, conn)
		}()
	}

	l.wg.Wait()
	return nil
}

// Close shuts down the listener and removes the socket file.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	if l.ln != nil {
		// Unblock Accept by closing the listener.
		_ = l.ln.Close()
	}
	l.wg.Wait()
	var err error
	if rmErr := os.Remove(l.sockPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		err = rmErr
	}
	return err
}

// handleConn reads a SocketRequest, runs the agent, and streams NDJSON
// events back to the client.
func (l *Listener) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := readSocketRequest(conn)
	if err != nil {
		slog.Warn("Invalid socket request", "error", err)
		writeSocketError(conn, err.Error())
		return
	}

	// Resolve session: explicit ID > TUI's active session > new session.
	var sess session.Session
	if req.SessionID != "" {
		sess, err = l.app.Sessions.Get(connCtx, req.SessionID)
		if err != nil {
			writeSocketError(conn, fmt.Sprintf("session not found: %s", req.SessionID))
			return
		}
	} else if activeID := l.app.ActiveSessionID.Get(); activeID != "" {
		sess, err = l.app.Sessions.Get(connCtx, activeID)
		if err != nil {
			writeSocketError(conn, fmt.Sprintf("active session not found: %s", activeID))
			return
		}
	} else {
		sess, err = l.app.Sessions.Create(connCtx, "socket")
		if err != nil {
			writeSocketError(conn, fmt.Sprintf("failed to create session: %v", err))
			return
		}
		// Ask the TUI to navigate to this session so the user sees it.
		select {
		case l.app.events <- SwitchSessionMsg{SessionID: sess.ID}:
		default:
		}
	}

	l.app.Permissions.AutoApproveSession(sess.ID)

	// Write init event.
	modelInfo := l.app.modelInfo()
	if err := writeStreamEvent(conn, StreamEvent{
		Type:      "init",
		SessionID: sess.ID,
		Model:     &modelInfo,
	}); err != nil {
		slog.Error("Failed to write init event to socket", "error", err)
		return
	}

	// Launch the agent in a goroutine. When connCtx is cancelled (parent
	// shutdown or client disconnect detected by streamToConn), the
	// coordinator will observe the context and stop.
	done := make(chan agentResponse, 1)
	go func() {
		result, runErr := l.app.AgentCoordinator.Run(connCtx, sess.ID, req.Prompt, req.Attachments...)
		done <- agentResponse{result: result, err: runErr}
	}()

	// Stream events to the client. On write failure (client disconnect),
	// streamToConn returns an error. The deferred cancel() above then
	// cancels connCtx, which signals the agent goroutine to stop.
	if err := l.streamToConn(connCtx, conn, sess.ID, done); err != nil {
		slog.Error("Socket stream error", "error", err, "session_id", sess.ID)
	}
}

// connWriter wraps an io.Writer and cancels a context on write errors,
// enabling early detection of client disconnects.
type connWriter struct {
	w      io.Writer
	cancel context.CancelFunc
}

func (cw *connWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	if err != nil {
		cw.cancel()
	}
	return n, err
}

// streamToConn subscribes to message and session events and writes NDJSON
// to the connection until the agent finishes.
func (l *Listener) streamToConn(ctx context.Context, w io.Writer, sessionID string, done <-chan agentResponse) error {
	start := time.Now()

	// Wrap the writer so that write failures (client disconnect) cancel
	// the context, which stops the agent and unblocks the select loop.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cw := &connWriter{w: w, cancel: cancel}

	// Subscribe to events.
	var messageEvents <-chan pubsub.Event[message.Message]
	if bs, ok := l.app.Messages.(pubsub.BlockingSubscriber[message.Message]); ok {
		messageEvents = bs.SubscribeBlocking(ctx)
	} else {
		messageEvents = l.app.Messages.Subscribe(ctx)
	}
	var sessionEvents <-chan pubsub.Event[session.Session]
	if bs, ok := l.app.Sessions.(pubsub.BlockingSubscriber[session.Session]); ok {
		sessionEvents = bs.SubscribeBlocking(ctx)
	} else {
		sessionEvents = l.app.Sessions.Subscribe(ctx)
	}

	childSessions := make(map[string]bool)
	msgState := make(map[string]*streamMessageState)
	contentBuf := newStreamContentBuffer(cw, sessionID)
	messageReadBytes := make(map[string]int)

	getState := func(id string) *streamMessageState {
		if s, ok := msgState[id]; ok {
			return s
		}
		s := &streamMessageState{
			toolCallsSeen:   make(map[string]bool),
			toolResultsSeen: make(map[string]bool),
		}
		msgState[id] = s
		return s
	}

	processMessage := func(msg message.Message) error {
		if len(msg.Parts) == 0 {
			return nil
		}

		isChildSession := childSessions[msg.SessionID]
		if msg.SessionID != sessionID && !isChildSession {
			return nil
		}

		// Buffer content deltas for parent assistant messages.
		if msg.Role == message.Assistant && !isChildSession {
			content := msg.Content().String()
			readBytes := messageReadBytes[msg.ID]
			if len(content) < readBytes {
				return fmt.Errorf("message content shorter than read bytes: %d < %d", len(content), readBytes)
			}
			part := content[readBytes:]
			if readBytes == 0 {
				part = strings.TrimLeft(part, " \t")
			}
			if part != "" {
				if err := contentBuf.Write(part); err != nil {
					return fmt.Errorf("failed to buffer stream content: %w", err)
				}
			}
			messageReadBytes[msg.ID] = len(content)
		}

		if err := contentBuf.Flush(); err != nil {
			return fmt.Errorf("failed to flush content buffer: %w", err)
		}

		return emitStreamEvents(cw, msg.SessionID, &msg, getState(msg.ID))
	}

	for {
		var flushTimer <-chan time.Time
		flushTimer = contentBuf.Timer()

		select {
		case result := <-done:
			// Drain buffered events.
			drainDeadline := time.After(500 * time.Millisecond)
		drain:
			for {
				select {
				case sev := <-sessionEvents:
					if sev.Payload.ParentSessionID == sessionID {
						childSessions[sev.Payload.ID] = true
					}
				case ev := <-messageEvents:
					if err := processMessage(ev.Payload); err != nil {
						slog.Error("Failed to process message during drain", "error", err)
					}
				case <-drainDeadline:
					break drain
				default:
					break drain
				}
			}

			if err := contentBuf.Flush(); err != nil {
				slog.Error("Failed to flush content buffer on completion", "error", err)
			}
			contentBuf.stopTimer()

			elapsed := durationMS(start)
			isError := false
			var errMsg string
			if result.err != nil {
				isError = true
				errMsg = result.err.Error()
			}

			usage := l.fetchUsage(sessionID)

			return writeStreamEvent(cw, StreamEvent{
				Type:       "result",
				SessionID:  sessionID,
				DurationMS: &elapsed,
				IsError:    &isError,
				Error:      errMsg,
				Usage:      &usage,
			})

		case <-flushTimer:
			if err := contentBuf.Flush(); err != nil {
				return fmt.Errorf("failed to flush content buffer: %w", err)
			}

		case sev := <-sessionEvents:
			if sev.Payload.ParentSessionID == sessionID {
				childSessions[sev.Payload.ID] = true
			}

		case ev := <-messageEvents:
			if err := processMessage(ev.Payload); err != nil {
				return err
			}

		case <-ctx.Done():
			_ = contentBuf.Flush()
			contentBuf.stopTimer()
			return ctx.Err()
		}
	}
}

// fetchUsage retrieves usage info for a session, best-effort.
func (l *Listener) fetchUsage(sessionID string) RunResultUsage {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := l.app.Sessions.Get(ctx, sessionID)
	if err != nil {
		slog.Warn("Failed to fetch session usage for socket response", "error", err)
		return RunResultUsage{}
	}
	return RunResultUsage{
		InputTokens:  sess.PromptTokens,
		OutputTokens: sess.CompletionTokens,
		CostEstimate: sess.Cost,
	}
}

// readSocketRequest reads and validates a SocketRequest from the connection.
func readSocketRequest(conn net.Conn) (SocketRequest, error) {
	if err := conn.SetReadDeadline(time.Now().Add(socketReadTimeout)); err != nil {
		return SocketRequest{}, fmt.Errorf("failed to set read deadline: %w", err)
	}

	// Use a JSON decoder so we consume exactly one JSON object without
	// requiring the client to close the write side of the connection.
	// The LimitReader guards against oversized payloads.
	reader := io.LimitReader(conn, maxSocketRequestSize)
	dec := json.NewDecoder(reader)

	var req SocketRequest
	if err := dec.Decode(&req); err != nil {
		return SocketRequest{}, fmt.Errorf("invalid JSON: %w", err)
	}

	// Reset the deadline for writing.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return SocketRequest{}, fmt.Errorf("failed to reset read deadline: %w", err)
	}

	if strings.TrimSpace(req.Prompt) == "" {
		return SocketRequest{}, fmt.Errorf("missing prompt field")
	}

	return req, nil
}

// writeSocketError writes a single error event to the connection.
func writeSocketError(w io.Writer, errMsg string) {
	_ = writeStreamEvent(w, StreamEvent{
		Type:  "error",
		Error: errMsg,
	})
}

// DefaultSocketPath returns a default socket path for the current process.
// Used by --listen auto to create a predictable socket location.
func DefaultSocketPath() string {
	dir := socketDir()
	return filepath.Join(dir, fmt.Sprintf("%d.sock", os.Getpid()))
}

// DiscoverSocketPath finds the most recently modified .sock file in the
// standard socket directory. Used by --socket auto to locate a running
// Crush instance. Returns an empty string if no socket is found.
func DiscoverSocketPath() string {
	dir := socketDir()
	pattern := filepath.Join(dir, "*.sock")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	// Pick the most recently modified socket.
	var best string
	var bestTime time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			bestTime = info.ModTime()
			best = m
		}
	}
	return best
}

// SocketDir returns the standard directory description used for auto
// socket path resolution, suitable for help text.
func SocketDir() string {
	return socketDir()
}

func socketDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "crush")
}
