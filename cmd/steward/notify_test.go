package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshsymonds/steward/internal/notify"
)

func testNotifyClientConfig(t *testing.T, socketPath string) notifyClientConfig {
	t.Helper()
	return notifyClientConfig{
		Log:         notify.DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")},
		SockPath:    socketPath,
		DialTimeout: 50 * time.Millisecond,
	}
}

func canonicalPiPayload(assistant string) string {
	wire, _ := json.Marshal(map[string]any{
		"schema_version": 1, "harness": "pi", "session_id": "session-1",
		"completion_id": "completion-1", "cwd": "/work/project",
		"hook_event_name": "TurnComplete", "message": "user", "last_assistant_message": assistant,
	})
	return string(wire)
}

func startAckSocket(t *testing.T, handler func(net.Conn, notify.Frame)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notifyd.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		frame, decodeErr := notify.DecodeFrame(connection)
		if decodeErr == nil {
			handler(connection, frame)
		}
	}()
	return path
}

func fallbackServer(t *testing.T) (notify.Sender, *atomic.Int32, <-chan string) {
	t.Helper()
	calls := &atomic.Int32{}
	bodies := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		calls.Add(1)
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return notify.Sender{URL: server.URL, Client: server.Client()}, calls, bodies
}

func TestDispatchNotifyAcceptedAndDuplicateAcksSkipInline(t *testing.T) {
	for _, status := range []string{"accepted", "duplicate"} {
		t.Run(status, func(t *testing.T) {
			frames := make(chan notify.Frame, 1)
			socket := startAckSocket(t, func(connection net.Conn, frame notify.Frame) {
				frames <- frame
				_ = notify.EncodeAck(connection, notify.Ack{Version: 1, Status: status})
			})
			sender, calls, _ := fallbackServer(t)
			cfg := testNotifyClientConfig(t, socket)
			cfg.Sender = sender
			cfg.Environ = []string{"TMUX_PANE=", "SECRET_DO_NOT_SEND=value"}
			var stdout, stderr bytes.Buffer
			dispatchNotify(
				context.Background(), cfg,
				strings.NewReader(canonicalPiPayload("must not inline")), &stdout, &stderr,
			)
			frame := <-frames
			if frame.Event.Harness != "pi" || frame.Event.Kind != "completion" ||
				frame.Event.CompletionID != "completion-1" || frame.Event.Assistant != "must not inline" {
				t.Fatalf("frame = %+v", frame)
			}
			wire, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"SECRET", "environ", "transcript_path", "hook_input"} {
				if strings.Contains(string(wire), secret) {
					t.Fatalf("frame leaked %q: %s", secret, wire)
				}
			}
			if calls.Load() != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("inline/stdout/stderr = %d/%q/%q", calls.Load(), stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunNotifyCommandRejectsPositionalPayload(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runNotifyCommandWithIO(
		[]string{"--dry-run", canonicalPiPayload("ignored")},
		strings.NewReader(canonicalPiPayload("stdin")), &stdout, &stderr,
	)
	if exitCode != 0 || stdout.Len() != 0 ||
		!strings.Contains(stderr.String(), "does not accept positional arguments") {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestDispatchNotifyAllAmbiguousAcksRunExactlyOneInlineFallback(t *testing.T) {
	cases := []struct {
		name    string
		handler func(net.Conn, notify.Frame)
	}{
		{name: "rejected", handler: func(connection net.Conn, _ notify.Frame) {
			_ = notify.EncodeAck(connection, notify.Ack{Version: 1, Status: "rejected"})
		}},
		{name: "bad JSON", handler: func(connection net.Conn, _ notify.Frame) {
			_, _ = connection.Write([]byte("bad\n"))
		}},
		{name: "truncated", handler: func(connection net.Conn, _ notify.Frame) {
			_, _ = connection.Write([]byte(`{"version":1`))
		}},
		{name: "oversize", handler: func(connection net.Conn, _ notify.Frame) {
			base := []byte(`{"version":1,"status":"accepted"}`)
			line := append(append([]byte{}, base...), bytes.Repeat([]byte(" "), 256-len(base))...)
			_, _ = connection.Write(append(line, '\n'))
		}},
		{name: "unknown status", handler: func(connection net.Conn, _ notify.Frame) {
			_, _ = connection.Write([]byte(`{"version":1,"status":"future"}` + "\n"))
		}},
		{name: "disconnect", handler: func(net.Conn, notify.Frame) {}},
		{name: "timeout", handler: func(net.Conn, notify.Frame) { time.Sleep(100 * time.Millisecond) }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			socket := startAckSocket(t, tt.handler)
			sender, calls, bodies := fallbackServer(t)
			cfg := testNotifyClientConfig(t, socket)
			cfg.Sender = sender
			cfg.DialTimeout = 25 * time.Millisecond
			var stdout, stderr bytes.Buffer
			dispatchNotify(
				context.Background(), cfg,
				strings.NewReader(canonicalPiPayload("one inline fallback")), &stdout, &stderr,
			)
			if calls.Load() != 1 {
				t.Fatalf("inline calls = %d, want exactly 1", calls.Load())
			}
			select {
			case body := <-bodies:
				if body != "one inline fallback" {
					t.Fatalf("body = %q", body)
				}
			default:
				t.Fatal("missing inline body")
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("stdout/stderr = %q/%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestDispatchNotifyAckSizeBoundaryControlsInlineFallback(t *testing.T) {
	const maximumAckLineBytes = 256
	base := []byte(`{"version":1,"status":"accepted"}`)
	for _, tt := range []struct {
		name         string
		wireBytes    int
		wantFallback bool
	}{
		{name: "exact limit accepted", wireBytes: maximumAckLineBytes},
		{name: "one byte over rejected", wireBytes: maximumAckLineBytes + 1, wantFallback: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			socket := startAckSocket(t, func(connection net.Conn, _ notify.Frame) {
				line := append(append([]byte{}, base...), bytes.Repeat(
					[]byte(" "), tt.wireBytes-len(base)-1,
				)...)
				_, _ = connection.Write(append(line, '\n'))
			})
			sender, calls, bodies := fallbackServer(t)
			cfg := testNotifyClientConfig(t, socket)
			cfg.Sender = sender
			dispatchNotify(
				context.Background(), cfg,
				strings.NewReader(canonicalPiPayload("boundary fallback")), io.Discard, io.Discard,
			)
			wantCalls := int32(0)
			if tt.wantFallback {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("inline calls = %d, want %d", calls.Load(), wantCalls)
			}
			if tt.wantFallback {
				if body := <-bodies; body != "boundary fallback" {
					t.Fatalf("fallback body = %q", body)
				}
			}
		})
	}
}

func TestDispatchNotifySocketOutageUsesOnePreparedClaudeSnapshot(t *testing.T) {
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"captured user"}}` + "\n" +
		`{"type":"assistant","uuid":"captured-uuid","message":{"role":"assistant","content":[{"type":"text","text":"captured assistant"}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := startAckSocket(t, func(connection net.Conn, frame notify.Frame) {
		if frame.Event.CompletionID != "captured-uuid" || frame.Event.Assistant != "captured assistant" {
			t.Errorf("prepared frame = %+v", frame)
		}
		if err := os.Remove(transcript); err != nil {
			t.Errorf("removing transcript: %v", err)
		}
		_ = notify.EncodeAck(connection, notify.Ack{Version: 1, Status: "rejected"})
	})
	sender, calls, bodies := fallbackServer(t)
	cfg := testNotifyClientConfig(t, socket)
	cfg.Sender = sender
	payload := `{"session_id":"claude-session","cwd":"/work/project","hook_event_name":"Stop",` +
		`"transcript_path":` + string(mustJSON(t, transcript)) + `,"last_assistant_message":"stale hook text"}`
	dispatchNotify(context.Background(), cfg, strings.NewReader(payload), io.Discard, io.Discard)
	if calls.Load() != 1 {
		t.Fatalf("inline calls = %d", calls.Load())
	}
	if body := <-bodies; body != "captured assistant" {
		t.Fatalf("inline body = %q, want original prepared snapshot", body)
	}
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	wire, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestSendFrameUsesOneCombinedBudgetAndEarlierContextDeadline(t *testing.T) {
	socket := startAckSocket(t, func(net.Conn, notify.Frame) { time.Sleep(200 * time.Millisecond) })
	frame := notify.Frame{Event: notify.PreparedEvent{
		Version: 1, Harness: "pi", SessionID: "s", Kind: "completion",
		SourceEvent: "TurnComplete", CompletionID: "id", Assistant: "fallback",
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if sendFrame(ctx, socket, frame, 200*time.Millisecond) {
		t.Fatal("timeout response accepted")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("sendFrame ignored earlier context deadline: %s", elapsed)
	}
}

func TestSendFrameReturnsAtAckNewlineWithoutWaitingForEOF(t *testing.T) {
	release := make(chan struct{})
	socket := startAckSocket(t, func(connection net.Conn, _ notify.Frame) {
		_ = notify.EncodeAck(connection, notify.Ack{Version: 1, Status: "accepted"})
		<-release
	})
	defer close(release)
	frame := notify.Frame{Event: notify.PreparedEvent{
		Version: 1, Harness: "pi", SessionID: "s", Kind: "completion",
		SourceEvent: "TurnComplete", CompletionID: "id", Assistant: "fallback",
	}}
	started := time.Now()
	if !sendFrame(context.Background(), socket, frame, 100*time.Millisecond) {
		t.Fatal("accepted ack was not accepted")
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("sendFrame waited for EOF: %s", elapsed)
	}
}

func writeMarkerHelper(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	marker := filepath.Join(directory, "invoked")
	path := filepath.Join(directory, "helper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch \"$MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MARKER", marker)
	return path, marker
}

func TestDispatchNotifyInlineAndDryRunNeverInvokePiHelper(t *testing.T) {
	helper, marker := writeMarkerHelper(t)
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "outage", true: "dry-run"}[dryRun], func(t *testing.T) {
			sender, calls, _ := fallbackServer(t)
			cfg := testNotifyClientConfig(t, filepath.Join(t.TempDir(), "missing.sock"))
			cfg.DryRun = dryRun
			cfg.Sender = sender
			cfg.Environ = []string{"STEWARD_HELPER_BIN=" + helper}
			var stdout, stderr bytes.Buffer
			dispatchNotify(
				context.Background(), cfg,
				strings.NewReader(canonicalPiPayload("model-free fallback")), &stdout, &stderr,
			)
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("helper invoked: %v", err)
			}
			if dryRun {
				if calls.Load() != 0 || !strings.Contains(stdout.String(), "model-free fallback") {
					t.Fatalf("dry-run calls/stdout = %d/%q", calls.Load(), stdout.String())
				}
			} else if calls.Load() != 1 || stdout.Len() != 0 {
				t.Fatalf("outage calls/stdout = %d/%q", calls.Load(), stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q", stderr.String())
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.Log.Path), "session-labels")); !os.IsNotExist(err) {
				t.Fatalf("inline/dry-run created label state: %v", err)
			}
		})
	}
}

func TestRunNotifyCommandHarnessFlagPreparesPiFrameAndWaitsForAck(t *testing.T) {
	runtimeDirectory := t.TempDir()
	socketPath := filepath.Join(runtimeDirectory, "steward", "notifyd.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	frames := make(chan notify.Frame, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		frame, decodeErr := notify.DecodeFrame(connection)
		if decodeErr == nil {
			frames <- frame
			_ = notify.EncodeAck(connection, notify.Ack{Version: 1, Status: "accepted"})
		}
	}()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDirectory)
	t.Setenv("STEWARD_NTFY_URL", "http://unused.invalid/topic")
	payload := `{"schema_version":1,"harness":"pi","session_id":"thread",` +
		`"completion_id":"turn","hook_event_name":"TurnComplete","cwd":"/tmp"}`
	var stdout, stderr bytes.Buffer
	if code := runNotifyCommandWithIO(
		[]string{"--harness=pi", "--state-base=" + t.TempDir()},
		strings.NewReader(payload), &stdout, &stderr,
	); code != 0 {
		t.Fatalf("exit code = %d stderr=%q", code, stderr.String())
	}
	select {
	case frame := <-frames:
		if frame.Event.Harness != "pi" || frame.Event.Kind != "completion" ||
			frame.Event.SessionID != "thread" || frame.Event.CompletionID != "turn" {
			t.Fatalf("frame = %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("no normalized frame")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout/stderr = %q/%q", stdout.String(), stderr.String())
	}
}

func TestNewNotifydPipelineUsesCentralPiConfigurationAndDaemonOnlyLabels(t *testing.T) {
	directory := t.TempDir()
	helperPath := filepath.Join(directory, "helper")
	helperScript := "#!/bin/sh\nprintf '%s\\n' " +
		"'{\"version\":1,\"ok\":true,\"body\":\"summary\"}'\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatal(err)
	}
	environ := []string{
		"STEWARD_HELPER_BIN=" + helperPath,
		"STEWARD_MODEL_PROVIDER=provider-central",
		"STEWARD_MODEL_ID=model-central",
		"STEWARD_MODEL_THINKING=xhigh",
	}
	pipeline := newNotifydPipeline(
		false, notify.Sender{}, notify.DecisionLog{}, environ, directory,
	)
	composer, ok := pipeline.Composer.(notify.PiComposer)
	if !ok || composer.Bin != helperPath || composer.Model != (notify.ComposeModel{
		Provider: "provider-central", ID: "model-central", Thinking: "xhigh",
	}) {
		t.Fatalf("composer = %+v (%T)", composer, pipeline.Composer)
	}
	if pipeline.LabelStore == nil {
		t.Fatal("real daemon pipeline has no persistent label store")
	}
	if dry := newNotifydPipeline(
		true, notify.Sender{}, notify.DecisionLog{}, environ, directory,
	); dry.LabelStore != nil {
		t.Fatal("dry-run daemon pipeline must not have a label store")
	}
	if _, err := os.Stat(filepath.Join(directory, "session-labels")); !os.IsNotExist(err) {
		t.Fatalf("constructing unused pipelines wrote label state: %v", err)
	}
}

func TestSessionLabelDocumentationCoversPersistenceCadenceAndFallback(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "docs", "session-labels.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"first", "four additional", "1, 5, and 9", "changed", "KEEP",
		"session-labels", "SHA-256", "0700", "0600", "source generation",
		"minimal", "not an outbox", "daemon", "inline", "dry-run", "cwd",
		"harness", "resume", "manual",
	} {
		if !strings.Contains(string(document), required) {
			t.Errorf("session label documentation missing %q", required)
		}
	}
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"shared session label", "four additional", "cwd", "session-labels"} {
		if !strings.Contains(string(readme), required) {
			t.Errorf("README notification section missing %q", required)
		}
	}
}

func TestNotifyProtocolDocumentationCoversStrictAndNonDurableSemantics(t *testing.T) {
	protocol, err := os.ReadFile(filepath.Join("..", "..", "docs", "notify-protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"64 KiB", "256 bytes", `{"version":1,"status":"accepted"}`,
		`{"version":1,"status":"duplicate"}`, `{"version":1,"status":"rejected"}`,
		"24 hours", "10,000", "non-durable", "restart", "crash", "inline fallback",
		"same prepared snapshot", "no delivery guarantee", "non-null",
		"unpaired UTF-16 surrogate", "identity pair",
	} {
		if !strings.Contains(string(protocol), required) {
			t.Errorf("protocol documentation missing %q", required)
		}
	}
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"acknowledgement", "duplicate", "non-durable", "same prepared snapshot", "identity pair",
	} {
		if !strings.Contains(string(readme), required) {
			t.Errorf("README notification section missing %q", required)
		}
	}
}

func TestNotifydRequiresSenderAndDefaultStateBase(t *testing.T) {
	for _, test := range []struct{ senderOK, dryRun, want bool }{
		{false, false, true}, {false, true, false}, {true, false, false}, {true, true, false},
	} {
		if got := notifydRequiresSender(test.senderOK, test.dryRun); got != test.want {
			t.Errorf("notifydRequiresSender(%v,%v) = %v", test.senderOK, test.dryRun, got)
		}
	}
	t.Setenv("XDG_STATE_HOME", "/state")
	if got := defaultNotifyStateBase(); got != filepath.Join("/state", "steward", "notify") {
		t.Fatalf("defaultNotifyStateBase = %q", got)
	}
}
