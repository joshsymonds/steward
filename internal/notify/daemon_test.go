package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type synchronizedBuffer struct {
	mutex sync.Mutex
	data  bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.data.Write(data)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.data.String()
}

type runningTestDaemon struct {
	socket string
	cancel context.CancelFunc
	done   <-chan error
}

func startTestDaemon(t *testing.T, daemon Daemon) runningTestDaemon {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "notifyd.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Serve(ctx, listener) }()
	running := runningTestDaemon{socket: socketPath, cancel: cancel, done: done}
	t.Cleanup(func() { running.stop(t) })
	return running
}

func (daemon runningTestDaemon) stop(t *testing.T) {
	t.Helper()
	daemon.cancel()
	select {
	case err := <-daemon.done:
		if err != nil {
			t.Errorf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		// A test may already have consumed done after an explicit stop.
	}
}

func exchangeTestFrame(socketPath string, frame Frame) (Ack, error) {
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		return Ack{}, err
	}
	defer func() { _ = connection.Close() }()
	if encodeErr := EncodeFrame(connection, frame); encodeErr != nil {
		return Ack{}, encodeErr
	}
	return DecodeAck(connection)
}

func requireFrameAckAndHandlerCompletion(t *testing.T, socketPath string, frame Frame, want string) {
	t.Helper()
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err = connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = EncodeFrame(connection, frame); err != nil {
		t.Fatal(err)
	}
	ack, err := DecodeAck(connection)
	if err != nil {
		t.Fatal(err)
	}
	if ack != (Ack{Version: 1, Status: want}) {
		t.Fatalf("ack = %+v, want %s", ack, want)
	}
	remaining, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("waiting for handler completion: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("unexpected data after ack: %q", remaining)
	}
}

func exchangeTestWire(socketPath string, wire []byte) (Ack, error) {
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		return Ack{}, err
	}
	defer func() { _ = connection.Close() }()
	if _, err = connection.Write(wire); err != nil {
		return Ack{}, err
	}
	return DecodeAck(connection)
}

func requireFrameAck(t *testing.T, socketPath string, frame Frame, want string) {
	t.Helper()
	ack, err := exchangeTestFrame(socketPath, frame)
	if err != nil {
		t.Fatal(err)
	}
	if ack != (Ack{Version: 1, Status: want}) {
		t.Fatalf("ack = %+v, want %s", ack, want)
	}
}

func waitForDecisionRecords(t *testing.T, path string, count int) []DecisionRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			records := readDecisionLog(t, path)
			if len(records) >= count {
				return records
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d decision records", count)
	return nil
}

func completionFrame(sessionID, completionID, assistant string) Frame {
	return Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessPi, SessionID: sessionID, Kind: eventKindCompletion,
		SourceEvent: eventTurnComplete, CompletionID: completionID, CWD: "/work/project",
		User: "same user", Assistant: assistant,
	}, Workspace: "earth:3"}
}

type blockingDaemonComposer struct {
	entered chan ComposeInput
	release <-chan struct{}
	result  ComposeResult
	err     error
}

func (composer blockingDaemonComposer) Compose(
	ctx context.Context,
	input ComposeInput,
	_ ComposeLabel,
) (ComposeResult, error) {
	composer.entered <- input
	select {
	case <-composer.release:
		return composer.result, composer.err
	case <-ctx.Done():
		return ComposeResult{}, errorsForCanceledComposer()
	}
}

func errorsForCanceledComposer() error { return fmt.Errorf("pi composer: helper canceled") }

func TestDaemonFloodSameCompletionIDComposesAndSendsExactlyOnce(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &recordingComposer{result: ComposeResult{Body: "one summary"}}
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: composer, Sender: Sender{URL: server.URL, Client: server.Client()},
		Log: DecisionLog{Path: logPath}, Host: "host",
	}})
	frame := completionFrame("session", "completion", "same assistant")

	const flood = 40
	results := make(chan Ack, flood)
	errors := make(chan error, flood)
	var wait sync.WaitGroup
	for range flood {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ack, err := exchangeTestFrame(running.socket, frame)
			if err != nil {
				errors <- err
				return
			}
			results <- ack
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Errorf("exchange: %v", err)
	}
	accepted, duplicates := 0, 0
	for ack := range results {
		switch ack.Status {
		case ackStatusAccepted:
			accepted++
		case ackStatusDuplicate:
			duplicates++
		default:
			t.Errorf("unexpected ack: %+v", ack)
		}
	}
	if accepted != 1 || duplicates != flood-1 {
		t.Fatalf("accepted/duplicate = %d/%d", accepted, duplicates)
	}
	if request := waitNotification(t, requests); request.Body != "one summary" {
		t.Fatalf("request = %+v", request)
	}
	select {
	case extra := <-requests:
		t.Fatalf("duplicate delivery = %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
	if calls := composer.Calls(); len(calls) != 1 {
		t.Fatalf("Compose calls = %d, want 1", len(calls))
	}
	if records := waitForDecisionRecords(t, logPath, 1); len(records) != 1 {
		t.Fatalf("decision records = %d", len(records))
	}
}

func TestDaemonDistinctSameTextIDsAndScopesRemainIndependent(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &recordingComposer{result: ComposeResult{Body: "summary"}}
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: composer, Sender: Sender{URL: server.URL, Client: server.Client()},
		Log: DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
	}})
	frames := []Frame{
		completionFrame("session-a", "id-a", "identical text"),
		completionFrame("session-a", "id-b", "identical text"),
		completionFrame("session-b", "id-a", "identical text"),
		{Event: PreparedEvent{
			Version: 1, Harness: harnessClaude, SessionID: "session-a", Kind: eventKindCompletion,
			SourceEvent: eventStop, CompletionID: "id-a", CWD: "/work/project",
			User: "same user", Assistant: "identical text",
		}, Workspace: "earth:3"},
	}
	for _, frame := range frames {
		requireFrameAck(t, running.socket, frame, ackStatusAccepted)
	}
	for range frames {
		_ = waitNotification(t, requests)
	}
	if calls := composer.Calls(); len(calls) != len(frames) {
		t.Fatalf("Compose calls = %d, want %d", len(calls), len(frames))
	}
}

type attributedBlockingDaemonComposer struct {
	entered  chan ComposeInput
	releases map[ComposeInput]<-chan struct{}
	bodies   map[ComposeInput]string
}

func (composer attributedBlockingDaemonComposer) Compose(
	ctx context.Context,
	input ComposeInput,
	_ ComposeLabel,
) (ComposeResult, error) {
	composer.entered <- input
	select {
	case <-composer.releases[input]:
		return ComposeResult{Body: composer.bodies[input]}, nil
	case <-ctx.Done():
		return ComposeResult{}, errorsForCanceledComposer()
	}
}

func TestDaemonConcurrentCompletionsRetainOriginalAttribution(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	inputA := ComposeInput{User: "user alpha", Assistant: "assistant alpha"}
	inputB := ComposeInput{User: "user beta", Assistant: "assistant beta"}
	channelA := make(chan struct{})
	channelB := make(chan struct{})
	var releaseAOnce, releaseBOnce sync.Once
	releaseA := func() { releaseAOnce.Do(func() { close(channelA) }) }
	releaseB := func() { releaseBOnce.Do(func() { close(channelB) }) }
	t.Cleanup(releaseA)
	t.Cleanup(releaseB)
	composer := attributedBlockingDaemonComposer{
		entered: make(chan ComposeInput, 2),
		releases: map[ComposeInput]<-chan struct{}{
			inputA: channelA,
			inputB: channelB,
		},
		bodies: map[ComposeInput]string{
			inputA: "summary alpha",
			inputB: "summary beta",
		},
	}
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: composer, Sender: Sender{URL: server.URL, Client: server.Client()},
		Log: DecisionLog{Path: logPath}, Host: "host",
	}})
	frameA := Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessPi, SessionID: "session-alpha", Kind: eventKindCompletion,
		SourceEvent: eventTurnComplete, CWD: "/work/project-alpha", CompletionID: "completion-alpha",
		User: inputA.User, Assistant: inputA.Assistant,
	}, Workspace: "earth:3"}
	frameB := Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "session-beta", Kind: eventKindCompletion,
		SourceEvent: eventStop, CWD: "/work/project-beta", CompletionID: "completion-beta",
		User: inputB.User, Assistant: inputB.Assistant,
	}, Workspace: "mars:9"}

	requireFrameAck(t, running.socket, frameA, ackStatusAccepted)
	requireFrameAck(t, running.socket, frameB, ackStatusAccepted)
	entered := map[ComposeInput]bool{}
	for range 2 {
		select {
		case input := <-composer.entered:
			entered[input] = true
		case <-time.After(time.Second):
			t.Fatal("both compositions did not block")
		}
	}
	if !entered[inputA] || !entered[inputB] {
		t.Fatalf("composition inputs = %+v", entered)
	}
	requireFrameAck(t, running.socket, frameB, ackStatusDuplicate)
	requireFrameAck(t, running.socket, frameA, ackStatusDuplicate)

	releaseB()
	if request := waitNotification(t, requests); request != (capturedNotification{
		Title: "project-beta · mars:9", Body: "summary beta",
		Priority: "4", Tags: "white_check_mark",
	}) {
		t.Fatalf("beta request = %+v", request)
	}
	releaseA()
	if request := waitNotification(t, requests); request != (capturedNotification{
		Title: "project-alpha · earth:3", Body: "summary alpha",
		Priority: "4", Tags: "white_check_mark",
	}) {
		t.Fatalf("alpha request = %+v", request)
	}

	records := waitForDecisionRecords(t, logPath, 2)
	byID := make(map[string]DecisionRecord, len(records))
	for _, record := range records {
		byID[record.CompletionID] = record
	}
	for _, want := range []DecisionRecord{
		{
			Harness: harnessPi, SessionID: "session-alpha", Event: eventTurnComplete,
			CompletionID: "completion-alpha", Title: "project-alpha · earth:3", Body: "summary alpha",
			Outcome: OutcomeSend.String(), CompositionOutcome: compositionComposed,
		},
		{
			Harness: harnessClaude, SessionID: "session-beta", Event: eventStop,
			CompletionID: "completion-beta", Title: "project-beta · mars:9", Body: "summary beta",
			Outcome: OutcomeSend.String(), CompositionOutcome: compositionComposed,
		},
	} {
		got, ok := byID[want.CompletionID]
		if !ok || got.Harness != want.Harness || got.SessionID != want.SessionID ||
			got.Event != want.Event || got.Title != want.Title || got.Body != want.Body ||
			got.Outcome != want.Outcome || got.CompositionOutcome != want.CompositionOutcome {
			t.Fatalf("record for %q = %+v, want attribution %+v", want.CompletionID, got, want)
		}
	}
}

func TestDaemonAckPrecedesBlockedCompositionAndInputBypassesIt(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	entered := make(chan ComposeInput, 1)
	release := make(chan struct{})
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: blockingDaemonComposer{entered: entered, release: release, result: ComposeResult{Body: "completion"}},
		Sender:   Sender{URL: server.URL, Client: server.Client()},
		Log:      DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
	}})

	requireFrameAck(t, running.socket, completionFrame("slow", "slow-id", "fallback"), ackStatusAccepted)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("composer did not start after accepted ack")
	}
	input := Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "input", Kind: eventKindInput,
		SourceEvent: eventNotification, CWD: "/work/fast", NotificationType: "permission_prompt", Message: "allow now?",
	}, Workspace: "fast:2"}
	requireFrameAck(t, running.socket, input, ackStatusAccepted)
	if request := waitNotification(t, requests); request.Body != "allow now?" || request.Priority != "5" {
		t.Fatalf("first request = %+v", request)
	}
	close(release)
	if request := waitNotification(t, requests); request.Body != "completion" {
		t.Fatalf("second request = %+v", request)
	}
}

func TestDaemonFinalSendFailureReleasesClaimForSourceRetry(t *testing.T) {
	var attempts atomic.Int32
	firstFinished := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			firstFinished <- struct{}{}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	composer := &recordingComposer{result: ComposeResult{Body: "summary"}}
	stateBase := t.TempDir()
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: composer, Sender: Sender{URL: server.URL, Client: server.Client()},
		Log: DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
		LabelStore: NewLabelStore(stateBase),
	}})
	frame := completionFrame("retry", "same-id", "fallback")
	requireFrameAck(t, running.socket, frame, ackStatusAccepted)
	select {
	case <-firstFinished:
	case <-time.After(time.Second):
		t.Fatal("first send did not finish")
	}
	// Wait until the handler has released ownership after recording failure.
	deadline := time.Now().Add(time.Second)
	for {
		ack, err := exchangeTestFrame(running.socket, frame)
		if err != nil {
			t.Fatal(err)
		}
		if ack.Status == ackStatusAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed send claim was not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(time.Second)
	for attempts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	calls := composer.Calls()
	if attempts.Load() != 2 || len(calls) != 2 {
		t.Fatalf("attempts/compositions = %d/%d, want 2/2", attempts.Load(), len(calls))
	}
	if !calls[0].Label.Refresh || calls[1].Label.Refresh {
		t.Fatalf("source retry label plans = %+v, want naming only on first attempt", calls)
	}
	_, data := onlyLabelSnapshot(t, stateBase)
	snapshot := decodeLabelSnapshotForTest(t, data)
	if snapshot["exchange_count"] != float64(1) || snapshot["source_generation"] != float64(1) {
		t.Fatalf("source retry double-counted: %+v", snapshot)
	}
}

func TestDaemonDuplicateNeverMutatesLabelCounters(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &recordingComposer{result: ComposeResult{Body: "summary"}}
	stateBase := t.TempDir()
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: composer, Sender: Sender{URL: server.URL, Client: server.Client()},
		Log: DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
		LabelStore: NewLabelStore(stateBase),
	}})
	first := completionFrame("duplicates", "id-1", "identical material")
	second := completionFrame("duplicates", "id-2", "identical material")
	requireFrameAckAndHandlerCompletion(t, running.socket, first, ackStatusAccepted)
	_ = waitNotification(t, requests)
	requireFrameAckAndHandlerCompletion(t, running.socket, second, ackStatusAccepted)
	_ = waitNotification(t, requests)
	requireFrameAckAndHandlerCompletion(t, running.socket, second, ackStatusDuplicate)

	calls := composer.Calls()
	if len(calls) != 2 || !calls[0].Label.Refresh || calls[1].Label.Refresh {
		t.Fatalf("distinct-identical/duplicate calls = %+v", calls)
	}
	_, data := onlyLabelSnapshot(t, stateBase)
	snapshot := decodeLabelSnapshotForTest(t, data)
	if snapshot["exchange_count"] != float64(2) || snapshot["source_generation"] != float64(2) ||
		snapshot["latest_completion_id"] != "id-2" {
		t.Fatalf("duplicate mutated label counters: %+v", snapshot)
	}
	select {
	case request := <-requests:
		t.Fatalf("duplicate sent notification: %+v", request)
	default:
	}
}

func TestDaemonHelperFailureFallbackSuccessRetainsClaim(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &recordingComposer{err: fmt.Errorf("pi composer: helper unavailable")}
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: composer, Sender: Sender{URL: server.URL, Client: server.Client()},
		Log: DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
	}})
	frame := completionFrame("fallback", "id", "fallback body")
	requireFrameAckAndHandlerCompletion(t, running.socket, frame, ackStatusAccepted)
	if request := waitNotification(t, requests); request.Body != "fallback body" {
		t.Fatalf("request = %+v", request)
	}
	requireFrameAckAndHandlerCompletion(t, running.socket, frame, ackStatusDuplicate)
	select {
	case request := <-requests:
		t.Fatalf("duplicate delivery = %+v", request)
	default:
	}
	if calls := composer.Calls(); len(calls) != 1 {
		t.Fatalf("Compose calls = %d, want 1", len(calls))
	}
}

func TestDaemonCleanupRetainsClaimsAndLabelsWhileRestartStartsClaimsEmpty(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	stateBase := t.TempDir()
	firstComposer := &recordingComposer{result: ComposeResult{
		Body: "summary", Label: "Persistent Shared Label",
	}}
	pipeline := Pipeline{
		Composer: firstComposer,
		Sender:   Sender{URL: server.URL, Client: server.Client()}, Host: "host",
		LabelStore: NewLabelStore(stateBase),
	}
	first := startTestDaemon(t, Daemon{Pipeline: pipeline})
	frame := Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "session", Kind: eventKindCompletion,
		SourceEvent: eventStop, CWD: "/work/project", CompletionID: "assistant-uuid",
		User: "same user", Assistant: "fallback",
	}, Workspace: "earth:3"}
	requireFrameAckAndHandlerCompletion(t, first.socket, frame, ackStatusAccepted)
	if request := waitNotification(t, requests); request.Body != "summary" ||
		request.Title != "Persistent Shared Label · earth:3" {
		t.Fatalf("request = %+v", request)
	}
	cleanup := Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "session", Kind: eventKindCleanup,
		SourceEvent: eventSessionEnd,
	}}
	requireFrameAckAndHandlerCompletion(t, first.socket, cleanup, ackStatusAccepted)
	if label, err := NewLabelStore(
		stateBase,
	).lookupLabel(harnessClaude, "session"); err != nil ||
		label != "Persistent Shared Label" {
		t.Fatalf("SessionEnd label/error = %q/%v", label, err)
	}
	requireFrameAckAndHandlerCompletion(t, first.socket, frame, ackStatusDuplicate)
	first.cancel()
	select {
	case err := <-first.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first daemon did not stop")
	}

	secondComposer := &recordingComposer{result: ComposeResult{Body: "summary"}}
	pipeline.Composer = secondComposer
	pipeline.LabelStore = NewLabelStore(stateBase)
	second := startTestDaemon(t, Daemon{Pipeline: pipeline})
	requireFrameAckAndHandlerCompletion(t, second.socket, frame, ackStatusAccepted)
	if request := waitNotification(t, requests); request.Body != "summary" ||
		request.Title != "Persistent Shared Label · earth:3" {
		t.Fatalf("request after restart = %+v", request)
	}
	calls := secondComposer.Calls()
	if len(calls) != 1 || calls[0].Label != (ComposeLabel{Current: "Persistent Shared Label"}) {
		t.Fatalf("reconstructed daemon label request = %+v", calls)
	}
	_, data := onlyLabelSnapshot(t, stateBase)
	if snapshot := decodeLabelSnapshotForTest(t, data); snapshot["exchange_count"] != float64(1) {
		t.Fatalf("restart source retry double-counted: %+v", snapshot)
	}
}

func TestDaemonDryRunAndMissingIdentityNeverClaimOrCompose(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		var stdout synchronizedBuffer
		running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
			Composer: panicComposer{}, DryRun: true, Stdout: &stdout,
			Log: DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
		}})
		frame := completionFrame("dry", "id", "dry fallback")
		for range 2 {
			requireFrameAck(t, running.socket, frame, ackStatusAccepted)
		}
		deadline := time.Now().Add(time.Second)
		for strings.Count(stdout.String(), "DRY RUN") < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if strings.Count(stdout.String(), "DRY RUN") != 2 {
			t.Fatalf("stdout = %q", stdout.String())
		}
	})

	t.Run("per-frame dry run", func(t *testing.T) {
		var stdout synchronizedBuffer
		running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
			Composer: panicComposer{}, Stdout: &stdout,
			Log: DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
		}})
		frame := completionFrame("frame-dry", "id", "frame dry fallback")
		frame.DryRun = true
		for range 2 {
			requireFrameAck(t, running.socket, frame, ackStatusAccepted)
		}
		deadline := time.Now().Add(time.Second)
		for strings.Count(stdout.String(), "DRY RUN") < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if strings.Count(stdout.String(), "DRY RUN") != 2 {
			t.Fatalf("stdout = %q", stdout.String())
		}
	})

	t.Run("incomplete identity pair", func(t *testing.T) {
		server, requests := captureNotificationServer(t)
		defer server.Close()
		claims := newClaimStore(nil)
		logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
		running := startTestDaemon(t, Daemon{
			Pipeline: Pipeline{
				Composer: panicComposer{}, Sender: Sender{URL: server.URL, Client: server.Client()},
				Log: DecisionLog{Path: logPath}, Host: "host",
			},
			claims: claims,
		})
		frames := []Frame{
			completionFrame("session-only", "", "session-only fallback"),
			completionFrame("", "completion-only", "completion-only fallback"),
			completionFrame("", "", "identity-free fallback"),
		}
		for _, frame := range frames {
			for range 2 {
				requireFrameAck(t, running.socket, frame, ackStatusAccepted)
				if request := waitNotification(t, requests); request.Body != frame.Event.Assistant {
					t.Fatalf("request = %+v, want %q", request, frame.Event.Assistant)
				}
			}
		}
		records := waitForDecisionRecords(t, logPath, 2*len(frames))
		for _, record := range records {
			if record.CompositionOutcome != compositionFallback ||
				record.CompositionError != compositionErrorIdentityUnavailable {
				t.Fatalf("record = %+v", record)
			}
		}
		claims.mutex.Lock()
		claimCount := len(claims.entries)
		claims.mutex.Unlock()
		if claimCount != 0 {
			t.Fatalf("incomplete identity created %d claims", claimCount)
		}
	})
}

func TestDaemonStructuralIgnoredChildGoalAndCleanupHaveNoEffects(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: panicComposer{}, Sender: Sender{URL: server.URL, Client: server.Client()},
		Log: DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
	}})
	child := completionFrame("child", "id", "must not send")
	child.Event.AgentType = "worker"
	goal := Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "goal", Kind: eventKindCompletion,
		SourceEvent: eventStop, CompletionID: "id", Assistant: "must not send", GoalActive: true,
	}}
	ignored := Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "ignored", Kind: eventKindIgnored,
		SourceEvent: eventNotification, NotificationType: "idle_prompt",
	}}
	cleanup := Frame{Event: PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "cleanup", Kind: eventKindCleanup,
		SourceEvent: eventSessionEnd,
	}}
	for _, frame := range []Frame{child, goal, ignored, cleanup} {
		requireFrameAck(t, running.socket, frame, ackStatusAccepted)
	}
	select {
	case request := <-requests:
		t.Fatalf("structurally silent frame delivered: %+v", request)
	case <-time.After(100 * time.Millisecond):
	}
}

type failedAckWriteConn struct{ net.Conn }

func (connection failedAckWriteConn) Write([]byte) (int, error) {
	return 0, fmt.Errorf("sentinel ack write failure")
}

func TestDaemonAckWriteFailureDoesNotRevokeAcceptedWork(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	client, daemonConnection := net.Pipe()
	done := make(chan struct{})
	daemon := Daemon{Pipeline: Pipeline{
		Composer: &recordingComposer{result: ComposeResult{Body: "accepted work"}},
		Sender:   Sender{URL: server.URL, Client: server.Client()}, Host: "host",
	}}
	go func() {
		defer close(done)
		daemon.handleConn(context.Background(), failedAckWriteConn{daemonConnection})
	}()
	if err := EncodeFrame(client, completionFrame("ambiguous", "id", "fallback")); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if request := waitNotification(t, requests); request.Body != "accepted work" {
		t.Fatalf("request = %+v", request)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish accepted work after ack failure")
	}
}

func TestDaemonUsesPreparedSnapshotAfterTranscriptRemoval(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	composer := &recordingComposer{result: ComposeResult{Body: "snapshot summary"}}
	running := startTestDaemon(t, Daemon{Pipeline: Pipeline{
		Composer: composer, Sender: Sender{URL: server.URL, Client: server.Client()},
		Log: DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
	}})
	path := conversationTranscript(t, "snapshot-uuid", "message-id", false)
	event, err := PrepareEvent(HookInput{
		Harness: harnessClaude, SessionID: "snapshot", HookEventName: eventStop,
		TranscriptPath: path, LastAssistantMessage: "stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	requireFrameAck(t, running.socket, Frame{Event: event}, ackStatusAccepted)
	_ = waitNotification(t, requests)
	if calls := composer.Calls(); len(calls) != 1 || calls[0].Input.Assistant != "latest assistant text" {
		t.Fatalf("Compose calls = %+v", calls)
	}
}

func TestDaemonMalformedAndInvalidFramesReceiveRejectedSafeAck(t *testing.T) {
	var logs synchronizedBuffer
	running := startTestDaemon(t, Daemon{
		Pipeline: Pipeline{DryRun: true, Stdout: io.Discard},
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	})
	cases := [][]byte{
		[]byte("SECRET-MALFORMED\n"),
		[]byte(`{"event":null,"workspace":""}` + "\n"),
	}
	for _, raw := range cases {
		connection, err := net.Dial("unix", running.socket)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = connection.Write(raw); err != nil {
			t.Fatal(err)
		}
		ack, err := DecodeAck(connection)
		_ = connection.Close()
		if err != nil || ack.Status != ackStatusRejected {
			t.Fatalf("ack/error = %+v/%v", ack, err)
		}
	}
	if strings.Contains(logs.String(), "SECRET-MALFORMED") {
		t.Fatalf("daemon log leaked raw frame: %s", logs.String())
	}
}

type shortenedReadDeadlineConn struct{ net.Conn }

func (connection shortenedReadDeadlineConn) SetReadDeadline(time.Time) error {
	return connection.Conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
}

func TestDaemonUnfinishedLineExpiresAtReadDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		(Daemon{Pipeline: Pipeline{DryRun: true}}).handleConn(context.Background(), shortenedReadDeadlineConn{server})
	}()
	if _, err := client.Write([]byte(`{"event":`)); err != nil {
		t.Fatal(err)
	}
	ack, err := DecodeAck(client)
	if err != nil || ack.Status != ackStatusRejected {
		t.Fatalf("deadline ack/error = %+v/%v", ack, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unfinished connection handler did not stop")
	}
}

type cancelAwareDaemonComposer struct{ entered chan struct{} }

func (composer cancelAwareDaemonComposer) Compose(
	ctx context.Context,
	_ ComposeInput,
	_ ComposeLabel,
) (ComposeResult, error) {
	composer.entered <- struct{}{}
	<-ctx.Done()
	return ComposeResult{}, errorsForCanceledComposer()
}

func TestDaemonShutdownCancelsCompositionAndDrainsFallback(t *testing.T) {
	server, requests := captureNotificationServer(t)
	defer server.Close()
	entered := make(chan struct{}, 1)
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "notifyd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	daemon := Daemon{Pipeline: Pipeline{
		Composer: cancelAwareDaemonComposer{entered: entered},
		Sender:   Sender{URL: server.URL, Client: server.Client()},
		Log:      DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
	}}
	go func() { done <- daemon.Serve(ctx, listener) }()
	frame := completionFrame("shutdown", "id", "shutdown fallback")
	requireFrameAck(t, listener.Addr().String(), frame, ackStatusAccepted)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("composer did not start")
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not drain cancellation fallback")
	}
	if request := waitNotification(t, requests); request.Body != "shutdown fallback" {
		t.Fatalf("request = %+v", request)
	}
}

type uncooperativeComposer struct {
	entered chan struct{}
	release <-chan struct{}
}

func (composer uncooperativeComposer) Compose(context.Context, ComposeInput, ComposeLabel) (ComposeResult, error) {
	composer.entered <- struct{}{}
	<-composer.release
	return ComposeResult{Body: "late"}, nil
}

func TestDaemonShutdownDrainRemainsBounded(t *testing.T) {
	if got := (Daemon{}).gracefulDrainTimeout(); got != maximumGracefulDrain {
		t.Fatalf("default drain = %s", got)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseComposer := func() { releaseOnce.Do(func() { close(release) }) }
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "notifyd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		releaseComposer()
		cancel()
		_ = listener.Close()
	})
	delivered := make(chan struct{}, 1)
	client := &http.Client{Transport: pipelineRoundTripFunc(func(*http.Request) (*http.Response, error) {
		delivered <- struct{}{}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})}
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	done := make(chan error, 1)
	go func() {
		done <- (Daemon{Pipeline: Pipeline{
			Composer: uncooperativeComposer{entered: entered, release: release},
			Sender:   Sender{URL: "http://notify.test/topic", Client: client},
			Log:      DecisionLog{Path: logPath}, Host: "host",
		}, drainTimeout: 40 * time.Millisecond}).Serve(ctx, listener)
	}()
	requireFrameAck(
		t, listener.Addr().String(),
		completionFrame("bounded", "id", "fallback"), ackStatusAccepted,
	)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("composer did not block")
	}
	started := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve waited indefinitely")
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("drain elapsed = %s", elapsed)
	}
	releaseComposer()
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("released handler did not complete fake delivery")
	}
	if records := waitForDecisionRecords(t, logPath, 1); len(records) != 1 || records[0].Body != "late" {
		t.Fatalf("released handler records = %+v", records)
	}
}

const listenHelperEnv = "STEWARD_NOTIFY_LISTEN_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(listenHelperEnv) == "1" {
		runListenHelper()
		return
	}
	os.Exit(m.Run())
}

func runListenHelper() {
	listener, err := Listen(filepath.Join(os.TempDir(), "unused.sock"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("LISTENING")
	connection, err := listener.Accept()
	if err != nil {
		fmt.Fprintf(os.Stderr, "accept error: %v\n", err)
		os.Exit(1)
	}
	_ = connection.Close()
	fmt.Println("ACCEPTED")
	os.Exit(0)
}

func TestListenSelfBindCreatesPrivateSocketAndRemovesStaleFile(t *testing.T) {
	t.Setenv("LISTEN_FDS", "")
	directory := filepath.Join(t.TempDir(), "notifyd")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "notifyd.sock")
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := Listen(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	for path, want := range map[string]os.FileMode{directory: 0o700, socketPath: 0o600} {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != want {
			t.Fatalf("%s stat/mode = %v/%o, want %o", path, statErr, info.Mode().Perm(), want)
		}
	}
}

func TestListenSelfBindRejectsUnsafeDirectory(t *testing.T) {
	t.Setenv("LISTEN_FDS", "")
	directory := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(directory, "notifyd.sock")); err == nil {
		t.Fatal("unsafe directory accepted")
	}
}

func TestListenSystemdSocketActivation(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "systemd.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener = %T, want Unix listener", listener)
	}
	file, err := unixListener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(), listenHelperEnv+"=1", "LISTEN_FDS=1")
	command.ExtraFiles = []*os.File{file}
	var stdout strings.Builder
	command.Stdout = &stdout
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		connection, dialErr := net.Dial("unix", socketPath)
		if dialErr == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = command.Wait(); err != nil || !strings.Contains(stdout.String(), "ACCEPTED") {
		t.Fatalf("helper = %v stdout=%q", err, stdout.String())
	}
}

func TestSocketPathResolution(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := SocketPath(); got != filepath.Join("/run/user/1000", "steward", "notifyd.sock") {
		t.Fatalf("XDG SocketPath = %q", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	want := filepath.Join("/tmp", "steward-"+strconv.Itoa(os.Getuid()), "notifyd.sock")
	if got := SocketPath(); got != want {
		t.Fatalf("fallback SocketPath = %q", got)
	}
}

func TestDaemonListenerErrorStopsCancellationWatcher(t *testing.T) {
	listener := &failingTestListener{}
	ctx, cancel := context.WithCancel(context.Background())
	err := (Daemon{}).Serve(ctx, listener)
	if err == nil {
		t.Fatal("listener error was lost")
	}
	calls := listener.closeCalls.Load()
	cancel()
	time.Sleep(20 * time.Millisecond)
	if listener.closeCalls.Load() != calls {
		t.Fatal("cancellation watcher leaked")
	}
}

type failingTestListener struct{ closeCalls atomic.Int32 }

func (*failingTestListener) Accept() (net.Conn, error) { return nil, fmt.Errorf("sentinel") }
func (listener *failingTestListener) Close() error {
	listener.closeCalls.Add(1)
	return nil
}
func (*failingTestListener) Addr() net.Addr { return &net.UnixAddr{Name: "failed", Net: "unix"} }

func TestDaemonAcceptedConnectionKeepsFiveSecondReadBound(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	deadlines := make(chan time.Time, 1)
	wrapped := recordingReadDeadlineConn{Conn: server, deadlines: deadlines}
	go (Daemon{Pipeline: Pipeline{DryRun: true}}).handleConn(context.Background(), wrapped)
	started := time.Now()
	select {
	case deadline := <-deadlines:
		if delay := deadline.Sub(started); delay < 4900*time.Millisecond || delay > 5100*time.Millisecond {
			t.Fatalf("deadline = %s", delay)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline not set")
	}
	_ = client.Close()
}

type recordingReadDeadlineConn struct {
	net.Conn

	deadlines chan<- time.Time
}

func (connection recordingReadDeadlineConn) SetReadDeadline(deadline time.Time) error {
	connection.deadlines <- deadline
	return connection.Conn.SetReadDeadline(deadline)
}

func TestDaemonAcceptedLogsCarryOnlySafeMetadata(t *testing.T) {
	var logs synchronizedBuffer
	running := startTestDaemon(t, Daemon{
		Pipeline: Pipeline{DryRun: true, Stdout: io.Discard},
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	})
	frame := completionFrame("safe-session", "safe-id", "RAW BODY MUST NOT LOG")
	requireFrameAck(t, running.socket, frame, ackStatusAccepted)
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), "accepted") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for _, want := range []string{"safe-session", "safe-id", harnessPi, eventTurnComplete} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing safe metadata %q: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "RAW BODY MUST NOT LOG") {
		t.Fatalf("log leaked body: %s", logs.String())
	}
}

func TestDaemonRejectsUnsupportedWireVersionBeforeEffects(t *testing.T) {
	var sends atomic.Int32
	client := &http.Client{Transport: pipelineRoundTripFunc(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})}
	composer := &recordingComposer{result: ComposeResult{Body: "must not compose"}}
	claims := newClaimStore(nil)
	running := startTestDaemon(t, Daemon{
		Pipeline: Pipeline{
			Composer: composer,
			Sender:   Sender{URL: "http://notify.test/topic", Client: client},
			Log:      DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
		},
		claims: claims,
	})
	wire := replaceFrameWireField(
		t, completionFrame("version", "id", "fallback"),
		`"version":1`, `"version":2`,
	)
	ack, err := exchangeTestWire(running.socket, wire)
	if err != nil || ack != (Ack{Version: 1, Status: ackStatusRejected}) {
		t.Fatalf("ack/error = %+v/%v", ack, err)
	}
	time.Sleep(20 * time.Millisecond)
	claims.mutex.Lock()
	claimCount := len(claims.entries)
	claims.mutex.Unlock()
	if len(composer.Calls()) != 0 || sends.Load() != 0 || claimCount != 0 {
		t.Fatalf(
			"rejected version effects: compositions=%d sends=%d claims=%d",
			len(composer.Calls()), sends.Load(), claimCount,
		)
	}
}

func TestDaemonRejectsNullScalarBeforeEffects(t *testing.T) {
	var sends atomic.Int32
	client := &http.Client{Transport: pipelineRoundTripFunc(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})}
	composer := &recordingComposer{result: ComposeResult{Body: "must not compose"}}
	claims := newClaimStore(nil)
	running := startTestDaemon(t, Daemon{
		Pipeline: Pipeline{
			Composer: composer,
			Sender:   Sender{URL: "http://notify.test/topic", Client: client},
			Log:      DecisionLog{Path: filepath.Join(t.TempDir(), "decisions.jsonl")}, Host: "host",
		},
		claims: claims,
	})
	wire := replaceFrameWireField(
		t, completionFrame("strict-scalars", "id", "fallback"),
		`"goal_active":false`, `"goal_active":null`,
	)
	ack, err := exchangeTestWire(running.socket, wire)
	if err != nil || ack != (Ack{Version: 1, Status: ackStatusRejected}) {
		t.Fatalf("ack/error = %+v/%v", ack, err)
	}
	time.Sleep(20 * time.Millisecond)
	claims.mutex.Lock()
	claimCount := len(claims.entries)
	claims.mutex.Unlock()
	if len(composer.Calls()) != 0 || sends.Load() != 0 || claimCount != 0 {
		t.Fatalf(
			"rejected scalar effects: compositions=%d sends=%d claims=%d",
			len(composer.Calls()), sends.Load(), claimCount,
		)
	}
}
