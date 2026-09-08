package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const piHelperScript = `#!/bin/sh
if [ -n "$PI_STUB_CALL_LOG" ]; then
  printf '%s\n' 'call' >> "$PI_STUB_CALL_LOG"
fi
if [ -n "$PI_STUB_PID_FILE" ]; then
  printf '%s\n' "$$" > "$PI_STUB_PID_FILE"
fi
if [ -n "$PI_STUB_ARGS_FILE" ]; then
  : > "$PI_STUB_ARGS_FILE"
  for arg in "$@"; do
    printf '%s\n' "$arg" >> "$PI_STUB_ARGS_FILE"
  done
fi

if [ "$PI_STUB_MODE" = "no_read_hang" ]; then
  exec sleep 30
fi
if [ -n "$PI_STUB_STDIN_FILE" ]; then
  cat > "$PI_STUB_STDIN_FILE"
else
  cat > /dev/null
fi

case "$PI_STUB_MODE" in
  hang)
    exec sleep 30
    ;;
  overflow_forever)
    while :; do
      printf '%s' 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'
    done
    ;;
  invalid_utf8)
    printf '\377\n'
    exit "${PI_STUB_EXIT:-0}"
    ;;
esac

if [ -n "$PI_STUB_STDERR" ]; then
  printf '%s' "$PI_STUB_STDERR" >&2
fi
if [ -n "$PI_STUB_STDOUT" ]; then
  printf '%s' "$PI_STUB_STDOUT"
fi
exit "${PI_STUB_EXIT:-0}"
`

type piWireRequest struct {
	Version   int          `json:"version"`
	Operation string       `json:"operation"`
	Model     ComposeModel `json:"model"`
	Input     ComposeInput `json:"input"`
	Label     ComposeLabel `json:"label"`
}

func writePiHelper(t *testing.T) string {
	t.Helper()
	return writePiHelperAt(t, filepath.Join(t.TempDir(), "steward-pi-helper"))
}

func writePiHelperAt(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating helper directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(piHelperScript), 0o755); err != nil {
		t.Fatalf("writing helper: %v", err)
	}
	return path
}

func setPiOutput(t *testing.T, output string) {
	t.Helper()
	t.Setenv("PI_STUB_STDOUT", output)
}

func assertPiFailure(t *testing.T, got ComposeResult, err error, forbidden ...string) string {
	t.Helper()
	if err == nil {
		t.Fatal("Compose() error = nil, want bounded safe error")
	}
	if got != (ComposeResult{}) {
		t.Errorf("Compose() result = %+v, want zero value on error", got)
	}
	message := err.Error()
	if message == "" || len(message) > 128 {
		t.Errorf("Compose() error length = %d for %q, want 1..128", len(message), message)
	}
	for _, value := range forbidden {
		if value != "" && strings.Contains(message, value) {
			t.Errorf("Compose() error %q disclosed forbidden value %q", message, value)
		}
	}
	return message
}

func readPiRequest(t *testing.T, path string) (piWireRequest, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading helper stdin: %v", err)
	}
	var request piWireRequest
	if decodeErr := json.Unmarshal(raw, &request); decodeErr != nil {
		t.Fatalf("decoding helper stdin %q: %v", raw, decodeErr)
	}
	return request, raw
}

func piSuccess(body string, label *string) string {
	output := struct {
		Version int     `json:"version"`
		OK      bool    `json:"ok"`
		Body    string  `json:"body"`
		Label   *string `json:"label,omitempty"`
	}{Version: 1, OK: true, Body: body, Label: label}
	encoded, err := json.Marshal(output)
	if err != nil {
		return ""
	}
	return string(encoded) + "\n"
}

func testUTF8Tail(value string, budget int) string {
	if len(value) <= budget {
		return value
	}
	start := len(value) - budget
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func TestNewPiComposer_DefaultsOverridesAndNoAliases(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		got, err := NewPiComposer(nil)
		if err != nil {
			t.Fatalf("NewPiComposer() error = %v", err)
		}
		want := PiComposer{
			Bin: "steward-pi-helper",
			Model: ComposeModel{
				Provider: "openai-codex",
				ID:       "gpt-5.6-luna",
				Thinking: "low",
			},
		}
		if got != want {
			t.Errorf("NewPiComposer() = %+v, want %+v", got, want)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		environ := []string{
			"STEWARD_HELPER_BIN=/opt/pi helper",
			"STEWARD_MODEL_PROVIDER=test-provider",
			"STEWARD_MODEL_ID=test-model",
			"STEWARD_MODEL_THINKING=xhigh",
		}
		got, err := NewPiComposer(environ)
		if err != nil {
			t.Fatalf("NewPiComposer() error = %v", err)
		}
		want := PiComposer{
			Bin: "/opt/pi helper",
			Model: ComposeModel{
				Provider: "test-provider",
				ID:       "test-model",
				Thinking: "xhigh",
			},
		}
		if got != want {
			t.Errorf("NewPiComposer() = %+v, want %+v", got, want)
		}
	})

	t.Run("legacy aliases ignored", func(t *testing.T) {
		environ := []string{
			"STEWARD_PI_HELPER=legacy-helper",
			"STEWARD_PI_MODEL=legacy-model",
			"ANTHROPIC_SMALL_FAST_MODEL=legacy-claude",
		}
		got, err := NewPiComposer(environ)
		if err != nil {
			t.Fatalf("NewPiComposer() error = %v", err)
		}
		if got.Bin != "steward-pi-helper" || got.Model.ID != "gpt-5.6-luna" {
			t.Errorf("NewPiComposer() honored a legacy alias: %+v", got)
		}
	})
}

func TestNewPiComposer_RejectsExplicitInvalidSettingsWithoutFallback(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name    string
		environ []string
		secret  string
	}{
		{name: "empty helper", environ: []string{"STEWARD_HELPER_BIN="}},
		{name: "invalid helper utf8", environ: []string{"STEWARD_HELPER_BIN=" + invalidUTF8}},
		{name: "helper control", environ: []string{"STEWARD_HELPER_BIN=bad\nhelper"}, secret: "bad"},
		{name: "empty provider", environ: []string{"STEWARD_MODEL_PROVIDER="}},
		{
			name:    "provider too long",
			environ: []string{"STEWARD_MODEL_PROVIDER=" + strings.Repeat("P", 257)},
			secret:  "PPP",
		},
		{name: "provider control", environ: []string{"STEWARD_MODEL_PROVIDER=provider\x7fsecret"}, secret: "secret"},
		{name: "provider invalid utf8", environ: []string{"STEWARD_MODEL_PROVIDER=" + invalidUTF8}},
		{name: "empty id", environ: []string{"STEWARD_MODEL_ID="}},
		{name: "id too long by utf8 bytes", environ: []string{"STEWARD_MODEL_ID=" + strings.Repeat("月", 86)}},
		{name: "id control", environ: []string{"STEWARD_MODEL_ID=id\x01secret"}, secret: "secret"},
		{name: "id invalid utf8", environ: []string{"STEWARD_MODEL_ID=" + invalidUTF8}},
		{name: "empty thinking", environ: []string{"STEWARD_MODEL_THINKING="}},
		{name: "unknown thinking", environ: []string{"STEWARD_MODEL_THINKING=ultra-secret"}, secret: "ultra-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewPiComposer(test.environ)
			if err == nil {
				t.Fatal("NewPiComposer() error = nil, want invalid configuration error")
			}
			if got != (PiComposer{}) {
				t.Errorf("NewPiComposer() = %+v, want zero value", got)
			}
			if message := err.Error(); message == "" || len(message) > 128 ||
				(test.secret != "" && strings.Contains(message, test.secret)) {
				t.Errorf("NewPiComposer() error = %q, want bounded and generic", message)
			}
		})
	}
}

func TestNewPiComposer_AcceptsEveryThinkingLevelAndByteBoundaries(t *testing.T) {
	for _, thinking := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		t.Run(thinking, func(t *testing.T) {
			got, err := NewPiComposer([]string{"STEWARD_MODEL_THINKING=" + thinking})
			if err != nil {
				t.Fatalf("NewPiComposer() error = %v", err)
			}
			if got.Model.Thinking != thinking {
				t.Errorf("Thinking = %q, want %q", got.Model.Thinking, thinking)
			}
		})
	}

	provider := strings.Repeat("p", 256)
	id := strings.Repeat("月", 85) + "x"
	got, err := NewPiComposer([]string{
		"STEWARD_MODEL_PROVIDER=" + provider,
		"STEWARD_MODEL_ID=" + id,
	})
	if err != nil {
		t.Fatalf("NewPiComposer() boundary error = %v", err)
	}
	if got.Model.Provider != provider || got.Model.ID != id || len(got.Model.ID) != 256 {
		t.Errorf("NewPiComposer() boundary model = %+v", got.Model)
	}
}

func TestPiComposer_ComposeUsesOnlyConfiguredExecutableAndExactWireProtocol(t *testing.T) {
	helper := writePiHelperAt(t, filepath.Join(t.TempDir(), "helper dir", "configured;helper"))
	argsFile := filepath.Join(t.TempDir(), "args")
	stdinFile := filepath.Join(t.TempDir(), "stdin")
	callLog := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PI_STUB_ARGS_FILE", argsFile)
	t.Setenv("PI_STUB_STDIN_FILE", stdinFile)
	t.Setenv("PI_STUB_CALL_LOG", callLog)
	setPiOutput(t, `{"version":1,"ok":true,"body":"Exact body","label":"Three word label"}`+"\n")

	composer, err := NewPiComposer([]string{
		"STEWARD_HELPER_BIN=" + helper,
		"STEWARD_MODEL_PROVIDER=source-provider",
		"STEWARD_MODEL_ID=source-model",
		"STEWARD_MODEL_THINKING=max",
	})
	if err != nil {
		t.Fatalf("NewPiComposer() error = %v", err)
	}
	input := ComposeInput{User: "latest user", Assistant: "latest assistant"}
	label := ComposeLabel{Current: "Current label here", Refresh: true}
	got, err := composer.Compose(context.Background(), input, label)
	if err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	if want := (ComposeResult{Body: "Exact body", Label: "Three word label"}); got != want {
		t.Errorf("Compose() = %+v, want %+v", got, want)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading argv capture: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("helper argv = %q, want no arguments", args)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("reading call log: %v", err)
	}
	if string(calls) != "call\n" {
		t.Errorf("helper calls = %q, want exactly one", calls)
	}

	raw, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("reading stdin capture: %v", err)
	}
	wantWire := `{"version":1,"operation":"compose","model":{"provider":"source-provider","id":"source-model","thinking":"max"},"input":{"user":"latest user","assistant":"latest assistant"},"label":{"current":"Current label here","refresh":true}}` + "\n"
	if string(raw) != wantWire {
		t.Errorf("helper stdin = %q, want exact wire %q", raw, wantWire)
	}
	if len(raw) > 64*1024 {
		t.Errorf("helper stdin size = %d, want at most 64 KiB", len(raw))
	}
}

func TestPiComposer_ComposeResolvesConfiguredBareExecutableOnPath(t *testing.T) {
	helperDirectory := t.TempDir()
	writePiHelperAt(t, filepath.Join(helperDirectory, "custom-pi-helper"))
	t.Setenv("PATH", helperDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	setPiOutput(t, piSuccess("Resolved helper.", nil))
	composer := PiComposer{
		Bin:   "custom-pi-helper",
		Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
	}
	got, err := composer.Compose(context.Background(), ComposeInput{}, ComposeLabel{})
	if err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	if got != (ComposeResult{Body: "Resolved helper."}) {
		t.Errorf("Compose() = %+v", got)
	}
}

func TestPiComposer_ComposeClipsCombinedInputBudgetsAtUTF8Tail(t *testing.T) {
	helper := writePiHelper(t)
	stdinFile := filepath.Join(t.TempDir(), "stdin")
	t.Setenv("PI_STUB_STDIN_FILE", stdinFile)
	setPiOutput(t, piSuccess("Clipped successfully.", nil))
	composer := PiComposer{
		Bin:   helper,
		Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
	}

	tests := []struct {
		name          string
		input         ComposeInput
		wantUserBytes int
		wantAsstBytes int
	}{
		{
			name: "both reserve four kibibytes",
			input: ComposeInput{
				User:      "discard-user-" + strings.Repeat("U", 5000),
				Assistant: "discard-assistant-" + strings.Repeat("A", 5000),
			},
			wantUserBytes: 4096,
			wantAsstBytes: 4096,
		},
		{
			name: "short user gives unused budget to assistant",
			input: ComposeInput{
				User:      "月",
				Assistant: "discard-assistant-" + strings.Repeat("A", 9000),
			},
			wantUserBytes: 3,
			wantAsstBytes: 8189,
		},
		{
			name: "short assistant gives unused budget to user",
			input: ComposeInput{
				User:      "discard-user-" + strings.Repeat("U", 9000),
				Assistant: "ok",
			},
			wantUserBytes: 8190,
			wantAsstBytes: 2,
		},
		{
			name: "tail starts only at utf8 boundary",
			input: ComposeInput{
				User:      "discard" + strings.Repeat("月", 2000),
				Assistant: strings.Repeat("A", 4096),
			},
			wantUserBytes: 4095,
			wantAsstBytes: 4096,
		},
		{
			name: "escaped json remains inside wire budget",
			input: ComposeInput{
				User:      "discard" + strings.Repeat("<", 5000),
				Assistant: "discard" + strings.Repeat("&", 5000),
			},
			wantUserBytes: 4096,
			wantAsstBytes: 4096,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := composer.Compose(
				context.Background(),
				test.input,
				ComposeLabel{Refresh: false},
			)
			if err != nil {
				t.Fatalf("Compose() error = %v", err)
			}
			if got != (ComposeResult{Body: "Clipped successfully."}) {
				t.Errorf("Compose() = %+v", got)
			}
			request, raw := readPiRequest(t, stdinFile)
			if request.Input.User != testUTF8Tail(test.input.User, 8192-min(len(test.input.Assistant), 4096)) {
				t.Errorf("user input is not the expected UTF-8 tail")
			}
			if request.Input.Assistant != testUTF8Tail(test.input.Assistant, 8192-min(len(test.input.User), 4096)) {
				t.Errorf("assistant input is not the expected UTF-8 tail")
			}
			if len(request.Input.User) != test.wantUserBytes || len(request.Input.Assistant) != test.wantAsstBytes {
				t.Errorf(
					"clipped byte lengths = user %d assistant %d, want %d and %d",
					len(request.Input.User), len(request.Input.Assistant), test.wantUserBytes, test.wantAsstBytes,
				)
			}
			if !utf8.ValidString(request.Input.User) || !utf8.ValidString(request.Input.Assistant) {
				t.Errorf("clipped input is invalid UTF-8: %+v", request.Input)
			}
			if len(request.Input.User)+len(request.Input.Assistant) > 8192 {
				t.Errorf("combined input size = %d, want <= 8192", len(request.Input.User)+len(request.Input.Assistant))
			}
			if len(raw) > 64*1024 {
				t.Errorf("wire size = %d, want <= 64 KiB", len(raw))
			}
		})
	}
}

func TestPiComposer_ComposeRejectsInvalidInputAndLabelBeforeExecution(t *testing.T) {
	helper := writePiHelper(t)
	callLog := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PI_STUB_CALL_LOG", callLog)
	setPiOutput(t, piSuccess("Should not execute.", nil))
	composer := PiComposer{
		Bin:   helper,
		Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
	}
	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name  string
		input ComposeInput
		label ComposeLabel
	}{
		{name: "invalid user utf8", input: ComposeInput{User: invalidUTF8}},
		{
			name:  "invalid user utf8 in discarded prefix",
			input: ComposeInput{User: invalidUTF8 + strings.Repeat("u", 9000)},
		},
		{name: "invalid assistant utf8", input: ComposeInput{Assistant: invalidUTF8}},
		{
			name:  "invalid assistant utf8 in discarded prefix",
			input: ComposeInput{Assistant: invalidUTF8 + strings.Repeat("a", 9000)},
		},
		{name: "label over 120 bytes", label: ComposeLabel{Current: strings.Repeat("L", 121), Refresh: true}},
		{name: "invalid label utf8", label: ComposeLabel{Current: invalidUTF8, Refresh: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := composer.Compose(context.Background(), test.input, test.label)
			assertPiFailure(t, got, err, invalidUTF8, test.label.Current)
		})
	}
	if calls, err := os.ReadFile(callLog); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("helper call log = %q, %v; want helper never executed", calls, err)
	}

	stdinFile := filepath.Join(t.TempDir(), "stdin")
	t.Setenv("PI_STUB_STDIN_FILE", stdinFile)
	got, err := composer.Compose(
		context.Background(),
		ComposeInput{},
		ComposeLabel{Current: strings.Repeat("月", 40), Refresh: false},
	)
	if err != nil {
		t.Fatalf("Compose() 120-byte label error = %v", err)
	}
	if got.Body != "Should not execute." {
		t.Errorf("Compose() boundary result = %+v", got)
	}
	request, _ := readPiRequest(t, stdinFile)
	if request.Label.Current != strings.Repeat("月", 40) {
		t.Errorf("120-byte current label was rewritten: %q", request.Label.Current)
	}
}

func TestPiComposer_ComposeReturnsFirstLabelKeepAndNoRefreshSemantics(t *testing.T) {
	tests := []struct {
		name   string
		label  ComposeLabel
		output string
		want   ComposeResult
	}{
		{
			name:   "first generated label",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("First outcome.", pointerTo("First useful label")),
			want:   ComposeResult{Body: "First outcome.", Label: "First useful label"},
		},
		{
			name:   "existing omission means keep",
			label:  ComposeLabel{Current: "Existing useful label", Refresh: true},
			output: piSuccess("Keep outcome.", nil),
			want:   ComposeResult{Body: "Keep outcome."},
		},
		{
			name:   "no refresh omits update",
			label:  ComposeLabel{Current: "Existing useful label", Refresh: false},
			output: piSuccess("No refresh outcome.", nil),
			want:   ComposeResult{Body: "No refresh outcome."},
		},
		{
			name:   "output is not rewritten",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("  Plain outcome stays.  ", pointerTo("Three  word label")),
			want:   ComposeResult{Body: "  Plain outcome stays.  ", Label: "Three  word label"},
		},
		{
			name:   "javascript unicode whitespace is preserved",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("Unicode spacing stays.", pointerTo("One\ufefftwo three")),
			want:   ComposeResult{Body: "Unicode spacing stays.", Label: "One\ufefftwo three"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			helper := writePiHelper(t)
			setPiOutput(t, test.output)
			composer := PiComposer{
				Bin:   helper,
				Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
			}
			got, err := composer.Compose(context.Background(), ComposeInput{}, test.label)
			if err != nil {
				t.Fatalf("Compose() error = %v", err)
			}
			if got != test.want {
				t.Errorf("Compose() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestPiComposer_ComposeStrictlyRejectsMalformedAndConflictingResults(t *testing.T) {
	body181 := strings.Repeat("月", 181)
	oversized := strings.Repeat("x", 4096) + "\n"
	tests := []struct {
		name   string
		label  ComposeLabel
		output string
		mode   string
	}{
		{name: "empty stdout"},
		{name: "malformed json", output: "OUTPUT_SECRET not json\n"},
		{name: "json null", output: "null\n"},
		{name: "top level array", output: "[]\n"},
		{name: "missing newline", output: `{"version":1,"ok":true,"body":"Body"}`},
		{name: "blank trailing line", output: `{"version":1,"ok":true,"body":"Body"}` + "\n\n"},
		{
			name: "second object",
			output: `{"version":1,"ok":true,"body":"Body"}` + "\n" +
				`{"version":1,"ok":true,"body":"Other"}` + "\n",
		},
		{name: "unknown success field", output: `{"version":1,"ok":true,"body":"Body","extra":1}` + "\n"},
		{name: "conflicting success error", output: `{"version":1,"ok":true,"body":"Body","error":"timeout"}` + "\n"},
		{name: "duplicate body", output: `{"version":1,"ok":true,"body":"Body","body":"Other"}` + "\n"},
		{name: "wrong version", output: `{"version":2,"ok":true,"body":"Body"}` + "\n"},
		{name: "fractional version", output: `{"version":1.0,"ok":true,"body":"Body"}` + "\n"},
		{name: "null version", output: `{"version":null,"ok":true,"body":"Body"}` + "\n"},
		{name: "wrong ok type", output: `{"version":1,"ok":"true","body":"Body"}` + "\n"},
		{name: "null ok", output: `{"version":1,"ok":null,"body":"Body"}` + "\n"},
		{name: "missing body", output: `{"version":1,"ok":true}` + "\n"},
		{name: "null body", output: `{"version":1,"ok":true,"body":null}` + "\n"},
		{name: "empty body", output: piSuccess("", nil)},
		{name: "whitespace body", output: piSuccess(" \t ", nil)},
		{name: "bom whitespace body", output: piSuccess("\ufeff", nil)},
		{name: "body over 180 codepoints", output: piSuccess(body181, nil)},
		{name: "body control", output: `{"version":1,"ok":true,"body":"bad\u0001body"}` + "\n"},
		{name: "lone surrogate body", output: `{"version":1,"ok":true,"body":"bad\ud800body"}` + "\n"},
		{name: "body markdown heading", output: piSuccess("# Heading", nil)},
		{name: "body markdown bullet", output: piSuccess("- item", nil)},
		{name: "body markdown numbered item", output: piSuccess("1. item", nil)},
		{name: "body markdown inline code", output: piSuccess("Ran `go test`.", nil)},
		{name: "body markdown emphasis", output: piSuccess("This is **bold**.", nil)},
		{name: "body markdown underscore emphasis", output: piSuccess("This is _italic_.", nil)},
		{name: "body markdown strikethrough", output: piSuccess("This is ~~obsolete~~.", nil)},
		{name: "body markdown after unicode space", output: piSuccess("\u00a0**bold**", nil)},
		{name: "body markdown heading after unicode space", output: piSuccess("\u00a0# Heading", nil)},
		{name: "body markdown link", output: piSuccess("See [report](https://example.com).", nil)},
		{name: "body markdown blockquote", output: piSuccess("> quote", nil)},
		{name: "body markdown horizontal rule", output: piSuccess("---", nil)},
		{name: "body markdown indented code", output: piSuccess("    code", nil)},
		{name: "body markdown autolink", output: piSuccess("See <https://example.com>.", nil)},
		{name: "body markdown fence", output: piSuccess("```text\nvalue\n```", nil)},
		{name: "oversized stdout including newline", output: oversized},
		{name: "invalid stdout utf8", mode: "invalid_utf8"},
		{
			name:   "refresh false must omit label",
			label:  ComposeLabel{Refresh: false},
			output: piSuccess("Body", pointerTo("Three word label")),
		},
		{
			name:   "empty current refresh requires label",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("Body", nil),
		},
		{
			name:   "literal keep is not generated label",
			label:  ComposeLabel{Current: "Existing useful label", Refresh: true},
			output: piSuccess("Body", pointerTo("KEEP")),
		},
		{
			name:   "generated label too few words",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("Body", pointerTo("Two words")),
		},
		{
			name:   "generated label too many words",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("Body", pointerTo("These are five label words")),
		},
		{
			name:   "generated label over 60 bytes",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("Body", pointerTo("This label "+strings.Repeat("x", 50)+" words")),
		},
		{
			name:   "generated label control",
			label:  ComposeLabel{Refresh: true},
			output: `{"version":1,"ok":true,"body":"Body","label":"Three\u007fword label"}` + "\n",
		},
		{
			name:   "generated label lone surrogate",
			label:  ComposeLabel{Refresh: true},
			output: `{"version":1,"ok":true,"body":"Body","label":"Three word \ud800"}` + "\n",
		},
		{
			name:   "generated label markdown",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("Body", pointerTo("Three **bold** words")),
		},
		{
			name:   "generated label markdown strikethrough",
			label:  ComposeLabel{Refresh: true},
			output: piSuccess("Body", pointerTo("Review ~~obsolete~~ outcome")),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			helper := writePiHelper(t)
			t.Setenv("PI_STUB_MODE", test.mode)
			setPiOutput(t, test.output)
			composer := PiComposer{
				Bin:   helper,
				Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
			}
			got, err := composer.Compose(context.Background(), ComposeInput{}, test.label)
			assertPiFailure(t, got, err, "OUTPUT_SECRET", body181)
		})
	}
}

func TestPiComposer_ComposeRejectsEveryBidiControlInGeneratedOutput(t *testing.T) {
	bidiControls := []rune{
		'\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
	}
	for _, control := range bidiControls {
		for _, output := range []string{
			piSuccess("Safe"+string(control)+" outcome", pointerTo("Three safe words")),
			piSuccess("Safe outcome", pointerTo("Three"+string(control)+" safe words")),
		} {
			helper := writePiHelper(t)
			setPiOutput(t, output)
			composer := PiComposer{
				Bin: helper, Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
			}
			got, err := composer.Compose(context.Background(), ComposeInput{}, ComposeLabel{Refresh: true})
			assertPiFailure(t, got, err)
		}
	}
}

func TestPiComposer_PipelineRejectsBidiOutputWithoutPersistingGeneratedLabel(t *testing.T) {
	helper := writePiHelper(t)
	setPiOutput(t, piSuccess("Reordered\u202e body", pointerTo("Unsafe generated label")))
	composer := &PiComposer{
		Bin: helper, Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
	}
	server, requests := captureNotificationServer(t)
	defer server.Close()
	pipeline, logPath := testPipeline(t, server, composer)
	stateBase := t.TempDir()
	pipeline.LabelStore = NewLabelStore(stateBase)

	event := preparedCompletion(
		harnessPi, "bidi-session", "completion-1", "/work/fallback-project", "user", "assistant result",
	)
	if err := pipeline.RunPrepared(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	notification := waitNotification(t, requests)
	if notification.Body != "assistant result" || notification.Title != "fallback-project · earth:3" {
		t.Errorf("notification = %+v, want deterministic fallback without generated label", notification)
	}
	label, err := pipeline.LabelStore.lookupLabel(harnessPi, "bidi-session")
	if err != nil {
		t.Fatalf("lookupLabel() error = %v", err)
	}
	if label != "" {
		t.Errorf("persisted label = %q, want empty", label)
	}
	record := readDecisionLog(t, logPath)[0]
	if record.CompositionOutcome != compositionFallback || record.CompositionError != compositionErrorInvalidProtocol {
		t.Errorf("record = %+v, want invalid-protocol fallback", record)
	}
}

func TestPiComposer_ComposeAcceptsPlainTextBoundaries(t *testing.T) {
	body := strings.Repeat("月", 180)
	label := "One two " + strings.Repeat("x", 52)
	if len(label) != 60 {
		t.Fatalf("test label is %d bytes, want 60", len(label))
	}
	helper := writePiHelper(t)
	setPiOutput(t, piSuccess(body, &label))
	composer := PiComposer{
		Bin:   helper,
		Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
	}
	got, err := composer.Compose(context.Background(), ComposeInput{}, ComposeLabel{Refresh: true})
	if err != nil {
		t.Fatalf("Compose() error = %v", err)
	}
	if got != (ComposeResult{Body: body, Label: label}) {
		t.Errorf("Compose() = %+v, want boundary output preserved", got)
	}

	body = "Café 👩‍💻 completed successfully."
	label = "Café 👩‍💻 result"
	setPiOutput(t, piSuccess(body, &label))
	got, err = composer.Compose(context.Background(), ComposeInput{}, ComposeLabel{Refresh: true})
	if err != nil {
		t.Fatalf("Compose() ordinary Unicode error = %v", err)
	}
	if got != (ComposeResult{Body: body, Label: label}) {
		t.Errorf("Compose() = %+v, want ordinary Unicode preserved", got)
	}

	body = "Version ~1 keeps task~id and unmatched~~ punctuation."
	label = "Track task~id status"
	setPiOutput(t, piSuccess(body, &label))
	got, err = composer.Compose(context.Background(), ComposeInput{}, ComposeLabel{Refresh: true})
	if err != nil {
		t.Fatalf("Compose() literal tilde error = %v", err)
	}
	if got != (ComposeResult{Body: body, Label: label}) {
		t.Errorf("Compose() = %+v, want literal tildes preserved", got)
	}
}

func TestPiComposer_ComposeEnforcesFailureEnumsAndExitAgreement(t *testing.T) {
	known := []struct {
		code    string
		message string
	}{
		{code: "invalid_request", message: "pi composer: helper rejected request"},
		{code: "unavailable_model", message: "pi composer: helper model unavailable"},
		{code: "generation_failed", message: "pi composer: helper generation failed"},
		{code: "timeout", message: "pi composer: helper reported timeout"},
		{code: "invalid_output", message: "pi composer: helper output invalid"},
	}
	seenMessages := make(map[string]string, len(known))
	for _, failure := range known {
		t.Run(failure.code, func(t *testing.T) {
			helper := writePiHelper(t)
			t.Setenv("PI_STUB_EXIT", "1")
			t.Setenv("PI_STUB_STDERR", "HELPER_DIAGNOSTIC_SECRET")
			setPiOutput(t, `{"version":1,"ok":false,"error":"`+failure.code+`"}`+"\n")
			composer := PiComposer{
				Bin:   helper,
				Model: ComposeModel{Provider: "CONFIG_PROVIDER_SECRET", ID: "model", Thinking: "low"},
			}
			got, err := composer.Compose(context.Background(), ComposeInput{}, ComposeLabel{})
			message := assertPiFailure(t, got, err, "HELPER_DIAGNOSTIC_SECRET", "CONFIG_PROVIDER_SECRET")
			if message != failure.message {
				t.Errorf("failure error = %q, want %q", message, failure.message)
			}
			if previous, duplicate := seenMessages[message]; duplicate {
				t.Errorf("failure %q has same error category as %q: %q", failure.code, previous, message)
			}
			seenMessages[message] = failure.code
		})
	}

	tests := []struct {
		name   string
		output string
		exit   string
	}{
		{
			name:   "failure with zero exit",
			output: `{"version":1,"ok":false,"error":"timeout"}` + "\n",
		},
		{
			name:   "success with nonzero exit has no partial data",
			output: piSuccess("PARTIAL_BODY_SECRET", nil),
			exit:   "1",
		},
		{
			name:   "unknown failure enum",
			output: `{"version":1,"ok":false,"error":"PROVIDER_SECRET"}` + "\n",
			exit:   "1",
		},
		{
			name:   "failure has unknown field",
			output: `{"version":1,"ok":false,"error":"timeout","body":"CONFLICT_SECRET"}` + "\n",
			exit:   "1",
		},
		{
			name:   "failure missing error",
			output: `{"version":1,"ok":false}` + "\n",
			exit:   "1",
		},
		{
			name:   "failure null error",
			output: `{"version":1,"ok":false,"error":null}` + "\n",
			exit:   "1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			helper := writePiHelper(t)
			t.Setenv("PI_STUB_EXIT", test.exit)
			setPiOutput(t, test.output)
			composer := PiComposer{
				Bin:   helper,
				Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
			}
			got, err := composer.Compose(context.Background(), ComposeInput{}, ComposeLabel{})
			message := assertPiFailure(
				t,
				got,
				err,
				"PARTIAL_BODY_SECRET",
				"PROVIDER_SECRET",
				"CONFLICT_SECRET",
				"timeout",
			)
			if message != "pi composer: invalid helper protocol" {
				t.Errorf("Compose() error = %q, want invalid helper protocol", message)
			}
		})
	}
}

func TestPiComposer_ComposeDiscardsDiagnosticsAndNeverRetries(t *testing.T) {
	helper := writePiHelper(t)
	callLog := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PI_STUB_CALL_LOG", callLog)
	t.Setenv("PI_STUB_STDERR", "STDERR_CREDENTIAL_SECRET")
	t.Setenv("PI_STUB_EXIT", "7")
	setPiOutput(t, "STDOUT_PROVIDER_SECRET\n")
	composer := PiComposer{
		Bin:   helper,
		Model: ComposeModel{Provider: "CONFIG_PROVIDER_SECRET", ID: "CONFIG_MODEL_SECRET", Thinking: "low"},
	}
	got, err := composer.Compose(
		context.Background(),
		ComposeInput{User: "INPUT_USER_SECRET", Assistant: "INPUT_ASSISTANT_SECRET"},
		ComposeLabel{},
	)
	assertPiFailure(
		t,
		got,
		err,
		"STDERR_CREDENTIAL_SECRET",
		"STDOUT_PROVIDER_SECRET",
		"CONFIG_PROVIDER_SECRET",
		"CONFIG_MODEL_SECRET",
		"INPUT_USER_SECRET",
		"INPUT_ASSISTANT_SECRET",
		"exit status",
	)
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("reading call log: %v", readErr)
	}
	if string(calls) != "call\n" {
		t.Errorf("helper calls = %q, want one attempt and no retry", calls)
	}
}

func TestPiComposer_ComposeCategorizesMissingHelperAndInvalidManualConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "MISSING_BIN_SECRET")
	composer := PiComposer{
		Bin:   missing,
		Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
	}
	missingResult, missingErr := composer.Compose(context.Background(), ComposeInput{}, ComposeLabel{})
	missingMessage := assertPiFailure(t, missingResult, missingErr, "MISSING_BIN_SECRET", missing, "no such file")
	if missingMessage != "pi composer: helper unavailable" {
		t.Errorf("missing helper error = %q, want helper unavailable", missingMessage)
	}

	invalid := []PiComposer{
		{},
		{Bin: "helper", Model: ComposeModel{Provider: "", ID: "model", Thinking: "low"}},
		{Bin: "helper", Model: ComposeModel{Provider: "provider", ID: "", Thinking: "low"}},
		{Bin: "helper", Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "SECRET_THINKING"}},
	}
	for index, value := range invalid {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			got, err := value.Compose(context.Background(), ComposeInput{}, ComposeLabel{})
			message := assertPiFailure(t, got, err, "SECRET_THINKING")
			if message != "pi composer: invalid configuration" {
				t.Errorf("invalid configuration error = %q", message)
			}
		})
	}
}

func TestPiComposer_ComposeCancelsHangsOverflowAndCallerContextPromptly(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		alreadyCancel bool
		input         ComposeInput
		wantError     string
	}{
		{name: "hung helper", mode: "hang", wantError: "pi composer: helper timed out"},
		{
			name:      "helper not reading stdin",
			mode:      "no_read_hang",
			input:     ComposeInput{User: strings.Repeat("u", 100000)},
			wantError: "pi composer: helper timed out",
		},
		{name: "unbounded stdout", mode: "overflow_forever", wantError: "pi composer: invalid helper protocol"},
		{name: "already canceled caller", alreadyCancel: true, wantError: "pi composer: helper canceled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			helper := writePiHelper(t)
			pidFile := filepath.Join(t.TempDir(), "pid")
			callLog := filepath.Join(t.TempDir(), "calls")
			t.Setenv("PI_STUB_PID_FILE", pidFile)
			t.Setenv("PI_STUB_CALL_LOG", callLog)
			t.Setenv("PI_STUB_MODE", test.mode)
			setPiOutput(t, piSuccess("Should not return.", nil))
			composer := PiComposer{
				Bin:   helper,
				Model: ComposeModel{Provider: "provider", ID: "model", Thinking: "low"},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			if test.alreadyCancel {
				cancel()
			} else {
				defer cancel()
			}
			started := time.Now()
			got, err := composer.Compose(ctx, test.input, ComposeLabel{})
			elapsed := time.Since(started)
			message := assertPiFailure(t, got, err)
			if message != test.wantError {
				t.Errorf("Compose() error = %q, want %q", message, test.wantError)
			}
			if elapsed > 2*time.Second {
				t.Errorf("Compose() returned after %s, want prompt cancellation", elapsed)
			}

			if test.alreadyCancel {
				if calls, readErr := os.ReadFile(callLog); readErr == nil || !errors.Is(readErr, os.ErrNotExist) {
					t.Errorf("already-canceled context executed helper: calls %q, error %v", calls, readErr)
				}
				return
			}
			pidRaw, readErr := os.ReadFile(pidFile)
			if readErr != nil {
				t.Fatalf("reading helper pid: %v", readErr)
			}
			pid := strings.TrimSpace(string(pidRaw))
			if _, statErr := os.Stat(filepath.Join("/proc", pid)); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("helper process %s was not reaped: %v", pid, statErr)
			}
			calls, readErr := os.ReadFile(callLog)
			if readErr != nil {
				t.Fatalf("reading helper calls: %v", readErr)
			}
			if string(calls) != "call\n" {
				t.Errorf("helper calls = %q, want exactly one", calls)
			}
		})
	}
}

func pointerTo(value string) *string {
	return &value
}
