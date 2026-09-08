package notify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type composeCall struct {
	Input ComposeInput
	Label ComposeLabel
}

type recordingComposer struct {
	mutex  sync.Mutex
	calls  []composeCall
	result ComposeResult
	err    error
}

func (c *recordingComposer) Compose(
	_ context.Context,
	input ComposeInput,
	label ComposeLabel,
) (ComposeResult, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.calls = append(c.calls, composeCall{Input: input, Label: label})
	return c.result, c.err
}

func (c *recordingComposer) Calls() []composeCall {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return append([]composeCall(nil), c.calls...)
}

type panicComposer struct{}

func (panicComposer) Compose(context.Context, ComposeInput, ComposeLabel) (ComposeResult, error) {
	panic("composer must not be called")
}

type capturedNotification struct {
	Title    string
	Body     string
	Priority string
	Tags     string
}

type pipelineRoundTripFunc func(*http.Request) (*http.Response, error)

func (function pipelineRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func captureNotificationServer(t *testing.T) (*httptest.Server, <-chan capturedNotification) {
	t.Helper()
	requests := make(chan capturedNotification, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("reading notification body: %v", err)
		}
		requests <- capturedNotification{
			Title: req.Header.Get("Title"), Body: string(body),
			Priority: req.Header.Get("Priority"), Tags: req.Header.Get("Tags"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	return server, requests
}

func waitNotification(t *testing.T, requests <-chan capturedNotification) capturedNotification {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
		return capturedNotification{}
	}
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing transcript: %v", err)
	}
	return path
}

func conversationTranscript(t *testing.T, uuid, messageID string, malformedTail bool) string {
	t.Helper()
	assistant := map[string]any{
		"type":      "assistant",
		"timestamp": "2026-07-01T00:00:01Z",
		"message": map[string]any{
			"role": "assistant",
			"id":   messageID,
			"content": []map[string]string{{
				"type": "text", "text": "latest assistant text",
			}},
		},
	}
	if uuid != "" {
		assistant["uuid"] = uuid
	}
	user, err := json.Marshal(map[string]any{
		"type":      "user",
		"timestamp": "2026-07-01T00:00:00Z",
		"message": map[string]any{
			"role": "user", "content": "latest user text",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assistantJSON, err := json.Marshal(assistant)
	if err != nil {
		t.Fatal(err)
	}
	earlierUser := `{"type":"user","timestamp":"2026-06-30T23:59:58Z",` +
		`"message":{"role":"user","content":"earlier user text"}}`
	earlierAssistant := `{"type":"assistant","uuid":"earlier-assistant-uuid",` +
		`"timestamp":"2026-06-30T23:59:59Z","message":{"role":"assistant",` +
		`"content":[{"type":"text","text":"earlier assistant text"}]}}`
	lines := []string{earlierUser, earlierAssistant, string(user), string(assistantJSON)}
	if malformedTail {
		lines = append(lines, "{")
	}
	return writeTranscript(t, lines...)
}

func testPipeline(t *testing.T, server *httptest.Server, composer Composer) (Pipeline, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	return Pipeline{
		Composer:  composer,
		Sender:    Sender{URL: server.URL, Client: server.Client()},
		Log:       DecisionLog{Path: logPath},
		Host:      "testhost",
		Workspace: "earth:3",
	}, logPath
}

func readDecisionLog(t *testing.T, path string) []DecisionRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading decision log: %v", err)
	}
	var records []DecisionRecord
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var record DecisionRecord
		if unmarshalErr := json.Unmarshal(scanner.Bytes(), &record); unmarshalErr != nil {
			t.Fatalf("decoding decision record: %v", unmarshalErr)
		}
		records = append(records, record)
	}
	if scanErr := scanner.Err(); scanErr != nil {
		t.Fatalf("scanning decision log: %v", scanErr)
	}
	return records
}

func TestPipelineClaudeCompletionUsesReliableNativeIdentityAndLatestConversation(t *testing.T) {
	tests := []struct {
		name      string
		uuid      string
		messageID string
		wantID    string
	}{
		{name: "UUID takes precedence", uuid: "assistant-uuid", messageID: "message-id", wantID: "assistant-uuid"},
		{name: "message ID is reliable fallback", messageID: "message-id", wantID: "message-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			composer := &recordingComposer{result: ComposeResult{Body: "Composed completion summary."}}
			pipeline, logPath := testPipeline(t, server, composer)
			in := HookInput{
				Harness: harnessClaude, SessionID: "session-1", CompletionID: "stale-supplied-id",
				CWD: "/home/user/project", HookEventName: eventStop,
				TranscriptPath:       conversationTranscript(t, tt.uuid, tt.messageID, false),
				LastAssistantMessage: "hook fallback must not win",
			}

			if err := pipeline.Run(context.Background(), in); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			request := waitNotification(t, requests)
			if request.Title != "project · earth:3" || request.Body != "Composed completion summary." {
				t.Errorf("notification = %+v, want composed body with cwd/locator title", request)
			}
			if request.Priority != "4" || request.Tags != "white_check_mark" {
				t.Errorf("headers = priority %q tags %q, want done urgency", request.Priority, request.Tags)
			}
			calls := composer.Calls()
			if len(calls) != 1 {
				t.Fatalf("Compose calls = %d, want exactly one", len(calls))
			}
			if calls[0].Input != (ComposeInput{User: "latest user text", Assistant: "latest assistant text"}) {
				t.Errorf("Compose input = %+v, want latest scanned conversation", calls[0].Input)
			}
			if calls[0].Label != (ComposeLabel{Current: "", Refresh: false}) {
				t.Errorf("Compose label = %+v, want naming disabled", calls[0].Label)
			}

			records := readDecisionLog(t, logPath)
			if len(records) != 1 {
				t.Fatalf("records = %+v, want one", records)
			}
			record := records[0]
			if record.Harness != harnessClaude || record.SessionID != "session-1" ||
				record.CompletionID != tt.wantID || record.CompositionOutcome != compositionComposed {
				t.Errorf("record attribution/composition = %+v", record)
			}
			if record.Urgency != UrgencyDone || record.Outcome != OutcomeSend.String() {
				t.Errorf("record decision = %+v, want send/done", record)
			}
		})
	}
}

func TestPipelineRunImplicitClaudeUsesTranscriptIdentity(t *testing.T) {
	for _, tt := range []struct {
		name         string
		completionID string
	}{
		{name: "absent supplied ID"},
		{name: "stale supplied ID", completionID: "stale-hook-id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			composer := &recordingComposer{result: ComposeResult{Body: "Implicit Claude summary."}}
			pipeline, logPath := testPipeline(t, server, composer)
			if err := pipeline.Run(context.Background(), HookInput{
				SessionID: "implicit-session", CompletionID: tt.completionID,
				CWD: "/work/implicit", HookEventName: eventStop,
				TranscriptPath:       conversationTranscript(t, "implicit-uuid", "message-id", false),
				LastAssistantMessage: "stale fallback",
			}); err != nil {
				t.Fatal(err)
			}
			if request := waitNotification(t, requests); request.Body != "Implicit Claude summary." {
				t.Fatalf("request = %+v", request)
			}
			calls := composer.Calls()
			if len(calls) != 1 || calls[0].Input != (ComposeInput{
				User: "latest user text", Assistant: "latest assistant text",
			}) {
				t.Fatalf("Compose calls = %+v", calls)
			}
			record := readDecisionLog(t, logPath)[0]
			if record.Harness != harnessClaude || record.CompletionID != "implicit-uuid" {
				t.Fatalf("record = %+v", record)
			}
		})
	}
}

func TestPipelineProviderTurnCompleteComposesOnceWithNativeInput(t *testing.T) {
	for _, harness := range []string{harnessPi} {
		t.Run(harness, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			composer := &recordingComposer{result: ComposeResult{Body: "Provider completion summary."}}
			pipeline, logPath := testPipeline(t, server, composer)
			in := HookInput{
				Harness: harness, SessionID: harness + "-session", CompletionID: harness + "-completion",
				CWD: "/work/provider-project", HookEventName: eventTurnComplete,
				Message: "provider user text", LastAssistantMessage: "provider assistant text",
			}
			if err := pipeline.Run(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			request := waitNotification(t, requests)
			if request.Priority != "4" || request.Body != "Provider completion summary." {
				t.Fatalf("notification = %+v, want composed done notification", request)
			}
			calls := composer.Calls()
			if len(calls) != 1 || calls[0].Input != (ComposeInput{
				User: "provider user text", Assistant: "provider assistant text",
			}) {
				t.Fatalf("Compose calls = %+v", calls)
			}
			record := readDecisionLog(t, logPath)[0]
			if record.Harness != harness || record.CompletionID != harness+"-completion" {
				t.Errorf("record = %+v, want original source and ID", record)
			}
		})
	}
}

func TestPipelineClaudeIdentityDegradationSkipsCompositionAndClearsStaleID(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "absent transcript"},
		{
			name: "unreadable transcript",
			path: filepath.Join(t.TempDir(), "missing.jsonl"),
		},
		{name: "empty transcript", path: writeTranscript(t)},
		{name: "malformed transcript", path: writeTranscript(t, "{")},
		{name: "assistant identity absent", path: conversationTranscript(t, "", "", false)},
		{
			name: "assistant identity unreliable",
			path: conversationTranscript(t, "assistant-uuid", "message-id", true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			pipeline, logPath := testPipeline(t, server, panicComposer{})
			in := HookInput{
				Harness: harnessClaude, SessionID: "degraded", CompletionID: "stale-supplied-id",
				CWD: "/home/user/project", HookEventName: eventStop,
				TranscriptPath: tt.path, LastAssistantMessage: "Hook fallback summary.",
			}
			if err := pipeline.Run(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			request := waitNotification(t, requests)
			if request.Body != "Hook fallback summary." || request.Priority != "4" {
				t.Errorf("fallback notification = %+v", request)
			}
			record := readDecisionLog(t, logPath)[0]
			if record.CompletionID != "" {
				t.Errorf("CompletionID = %q, want stale supplied ID cleared", record.CompletionID)
			}
			if record.CompositionOutcome != compositionFallback ||
				record.CompositionError != compositionErrorIdentityUnavailable {
				t.Errorf("composition fields = %+v, want identity-unavailable fallback", record)
			}
		})
	}
}

func TestPipelineClaudeStalePrefixFallsBackDespitePriorAssistantClaim(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	pipeline, logPath := testPipeline(t, server, panicComposer{})
	path := writeTranscript(
		t,
		`{"type":"user","message":{"role":"user","content":"old question"}}`,
		`{"type":"assistant","uuid":"old-assistant","message":{"id":"old-message","role":"assistant","content":[{"type":"text","text":"old answer"}]}}`,
		`{"type":"user","message":{"role":"user","content":"new question"}}`,
	)

	claims := newClaimStore(nil)
	oldKey := claimKey{
		Harness:      harnessClaude,
		SessionID:    "session",
		Kind:         eventKindCompletion,
		CompletionID: "old-assistant",
	}
	oldToken, admitted := claims.claim(oldKey)
	if !admitted {
		t.Fatal("prior assistant claim rejected")
	}
	claims.finish(oldKey, oldToken, true)

	prepared, err := PrepareEvent(HookInput{
		Harness: harnessClaude, SessionID: "session", CompletionID: "stale-hook-id",
		CWD: "/work/project", HookEventName: eventStop, TranscriptPath: path,
		LastAssistantMessage: "new answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	running := startTestDaemon(t, Daemon{Pipeline: pipeline, claims: claims})
	oldFrame := Frame{Event: prepared, Workspace: "earth:3"}
	oldFrame.Event.CompletionID = oldKey.CompletionID
	requireFrameAck(t, running.socket, oldFrame, ackStatusDuplicate)
	requireFrameAck(
		t,
		running.socket,
		Frame{Event: prepared, Workspace: "earth:3"},
		ackStatusAccepted,
	)
	if request := waitNotification(t, requests); request.Body != "new answer" {
		t.Fatalf("request = %+v, want current hook fallback", request)
	}
	record := waitForDecisionRecords(t, logPath, 1)[0]
	if prepared.CompletionID != "" {
		t.Fatalf("CompletionID = %q, want no stale identity", prepared.CompletionID)
	}
	if record.CompletionID != "" || record.CompositionOutcome != compositionFallback ||
		record.CompositionError != compositionErrorIdentityUnavailable {
		t.Fatalf("record = %+v, want unattributed identity-unavailable fallback", record)
	}
}

func TestPipelineGoalActiveSuppressesBeforeIdentityDegradation(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	pipeline, logPath := testPipeline(t, server, panicComposer{})
	if err := pipeline.Run(context.Background(), HookInput{
		Harness: harnessClaude, SessionID: "active-goal", CompletionID: "stale-id",
		CWD: "/home/user/project", HookEventName: eventStop,
		TranscriptPath:       filepath.Join("testdata", "goal_active_set.jsonl"),
		LastAssistantMessage: "must remain silent",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		t.Fatalf("sent = %+v, want structural silence", request)
	case <-time.After(100 * time.Millisecond):
	}
	record := readDecisionLog(t, logPath)[0]
	if record.Outcome != OutcomeSilent.String() || !strings.Contains(record.Reason, "goal active") {
		t.Fatalf("record = %+v, want goal-active silence", record)
	}
	if record.CompletionID != "" {
		t.Errorf("CompletionID = %q, want stale supplied ID cleared", record.CompletionID)
	}
}

func TestPipelineExplicitInputBypassesScanAndComposer(t *testing.T) {
	tests := []struct {
		notificationType string
		message          string
		fallback         string
	}{
		{notificationType: "permission_prompt", message: "allow command?", fallback: "needs permission"},
		{notificationType: "elicitation_dialog", message: "choose a region", fallback: "needs input"},
		{notificationType: "elicitation_url_dialog", message: "open the form", fallback: "needs input"},
		{notificationType: "agent_needs_input", message: "answer the worker", fallback: "needs input"},
		{notificationType: "permission_prompt", fallback: "needs permission"},
		{notificationType: "elicitation_url_dialog", fallback: "needs input"},
	}
	for _, tt := range tests {
		t.Run(tt.notificationType+tt.message, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			pipeline, _ := testPipeline(t, server, panicComposer{})
			if err := pipeline.Run(context.Background(), HookInput{
				Harness: harnessClaude, SessionID: "input", CWD: "/work/project",
				HookEventName: eventNotification, NotificationType: tt.notificationType,
				Message: tt.message, TranscriptPath: "/must/not/be/read",
			}); err != nil {
				t.Fatal(err)
			}
			request := waitNotification(t, requests)
			wantBody := tt.message
			if wantBody == "" {
				wantBody = tt.fallback
			}
			if request.Body != wantBody || request.Priority != "5" || request.Tags != "question" {
				t.Errorf("notification = %+v, want immediate blocked input", request)
			}
		})
	}
}

func TestPipelineExplicitInputRepeatsAreNeverQuieted(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	pipeline, _ := testPipeline(t, server, panicComposer{})
	input := HookInput{
		Harness: harnessClaude, SessionID: "repeat", CWD: "/work/project",
		HookEventName: eventNotification, NotificationType: "permission_prompt",
		Message: "allow command?",
	}
	for range 2 {
		if err := pipeline.Run(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if request := waitNotification(t, requests); request.Body != "allow command?" || request.Priority != "5" {
			t.Errorf("request = %+v, want unsuppressed blocked repeat", request)
		}
	}
}

func TestPipelineDeliveryFailureLogIsObservableAndSecretSafe(t *testing.T) {
	const rawDiagnostic = "RAW_TRANSPORT_DIAGNOSTIC"
	attempts := 0
	client := &http.Client{Transport: pipelineRoundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New(rawDiagnostic)
	})}
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	pipeline := Pipeline{
		Sender: Sender{
			URL:    "https://topic-user:topic-secret@notify.invalid/private-topic-token",
			Token:  "header-secret",
			Client: client,
		},
		Log: DecisionLog{Path: logPath}, Host: "testhost",
	}
	if err := pipeline.Run(context.Background(), HookInput{
		Harness: harnessClaude, SessionID: "send-failure", CWD: "/work/project",
		HookEventName: eventNotification, NotificationType: "permission_prompt",
		Message: "allow command?",
	}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("delivery attempts = %d, want Sender's initial attempt and retry", attempts)
	}
	records := readDecisionLog(t, logPath)
	if len(records) != 1 || !strings.Contains(records[0].Reason, "send failed") {
		t.Fatalf("records = %+v, want observable send-failure category", records)
	}
	wire, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"topic-user", "topic-secret", "private-topic-token", "header-secret", rawDiagnostic,
	} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("decision log leaked %q: %s", secret, wire)
		}
	}
}

func TestPipelineCompositionFailuresUseBoundedSafeFallback(t *testing.T) {
	tests := []struct {
		name      string
		composer  *recordingComposer
		wantError string
	}{
		{
			name:      "invalid request",
			composer:  &recordingComposer{err: errors.New("pi composer: invalid compose request")},
			wantError: compositionErrorInvalidRequest,
		},
		{
			name:      "helper unavailable",
			composer:  &recordingComposer{err: errors.New("pi composer: helper unavailable")},
			wantError: compositionErrorHelperUnavailable,
		},
		{
			name:      "helper execution failure",
			composer:  &recordingComposer{err: errors.New("pi composer: helper execution failed")},
			wantError: compositionErrorHelperExecution,
		},
		{
			name:      "helper timeout",
			composer:  &recordingComposer{err: errors.New("pi composer: helper timed out")},
			wantError: compositionErrorHelperTimeout,
		},
		{
			name:      "helper cancellation",
			composer:  &recordingComposer{err: errors.New("pi composer: helper canceled")},
			wantError: compositionErrorHelperCanceled,
		},
		{
			name:      "malformed protocol",
			composer:  &recordingComposer{err: errors.New("pi composer: invalid helper protocol")},
			wantError: compositionErrorInvalidProtocol,
		},
		{
			name:      "helper rejection",
			composer:  &recordingComposer{err: errors.New("pi composer: helper rejected request")},
			wantError: compositionErrorRejectedRequest,
		},
		{
			name:      "model or authentication unavailable",
			composer:  &recordingComposer{err: errors.New("pi composer: helper model unavailable")},
			wantError: compositionErrorModelUnavailable,
		},
		{
			name:      "generation failure",
			composer:  &recordingComposer{err: errors.New("pi composer: helper generation failed")},
			wantError: compositionErrorGenerationFailed,
		},
		{
			name:      "helper reported timeout",
			composer:  &recordingComposer{err: errors.New("pi composer: helper reported timeout")},
			wantError: compositionErrorReportedTimeout,
		},
		{
			name:      "helper invalid output",
			composer:  &recordingComposer{err: errors.New("pi composer: helper output invalid")},
			wantError: compositionErrorInvalidOutput,
		},
		{
			name:      "unknown error is categorized without leaking it",
			composer:  &recordingComposer{err: errors.New("SECRET raw subprocess stderr")},
			wantError: compositionErrorFailed,
		},
		{
			name:      "invalid injected result",
			composer:  &recordingComposer{result: ComposeResult{Body: "**markdown is invalid**"}},
			wantError: compositionErrorInvalidResult,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			pipeline, logPath := testPipeline(t, server, tt.composer)
			longFinal := strings.Repeat("# Earlier **detail** [link](https://invalid) ", 10) +
				"`meaningful` tail"
			if err := pipeline.Run(context.Background(), HookInput{
				Harness: harnessPi, SessionID: "failure", CompletionID: "turn-1",
				CWD: "/work/project", HookEventName: eventTurnComplete,
				Message: "user", LastAssistantMessage: longFinal,
			}); err != nil {
				t.Fatal(err)
			}
			request := waitNotification(t, requests)
			if len(request.Body) > maxNotificationFallbackBytes || !utf8.ValidString(request.Body) ||
				!strings.HasPrefix(request.Body, truncationEllipsis) ||
				!strings.HasSuffix(request.Body, "meaningful tail") || strings.Contains(request.Body, "`") {
				t.Errorf("fallback body = %q (%d bytes), want bounded plain-text tail", request.Body, len(request.Body))
			}
			if calls := tt.composer.Calls(); len(calls) != 1 {
				t.Errorf("Compose calls = %d, want one attempt with no retry", len(calls))
			}
			record := readDecisionLog(t, logPath)[0]
			if record.CompositionOutcome != compositionFallback || record.CompositionError != tt.wantError {
				t.Errorf("record = %+v, want categorized fallback", record)
			}
			wire, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "SECRET") {
				t.Fatalf("decision log leaked raw composer error: %s", wire)
			}
		})
	}
}

func TestPipelineInlineAndDryRunNeverCompose(t *testing.T) {
	t.Run("inline composer unavailable", func(t *testing.T) {
		server, requests := captureNotificationServer(t)
		defer server.Close()
		pipeline, logPath := testPipeline(t, server, nil)
		if err := pipeline.Run(context.Background(), HookInput{
			Harness: harnessPi, SessionID: "inline", CompletionID: "completion",
			CWD: "/work/project", HookEventName: eventTurnComplete,
			LastAssistantMessage: "Inline fallback.",
		}); err != nil {
			t.Fatal(err)
		}
		if request := waitNotification(t, requests); request.Body != "Inline fallback." {
			t.Errorf("request = %+v", request)
		}
		record := readDecisionLog(t, logPath)[0]
		if record.CompositionError != compositionErrorUnavailable {
			t.Errorf("record = %+v, want unavailable category", record)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		server, requests := captureNotificationServer(t)
		defer server.Close()
		pipeline, logPath := testPipeline(t, server, panicComposer{})
		pipeline.DryRun = true
		var stdout strings.Builder
		pipeline.Stdout = &stdout
		if err := pipeline.Run(context.Background(), HookInput{
			Harness: harnessPi, SessionID: "dry", CompletionID: "completion",
			CWD: "/work/project", HookEventName: eventTurnComplete,
			LastAssistantMessage: "Dry-run fallback.",
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), "Dry-run fallback.") || !strings.Contains(stdout.String(), "[done]") {
			t.Errorf("stdout = %q, want done fallback rehearsal", stdout.String())
		}
		select {
		case request := <-requests:
			t.Fatalf("dry run delivered %+v", request)
		default:
		}
		record := readDecisionLog(t, logPath)[0]
		if record.CompositionError != compositionErrorDryRun {
			t.Errorf("record = %+v, want dry-run category", record)
		}
	})
}

func TestPipelineComposedBodyRemainsBoundedAndUrgencyCannotChange(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &recordingComposer{result: ComposeResult{Body: strings.Repeat("完", 100)}}
	pipeline, _ := testPipeline(t, server, composer)
	if err := pipeline.Run(context.Background(), HookInput{
		Harness: harnessPi, SessionID: "bounded", CompletionID: "completion",
		CWD: "/work/project", HookEventName: eventTurnComplete,
	}); err != nil {
		t.Fatal(err)
	}
	request := waitNotification(t, requests)
	if len(request.Body) > maxNotificationBodyBytes || !utf8.ValidString(request.Body) ||
		!strings.HasSuffix(request.Body, truncationEllipsis) {
		t.Errorf("body = %q (%d bytes), want bounded valid UTF-8", request.Body, len(request.Body))
	}
	if request.Priority != "4" || request.Tags != "white_check_mark" {
		t.Errorf("headers = %+v, model must not alter done urgency", request)
	}
}

func TestPipelineRunPreparedUsesFrozenClaudeSnapshot(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &recordingComposer{result: ComposeResult{Body: "Frozen summary."}}
	pipeline, _ := testPipeline(t, server, composer)
	path := conversationTranscript(t, "frozen-uuid", "frozen-message", false)
	prepared, err := PrepareEvent(HookInput{
		Harness: harnessClaude, SessionID: "frozen", HookEventName: eventStop,
		TranscriptPath: path, LastAssistantMessage: "stale hook fallback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = pipeline.RunPrepared(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	calls := composer.Calls()
	wantInput := ComposeInput{User: "latest user text", Assistant: "latest assistant text"}
	if len(calls) != 1 || calls[0].Input != wantInput {
		t.Fatalf("Compose calls after transcript removal = %+v", calls)
	}
	if request := waitNotification(t, requests); request.Body != "Frozen summary." {
		t.Fatalf("request = %+v", request)
	}
}

func TestPipelineRunPreparedClaudeFallbackUsesCapturedAssistant(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	pipeline, _ := testPipeline(t, server, nil)
	path := conversationTranscript(t, "frozen-uuid", "frozen-message", false)
	prepared, err := PrepareEvent(HookInput{
		Harness: harnessClaude, SessionID: "frozen", HookEventName: eventStop,
		TranscriptPath: path, LastAssistantMessage: "stale hook fallback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = pipeline.RunPrepared(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if request := waitNotification(t, requests); request.Body != "latest assistant text" {
		t.Fatalf("fallback = %q, want captured assistant", request.Body)
	}
}

func TestPipelineRunPreparedIncompleteIdentityPairSkipsCompositionOnEveryRepeat(t *testing.T) {
	for _, tt := range []struct {
		name         string
		sessionID    string
		completionID string
	}{
		{name: "session only", sessionID: "session"},
		{name: "completion only", completionID: "completion"},
		{name: "both empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			pipeline, logPath := testPipeline(t, server, panicComposer{})
			prepared := PreparedEvent{
				Version: 1, Harness: harnessPi, SessionID: tt.sessionID,
				Kind: eventKindCompletion, SourceEvent: eventTurnComplete,
				CompletionID: tt.completionID,
				Assistant:    strings.Repeat("bounded fallback words ", 20),
			}
			for range 2 {
				if err := pipeline.RunPrepared(context.Background(), prepared); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				request := waitNotification(t, requests)
				if len(request.Body) > maxNotificationFallbackBytes || request.Body == "" {
					t.Fatalf("fallback body = %q (%d bytes)", request.Body, len(request.Body))
				}
			}
			records := readDecisionLog(t, logPath)
			if len(records) != 2 {
				t.Fatalf("records = %+v", records)
			}
			for _, record := range records {
				if record.CompositionOutcome != compositionFallback ||
					record.CompositionError != compositionErrorIdentityUnavailable {
					t.Fatalf("record = %+v", record)
				}
			}
		})
	}
}

func TestPipelineRunPreparedRejectsInvalidEvent(t *testing.T) {
	err := (Pipeline{}).RunPrepared(context.Background(), PreparedEvent{})
	if err == nil || err.Error() != "notify: invalid prepared event" {
		t.Fatalf("RunPrepared() error = %v", err)
	}
}

func TestPipelineSessionEndAndSilentEventsNeverComposeOrSend(t *testing.T) {
	tests := []HookInput{
		{Harness: harnessClaude, SessionID: "end", HookEventName: eventSessionEnd},
		{Harness: harnessClaude, SessionID: "idle", HookEventName: eventNotification, NotificationType: "idle_prompt"},
		{
			Harness: harnessClaude, SessionID: "completed",
			HookEventName: eventNotification, NotificationType: "agent_completed",
		},
		{
			Harness: harnessPi, SessionID: "child", HookEventName: eventTurnComplete,
			CompletionID: "id", AgentType: "worker",
		},
	}
	for _, in := range tests {
		t.Run(in.SessionID, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			pipeline, logPath := testPipeline(t, server, panicComposer{})
			if err := pipeline.Run(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			select {
			case request := <-requests:
				t.Fatalf("silent event delivered %+v", request)
			case <-time.After(50 * time.Millisecond):
			}
			if record := readDecisionLog(t, logPath)[0]; record.Outcome != OutcomeSilent.String() {
				t.Errorf("record = %+v, want silent", record)
			}
		})
	}
}

type sequencedComposer struct {
	mutex   sync.Mutex
	calls   []composeCall
	results []ComposeResult
	errors  []error
}

func (composer *sequencedComposer) Compose(
	_ context.Context,
	input ComposeInput,
	label ComposeLabel,
) (ComposeResult, error) {
	composer.mutex.Lock()
	defer composer.mutex.Unlock()
	index := len(composer.calls)
	composer.calls = append(composer.calls, composeCall{Input: input, Label: label})
	var result ComposeResult
	if index < len(composer.results) {
		result = composer.results[index]
	}
	var err error
	if index < len(composer.errors) {
		err = composer.errors[index]
	}
	return result, err
}

func (composer *sequencedComposer) Calls() []composeCall {
	composer.mutex.Lock()
	defer composer.mutex.Unlock()
	return append([]composeCall(nil), composer.calls...)
}

func preparedCompletion(harness, session, completion, cwd, user, assistant string) PreparedEvent {
	sourceEvent := eventTurnComplete
	if harness == harnessClaude {
		sourceEvent = eventStop
	}
	return PreparedEvent{
		Version: preparedEventVersion, Harness: harness, SessionID: session,
		Kind: eventKindCompletion, SourceEvent: sourceEvent, CWD: cwd,
		CompletionID: completion, User: user, Assistant: assistant,
	}
}

func TestPipelinePeriodicLabelsUseSameCompositionAtFirstFifthNinth(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	results := make([]ComposeResult, 9)
	for index := range results {
		results[index].Body = "summary"
	}
	results[0].Label = "Initial Shared Label"
	// The fifth response intentionally omits a label: with a prior label this
	// is a successful KEEP and must advance the cadence without deleting it.
	results[8].Label = "Updated Shared Label"
	composer := &sequencedComposer{results: results}
	stateBase := t.TempDir()
	pipeline, _ := testPipeline(t, server, composer)
	pipeline.LabelStore = NewLabelStore(stateBase)

	for exchange := 1; exchange <= 9; exchange++ {
		user, assistant := fmt.Sprintf("user-%d", exchange), fmt.Sprintf("assistant-%d", exchange)
		if exchange == 1 {
			user, assistant = "", ""
		}
		event := preparedCompletion(
			harnessPi, "cadence-session", fmt.Sprintf("completion-%d", exchange),
			"/work/project", user, assistant,
		)
		if err := pipeline.RunPrepared(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		request := waitNotification(t, requests)
		wantTitle := "Initial Shared Label · earth:3"
		if exchange == 9 {
			wantTitle = "Updated Shared Label · earth:3"
		}
		if request.Title != wantTitle || request.Body != "summary" || request.Priority != "4" {
			t.Errorf("exchange %d notification = %+v, want title %q", exchange, request, wantTitle)
		}
	}

	calls := composer.Calls()
	if len(calls) != 9 {
		t.Fatalf("Compose calls = %d, want exactly one per exchange", len(calls))
	}
	for index, call := range calls {
		exchange := index + 1
		wantRefresh := exchange == 1 || exchange == 5 || exchange == 9
		wantCurrent := ""
		if exchange > 1 {
			wantCurrent = "Initial Shared Label"
		}
		if call.Label != (ComposeLabel{Current: wantCurrent, Refresh: wantRefresh}) {
			t.Errorf(
				"exchange %d label request = %+v, want current=%q refresh=%v",
				exchange,
				call.Label,
				wantCurrent,
				wantRefresh,
			)
		}
	}

	_, data := onlyLabelSnapshot(t, stateBase)
	snapshot := decodeLabelSnapshotForTest(t, data)
	if snapshot["label"] != "Updated Shared Label" || snapshot["source_generation"] != float64(9) ||
		snapshot["exchange_count"] != float64(9) ||
		snapshot["last_successful_refresh_exchange"] != float64(9) ||
		snapshot["latest_completion_id"] != "completion-9" {
		t.Fatalf("final cadence snapshot = %+v", snapshot)
	}
}

func TestPipelineLabelFailuresRetryOnlyChangedDistinctMaterialAndKEEP(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	results := make([]ComposeResult, 9)
	for index := range results {
		results[index].Body = "summary"
	}
	// Exchange 1 omits the required initial label. Exchange 3 retries changed
	// material successfully. Exchange 7 returns an invalid refresh label;
	// exchange 9 retries changed material and omits the label as KEEP.
	results[2].Label = "Recovered Shared Label"
	results[6].Label = "invalid"
	composer := &sequencedComposer{results: results}
	stateBase := t.TempDir()
	pipeline, _ := testPipeline(t, server, composer)
	pipeline.LabelStore = NewLabelStore(stateBase)

	materials := []string{
		"same initial material", "same initial material", "changed initial retry",
		"four", "five", "six", "failed refresh material", "failed refresh material", "changed refresh retry",
	}
	for index, material := range materials {
		event := preparedCompletion(
			harnessPi, "retry-session", fmt.Sprintf("id-%d", index+1), "/work/fallback-project",
			"user", material,
		)
		if err := pipeline.RunPrepared(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		request := waitNotification(t, requests)
		wantTitle := "fallback-project · earth:3"
		if index >= 2 {
			wantTitle = "Recovered Shared Label · earth:3"
		}
		wantBody := "summary"
		if index == 0 || index == 6 {
			wantBody = material
		}
		if request.Title != wantTitle || request.Body != wantBody {
			t.Errorf(
				"exchange %d notification = %+v, want title=%q body=%q",
				index+1, request, wantTitle, wantBody,
			)
		}
	}

	wantRefresh := []bool{true, false, true, false, false, false, true, false, true}
	calls := composer.Calls()
	if len(calls) != len(wantRefresh) {
		t.Fatalf("calls = %d, want %d", len(calls), len(wantRefresh))
	}
	for index, call := range calls {
		if call.Label.Refresh != wantRefresh[index] {
			t.Errorf("exchange %d refresh = %v, want %v", index+1, call.Label.Refresh, wantRefresh[index])
		}
		wantCurrent := ""
		if index >= 3 {
			wantCurrent = "Recovered Shared Label"
		}
		if call.Label.Current != wantCurrent {
			t.Errorf("exchange %d current = %q, want %q", index+1, call.Label.Current, wantCurrent)
		}
	}

	_, data := onlyLabelSnapshot(t, stateBase)
	snapshot := decodeLabelSnapshotForTest(t, data)
	if snapshot["label"] != "Recovered Shared Label" ||
		snapshot["last_successful_refresh_exchange"] != float64(9) ||
		snapshot["exchange_count"] != float64(9) {
		t.Fatalf("retry snapshot = %+v", snapshot)
	}
}

func TestPipelineSameSourceRetryDoesNotDoubleCountOrRetryNaming(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &sequencedComposer{results: []ComposeResult{{Body: "first"}, {Body: "source retry"}}}
	stateBase := t.TempDir()
	pipeline, _ := testPipeline(t, server, composer)
	pipeline.LabelStore = NewLabelStore(stateBase)
	event := preparedCompletion(
		harnessPi, "same-source", "same-id", "/work/project", "user", "assistant",
	)
	for range 2 {
		if err := pipeline.RunPrepared(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		_ = waitNotification(t, requests)
	}
	calls := composer.Calls()
	if len(calls) != 2 || !calls[0].Label.Refresh || calls[1].Label.Refresh {
		t.Fatalf("same-source calls = %+v, want one initial naming attempt only", calls)
	}
	_, data := onlyLabelSnapshot(t, stateBase)
	snapshot := decodeLabelSnapshotForTest(t, data)
	if snapshot["exchange_count"] != float64(1) || snapshot["source_generation"] != float64(1) ||
		snapshot["latest_completion_id"] != "same-id" {
		t.Fatalf("same-source snapshot = %+v", snapshot)
	}
}

func TestPipelineKnownLabelTitlesInputAndIgnoresUnrequestedLabel(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &sequencedComposer{results: []ComposeResult{
		{Body: "first", Label: "Known Shared Label"},
		{Body: "second", Label: "Unrequested New Label"},
	}}
	stateBase := t.TempDir()
	pipeline, logPath := testPipeline(t, server, composer)
	pipeline.LabelStore = NewLabelStore(stateBase)
	completionEvents := []PreparedEvent{
		preparedCompletion(harnessPi, "known", "id-1", "/work/project", "u1", "a1"),
		preparedCompletion(harnessPi, "known", "id-2", "/work/project", "u2", "a2"),
	}
	for index, event := range completionEvents {
		if err := pipeline.RunPrepared(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		request := waitNotification(t, requests)
		wantBody := "first"
		if index == 1 {
			wantBody = "a2"
		}
		if request.Title != "Known Shared Label · earth:3" || request.Body != wantBody {
			t.Fatalf("completion notification = %+v, want body %q", request, wantBody)
		}
	}
	// Inputs are currently Claude-native, so seed the same label under the
	// Claude scope and prove the local lookup neither composes nor advances it.
	seed, err := pipeline.LabelStore.planCompletion(preparedCompletion(
		harnessClaude, "known", "seed-id", "/work/project", "seed", "seed",
	))
	if err != nil {
		t.Fatal(err)
	}
	if err = pipeline.LabelStore.finishCompletion(seed, "Known Input Label"); err != nil {
		t.Fatal(err)
	}
	claudeSnapshotPath := filepath.Join(
		stateBase,
		labelStateDirectoryName,
		labelSnapshotName(harnessClaude, "known"),
	)
	claudeSnapshotBefore, err := os.ReadFile(claudeSnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	input := PreparedEvent{
		Version: preparedEventVersion, Harness: harnessClaude, SessionID: "known",
		Kind: eventKindInput, SourceEvent: eventNotification, CWD: "/work/project",
		NotificationType: "permission_prompt", Message: "allow?",
	}
	if err = pipeline.RunPrepared(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if request := waitNotification(
		t,
		requests,
	); request.Title != "Known Input Label · earth:3" ||
		request.Body != "allow?" {
		t.Fatalf("input notification = %+v", request)
	}
	claudeSnapshotAfter, err := os.ReadFile(claudeSnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claudeSnapshotAfter, claudeSnapshotBefore) {
		t.Fatalf(
			"explicit input mutated its Claude label snapshot: before=%s after=%s",
			claudeSnapshotBefore,
			claudeSnapshotAfter,
		)
	}
	calls := composer.Calls()
	if len(calls) != 2 || calls[1].Label.Refresh || calls[1].Label.Current != "Known Shared Label" {
		t.Fatalf("completion calls = %+v", calls)
	}
	label, err := pipeline.LabelStore.lookupLabel(harnessPi, "known")
	if err != nil || label != "Known Shared Label" {
		t.Fatalf("unrequested result overwrote label: %q/%v", label, err)
	}
	records := readDecisionLog(t, logPath)
	if len(records) != 3 || records[1].CompositionError != compositionErrorInvalidResult {
		t.Fatalf("unrequested label was not rejected: %+v", records)
	}
}

type selectiveBlockingComposer struct {
	entered chan ComposeInput
	release <-chan struct{}
}

func (composer selectiveBlockingComposer) Compose(
	ctx context.Context,
	input ComposeInput,
	label ComposeLabel,
) (ComposeResult, error) {
	if input.User == "slow" {
		composer.entered <- input
		select {
		case <-composer.release:
			result := ComposeResult{Body: "slow body"}
			if label.Refresh {
				result.Label = "Slow Session Label"
			}
			return result, nil
		case <-ctx.Done():
			return ComposeResult{}, errorsForCanceledComposer()
		}
	}
	return ComposeResult{Body: "fast body", Label: "Fast Session Label"}, nil
}

func TestPipelineBlockedCompositionDoesNotBlockSameSessionInputOrOtherSession(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	entered := make(chan ComposeInput, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseModel := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseModel)
	stateBase := t.TempDir()
	store := NewLabelStore(stateBase)
	seed, err := store.planCompletion(preparedCompletion(
		harnessClaude, "shared-session", "seed-id", "/work/input", "seed", "seed",
	))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.finishCompletion(seed, "Immediate Input Label"); err != nil {
		t.Fatal(err)
	}
	pipeline, _ := testPipeline(t, server, selectiveBlockingComposer{entered: entered, release: release})
	pipeline.LabelStore = store

	slowContext, cancelSlow := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancelSlow)
	slowDone := make(chan error, 1)
	go func() {
		slowDone <- pipeline.RunPrepared(slowContext, preparedCompletion(
			harnessClaude, "shared-session", "slow-id", "/work/slow", "slow", "assistant",
		))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("slow composition did not block")
	}
	sharedSnapshotPath := filepath.Join(
		stateBase,
		labelStateDirectoryName,
		labelSnapshotName(harnessClaude, "shared-session"),
	)
	sharedSnapshotBeforeInput, err := os.ReadFile(sharedSnapshotPath)
	if err != nil {
		t.Fatal(err)
	}

	inputDone := make(chan error, 1)
	go func() {
		inputDone <- pipeline.RunPrepared(context.Background(), PreparedEvent{
			Version: preparedEventVersion, Harness: harnessClaude, SessionID: "shared-session",
			Kind: eventKindInput, SourceEvent: eventNotification, CWD: "/work/input",
			NotificationType: "permission_prompt", Message: "allow immediately?",
		})
	}()
	select {
	case err = <-inputDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("same-session input waited for blocked composition")
	}
	if request := waitNotification(
		t,
		requests,
	); request.Title != "Immediate Input Label · earth:3" ||
		request.Body != "allow immediately?" {
		t.Fatalf("input notification = %+v", request)
	}
	sharedSnapshotAfterInput, err := os.ReadFile(sharedSnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sharedSnapshotAfterInput, sharedSnapshotBeforeInput) {
		t.Fatalf(
			"same-session input mutated blocked completion metadata: before=%s after=%s",
			sharedSnapshotBeforeInput,
			sharedSnapshotAfterInput,
		)
	}
	select {
	case err = <-slowDone:
		t.Fatalf("slow composition completed before release: %v", err)
	default:
	}

	fastDone := make(chan error, 1)
	go func() {
		fastDone <- pipeline.RunPrepared(context.Background(), preparedCompletion(
			harnessPi, "fast-session", "fast-id", "/work/fast", "fast", "assistant",
		))
	}()
	select {
	case err = <-fastDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("other-session completion waited for blocked composition")
	}
	if request := waitNotification(
		t,
		requests,
	); request.Title != "Fast Session Label · earth:3" ||
		request.Body != "fast body" {
		t.Fatalf("fast notification = %+v", request)
	}

	releaseModel()
	select {
	case err = <-slowDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow composition did not finish after release")
	}
	if request := waitNotification(
		t,
		requests,
	); request.Title != "Immediate Input Label · earth:3" ||
		request.Body != "slow body" {
		t.Fatalf("slow notification = %+v", request)
	}
}

type sourceGuardComposer struct {
	entered  chan ComposeInput
	releases map[ComposeInput]<-chan struct{}
	results  map[ComposeInput]ComposeResult
	errors   map[ComposeInput]error
}

func (composer sourceGuardComposer) Compose(
	ctx context.Context,
	input ComposeInput,
	_ ComposeLabel,
) (ComposeResult, error) {
	composer.entered <- input
	select {
	case <-composer.releases[input]:
		return composer.results[input], composer.errors[input]
	case <-ctx.Done():
		return ComposeResult{}, errorsForCanceledComposer()
	}
}

func TestPipelineOlderSameSessionResultCannotOverwriteNewerFailedSource(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	oldInput := ComposeInput{User: "old user", Assistant: "old assistant"}
	newInput := ComposeInput{User: "new user", Assistant: "new assistant"}
	oldRelease := make(chan struct{})
	newRelease := make(chan struct{})
	composer := sourceGuardComposer{
		entered: make(chan ComposeInput, 2),
		releases: map[ComposeInput]<-chan struct{}{
			oldInput: oldRelease,
			newInput: newRelease,
		},
		results: map[ComposeInput]ComposeResult{
			oldInput: {Body: "old composed body", Label: "Older Result Label"},
		},
		errors: map[ComposeInput]error{
			newInput: errors.New("pi composer: helper generation failed"),
		},
	}
	stateBase := t.TempDir()
	base, _ := testPipeline(t, server, composer)
	base.LabelStore = NewLabelStore(stateBase)
	oldPipeline := base
	oldPipeline.Workspace = "old:1"
	newPipeline := base
	newPipeline.Workspace = "new:2"
	oldEvent := preparedCompletion(
		harnessPi,
		"race-session",
		"old-id",
		"/work/old-project",
		oldInput.User,
		oldInput.Assistant,
	)
	newEvent := preparedCompletion(
		harnessPi,
		"race-session",
		"new-id",
		"/work/new-project",
		newInput.User,
		newInput.Assistant,
	)

	oldDone := make(chan error, 1)
	newDone := make(chan error, 1)
	go func() { oldDone <- oldPipeline.RunPrepared(context.Background(), oldEvent) }()
	if input := <-composer.entered; input != oldInput {
		t.Fatalf("first entered = %+v", input)
	}
	go func() { newDone <- newPipeline.RunPrepared(context.Background(), newEvent) }()
	if input := <-composer.entered; input != newInput {
		t.Fatalf("second entered = %+v", input)
	}

	close(newRelease)
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}
	if request := waitNotification(t, requests); request != (capturedNotification{
		Title: "new-project · new:2", Body: "new assistant", Priority: "4", Tags: "white_check_mark",
	}) {
		t.Fatalf("new notification = %+v", request)
	}
	close(oldRelease)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if request := waitNotification(t, requests); request != (capturedNotification{
		Title: "Older Result Label · old:1", Body: "old composed body", Priority: "4", Tags: "white_check_mark",
	}) {
		t.Fatalf("old notification = %+v", request)
	}

	_, data := onlyLabelSnapshot(t, stateBase)
	snapshot := decodeLabelSnapshotForTest(t, data)
	if snapshot["latest_completion_id"] != "new-id" || snapshot["source_generation"] != float64(2) ||
		snapshot["exchange_count"] != float64(2) || snapshot["label"] != "" ||
		snapshot["last_successful_refresh_exchange"] != float64(0) {
		t.Fatalf("late older result mutated newer source: %+v", snapshot)
	}
}

func TestPipelineLabelStateFailureIsSafeObservableAndCompositionContinues(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	stateBase := t.TempDir()
	const corruptSecret = "SECRET_CORRUPT_LABEL"
	writeRawLabelSnapshot(t, stateBase, "corrupt", corruptSecret, labelFileMode)
	composer := &recordingComposer{result: ComposeResult{Body: "safe composed body"}}
	pipeline, logPath := testPipeline(t, server, composer)
	pipeline.LabelStore = NewLabelStore(stateBase)
	if err := pipeline.RunPrepared(context.Background(), preparedCompletion(
		harnessPi, "corrupt", "id", "/work/fallback", "user", "assistant",
	)); err != nil {
		t.Fatal(err)
	}
	if request := waitNotification(
		t,
		requests,
	); request.Title != "fallback · earth:3" ||
		request.Body != "safe composed body" {
		t.Fatalf("notification = %+v", request)
	}
	calls := composer.Calls()
	if len(calls) != 1 || calls[0].Label != (ComposeLabel{}) {
		t.Fatalf("compose calls = %+v, want body-only continuation", calls)
	}
	record := readDecisionLog(t, logPath)[0]
	if record.CompositionOutcome != compositionComposed ||
		record.CompositionError != compositionErrorLabelsUnavailable ||
		!strings.Contains(record.Reason, compositionErrorLabelsUnavailable) {
		t.Fatalf("decision record = %+v", record)
	}
	wire, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), corruptSecret) {
		t.Fatalf("decision log leaked corrupt label state: %s", wire)
	}
}

func TestPipelineIneligiblePathsNeverWriteLabelStateOrInvokeNaming(t *testing.T) {
	tests := []struct {
		name   string
		event  PreparedEvent
		dryRun bool
	}{
		{
			name:  "no completion ID",
			event: preparedCompletion(harnessPi, "no-id", "", "/work/project", "user", "assistant"),
		},
		{
			name:   "dry run",
			event:  preparedCompletion(harnessPi, "dry", "id", "/work/project", "user", "assistant"),
			dryRun: true,
		},
		{
			name: "child",
			event: func() PreparedEvent {
				event := preparedCompletion(harnessPi, "child", "id", "/work/project", "user", "assistant")
				event.AgentType = "worker"
				return event
			}(),
		},
		{
			name: "active goal",
			event: func() PreparedEvent {
				event := preparedCompletion(harnessClaude, "goal", "id", "/work/project", "user", "assistant")
				event.GoalActive = true
				return event
			}(),
		},
		{
			name: "session end",
			event: PreparedEvent{
				Version: preparedEventVersion, Harness: harnessClaude, SessionID: "cleanup",
				Kind: eventKindCleanup, SourceEvent: eventSessionEnd,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := captureNotificationServer(t)
			defer server.Close()
			stateBase := t.TempDir()
			pipeline, _ := testPipeline(t, server, panicComposer{})
			pipeline.LabelStore = NewLabelStore(stateBase)
			pipeline.DryRun = tt.dryRun
			pipeline.Stdout = io.Discard
			if err := pipeline.RunPrepared(context.Background(), tt.event); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(stateBase, labelStateDirectoryName)); !os.IsNotExist(err) {
				t.Fatalf("label state created: %v", err)
			}
			if tt.name == "no completion ID" {
				_ = waitNotification(t, requests)
			} else {
				select {
				case request := <-requests:
					t.Fatalf("unexpected delivery: %+v", request)
				default:
				}
			}
		})
	}
}

type permissionBreakingComposer struct {
	directory string
}

func (composer permissionBreakingComposer) Compose(
	context.Context,
	ComposeInput,
	ComposeLabel,
) (ComposeResult, error) {
	if err := os.Chmod(composer.directory, 0o500); err != nil {
		return ComposeResult{}, err
	}
	return ComposeResult{Body: "composed despite persistence failure", Label: "Unpublished Result Label"}, nil
}

func TestPipelineInitialLabelPublishFailureKeepsCWDNotificationAndReportsSafely(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	stateBase := t.TempDir()
	labelDirectory := filepath.Join(stateBase, labelStateDirectoryName)
	t.Cleanup(func() { _ = os.Chmod(labelDirectory, labelDirectoryMode) })
	pipeline, logPath := testPipeline(t, server, permissionBreakingComposer{directory: labelDirectory})
	pipeline.LabelStore = NewLabelStore(stateBase)
	if err := pipeline.RunPrepared(context.Background(), preparedCompletion(
		harnessPi, "publish-failure", "id", "/work/original-project", "user", "assistant",
	)); err != nil {
		t.Fatal(err)
	}
	if request := waitNotification(t, requests); request != (capturedNotification{
		Title: "original-project · earth:3", Body: "composed despite persistence failure",
		Priority: "4", Tags: "white_check_mark",
	}) {
		t.Fatalf("notification = %+v", request)
	}
	record := readDecisionLog(t, logPath)[0]
	if record.CompositionOutcome != compositionComposed ||
		record.CompositionError != compositionErrorLabelsUnavailable ||
		!strings.Contains(record.Reason, compositionErrorLabelsUnavailable) {
		t.Fatalf("decision record = %+v", record)
	}
	if err := os.Chmod(labelDirectory, labelDirectoryMode); err != nil {
		t.Fatal(err)
	}
	_, data := onlyLabelSnapshot(t, stateBase)
	snapshot := decodeLabelSnapshotForTest(t, data)
	if snapshot["label"] != "" || snapshot["last_successful_refresh_exchange"] != float64(0) ||
		snapshot["exchange_count"] != float64(1) {
		t.Fatalf("failed initial publish mutated label/cadence metadata: %+v", snapshot)
	}
}

func TestPipelineRefreshLabelPublishFailureKeepsPriorLabelAndCadence(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	stateBase := t.TempDir()
	store := NewLabelStore(stateBase)
	seed, err := store.planCompletion(preparedCompletion(
		harnessPi, "refresh-publish-failure", "id-1", "/work/project", "user-1", "assistant-1",
	))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.finishCompletion(seed, "Prior Shared Label"); err != nil {
		t.Fatal(err)
	}
	labelDirectory := filepath.Join(stateBase, labelStateDirectoryName)
	t.Cleanup(func() { _ = os.Chmod(labelDirectory, labelDirectoryMode) })
	composer := &sequencedComposer{results: []ComposeResult{
		{Body: "body-2"},
		{Body: "body-3"},
		{Body: "body-4"},
		{Body: "composed refresh body", Label: "Unpublished Refreshed Label"},
	}}
	pipeline, logPath := testPipeline(t, server, composer)
	pipeline.LabelStore = store
	for exchange := 2; exchange <= 4; exchange++ {
		if err = pipeline.RunPrepared(context.Background(), preparedCompletion(
			harnessPi, "refresh-publish-failure", fmt.Sprintf("id-%d", exchange),
			"/work/project", fmt.Sprintf("user-%d", exchange), fmt.Sprintf("assistant-%d", exchange),
		)); err != nil {
			t.Fatal(err)
		}
		_ = waitNotification(t, requests)
	}

	pipeline.Composer = permissionBreakingComposer{directory: labelDirectory}
	if err = pipeline.RunPrepared(context.Background(), preparedCompletion(
		harnessPi, "refresh-publish-failure", "id-5", "/work/project", "user-5", "assistant-5",
	)); err != nil {
		t.Fatal(err)
	}
	if request := waitNotification(t, requests); request != (capturedNotification{
		Title: "Prior Shared Label · earth:3", Body: "composed despite persistence failure",
		Priority: "4", Tags: "white_check_mark",
	}) {
		t.Fatalf("refresh notification = %+v", request)
	}
	record := readDecisionLog(t, logPath)[3]
	if record.CompositionOutcome != compositionComposed ||
		record.CompositionError != compositionErrorLabelsUnavailable ||
		!strings.Contains(record.Reason, compositionErrorLabelsUnavailable) {
		t.Fatalf("refresh decision record = %+v", record)
	}
	if err = os.Chmod(labelDirectory, labelDirectoryMode); err != nil {
		t.Fatal(err)
	}
	_, data := onlyLabelSnapshot(t, stateBase)
	snapshot := decodeLabelSnapshotForTest(t, data)
	if snapshot["label"] != "Prior Shared Label" ||
		snapshot["last_successful_refresh_exchange"] != float64(1) ||
		snapshot["exchange_count"] != float64(5) {
		t.Fatalf("failed refresh mutated prior label/cadence metadata: %+v", snapshot)
	}
}

func TestPipelineLabelInitializationFailureDoesNotDisableComposition(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	stateBase := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(stateBase, []byte("SECRET_STATE_BASE"), labelFileMode); err != nil {
		t.Fatal(err)
	}
	composer := &recordingComposer{result: ComposeResult{Body: "body composition continues"}}
	pipeline, logPath := testPipeline(t, server, composer)
	pipeline.LabelStore = NewLabelStore(stateBase)
	if err := pipeline.RunPrepared(context.Background(), preparedCompletion(
		harnessPi, "initialization-failure", "id", "/work/fallback", "user", "assistant",
	)); err != nil {
		t.Fatal(err)
	}
	if request := waitNotification(t, requests); request.Title != "fallback · earth:3" ||
		request.Body != "body composition continues" {
		t.Fatalf("notification = %+v", request)
	}
	calls := composer.Calls()
	if len(calls) != 1 || calls[0].Label != (ComposeLabel{}) {
		t.Fatalf("composition calls = %+v", calls)
	}
	record := readDecisionLog(t, logPath)[0]
	if record.CompositionOutcome != compositionComposed ||
		record.CompositionError != compositionErrorLabelsUnavailable ||
		!strings.Contains(record.Reason, compositionErrorLabelsUnavailable) {
		t.Fatalf("decision record = %+v", record)
	}
	wire, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "SECRET_STATE_BASE") {
		t.Fatalf("decision log leaked state error contents: %s", wire)
	}
}
