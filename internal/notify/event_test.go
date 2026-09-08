package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareEventClaudeStopFreezesReliableTranscriptSnapshot(t *testing.T) {
	path := conversationTranscript(t, "assistant-uuid", "message-id", false)
	prepared, err := PrepareEvent(HookInput{
		Harness: harnessClaude, SessionID: "session", CompletionID: "stale-hook-id",
		CWD: "/work/project", HookEventName: eventStop, TranscriptPath: path,
		LastAssistantMessage: "stale hook assistant", AgentID: "agent", AgentType: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "session", Kind: eventKindCompletion,
		SourceEvent: eventStop, CWD: "/work/project", CompletionID: "assistant-uuid",
		User: "latest user text", Assistant: "latest assistant text",
		AgentID: "agent", AgentType: "worker",
	}
	if prepared != want {
		t.Fatalf("PrepareEvent() = %+v, want %+v", prepared, want)
	}
	if err = os.WriteFile(path, []byte("changed after preparation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if prepared.User != want.User || prepared.Assistant != want.Assistant ||
		prepared.CompletionID != want.CompletionID {
		t.Fatalf("prepared snapshot changed after transcript mutation: %+v", prepared)
	}
}

func TestPrepareEventClaudeStopDegradesWithoutReliableIdentity(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "absent"},
		{name: "unreadable", path: filepath.Join(t.TempDir(), "missing.jsonl")},
		{name: "empty", path: writeTranscript(t)},
		{name: "malformed", path: writeTranscript(t, "{")},
		{name: "identity absent", path: conversationTranscript(t, "", "", false)},
		{name: "identity unreliable", path: conversationTranscript(t, "assistant-uuid", "message-id", true)},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := PrepareEvent(HookInput{
				Harness: harnessClaude, SessionID: "session", CompletionID: "stale-id",
				CWD: "/work/project", HookEventName: eventStop, TranscriptPath: tt.path,
				LastAssistantMessage: "safe hook fallback",
			})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.CompletionID != "" || prepared.User != "" || prepared.Assistant != "safe hook fallback" {
				t.Fatalf("degraded event = %+v", prepared)
			}
		})
	}
}

func TestPrepareEventImplicitClaudeUsesNormalizedSourceForIdentity(t *testing.T) {
	for _, tt := range []struct {
		name         string
		completionID string
	}{
		{name: "absent supplied ID"},
		{name: "stale supplied ID", completionID: "stale-hook-id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := PrepareEvent(HookInput{
				SessionID: "implicit-claude", CompletionID: tt.completionID,
				CWD: "/work/project", HookEventName: eventStop,
				TranscriptPath:       conversationTranscript(t, "transcript-uuid", "message-id", false),
				LastAssistantMessage: "stale hook assistant",
			})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Harness != harnessClaude || prepared.CompletionID != "transcript-uuid" ||
				prepared.User != "latest user text" || prepared.Assistant != "latest assistant text" {
				t.Fatalf("prepared = %+v", prepared)
			}
		})
	}

	for _, tt := range []struct {
		name string
		path string
	}{
		{name: "unreadable", path: filepath.Join(t.TempDir(), "missing.jsonl")},
		{name: "unreliable", path: conversationTranscript(t, "transcript-uuid", "message-id", true)},
	} {
		t.Run(tt.name+" clears stale ID", func(t *testing.T) {
			prepared, err := PrepareEvent(HookInput{
				SessionID: "implicit-claude", CompletionID: "stale-hook-id",
				HookEventName: eventStop, TranscriptPath: tt.path,
				LastAssistantMessage: "safe fallback",
			})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.CompletionID != "" || prepared.Assistant != "safe fallback" {
				t.Fatalf("degraded prepared event = %+v", prepared)
			}
		})
	}
}

func TestPrepareEventCapturesStructuralKindsAndFacts(t *testing.T) {
	activePath := filepath.Join("testdata", "goal_active_set.jsonl")
	cases := []struct {
		name string
		in   HookInput
		kind string
		goal bool
	}{
		{
			name: "active Claude completion",
			in: HookInput{
				Harness: harnessClaude, SessionID: "s", HookEventName: eventStop,
				TranscriptPath: activePath,
			},
			kind: eventKindCompletion, goal: true,
		},
		{
			name: "Pi completion",
			in: HookInput{
				Harness: harnessPi, SessionID: "s", CompletionID: "turn",
				HookEventName: eventTurnComplete,
			},
			kind: eventKindCompletion,
		},
		{
			name: "input",
			in: HookInput{
				Harness: harnessClaude, SessionID: "s", HookEventName: eventNotification,
				NotificationType: "permission_prompt", Message: "allow?",
			},
			kind: eventKindInput,
		},
		{
			name: "cleanup",
			in: HookInput{
				Harness: harnessClaude, SessionID: "s", HookEventName: eventSessionEnd,
			},
			kind: eventKindCleanup,
		},
		{
			name: "unsupported notification",
			in: HookInput{
				Harness: harnessClaude, SessionID: "s", HookEventName: eventNotification,
				NotificationType: "idle_prompt",
			},
			kind: eventKindIgnored,
		},
		{
			name: "unsupported event",
			in: HookInput{
				Harness: harnessClaude, SessionID: "s", HookEventName: "FutureEvent",
			},
			kind: eventKindIgnored,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := PrepareEvent(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Version != 1 || prepared.Kind != tt.kind ||
				prepared.SourceEvent != tt.in.HookEventName || prepared.GoalActive != tt.goal {
				t.Fatalf("prepared = %+v", prepared)
			}
		})
	}
}

func TestPrepareEventBoundsCompositionTextWithDonationAndUTF8SafeTails(t *testing.T) {
	cases := []struct {
		name          string
		user          string
		assistant     string
		wantUserBytes int
		wantAsstBytes int
	}{
		{
			name: "equal reservations", user: strings.Repeat("u", 6000),
			assistant: strings.Repeat("a", 6000), wantUserBytes: 4096, wantAsstBytes: 4096,
		},
		{
			name: "user donates", user: strings.Repeat("u", 100),
			assistant: strings.Repeat("a", 9000), wantUserBytes: 100, wantAsstBytes: 8092,
		},
		{
			name: "assistant donates", user: strings.Repeat("u", 9000),
			assistant: strings.Repeat("a", 100), wantUserBytes: 8092, wantAsstBytes: 100,
		},
		{
			name: "multibyte tails", user: strings.Repeat("é", 5000),
			assistant: strings.Repeat("界", 3000), wantUserBytes: 4096, wantAsstBytes: 4095,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := PrepareEvent(HookInput{
				Harness: harnessPi, SessionID: "s", CompletionID: "id", HookEventName: eventTurnComplete,
				Message: tt.user, LastAssistantMessage: tt.assistant,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(prepared.User) != tt.wantUserBytes || len(prepared.Assistant) != tt.wantAsstBytes ||
				len(prepared.User)+len(prepared.Assistant) > maximumPreparedTextBytes ||
				!utf8.ValidString(prepared.User) || !utf8.ValidString(prepared.Assistant) {
				t.Fatalf(
					"prepared text lengths = %d+%d, valid=%v/%v",
					len(prepared.User), len(prepared.Assistant),
					utf8.ValidString(prepared.User), utf8.ValidString(prepared.Assistant),
				)
			}
			if !strings.HasSuffix(tt.user, prepared.User) || !strings.HasSuffix(tt.assistant, prepared.Assistant) {
				t.Fatal("prepared text did not retain UTF-8-safe tails")
			}
		})
	}
}

func TestPrepareEventNormalizesAndBoundsInputMessage(t *testing.T) {
	prepared, err := PrepareEvent(HookInput{
		Harness: harnessClaude, SessionID: "s", HookEventName: eventNotification,
		NotificationType: "permission_prompt",
		Message:          "# **Allow** [the command](https://invalid)?\n" + strings.Repeat("details ", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Message) > maximumPreparedInputBytes || !utf8.ValidString(prepared.Message) ||
		strings.ContainsAny(prepared.Message, "*[]`\n") || !strings.HasPrefix(prepared.Message, "Allow the command?") {
		t.Fatalf("prepared input message = %q (%d bytes)", prepared.Message, len(prepared.Message))
	}
}

func TestPreparedEventJSONUsesOnlyValueFieldsAndSnakeCase(t *testing.T) {
	wire, err := json.Marshal(PreparedEvent{Version: 1, SourceEvent: eventStop, GoalActive: true})
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{
		"version", "harness", "session_id", "kind", "source_event", "cwd",
		"completion_id", "user", "assistant", "notification_type", "message",
		"agent_id", "agent_type", "goal_active",
	}
	var fields map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal(wire, &fields); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if len(fields) != len(wantKeys) {
		t.Fatalf("fields = %v", fields)
	}
	for _, key := range wantKeys {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing JSON field %q in %s", key, wire)
		}
	}
	for _, forbidden := range []string{"transcript_path", "scan_result", "tasks", "environ", "pid", "token", "url"} {
		if strings.Contains(string(wire), forbidden) {
			t.Errorf("prepared event leaked %q: %s", forbidden, wire)
		}
	}
}

func TestValidatePreparedEventAcceptsExactMetadataAndTextBounds(t *testing.T) {
	completion := PreparedEvent{
		Version: 1, Harness: harnessPi, SessionID: strings.Repeat("s", 256),
		Kind: eventKindCompletion, SourceEvent: eventTurnComplete,
		CWD: strings.Repeat("c", 4096), CompletionID: strings.Repeat("i", 256),
		User: strings.Repeat("u", 4096), Assistant: strings.Repeat("a", 4096),
		AgentID: strings.Repeat("g", 256), AgentType: strings.Repeat("t", 256),
	}
	if err := validatePreparedEvent(completion); err != nil {
		t.Fatalf("exact completion bounds rejected: %v", err)
	}
	ignored := PreparedEvent{
		Version: 1, Harness: harnessClaude, SessionID: "s", Kind: eventKindIgnored,
		SourceEvent: strings.Repeat("e", 128), NotificationType: strings.Repeat("n", 128),
		Message: strings.Repeat("m", 160),
	}
	if err := validatePreparedEvent(ignored); err != nil {
		t.Fatalf("exact ignored/input metadata bounds rejected: %v", err)
	}
}

func TestValidatePreparedEventMetadataBoundsAndShapes(t *testing.T) {
	valid := PreparedEvent{
		Version: 1, Harness: harnessPi, SessionID: "", Kind: eventKindCompletion,
		SourceEvent: eventTurnComplete, CompletionID: "", Assistant: "fallback",
	}
	if err := validatePreparedEvent(valid); err != nil {
		t.Fatalf("empty identity should be accepted degraded: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*PreparedEvent)
	}{
		{name: "version", mutate: func(event *PreparedEvent) { event.Version = 2 }},
		{name: "harness", mutate: func(event *PreparedEvent) { event.Harness = "future" }},
		{name: "kind", mutate: func(event *PreparedEvent) { event.Kind = "future" }},
		{name: "session control", mutate: func(event *PreparedEvent) { event.SessionID = "bad\nvalue" }},
		{name: "session oversized", mutate: func(event *PreparedEvent) {
			event.SessionID = strings.Repeat("s", 257)
		}},
		{name: "completion control", mutate: func(event *PreparedEvent) {
			event.CompletionID = "bad\nvalue"
		}},
		{name: "completion oversized", mutate: func(event *PreparedEvent) {
			event.CompletionID = strings.Repeat("c", 257)
		}},
		{name: "cwd control", mutate: func(event *PreparedEvent) { event.CWD = "bad\x00path" }},
		{name: "cwd oversized", mutate: func(event *PreparedEvent) { event.CWD = strings.Repeat("c", 4097) }},
		{name: "source control", mutate: func(event *PreparedEvent) { event.SourceEvent = "Stop\n" }},
		{name: "source oversized", mutate: func(event *PreparedEvent) {
			event.SourceEvent = strings.Repeat("e", 129)
		}},
		{name: "type control", mutate: func(event *PreparedEvent) {
			event.NotificationType = "bad\n"
		}},
		{name: "type oversized", mutate: func(event *PreparedEvent) {
			event.NotificationType = strings.Repeat("t", 129)
		}},
		{name: "agent id oversized", mutate: func(event *PreparedEvent) {
			event.AgentID = strings.Repeat("a", 257)
		}},
		{name: "agent type control", mutate: func(event *PreparedEvent) { event.AgentType = "bad\n" }},
		{name: "text total", mutate: func(event *PreparedEvent) {
			event.User = strings.Repeat("u", 4097)
			event.Assistant = strings.Repeat("a", 4096)
		}},
		{name: "input source mismatch", mutate: func(event *PreparedEvent) {
			event.Kind = eventKindInput
			event.NotificationType = "permission_prompt"
			event.Assistant = ""
		}},
		{name: "input message over bound", mutate: func(event *PreparedEvent) {
			event.Harness = harnessClaude
			event.Kind = eventKindInput
			event.SourceEvent = eventNotification
			event.NotificationType = "permission_prompt"
			event.Assistant = ""
			event.Message = strings.Repeat("m", 161)
		}},
		{name: "conflicting completion type", mutate: func(event *PreparedEvent) {
			event.NotificationType = "permission_prompt"
		}},
		{name: "goal on Pi", mutate: func(event *PreparedEvent) { event.GoalActive = true }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			event := valid
			tt.mutate(&event)
			if err := validatePreparedEvent(event); err == nil ||
				err.Error() != "notify: invalid prepared event" {
				t.Fatalf("validatePreparedEvent(%+v) error = %v", event, err)
			}
		})
	}

	invalidUTF8 := valid
	invalidUTF8.Assistant = string([]byte{0xff})
	if err := validatePreparedEvent(invalidUTF8); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
}
