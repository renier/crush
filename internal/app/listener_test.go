package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// newListenerTestApp builds a minimal App suitable for listener tests.
func newListenerTestApp(
	t *testing.T,
	sessions *mockSessionServiceWithBroker,
	messages *mockMessageService,
	coordinator *mockCoordinator,
) *App {
	t.Helper()

	tmpDir := t.TempDir()
	store, err := config.Load(tmpDir, tmpDir, false)
	require.NoError(t, err)

	mcp.Initialize(t.Context(), newMockPermissionService(), store)

	perms := newMockPermissionService()

	a := &App{
		Sessions:           sessions,
		Messages:           messages,
		Permissions:        perms,
		AgentCoordinator:   coordinator,
		config:             store,
		agentNotifications: pubsub.NewBroker[notify.Notification](),
		serviceEventsWG:    &sync.WaitGroup{},
		events:             make(chan tea.Msg, 100),
	}
	t.Cleanup(func() {
		sessions.Broker.Shutdown()
		messages.Broker.Shutdown()
		perms.Broker.Shutdown()
		perms.notifyBroker.Shutdown()
		a.agentNotifications.Shutdown()
	})

	return a
}

// testSocketPath returns a short socket path under the system temp directory
// that stays within the 104-byte sun_path limit on macOS.
func testSocketPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "crush-*.sock")
	require.NoError(t, err)
	p := f.Name()
	f.Close()
	os.Remove(p)
	t.Cleanup(func() { os.Remove(p) })
	return p
}

// startListener creates a Listener, starts it in a goroutine, and waits
// for the socket to be connectable. Returns the listener for cleanup.
func startListener(t *testing.T, a *App, sockPath string, ctx context.Context) *Listener {
	t.Helper()
	l := NewListener(a, sockPath)
	go l.Listen(ctx)
	t.Cleanup(func() { l.Close() })

	waitFor(t, 2*time.Second, func() bool {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
	return l
}

// dialSocket connects to a Unix socket and sends a JSON request, returning
// the connection for reading the response.
func dialSocket(t *testing.T, sockPath string, req SocketRequest) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	require.NoError(t, err)
	data, err := json.Marshal(req)
	require.NoError(t, err)
	_, err = conn.Write(data)
	require.NoError(t, err)
	conn.(*net.UnixConn).CloseWrite()
	return conn
}

// readAllEvents reads all NDJSON events from a connection.
func readAllEvents(t *testing.T, conn net.Conn) []StreamEvent {
	t.Helper()
	var buf bytes.Buffer
	_, err := io.Copy(&buf, conn)
	require.NoError(t, err)
	return parseNDJSON(t, buf.String())
}

func TestListener_BasicPrompt(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Hello from socket. "},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})
			// Give pubsub time to deliver before the agent "finishes".
			time.Sleep(20 * time.Millisecond)
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	conn := dialSocket(t, sockPath, SocketRequest{Prompt: "hello"})
	defer conn.Close()

	events := readAllEvents(t, conn)
	require.NotEmpty(t, events)

	require.Equal(t, "init", events[0].Type)
	require.NotEmpty(t, events[0].SessionID)

	last := events[len(events)-1]
	require.Equal(t, "result", last.Type)
	require.NotNil(t, last.IsError)
	require.False(t, *last.IsError)

	var content string
	for _, ev := range events {
		if ev.Type == "content" {
			content += ev.Content
		}
	}
	require.Contains(t, content, "Hello from socket.")
}

func TestListener_MalformedRequest(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()
	coord := &mockCoordinator{
		runFn: func(context.Context, string, string) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	conn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("not json"))
	require.NoError(t, err)
	conn.(*net.UnixConn).CloseWrite()

	events := readAllEvents(t, conn)
	require.NotEmpty(t, events)
	require.Equal(t, "error", events[0].Type)
	require.Contains(t, events[0].Error, "invalid")
}

func TestListener_MissingPrompt(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()
	coord := &mockCoordinator{
		runFn: func(context.Context, string, string) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	conn := dialSocket(t, sockPath, SocketRequest{Prompt: ""})
	defer conn.Close()

	events := readAllEvents(t, conn)
	require.NotEmpty(t, events)
	require.Equal(t, "error", events[0].Type)
	require.Contains(t, events[0].Error, "missing prompt")
}

func TestListener_ConcurrentConnections(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
			publishMsg(messages, message.Message{
				ID:        fmt.Sprintf("msg-%s-%d", sessionID, time.Now().UnixNano()),
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Reply to: " + prompt + ". "},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})
			time.Sleep(20 * time.Millisecond)
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dialSocket(t, sockPath, SocketRequest{Prompt: "test prompt"})
			defer conn.Close()
			events := readAllEvents(t, conn)
			require.NotEmpty(t, events)
			require.Equal(t, "init", events[0].Type)
			last := events[len(events)-1]
			require.Equal(t, "result", last.Type)
		}()
	}
	wg.Wait()
}

func TestListener_ContextCancellation(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()
	coord := &mockCoordinator{
		runFn: func(context.Context, string, string) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())

	l := NewListener(a, sockPath)
	done := make(chan error, 1)
	go func() {
		done <- l.Listen(ctx)
	}()

	waitFor(t, 2*time.Second, func() bool {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not shut down after context cancellation")
	}
}

func TestListener_AgentError(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(context.Context, string, string) (*fantasy.AgentResult, error) {
			return nil, io.ErrUnexpectedEOF
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	conn := dialSocket(t, sockPath, SocketRequest{Prompt: "trigger error"})
	defer conn.Close()

	events := readAllEvents(t, conn)
	require.NotEmpty(t, events)

	last := events[len(events)-1]
	require.Equal(t, "result", last.Type)
	require.NotNil(t, last.IsError)
	require.True(t, *last.IsError)
	require.Contains(t, last.Error, "unexpected EOF")
}

func TestDefaultSocketPath(t *testing.T) {
	t.Parallel()

	p := DefaultSocketPath()
	require.NotEmpty(t, p)
	require.Contains(t, p, ".sock")
}

// TestListener_NoCloseWrite verifies that the listener works when the
// client does NOT half-close the write side of the connection, which is
// the real-world behavior of most socket clients (socat, Python, etc.).
// The server must process the request after reading a complete JSON
// object without waiting for EOF.
func TestListener_NoCloseWrite(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Response without close. "},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})
			time.Sleep(20 * time.Millisecond)
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	// Connect and send request WITH a trailing newline but WITHOUT
	// closing the write side. This simulates what socat and most
	// real clients do.
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	require.NoError(t, err)
	defer conn.Close()

	data, err := json.Marshal(SocketRequest{Prompt: "hello"})
	require.NoError(t, err)
	data = append(data, '\n')
	_, err = conn.Write(data)
	require.NoError(t, err)
	// Deliberately NOT calling conn.CloseWrite().

	// Read NDJSON events line by line until we see the result event.
	scanner := bufio.NewScanner(conn)
	var events []StreamEvent
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for scanner.Scan() {
		var ev StreamEvent
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &ev))
		events = append(events, ev)
		if ev.Type == "result" {
			break
		}
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, events)

	require.Equal(t, "init", events[0].Type)

	last := events[len(events)-1]
	require.Equal(t, "result", last.Type)
	require.NotNil(t, last.IsError)
	require.False(t, *last.IsError)

	var content string
	for _, ev := range events {
		if ev.Type == "content" {
			content += ev.Content
		}
	}
	require.Contains(t, content, "Response without close.")
}

// TestListener_SlowAgent simulates a real LLM interaction where there is
// a significant delay between the user message being created and the
// assistant response arriving. The client must receive events for both
// the user message and the assistant response.
func TestListener_SlowAgent(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(ctx context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			// Simulate user message creation (the real agent does this
			// synchronously before calling the LLM).
			publishMsg(messages, message.Message{
				ID:        "user-msg",
				SessionID: sessionID,
				Role:      message.User,
				Parts: []message.ContentPart{
					message.TextContent{Text: "hello"},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})

			// Simulate LLM latency.
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			// Simulate assistant response.
			publishMsg(messages, message.Message{
				ID:        "asst-msg",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Slow response arrived. "},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})
			time.Sleep(20 * time.Millisecond)
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	// Connect WITHOUT closing the write side.
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	require.NoError(t, err)
	defer conn.Close()

	data, err := json.Marshal(SocketRequest{Prompt: "hello"})
	require.NoError(t, err)
	data = append(data, '\n')
	_, err = conn.Write(data)
	require.NoError(t, err)

	// Read events line by line.
	scanner := bufio.NewScanner(conn)
	var events []StreamEvent
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for scanner.Scan() {
		var ev StreamEvent
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &ev))
		events = append(events, ev)
		if ev.Type == "result" {
			break
		}
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, events)

	// Must have init, user message events, assistant message events, result.
	require.Equal(t, "init", events[0].Type)

	last := events[len(events)-1]
	require.Equal(t, "result", last.Type)
	require.NotNil(t, last.IsError)
	require.False(t, *last.IsError)

	// Verify we got assistant content.
	var content string
	for _, ev := range events {
		if ev.Type == "content" {
			content += ev.Content
		}
	}
	require.Contains(t, content, "Slow response arrived.")

	// Verify we saw an assistant message_start event.
	var hasAssistant bool
	for _, ev := range events {
		if ev.Type == "message_start" && ev.Role == "assistant" {
			hasAssistant = true
		}
	}
	require.True(t, hasAssistant, "expected an assistant message_start event")
}

func TestRunViaSocket_TextFormat(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Socket client response. "},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})
			time.Sleep(20 * time.Millisecond)
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	var buf bytes.Buffer
	err := RunViaSocket(t.Context(), &buf, SocketClientOptions{
		SocketPath: sockPath,
		Prompt:     "hello",
		Format:     OutputFormatText,
	})
	require.NoError(t, err)
	require.Contains(t, buf.String(), "Socket client response.")
}

func TestRunViaSocket_JSONFormat(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "JSON response. "},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})
			time.Sleep(20 * time.Millisecond)
			return &fantasy.AgentResult{}, nil
		},
	}

	a := newListenerTestApp(t, sessions, messages, coord)
	sockPath := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startListener(t, a, sockPath, ctx)

	var buf bytes.Buffer
	err := RunViaSocket(t.Context(), &buf, SocketClientOptions{
		SocketPath: sockPath,
		Prompt:     "hello",
		Format:     OutputFormatJSON,
	})
	require.NoError(t, err)

	var result RunResult
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Contains(t, result.Result.Content, "JSON response.")
	require.Equal(t, "success", result.Execution.Status)
}

func TestDiscoverSocketPath(t *testing.T) {
	// With no sockets, should return empty.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	require.Empty(t, DiscoverSocketPath())
}

func TestDiscoverSocketPath_FindsSocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	crushDir := dir + "/crush"
	require.NoError(t, os.MkdirAll(crushDir, 0o700))
	require.NoError(t, os.WriteFile(crushDir+"/1234.sock", nil, 0o600))

	p := DiscoverSocketPath()
	require.Contains(t, p, "1234.sock")
}
