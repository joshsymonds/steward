package notify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseHookInput(t *testing.T) {
	t.Run("valid full object", func(t *testing.T) {
		in, err := ParseHookInput(strings.NewReader(`{
			"session_id": "sess-1",
			"transcript_path": "/tmp/sess-1.jsonl",
			"cwd": "/home/josh/proj",
			"hook_event_name": "Stop",
			"notification_type": "idle_prompt",
			"message": "hello",
			"last_assistant_message": "all done",
			"agent_id": "agent-1",
			"agent_type": "worker"
		}`))
		if err != nil {
			t.Fatalf("ParseHookInput: unexpected error: %v", err)
		}
		want := HookInput{
			SchemaVersion:        1,
			Harness:              harnessClaude,
			SessionID:            "sess-1",
			TranscriptPath:       "/tmp/sess-1.jsonl",
			CWD:                  "/home/josh/proj",
			HookEventName:        "Stop",
			NotificationType:     "idle_prompt",
			Message:              "hello",
			LastAssistantMessage: "all done",
			AgentID:              "agent-1",
			AgentType:            "worker",
		}
		if in != want {
			t.Errorf("ParseHookInput mismatch\ngot:  %+v\nwant: %+v", in, want)
		}
	})

	t.Run("unknown fields ignored", func(t *testing.T) {
		in, err := ParseHookInput(strings.NewReader(`{
			"session_id": "sess-2",
			"hook_event_name": "SessionEnd",
			"some_future_field": {"nested": true},
			"another_one": [1, 2, 3]
		}`))
		if err != nil {
			t.Fatalf("ParseHookInput: unexpected error: %v", err)
		}
		want := HookInput{
			SchemaVersion: 1,
			Harness:       harnessClaude,
			SessionID:     "sess-2",
			HookEventName: "SessionEnd",
		}
		if in != want {
			t.Errorf("ParseHookInput mismatch\ngot:  %+v\nwant: %+v", in, want)
		}
	})

	t.Run("legacy Codex agent turn complete is rejected", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{
			"type": "agent-turn-complete",
			"thread-id": "019c-thread-1",
			"turn-id": "turn-7"
		}`))
		if err == nil {
			t.Fatal("legacy Codex payload was accepted")
		}
	})

	t.Run("explicit native Codex harness is rejected", func(t *testing.T) {
		_, err := ParseHookInputForHarness(strings.NewReader(`{
			"session_id":"session-1","turn_id":"turn-1","hook_event_name":"Stop"
		}`), "codex")
		if err == nil {
			t.Fatal("native Codex harness was accepted")
		}
	})

	t.Run("canonical Codex harness is rejected", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{
			"schema_version":1,"harness":"codex","session_id":"session-1",
			"completion_id":"turn-1","hook_event_name":"TurnComplete"
		}`))
		if err == nil {
			t.Fatal("canonical Codex payload was accepted")
		}
	})

	t.Run("unsupported provider event errors", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{"type":"future-event","thread-id":"thread-1"}`))
		if err == nil || !strings.Contains(err.Error(), "unsupported notification type") {
			t.Fatalf("ParseHookInput error = %v, want unsupported notification type", err)
		}
	})

	t.Run("empty input errors", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(""))
		if err == nil {
			t.Fatal("ParseHookInput: expected error for empty input, got nil")
		}
	})

	t.Run("malformed JSON errors", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{"session_id": "sess-3",`))
		if err == nil {
			t.Fatal("ParseHookInput: expected error for malformed JSON, got nil")
		}
	})

	// SessionID keys the daemon's in-memory dedupe state and is written
	// verbatim into every decision-log record, and validSessionID also
	// guards a future filesystem-path use. An empty, separator-carrying, or
	// traversal-carrying session_id must never reach any of that: these
	// three forms must all error rather than decode.
	t.Run("empty session_id errors", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{"session_id": "", "hook_event_name": "Stop"}`))
		if err == nil {
			t.Fatal("ParseHookInput: expected error for empty session_id, got nil")
		}
	})

	t.Run("session_id with path separator errors", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{"session_id": "sess/../evil", "hook_event_name": "Stop"}`))
		if err == nil {
			t.Fatal("ParseHookInput: expected error for session_id with path separator, got nil")
		}
	})

	t.Run("session_id with .. errors", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{"session_id": "sess..evil", "hook_event_name": "Stop"}`))
		if err == nil {
			t.Fatal("ParseHookInput: expected error for session_id containing .., got nil")
		}
	})

	// "." carries no separator and no "..", but filepath.Join(base, ".")
	// collapses to base itself, so a future path use keyed on session_id
	// could collide across sessions. It must be rejected explicitly.
	t.Run("session_id of dot errors", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{"session_id": ".", "hook_event_name": "Stop"}`))
		if err == nil {
			t.Fatal("ParseHookInput: expected error for session_id \".\", got nil")
		}
	})

	t.Run("session_id with trailing slash errors", func(t *testing.T) {
		_, err := ParseHookInput(strings.NewReader(`{"session_id": "abc/", "hook_event_name": "Stop"}`))
		if err == nil {
			t.Fatal("ParseHookInput: expected error for session_id with trailing slash, got nil")
		}
	})

	// Two top-level objects in the stream: json.Decoder.Decode reads exactly
	// one object and leaves the rest in the buffer unread, so the first
	// object wins and the second is simply ignored — that is the chosen,
	// documented behavior, not an error case.
	t.Run("two objects input: first object wins", func(t *testing.T) {
		in, err := ParseHookInput(strings.NewReader(
			`{"session_id": "first", "hook_event_name": "Stop"}` +
				`{"session_id": "second", "hook_event_name": "SessionEnd"}`))
		if err != nil {
			t.Fatalf("ParseHookInput: unexpected error: %v", err)
		}
		want := HookInput{
			SchemaVersion: 1,
			Harness:       harnessClaude,
			SessionID:     "first",
			HookEventName: "Stop",
		}
		if in != want {
			t.Errorf("ParseHookInput mismatch\ngot:  %+v\nwant: %+v", in, want)
		}
	})
}

func TestParseHookInputRejectsCanonicalPresenceAndInvalidUTF8(t *testing.T) {
	cases := []string{
		`{"schema_version":0,"harness":"claude-code","session_id":"s","hook_event_name":"Stop"}`,
		`{"schema_version":1,"session_id":"s","hook_event_name":"Stop"}`,
		`{"harness":"claude-code","session_id":"s","hook_event_name":"Stop"}`,
		`{"schema_version":1,"harness":"","session_id":"s","hook_event_name":"Stop"}`,
		`{"schema_version":1,"harness":null,"session_id":"s","hook_event_name":"Stop"}`,
		`{"schema_version":"1","harness":"pi","session_id":"s","hook_event_name":"TurnComplete","completion_id":"c"}`,
		`{"schema_version":2,"harness":"pi","session_id":"s","hook_event_name":"TurnComplete","completion_id":"c"}`,
		`{"schema_version":1,"harness":"unknown","session_id":"s","hook_event_name":"TurnComplete","completion_id":"c"}`,
		`{"schema_version":1,"harness":"pi","session_id":"s","hook_event_name":"Stop","completion_id":"c"}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseHookInput(strings.NewReader(raw)); err == nil {
				t.Fatal("expected generic parse error")
			}
		})
	}
	raw := []byte(`{"session_id":"s","hook_event_name":"Stop","completion_id":"`)
	raw = append(raw, 0xff)
	raw = append(raw, []byte(`"}`)...)
	if _, err := ParseHookInput(bytes.NewReader(raw)); err == nil {
		t.Fatal("expected invalid UTF-8 error")
	}
}

func TestParseCanonicalHookErrorsAreGenericAndBounded(t *testing.T) {
	const marker = "SECRET-PAYLOAD-MARKER-THAT-MUST-NOT-BE-ECHOED"
	cases := []struct {
		name   string
		raw    string
		source string
	}{
		{
			name: "unknown version",
			raw:  `{"schema_version":2,"harness":"pi","payload":"` + marker + `"}`,
		},
		{
			name: "null harness",
			raw:  `{"schema_version":1,"harness":null,"payload":"` + marker + `"}`,
		},
		{
			name: "wrong typed version",
			raw:  `{"schema_version":"1","harness":"pi","payload":"` + marker + `"}`,
		},
		{
			name: "source mismatch",
			raw: `{"schema_version":1,"harness":"pi","session_id":"s",` +
				`"completion_id":"c","hook_event_name":"TurnComplete","payload":"` + marker + `"}`,
			source: harnessClaude,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseHookInputForHarness(strings.NewReader(tt.raw), tt.source)
			if err == nil {
				t.Fatal("expected canonical parse error")
			}
			if strings.Contains(err.Error(), marker) || len(err.Error()) > 128 {
				t.Fatalf("parse error exposed input or was unbounded: %q", err)
			}
		})
	}
}

func TestParseHookInputForHarnessNativeIdentity(t *testing.T) {
	t.Run("canonical Pi requires completion id", func(t *testing.T) {
		_, err := ParseHookInputForHarness(
			strings.NewReader(
				`{"schema_version":1,"harness":"pi","hook_event_name":"TurnComplete","session_id":"pi-1"}`,
			),
			"pi",
		)
		if err == nil {
			t.Fatal("expected missing completion_id error")
		}
	})
	t.Run("canonical Pi is distinct", func(t *testing.T) {
		in, err := ParseHookInputForHarness(
			strings.NewReader(
				`{"schema_version":1,"harness":"pi","hook_event_name":"TurnComplete","session_id":"pi-1","completion_id":"c-1","message":"latest"}`,
			),
			"pi",
		)
		if err != nil {
			t.Fatalf("ParseHookInputForHarness: %v", err)
		}
		if in.Harness != "pi" || in.CompletionID != "c-1" {
			t.Fatalf("input = %+v", in)
		}
	})
	t.Run("explicit mismatch fails", func(t *testing.T) {
		_, err := ParseHookInputForHarness(
			strings.NewReader(
				`{"schema_version":1,"harness":"pi","hook_event_name":"TurnComplete","session_id":"pi-1","completion_id":"c-1"}`,
			),
			"claude-code",
		)
		if err == nil {
			t.Fatal("expected harness mismatch error")
		}
	})
}

func TestParseCanonicalHookValidatedIdentityCannotBeOverwrittenByAliases(t *testing.T) {
	in, err := ParseHookInputForHarness(
		strings.NewReader(
			`{"schema_version":1,"Schema_Version":2,"harness":"pi","Harness":"codex",`+
				`"hook_event_name":"TurnComplete","session_id":"pi-1","completion_id":"c-1"}`,
		),
		harnessPi,
	)
	if err != nil {
		t.Fatalf("ParseHookInputForHarness: %v", err)
	}
	if in.SchemaVersion != 1 || in.Harness != harnessPi {
		t.Fatalf("validated canonical identity overwritten by aliases: %+v", in)
	}
}

func TestParseHookInputCompletionIdentityMatrix(t *testing.T) {
	valid256ASCII := strings.Repeat("a", 256)
	valid256Multibyte := strings.Repeat("é", 128)
	invalid257ASCII := valid256ASCII + "a"
	invalid257Multibyte := strings.Repeat("é", 127) + "€"

	valid := []struct {
		name string
		id   string
	}{
		{name: "one byte", id: "x"},
		{name: "opaque path-like value", id: "opaque/../completion"},
		{name: "256 ASCII bytes", id: valid256ASCII},
		{name: "256 multibyte bytes", id: valid256Multibyte},
	}
	for _, tt := range valid {
		t.Run("valid "+tt.name, func(t *testing.T) {
			raw, err := json.Marshal(HookInput{
				SchemaVersion: 1,
				Harness:       harnessPi,
				SessionID:     "pi-1",
				CompletionID:  tt.id,
				HookEventName: eventTurnComplete,
			})
			if err != nil {
				t.Fatal(err)
			}
			in, err := ParseHookInput(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("ParseHookInput: %v", err)
			}
			if in.CompletionID != tt.id {
				t.Fatalf("CompletionID = %q, want original opaque ID", in.CompletionID)
			}
		})
	}

	invalid := []struct {
		name string
		raw  string
	}{
		{
			name: "empty",
			raw:  `{"schema_version":1,"harness":"pi","session_id":"s","hook_event_name":"TurnComplete","completion_id":""}`,
		},
		{
			name: "null",
			raw:  `{"schema_version":1,"harness":"pi","session_id":"s","hook_event_name":"TurnComplete","completion_id":null}`,
		},
		{
			name: "wrong type",
			raw:  `{"schema_version":1,"harness":"pi","session_id":"s","hook_event_name":"TurnComplete","completion_id":42}`,
		},
		{
			name: "control",
			raw:  `{"schema_version":1,"harness":"pi","session_id":"s","hook_event_name":"TurnComplete","completion_id":"bad\nvalue"}`,
		},
	}
	for _, tt := range invalid {
		t.Run("invalid "+tt.name, func(t *testing.T) {
			if _, err := ParseHookInput(strings.NewReader(tt.raw)); err == nil {
				t.Fatal("ParseHookInput accepted invalid completion identity")
			}
		})
	}
	for name, id := range map[string]string{
		"257 ASCII bytes":     invalid257ASCII,
		"257 multibyte bytes": invalid257Multibyte,
	} {
		t.Run("invalid "+name, func(t *testing.T) {
			raw, err := json.Marshal(HookInput{
				SchemaVersion: 1,
				Harness:       harnessPi,
				SessionID:     "pi-1",
				CompletionID:  id,
				HookEventName: eventTurnComplete,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, parseErr := ParseHookInput(bytes.NewReader(raw)); parseErr == nil {
				t.Fatal("ParseHookInput accepted oversized completion identity")
			}
		})
	}

	rawInvalidUTF8 := []byte(
		`{"schema_version":1,"harness":"pi","session_id":"s","hook_event_name":"TurnComplete","completion_id":"`,
	)
	rawInvalidUTF8 = append(rawInvalidUTF8, 0xff)
	rawInvalidUTF8 = append(rawInvalidUTF8, []byte(`"}`)...)
	if _, err := ParseHookInput(bytes.NewReader(rawInvalidUTF8)); err == nil {
		t.Fatal("ParseHookInput accepted invalid UTF-8 completion identity")
	}
}

func TestParseHookInputSourceAndNativeTypeMatrix(t *testing.T) {
	t.Run("unknown explicit source", func(t *testing.T) {
		if _, err := ParseHookInputForHarness(strings.NewReader(`{}`), "future"); err == nil {
			t.Fatal("unknown explicit harness was accepted")
		}
	})

	t.Run("snake case turn ID does not infer native Codex", func(t *testing.T) {
		in, err := ParseHookInput(strings.NewReader(
			`{"session_id":"sess-1","turn_id":"turn-1","hook_event_name":"Stop"}`,
		))
		if err != nil {
			t.Fatalf("ParseHookInput: %v", err)
		}
		if in.Harness != harnessClaude || in.HookEventName != eventStop || in.CompletionID != "" {
			t.Fatalf("native Claude payload misclassified: %+v", in)
		}
	})

	t.Run("canonical Pi Stop rejected", func(t *testing.T) {
		raw := `{"schema_version":1,"harness":"pi","session_id":"s",` +
			`"completion_id":"c","hook_event_name":"Stop"}`
		if _, err := ParseHookInput(strings.NewReader(raw)); err == nil {
			t.Fatal("Pi Stop entered canonical TurnComplete path")
		}
	})
}
