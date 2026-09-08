package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/joshsymonds/steward/internal/output"
	"github.com/joshsymonds/steward/internal/shared"
)

const sessionMetadataTestFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type sessionMetadataSnapshot struct {
	Version                          int    `json:"version"`
	Harness                          string `json:"harness"`
	Session                          string `json:"session"`
	Label                            string `json:"label"`
	SourceGeneration                 uint64 `json:"source_generation"`
	LatestCompletionID               string `json:"latest_completion_id"`
	ExchangeCount                    uint64 `json:"exchange_count"`
	LastSuccessfulRefreshExchange    uint64 `json:"last_successful_refresh_exchange"`
	LastAttemptedMaterialFingerprint string `json:"last_attempted_material_fingerprint"`
}

type failSessionMetadataStdin struct {
	reads atomic.Int32
}

func (stdin *failSessionMetadataStdin) Read([]byte) (int, error) {
	stdin.reads.Add(1)
	return 0, errors.New("session-metadata must not read stdin")
}

type failedSessionMetadataWriter struct{}

func (failedSessionMetadataWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

type shortSessionMetadataWriter struct{}

func (shortSessionMetadataWriter) Write(data []byte) (int, error) {
	return len(data) - 1, nil
}

func sessionMetadataSnapshotName(harness, session string) string {
	encoded := make([]byte, 0, 2*binary.MaxVarintLen64+len(harness)+len(session))
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(harness)))
	encoded = append(encoded, harness...)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(session)))
	encoded = append(encoded, session...)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]) + ".json"
}

func writeSessionMetadataSnapshot(
	t *testing.T,
	stateBase string,
	snapshot sessionMetadataSnapshot,
	wireBytes int,
	mode os.FileMode,
) string {
	t.Helper()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if wireBytes == 0 {
		wireBytes = len(data)
	}
	if wireBytes < len(data) {
		t.Fatalf("wire size %d is less than snapshot size %d", wireBytes, len(data))
	}
	data = append(data, bytes.Repeat([]byte(" "), wireBytes-len(data))...)
	directory := filepath.Join(stateBase, "session-labels")
	if err = os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, sessionMetadataSnapshotName(snapshot.Harness, snapshot.Session))
	if err = os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func validSessionMetadataSnapshot(harness, session string) sessionMetadataSnapshot {
	return sessionMetadataSnapshot{
		Version:                          1,
		Harness:                          harness,
		Session:                          session,
		Label:                            "Shared Session Label",
		SourceGeneration:                 9,
		LatestCompletionID:               "native-completion-9",
		ExchangeCount:                    9,
		LastSuccessfulRefreshExchange:    5,
		LastAttemptedMaterialFingerprint: sessionMetadataTestFingerprint,
	}
}

func runSessionMetadataForTest(
	t *testing.T,
	args []string,
) (int, string, string, *failSessionMetadataStdin) {
	t.Helper()
	stdin := &failSessionMetadataStdin{}
	var stdout, stderr bytes.Buffer
	exitCode := runSessionMetadataCommandWithIO(args, stdin, &stdout, &stderr)
	return exitCode, stdout.String(), stderr.String(), stdin
}

func TestRunSessionMetadataKnownResponseIsStrictBoundedAndQuiet(t *testing.T) {
	stateBase := t.TempDir()
	snapshot := validSessionMetadataSnapshot("pi", "native-session")
	writeSessionMetadataSnapshot(t, stateBase, snapshot, 4096, 0o600)

	exitCode, stdout, stderr, stdin := runSessionMetadataForTest(t, []string{
		"--harness", "pi", "--session-id", snapshot.Session, "--state-base", stateBase,
	})
	want := `{"version":1,"status":"known","harness":"pi","session_id":"native-session",` +
		`"label":"Shared Session Label","completion_id":"native-completion-9",` +
		`"source_generation":"9","label_generation":"5"}` + "\n"
	if exitCode != 0 || stdout != want || stderr != "" {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q, want 0/%q/empty", exitCode, stdout, stderr, want)
	}
	if stdin.reads.Load() != 0 {
		t.Fatalf("stdin reads = %d, want zero", stdin.reads.Load())
	}
	if !utf8.ValidString(stdout) || len(stdout) > maximumSessionMetadataOutputBytes ||
		!strings.HasSuffix(stdout, "\n") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("invalid output framing: bytes=%d output=%q", len(stdout), stdout)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := []string{
		"version", "status", "harness", "session_id", "label", "completion_id",
		"source_generation", "label_generation",
	}
	if len(fields) != len(wantFields) {
		t.Fatalf("output field count = %d, want %d: %s", len(fields), len(wantFields), stdout)
	}
	for _, field := range wantFields {
		if _, ok := fields[field]; !ok {
			t.Errorf("output missing field %q: %s", field, stdout)
		}
	}
	for _, forbidden := range []string{
		"exchange_count", "last_successful_refresh_exchange", "fingerprint", "body",
		"text", "auth", stateBase,
	} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("output leaked %q: %s", forbidden, stdout)
		}
	}
}

func TestRunSessionMetadataRejectsCodexHarness(t *testing.T) {
	exitCode, stdout, _, _ := runSessionMetadataForTest(t, []string{
		"--state-base=" + t.TempDir(),
		"--session-id=session-1",
		"--harness=codex",
	})
	if exitCode != sessionMetadataInvalidRequestExitCode ||
		!strings.Contains(stdout, `"status":"invalid_request"`) {
		t.Fatalf("exit/stdout = %d/%q", exitCode, stdout)
	}
}

func TestRunSessionMetadataEmitsCanonicalFullUint64Strings(t *testing.T) {
	stateBase := t.TempDir()
	snapshot := validSessionMetadataSnapshot("pi", "maximum-session")
	snapshot.SourceGeneration = ^uint64(0)
	snapshot.ExchangeCount = ^uint64(0)
	snapshot.LastSuccessfulRefreshExchange = ^uint64(0)
	writeSessionMetadataSnapshot(t, stateBase, snapshot, 0, 0o600)

	exitCode, stdout, stderr, _ := runSessionMetadataForTest(t, []string{
		"--state-base=" + stateBase,
		"--session-id=" + snapshot.Session,
		"--harness=pi",
	})
	if exitCode != 0 || stderr != "" {
		t.Fatalf("exit/stderr = %d/%q", exitCode, stderr)
	}
	for _, field := range []string{
		`"source_generation":"18446744073709551615"`,
		`"label_generation":"18446744073709551615"`,
	} {
		if !strings.Contains(stdout, field) {
			t.Errorf("output missing exact generation %s: %s", field, stdout)
		}
	}
}

func TestRunSessionMetadataUsesDefaultNotifyStateBase(t *testing.T) {
	xdgStateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdgStateHome)
	stateBase := filepath.Join(xdgStateHome, "steward", "notify")
	snapshot := validSessionMetadataSnapshot("claude-code", "default-state-session")
	writeSessionMetadataSnapshot(t, stateBase, snapshot, 0, 0o600)

	exitCode, stdout, stderr, _ := runSessionMetadataForTest(t, []string{
		"--harness", snapshot.Harness, "--session-id", snapshot.Session,
	})
	if exitCode != 0 || stderr != "" || !strings.Contains(stdout, `"status":"known"`) ||
		!strings.Contains(stdout, `"harness":"claude-code"`) {
		t.Fatalf("default state result = %d/%q/%q", exitCode, stdout, stderr)
	}
}

func TestRunSessionMetadataMissingAndUnavailableAreValidScopedResponses(t *testing.T) {
	parent := t.TempDir()
	missingBase := filepath.Join(parent, "absent", "notify")
	corruptBase := t.TempDir()
	corrupt := validSessionMetadataSnapshot("pi", "native-session")
	corrupt.Version = 2
	corruptPath := writeSessionMetadataSnapshot(t, corruptBase, corrupt, 0, 0o600)
	oversizedBase := t.TempDir()
	oversized := validSessionMetadataSnapshot("pi", "native-session")
	oversizedPath := writeSessionMetadataSnapshot(t, oversizedBase, oversized, 4097, 0o600)
	unsafeBase := t.TempDir()
	unsafe := validSessionMetadataSnapshot("pi", "native-session")
	unsafePath := writeSessionMetadataSnapshot(t, unsafeBase, unsafe, 0, 0o644)

	tests := []struct {
		name      string
		stateBase string
		status    string
		path      string
	}{
		{name: "missing state base", stateBase: missingBase, status: "missing"},
		{name: "missing snapshot", stateBase: t.TempDir(), status: "missing"},
		{name: "corrupt snapshot", stateBase: corruptBase, status: "unavailable", path: corruptPath},
		{name: "oversized snapshot", stateBase: oversizedBase, status: "unavailable", path: oversizedPath},
		{name: "unsafe snapshot", stateBase: unsafeBase, status: "unavailable", path: unsafePath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var before []byte
			var beforeInfo os.FileInfo
			if tt.path != "" {
				var err error
				before, err = os.ReadFile(tt.path)
				if err != nil {
					t.Fatal(err)
				}
				beforeInfo, err = os.Stat(tt.path)
				if err != nil {
					t.Fatal(err)
				}
			}
			exitCode, stdout, stderr, stdin := runSessionMetadataForTest(t, []string{
				"--harness", "pi", "--session-id", "native-session", "--state-base", tt.stateBase,
			})
			want := `{"version":1,"status":"` + tt.status + `","harness":"pi",` +
				`"session_id":"native-session","label":"","completion_id":"",` +
				`"source_generation":"0","label_generation":"0"}` + "\n"
			if exitCode != 0 || stdout != want || stderr != "" || stdin.reads.Load() != 0 {
				t.Fatalf(
					"result = %d/%q/%q/reads%d, want 0/%q/empty/0",
					exitCode, stdout, stderr, stdin.reads.Load(), want,
				)
			}
			if tt.path != "" {
				after, err := os.ReadFile(tt.path)
				if err != nil {
					t.Fatal(err)
				}
				afterInfo, err := os.Stat(tt.path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(after, before) || afterInfo.ModTime() != beforeInfo.ModTime() ||
					afterInfo.Mode() != beforeInfo.Mode() {
					t.Fatalf("query mutated snapshot: before=%v after=%v", beforeInfo, afterInfo)
				}
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(parent, "absent")); !os.IsNotExist(err) {
		t.Fatalf("missing query created state: %v", err)
	}
}

func TestRunSessionMetadataRejectsInvalidRequestsBeforeIO(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing all flags"},
		{name: "missing harness", args: []string{"--session-id", "session"}},
		{name: "missing session", args: []string{"--harness", "pi"}},
		{name: "empty session", args: []string{"--harness", "pi", "--session-id", ""}},
		{name: "unsupported harness", args: []string{"--harness", "PI", "--session-id", "session"}},
		{
			name: "duplicate harness",
			args: []string{"--harness", "pi", "--harness=pi", "--session-id", "session"},
		},
		{
			name: "duplicate session",
			args: []string{"--harness", "pi", "--session-id", "session", "--session-id=session"},
		},
		{
			name: "duplicate state base",
			args: []string{
				"--harness", "pi", "--session-id", "session", "--state-base", "/a", "--state-base=/b",
			},
		},
		{
			name: "unknown flag",
			args: []string{"--harness", "pi", "--session-id", "session", "--other", "value"},
		},
		{
			name: "short unknown flag",
			args: []string{"--harness", "pi", "--session-id", "session", "-x"},
		},
		{name: "positional", args: []string{"--harness", "pi", "--session-id", "session", "extra"}},
		{
			name: "flag separator positional",
			args: []string{"--harness", "pi", "--session-id", "session", "--", "extra"},
		},
		{name: "missing harness value", args: []string{"--harness"}},
		{name: "missing session value", args: []string{"--harness", "pi", "--session-id"}},
		{
			name: "missing state value",
			args: []string{"--harness", "pi", "--session-id", "session", "--state-base"},
		},
		{name: "control session", args: []string{"--harness", "pi", "--session-id", "line\nbreak"}},
		{name: "invalid UTF-8 session", args: []string{"--harness", "pi", "--session-id", invalidUTF8}},
		{
			name: "257-byte session",
			args: []string{"--harness", "pi", "--session-id", strings.Repeat("s", 257)},
		},
		{
			name: "help mixed with query",
			args: []string{"--help", "--harness", "pi", "--session-id", "session"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateParent := t.TempDir()
			stateBase := filepath.Join(stateParent, "must-not-exist")
			args := append([]string{}, tt.args...)
			switch {
			case tt.name == "missing all flags":
				t.Setenv("XDG_STATE_HOME", stateParent)
				stateBase = filepath.Join(stateParent, "steward", "notify")
			case strings.Contains(tt.name, "state base") || tt.name == "missing state value":
			default:
				args = append(args, "--state-base", stateBase)
			}
			exitCode, stdout, stderr, stdin := runSessionMetadataForTest(t, args)
			const want = "{\"version\":1,\"status\":\"invalid_request\"}\n"
			if exitCode != sessionMetadataInvalidRequestExitCode || stdout != want || stderr != "" {
				t.Fatalf(
					"result = %d/%q/%q, want %d/%q/empty",
					exitCode, stdout, stderr, sessionMetadataInvalidRequestExitCode, want,
				)
			}
			if stdin.reads.Load() != 0 {
				t.Fatalf("stdin reads = %d, want zero", stdin.reads.Load())
			}
			if _, err := os.Lstat(stateBase); !os.IsNotExist(err) {
				t.Fatalf("invalid request touched state path: %v", err)
			}
		})
	}
}

func TestRunSessionMetadataAcceptsExactHarnessAndSessionBoundaries(t *testing.T) {
	stateBase := filepath.Join(t.TempDir(), "absent-state")
	for _, harness := range []string{"claude-code", "pi"} {
		sessions := []string{strings.Repeat("s", 256), strings.Repeat("é", 128), "日本語-session"}
		for _, session := range sessions {
			exitCode, stdout, stderr, stdin := runSessionMetadataForTest(t, []string{
				"--harness", harness, "--session-id", session, "--state-base", stateBase,
			})
			if exitCode != 0 || stderr != "" || stdin.reads.Load() != 0 ||
				!strings.Contains(stdout, `"status":"missing"`) {
				t.Errorf(
					"valid boundary %q/%d = %d/%q/%q/reads%d",
					harness, len(session), exitCode, stdout, stderr, stdin.reads.Load(),
				)
			}
		}
	}
}

func TestRunSessionMetadataHelpIsDedicatedAndDoesNotReadStdin(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		exitCode, stdout, stderr, stdin := runSessionMetadataForTest(t, []string{flag})
		if exitCode != 0 || stderr != "" || stdin.reads.Load() != 0 {
			t.Fatalf("help %s = %d/%q/reads%d", flag, exitCode, stderr, stdin.reads.Load())
		}
		for _, text := range []string{
			"Usage:", "steward session-metadata", "--harness", "claude-code", "pi",
			"--session-id", "--state-base",
		} {
			if !strings.Contains(stdout, text) {
				t.Errorf("help missing %q: %s", text, stdout)
			}
		}
	}
}

func TestRunSessionMetadataReturnsQuietFailureForOutputWriteErrors(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		stdout   io.Writer
		wantCode int
	}{
		{
			name: "valid query failed writer",
			args: []string{
				"--harness", "pi", "--session-id", "native-id", "--state-base", t.TempDir(),
			},
			stdout: failedSessionMetadataWriter{}, wantCode: 1,
		},
		{
			name: "valid query short writer",
			args: []string{
				"--harness", "pi", "--session-id", "native-id", "--state-base", t.TempDir(),
			},
			stdout: shortSessionMetadataWriter{}, wantCode: 1,
		},
		{
			name: "help failed writer", args: []string{"--help"},
			stdout: failedSessionMetadataWriter{}, wantCode: 1,
		},
		{
			name: "help short writer", args: []string{"-h"},
			stdout: shortSessionMetadataWriter{}, wantCode: 1,
		},
		{name: "invalid failed writer", stdout: failedSessionMetadataWriter{}, wantCode: 1},
		{name: "invalid short writer", stdout: shortSessionMetadataWriter{}, wantCode: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin := &failSessionMetadataStdin{}
			var stderr bytes.Buffer
			exitCode := runSessionMetadataCommandWithIO(tt.args, stdin, tt.stdout, &stderr)
			if exitCode != tt.wantCode || stderr.Len() != 0 || stdin.reads.Load() != 0 {
				t.Fatalf(
					"write failure result = exit %d/stderr %q/stdin reads %d, want %d/empty/zero",
					exitCode, stderr.String(), stdin.reads.Load(), tt.wantCode,
				)
			}
		})
	}
}

func TestWriteSessionMetadataJSONReportsEncodingAndOversizeFailures(t *testing.T) {
	tests := []struct {
		name     string
		response any
	}{
		{name: "encoding error", response: map[string]any{"unsupported": make(chan int)}},
		{name: "oversized output", response: map[string]string{"value": strings.Repeat("x", 2048)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := writeSessionMetadataJSON(&stdout, tt.response); err == nil {
				t.Fatal("writeSessionMetadataJSON error = nil, want failure")
			}
			if stdout.Len() != 0 {
				t.Fatalf("failed serialization wrote %d bytes: %q", stdout.Len(), stdout.String())
			}
		})
	}
}

func TestMainUsageListsSessionMetadataCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	printUsage(output.NewTerminal(&stdout, &stderr))
	if !strings.Contains(stderr.String(), "session-metadata") ||
		!strings.Contains(stderr.String(), "Read shared session naming metadata") {
		t.Fatalf("main help missing session-metadata command: %s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("main help stdout/stderr = %q/%q", stdout.String(), stderr.String())
	}
}

func TestSessionMetadataCommandNeverInvokesHelperOrSenderAndNeverMutatesState(t *testing.T) {
	stateBase := t.TempDir()
	snapshot := validSessionMetadataSnapshot("pi", "quiet-session")
	path := writeSessionMetadataSnapshot(t, stateBase, snapshot, 0, 0o600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	helperMarker := filepath.Join(t.TempDir(), "helper-called")
	helper := filepath.Join(t.TempDir(), "fake-helper")
	if err = os.WriteFile(helper, []byte("#!/bin/sh\nprintf called >\"$HELPER_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEWARD_HELPER_BIN", helper)
	t.Setenv("HELPER_MARKER", helperMarker)
	var senderCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		senderCalls.Add(1)
	}))
	t.Cleanup(server.Close)
	t.Setenv("STEWARD_NTFY_URL", server.URL)

	for range 5 {
		exitCode, _, stderr, _ := runSessionMetadataForTest(t, []string{
			"--harness", snapshot.Harness, "--session-id", snapshot.Session, "--state-base", stateBase,
		})
		if exitCode != 0 || stderr != "" {
			t.Fatalf("query result = %d/%q", exitCode, stderr)
		}
	}
	if senderCalls.Load() != 0 {
		t.Fatalf("sender calls = %d, want zero", senderCalls.Load())
	}
	if _, err = os.Lstat(helperMarker); !os.IsNotExist(err) {
		t.Fatalf("helper marker exists: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || afterInfo.ModTime() != beforeInfo.ModTime() ||
		afterInfo.Mode() != beforeInfo.Mode() || afterInfo.Size() != beforeInfo.Size() {
		t.Fatalf("repeated commands changed state: before=%v after=%v", beforeInfo, afterInfo)
	}
	var afterSnapshot sessionMetadataSnapshot
	if err = json.Unmarshal(after, &afterSnapshot); err != nil {
		t.Fatal(err)
	}
	if afterSnapshot.SourceGeneration != snapshot.SourceGeneration ||
		afterSnapshot.ExchangeCount != snapshot.ExchangeCount ||
		afterSnapshot.LastSuccessfulRefreshExchange != snapshot.LastSuccessfulRefreshExchange {
		t.Fatalf("repeated commands changed counters: before=%+v after=%+v", snapshot, afterSnapshot)
	}
}

func TestSessionMetadataActualBinaryDispatchAndExitDoNotWaitForStdin(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "steward")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building steward test binary: %v: %s", err, output)
	}

	tests := []struct {
		name          string
		args          []string
		wantCode      int
		wantOut       string
		preserveDebug bool
	}{
		{
			name: "valid dispatch",
			args: []string{
				"session-metadata", "--harness", "pi", "--session-id", "native-id",
				"--state-base", filepath.Join(t.TempDir(), "missing"),
			},
			wantOut: `{"version":1,"status":"missing","harness":"pi","session_id":"native-id",` +
				`"label":"","completion_id":"","source_generation":"0","label_generation":"0"}` + "\n",
			preserveDebug: true,
		},
		{
			name:     "invalid dispatch exits two",
			args:     []string{"session-metadata", "--harness", "unsupported", "--session-id", "native-id"},
			wantCode: sessionMetadataInvalidRequestExitCode,
			wantOut:  "{\"version\":1,\"status\":\"invalid_request\"}\n",
		},
		{
			name: "help dispatch",
			args: []string{"session-metadata", "--help"},
			wantOut: `Usage:
  steward session-metadata --harness <claude-code|pi> --session-id <native-id> [--state-base <path>]

Read validated shared session naming metadata for one exact harness/session pair.
The command never reads stdin or modifies notification state.
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			workingDirectory := t.TempDir()
			debugPath := shared.GetDebugLogPathForDir(workingDirectory)
			if _, err := os.Lstat(debugPath); !os.IsNotExist(err) {
				t.Fatalf("test debug path is not fresh: %s: %v", debugPath, err)
			}
			t.Cleanup(func() { _ = os.Remove(debugPath) })

			const debugSentinel = "existing debug log must remain unchanged\n"
			var debugInfo os.FileInfo
			if tt.preserveDebug {
				if err := os.WriteFile(debugPath, []byte(debugSentinel), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				debugInfo, err = os.Stat(debugPath)
				if err != nil {
					t.Fatal(err)
				}
			}

			command := exec.Command(binary, tt.args...)
			command.Dir = workingDirectory
			command.Env = []string{
				"STEWARD_DEBUG=1",
				"HOME=" + home,
				"XDG_STATE_HOME=" + filepath.Join(home, "state"),
			}
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			wait := make(chan error, 1)
			go func() { wait <- command.Wait() }()
			select {
			case err = <-wait:
			case <-time.After(3 * time.Second):
				_ = command.Process.Kill()
				_ = stdin.Close()
				t.Fatal("session-metadata waited for open stdin")
			}
			_ = stdin.Close()
			gotCode := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatalf("waiting for command: %v", err)
				}
				gotCode = exitError.ExitCode()
			}
			if gotCode != tt.wantCode || stdout.String() != tt.wantOut || stderr.Len() != 0 {
				t.Fatalf(
					"binary result = %d/%q/%q, want %d/%q/empty",
					gotCode, stdout.String(), stderr.String(), tt.wantCode, tt.wantOut,
				)
			}

			for _, directory := range []string{home, workingDirectory} {
				entries, readErr := os.ReadDir(directory)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("session-metadata changed %s: entries=%v error=%v", directory, entries, readErr)
				}
			}
			if tt.preserveDebug {
				got, readErr := os.ReadFile(debugPath)
				if readErr != nil {
					t.Fatal(readErr)
				}
				afterInfo, statErr := os.Stat(debugPath)
				if statErr != nil {
					t.Fatal(statErr)
				}
				if string(got) != debugSentinel || afterInfo.ModTime() != debugInfo.ModTime() ||
					afterInfo.Mode() != debugInfo.Mode() {
					t.Fatalf(
						"session-metadata modified debug log: before=%v after=%v body=%q",
						debugInfo, afterInfo, got,
					)
				}
			} else if _, statErr := os.Lstat(debugPath); !os.IsNotExist(statErr) {
				t.Fatalf("session-metadata created debug log: %s: %v", debugPath, statErr)
			}
		})
	}
}

func TestSessionMetadataDebugExclusionPreservesOtherCommandLogging(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "steward")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building steward test binary: %v: %s", err, output)
	}

	home := t.TempDir()
	workingDirectory := t.TempDir()
	debugPath := shared.GetDebugLogPathForDir(workingDirectory)
	if _, err := os.Lstat(debugPath); !os.IsNotExist(err) {
		t.Fatalf("test debug path is not fresh: %s: %v", debugPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(debugPath) })

	command := exec.Command(binary, "version")
	command.Dir = workingDirectory
	command.Env = []string{"STEWARD_DEBUG=1", "HOME=" + home}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("running version command: %v", err)
	}
	if stdout.String() != "steward dev\n" || stderr.Len() != 0 {
		t.Fatalf("version result = %q/%q", stdout.String(), stderr.String())
	}
	debugLog, err := os.ReadFile(debugPath)
	if err != nil {
		t.Fatalf("reading debug log: %v", err)
	}
	if !strings.Contains(string(debugLog), "Command: version\n") {
		t.Fatalf("version debug log missing command: %q", debugLog)
	}
}

var _ io.Reader = (*failSessionMetadataStdin)(nil)
