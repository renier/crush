package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

// mockMessageService implements message.Service using a pubsub.Broker.
type mockMessageService struct {
	*pubsub.Broker[message.Message]
}

func newMockMessageService() *mockMessageService {
	return &mockMessageService{Broker: pubsub.NewBroker[message.Message]()}
}

func (m *mockMessageService) Create(context.Context, string, message.CreateMessageParams) (message.Message, error) {
	return message.Message{}, nil
}
func (m *mockMessageService) Update(context.Context, message.Message) error { return nil }
func (m *mockMessageService) Get(context.Context, string) (message.Message, error) {
	return message.Message{}, nil
}
func (m *mockMessageService) List(context.Context, string) ([]message.Message, error) {
	return nil, nil
}
func (m *mockMessageService) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}
func (m *mockMessageService) ListAllUserMessages(context.Context) ([]message.Message, error) {
	return nil, nil
}
func (m *mockMessageService) Delete(context.Context, string) error                { return nil }
func (m *mockMessageService) DeleteSessionMessages(context.Context, string) error { return nil }

// mockSessionServiceWithBroker extends mockSessionService with a real broker
// so that session creation events are delivered to subscribers.
type mockSessionServiceWithBroker struct {
	*pubsub.Broker[session.Session]
	sessions []session.Session
	mu       sync.Mutex
}

func newMockSessionService() *mockSessionServiceWithBroker {
	return &mockSessionServiceWithBroker{
		Broker: pubsub.NewBroker[session.Session](),
	}
}

func (m *mockSessionServiceWithBroker) Create(_ context.Context, title string) (session.Session, error) {
	s := session.Session{ID: "parent-sess", Title: title}
	m.mu.Lock()
	m.sessions = append(m.sessions, s)
	m.mu.Unlock()
	m.Publish(pubsub.CreatedEvent, s)
	return s, nil
}

func (m *mockSessionServiceWithBroker) CreateTitleSession(context.Context, string) (session.Session, error) {
	return session.Session{}, nil
}

func (m *mockSessionServiceWithBroker) CreateTaskSession(_ context.Context, id, parentID, title string) (session.Session, error) {
	s := session.Session{ID: id, ParentSessionID: parentID, Title: title}
	m.mu.Lock()
	m.sessions = append(m.sessions, s)
	m.mu.Unlock()
	m.Publish(pubsub.CreatedEvent, s)
	return s, nil
}

func (m *mockSessionServiceWithBroker) Get(_ context.Context, id string) (session.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return session.Session{}, nil
}

func (m *mockSessionServiceWithBroker) GetLast(context.Context) (session.Session, error) {
	return session.Session{}, nil
}
func (m *mockSessionServiceWithBroker) List(context.Context) ([]session.Session, error) {
	return nil, nil
}
func (m *mockSessionServiceWithBroker) Save(_ context.Context, s session.Session) (session.Session, error) {
	return s, nil
}
func (m *mockSessionServiceWithBroker) UpdateTitleAndUsage(context.Context, string, string, int64, int64, float64) error {
	return nil
}
func (m *mockSessionServiceWithBroker) Rename(context.Context, string, string) error { return nil }
func (m *mockSessionServiceWithBroker) Delete(context.Context, string) error         { return nil }
func (m *mockSessionServiceWithBroker) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return messageID + "$$" + toolCallID
}
func (m *mockSessionServiceWithBroker) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}
func (m *mockSessionServiceWithBroker) IsAgentToolSession(sessionID string) bool {
	_, _, ok := m.ParseAgentToolSessionID(sessionID)
	return ok
}

// mockPermissionService implements permission.Service with auto-approve.
type mockPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
	notifyBroker *pubsub.Broker[permission.PermissionNotification]
}

func newMockPermissionService() *mockPermissionService {
	return &mockPermissionService{
		Broker:       pubsub.NewBroker[permission.PermissionRequest](),
		notifyBroker: pubsub.NewBroker[permission.PermissionNotification](),
	}
}

func (m *mockPermissionService) GrantPersistent(permission.PermissionRequest) {}
func (m *mockPermissionService) Grant(permission.PermissionRequest)           {}
func (m *mockPermissionService) Deny(permission.PermissionRequest)            {}
func (m *mockPermissionService) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}
func (m *mockPermissionService) AutoApproveSession(string) {}
func (m *mockPermissionService) SetSkipRequests(bool)      {}
func (m *mockPermissionService) SkipRequests() bool        { return true }
func (m *mockPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return m.notifyBroker.Subscribe(ctx)
}

// mockCoordinator implements agent.Coordinator. The Run function is provided
// by the test so it can orchestrate message publishing.
type mockCoordinator struct {
	runFn func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error)
}

func (m *mockCoordinator) Run(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return m.runFn(ctx, sessionID, prompt)
}
func (m *mockCoordinator) Cancel(string)                           {}
func (m *mockCoordinator) CancelAll()                              {}
func (m *mockCoordinator) IsSessionBusy(string) bool               { return false }
func (m *mockCoordinator) IsBusy() bool                            { return false }
func (m *mockCoordinator) QueuedPrompts(string) int                { return 0 }
func (m *mockCoordinator) QueuedPromptsList(string) []string       { return nil }
func (m *mockCoordinator) ClearQueue(string)                       {}
func (m *mockCoordinator) Summarize(context.Context, string) error { return nil }
func (m *mockCoordinator) Model() agent.Model {
	return agent.Model{
		CatwalkCfg: catwalk.Model{Name: "test-model", ContextWindow: 128000},
		ModelCfg:   config.SelectedModel{Provider: "test-provider"},
	}
}
func (m *mockCoordinator) UpdateModels(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newNonInteractiveTestApp(
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

	app := &App{
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
		app.agentNotifications.Shutdown()
	})

	return app
}

// publishMsg is a convenience for publishing a message event.
func publishMsg(svc *mockMessageService, msg message.Message) {
	svc.Publish(pubsub.UpdatedEvent, msg)
}

// waitFor spins until cond returns true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("waitFor timed out")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRunNonInteractive_TextFormat_ChildSessionFiltered verifies that text
// format only outputs parent session assistant content and ignores child
// session messages.
func TestRunNonInteractive_TextFormat_ChildSessionFiltered(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			// Simulate parent assistant streaming text.
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "Parent output"}},
			})

			// Simulate child session creation and child assistant text.
			childID := "msg-1$$tool-1"
			sessions.CreateTaskSession(context.Background(), childID, sessionID, "sub-agent")

			time.Sleep(20 * time.Millisecond)

			publishMsg(messages, message.Message{
				ID:        "child-msg-1",
				SessionID: childID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "Child output"}},
			})

			time.Sleep(20 * time.Millisecond)

			// Parent gets the tool result and finishes.
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Parent output"},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})

			return &fantasy.AgentResult{}, nil
		},
	}

	app := newNonInteractiveTestApp(t, sessions, messages, coord)

	var buf bytes.Buffer
	err := app.RunNonInteractive(t.Context(), &buf, NonInteractiveOptions{
		Prompt:      "test",
		Format:      OutputFormatText,
		HideSpinner: true,
	})
	require.NoError(t, err)

	output := buf.String()
	require.Contains(t, output, "Parent output")
	require.NotContains(t, output, "Child output")
}

// TestRunNonInteractive_JSONFormat_ChildSessionFiltered verifies that json
// format only includes parent session content in the final output.
func TestRunNonInteractive_JSONFormat_ChildSessionFiltered(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "Parent content"}},
			})

			childID := "msg-1$$tool-1"
			sessions.CreateTaskSession(context.Background(), childID, sessionID, "sub-agent")

			time.Sleep(20 * time.Millisecond)

			publishMsg(messages, message.Message{
				ID:        "child-msg-1",
				SessionID: childID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "Child content"}},
			})

			time.Sleep(20 * time.Millisecond)

			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Parent content"},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})

			return &fantasy.AgentResult{}, nil
		},
	}

	app := newNonInteractiveTestApp(t, sessions, messages, coord)

	var buf bytes.Buffer
	err := app.RunNonInteractive(t.Context(), &buf, NonInteractiveOptions{
		Prompt:      "test",
		Format:      OutputFormatJSON,
		HideSpinner: true,
	})
	require.NoError(t, err)

	var result RunResult
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Contains(t, result.Result.Content, "Parent content")
	require.NotContains(t, result.Result.Content, "Child content")
}

// TestRunNonInteractive_StreamJSON_ChildSessionEventsEmitted verifies that
// stream-json format emits events from child sessions (tool calls, results,
// message lifecycle) so consumers see progress during sub-agent execution.
func TestRunNonInteractive_StreamJSON_ChildSessionEventsEmitted(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	const childSessionID = "msg-1$$tool-1"

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			// Parent assistant calls agent tool.
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Thinking"},
					message.ToolCall{ID: "tool-1", Name: "agent", Finished: true},
					message.Finish{Reason: message.FinishReasonToolUse, Time: time.Now().Unix()},
				},
			})

			time.Sleep(20 * time.Millisecond)

			// Child session created.
			sessions.CreateTaskSession(context.Background(), childSessionID, sessionID, "sub-agent")

			time.Sleep(20 * time.Millisecond)

			// Child assistant message with a tool call.
			publishMsg(messages, message.Message{
				ID:        "child-msg-1",
				SessionID: childSessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.ToolCall{ID: "child-tc-1", Name: "grep", Input: `{"pattern":"foo"}`, Finished: true},
				},
			})

			time.Sleep(20 * time.Millisecond)

			// Child tool result.
			publishMsg(messages, message.Message{
				ID:        "child-msg-2",
				SessionID: childSessionID,
				Role:      message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{ToolCallID: "child-tc-1", Name: "grep", Content: "found foo"},
				},
			})

			time.Sleep(20 * time.Millisecond)

			// Child finishes.
			publishMsg(messages, message.Message{
				ID:        "child-msg-3",
				SessionID: childSessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Done searching"},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})

			time.Sleep(20 * time.Millisecond)

			// Parent tool result and final response.
			publishMsg(messages, message.Message{
				ID:        "msg-2",
				SessionID: sessionID,
				Role:      message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{ToolCallID: "tool-1", Name: "agent", Content: "sub-agent result"},
				},
			})

			time.Sleep(20 * time.Millisecond)

			publishMsg(messages, message.Message{
				ID:        "msg-3",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Final answer"},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})

			return &fantasy.AgentResult{}, nil
		},
	}

	app := newNonInteractiveTestApp(t, sessions, messages, coord)

	var buf bytes.Buffer
	err := app.RunNonInteractive(t.Context(), &buf, NonInteractiveOptions{
		Prompt:      "test",
		Format:      OutputFormatStreamJSON,
		HideSpinner: true,
	})
	require.NoError(t, err)

	events := parseNDJSON(t, buf.String())
	require.NotEmpty(t, events)

	// Verify init event.
	require.Equal(t, "init", events[0].Type)
	require.Equal(t, "parent-sess", events[0].SessionID)

	// Collect event types and session IDs for child events.
	var childToolCalls []StreamEvent
	var childToolResults []StreamEvent
	var childMessageStarts []StreamEvent
	for _, ev := range events {
		if ev.SessionID == childSessionID {
			switch ev.Type {
			case "tool_call":
				childToolCalls = append(childToolCalls, ev)
			case "tool_result":
				childToolResults = append(childToolResults, ev)
			case "message_start":
				childMessageStarts = append(childMessageStarts, ev)
			}
		}
	}

	require.NotEmpty(t, childToolCalls, "child session tool_call events should be emitted")
	require.Equal(t, "grep", childToolCalls[0].ToolCall.Name)
	require.Equal(t, childSessionID, childToolCalls[0].SessionID)

	require.NotEmpty(t, childToolResults, "child session tool_result events should be emitted")
	require.Equal(t, "grep", childToolResults[0].ToolResult.Name)
	require.Contains(t, childToolResults[0].ToolResult.Content, "found foo")

	require.NotEmpty(t, childMessageStarts, "child session message_start events should be emitted")

	// Verify parent events are also present.
	var parentToolCalls []StreamEvent
	for _, ev := range events {
		if ev.SessionID == "parent-sess" && ev.Type == "tool_call" {
			parentToolCalls = append(parentToolCalls, ev)
		}
	}
	require.NotEmpty(t, parentToolCalls, "parent session tool_call events should be present")
	require.Equal(t, "agent", parentToolCalls[0].ToolCall.Name)

	// Verify the result event is present.
	lastEvent := events[len(events)-1]
	require.Equal(t, "result", lastEvent.Type)
}

// TestRunNonInteractive_StreamJSON_ChildContentNotMixedIntoParent verifies
// that child session assistant text is NOT written to the content buffer
// (which tracks the parent's final output), while still emitting structural
// stream events.
func TestRunNonInteractive_StreamJSON_ChildContentNotMixedIntoParent(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			// Parent starts.
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "Parent says hello. "}},
			})

			childID := "msg-1$$tool-1"
			sessions.CreateTaskSession(context.Background(), childID, sessionID, "sub-agent")

			time.Sleep(20 * time.Millisecond)

			// Child assistant text.
			publishMsg(messages, message.Message{
				ID:        "child-msg-1",
				SessionID: childID,
				Role:      message.Assistant,
				Parts:     []message.ContentPart{message.TextContent{Text: "Child says goodbye. "}},
			})

			time.Sleep(20 * time.Millisecond)

			// Parent finishes.
			publishMsg(messages, message.Message{
				ID:        "msg-1",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Parent says hello. "},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})

			return &fantasy.AgentResult{}, nil
		},
	}

	app := newNonInteractiveTestApp(t, sessions, messages, coord)

	var buf bytes.Buffer
	err := app.RunNonInteractive(t.Context(), &buf, NonInteractiveOptions{
		Prompt:      "test",
		Format:      OutputFormatStreamJSON,
		HideSpinner: true,
	})
	require.NoError(t, err)

	events := parseNDJSON(t, buf.String())

	// Collect all content events.
	var contentEvents []StreamEvent
	for _, ev := range events {
		if ev.Type == "content" {
			contentEvents = append(contentEvents, ev)
		}
	}

	// All content events should be from the parent session.
	for _, ev := range contentEvents {
		require.Equal(t, "parent-sess", ev.SessionID, "content events should only come from parent session")
		require.NotContains(t, ev.Content, "Child says goodbye")
	}

	// Concatenate all content to verify parent text is complete.
	var allContent strings.Builder
	for _, ev := range contentEvents {
		allContent.WriteString(ev.Content)
	}
	require.Contains(t, allContent.String(), "Parent says hello.")
}

// TestRunNonInteractive_StreamJSON_ChildSessionIDCorrect verifies that
// stream events from child sessions carry the child's session_id, not the
// parent's.
func TestRunNonInteractive_StreamJSON_ChildSessionIDCorrect(t *testing.T) {
	t.Parallel()

	sessions := newMockSessionService()
	messages := newMockMessageService()

	const childSessionID = "msg-1$$tool-1"

	coord := &mockCoordinator{
		runFn: func(_ context.Context, sessionID, _ string) (*fantasy.AgentResult, error) {
			sessions.CreateTaskSession(context.Background(), childSessionID, sessionID, "sub-agent")

			time.Sleep(20 * time.Millisecond)

			publishMsg(messages, message.Message{
				ID:        "child-msg-1",
				SessionID: childSessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.ToolCall{ID: "tc-1", Name: "view", Finished: true},
					message.Finish{Reason: message.FinishReasonToolUse, Time: time.Now().Unix()},
				},
			})

			time.Sleep(20 * time.Millisecond)

			publishMsg(messages, message.Message{
				ID:        "msg-finish",
				SessionID: sessionID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Done"},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
				},
			})

			return &fantasy.AgentResult{}, nil
		},
	}

	app := newNonInteractiveTestApp(t, sessions, messages, coord)

	var buf bytes.Buffer
	err := app.RunNonInteractive(t.Context(), &buf, NonInteractiveOptions{
		Prompt:      "test",
		Format:      OutputFormatStreamJSON,
		HideSpinner: true,
	})
	require.NoError(t, err)

	events := parseNDJSON(t, buf.String())

	// Find the child tool_call event and verify its session_id.
	for _, ev := range events {
		if ev.Type == "tool_call" && ev.ToolCall != nil && ev.ToolCall.Name == "view" {
			require.Equal(t, childSessionID, ev.SessionID,
				"child tool_call event should carry the child session ID")
		}
	}

	// Find the child message_start event.
	for _, ev := range events {
		if ev.Type == "message_start" && ev.SessionID == childSessionID {
			require.Equal(t, childSessionID, ev.SessionID)
			return
		}
	}
	t.Fatal("expected a message_start event with the child session ID")
}
