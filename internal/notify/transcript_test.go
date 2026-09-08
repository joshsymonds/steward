package notify

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("opening fixture %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = f.Close()
	})
	return f
}

func TestScanTranscriptGoalState(t *testing.T) {
	cases := []struct {
		fixture    string
		wantStatus GoalStatus
	}{
		{
			fixture:    "goal_none.jsonl",
			wantStatus: GoalNone,
		},
		{
			fixture:    "goal_active_set.jsonl",
			wantStatus: GoalActive,
		},
		{
			fixture:    "goal_active_iterating.jsonl",
			wantStatus: GoalActive,
		},
		{
			fixture:    "goal_met.jsonl",
			wantStatus: GoalMet,
		},
		{
			fixture:    "goal_cleared.jsonl",
			wantStatus: GoalCleared,
		},
		{
			fixture:    "goal_failed.jsonl",
			wantStatus: GoalFailed,
		},
	}

	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			f := openFixture(t, c.fixture)
			res, err := ScanTranscript(f)
			if err != nil {
				t.Fatalf("ScanTranscript(%s) returned error: %v", c.fixture, err)
			}
			if res.Goal.Status != c.wantStatus {
				t.Errorf("Status = %v, want %v", res.Goal.Status, c.wantStatus)
			}
		})
	}
}

func TestScanTranscriptMalformedLinesSkipped(t *testing.T) {
	f := openFixture(t, "malformed.jsonl")
	res, err := ScanTranscript(f)
	if err != nil {
		t.Fatalf("ScanTranscript returned error: %v", err)
	}
	if res.Goal.Status != GoalActive {
		t.Errorf("Status = %v, want GoalActive (from the one valid line)", res.Goal.Status)
	}
}

func TestScanTranscriptAssistantIdentity(t *testing.T) {
	tests := []struct {
		name          string
		transcript    string
		wantUUID      string
		wantMessageID string
		wantText      string
		wantReliable  bool
	}{
		{name: "empty input"},
		{
			name:          "distinct identities preserved",
			transcript:    `{"type":"assistant","uuid":"uuid-1","timestamp":"2026-07-05T01:00:00Z","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n",
			wantUUID:      "uuid-1",
			wantMessageID: "msg-1",
			wantText:      "done",
			wantReliable:  true,
		},
		{
			name: "identical text latest identity wins",
			transcript: `{"type":"assistant","uuid":"uuid-1","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"same"}]}}` + "\n" +
				`{"type":"assistant","uuid":"uuid-2","message":{"id":"msg-2","role":"assistant","content":[{"type":"text","text":"same"}]}}` + "\n",
			wantUUID:      "uuid-2",
			wantMessageID: "msg-2",
			wantText:      "same",
			wantReliable:  true,
		},
		{
			name: "newer user invalidates identity but preserves cached assistant metadata",
			transcript: `{"type":"assistant","uuid":"uuid-1","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"kept"}]}}` + "\n" +
				`{"type":"user","uuid":"user-uuid","message":{"id":"user-msg","role":"user","content":"hello there"}}` + "\n" +
				`{"type":"attachment","uuid":"attachment-uuid","attachment":{}}` + "\n" +
				`{"type":"system","uuid":"system-uuid","message":{"id":"system-msg","role":"system","content":"notice"}}` + "\n" +
				`{"type":"progress","uuid":"message-less"}` + "\n",
			wantUUID:      "uuid-1",
			wantMessageID: "msg-1",
			wantText:      "kept",
		},
		{
			name: "metadata-only tail preserves terminal assistant identity",
			transcript: `{"type":"assistant","uuid":"uuid-1","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"kept"}]}}` + "\n" +
				`{"type":"attachment","uuid":"attachment-uuid","attachment":{}}` + "\n" +
				`{"type":"system","uuid":"system-uuid","message":{"id":"system-msg","role":"system","content":"notice"}}` + "\n" +
				`{"type":"progress","uuid":"message-less"}` + "\n",
			wantUUID:      "uuid-1",
			wantMessageID: "msg-1",
			wantText:      "kept",
			wantReliable:  true,
		},
		{
			name: "next assistant restores identity after newer user",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"user","message":{"role":"user","content":"next question"}}` + "\n" +
				`{"type":"assistant","uuid":"new-uuid","message":{"id":"new-msg","role":"assistant","content":[{"type":"text","text":"new"}]}}` + "\n",
			wantUUID:      "new-uuid",
			wantMessageID: "new-msg",
			wantText:      "new",
			wantReliable:  true,
		},
		{
			name: "tool-result user tail invalidates identity",
			transcript: `{"type":"assistant","uuid":"uuid-1","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"kept"}]}}` + "\n" +
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"done"}]}}` + "\n",
			wantUUID:      "uuid-1",
			wantMessageID: "msg-1",
			wantText:      "kept",
		},
		{
			name: "missing uuid independently clears",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"assistant","message":{"id":"new-msg","role":"assistant","content":[{"type":"text","text":"new"}]}}` + "\n",
			wantMessageID: "new-msg",
			wantText:      "new",
			wantReliable:  true,
		},
		{
			name: "empty uuid independently clears",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"assistant","uuid":"","message":{"id":"new-msg","role":"assistant","content":[{"type":"text","text":"new"}]}}` + "\n",
			wantMessageID: "new-msg",
			wantText:      "new",
			wantReliable:  true,
		},
		{
			name: "empty message id independently clears",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"assistant","uuid":"new-uuid","message":{"id":"","role":"assistant","content":[{"type":"text","text":"new"}]}}` + "\n",
			wantUUID:     "new-uuid",
			wantText:     "new",
			wantReliable: true,
		},
		{
			name: "missing message id independently clears",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"assistant","uuid":"new-uuid","message":{"role":"assistant","content":[{"type":"text","text":"new"}]}}` + "\n",
			wantUUID:     "new-uuid",
			wantText:     "new",
			wantReliable: true,
		},
		{
			name: "both missing clear after known identities",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"new"}]}}` + "\n",
			wantText: "new",
		},
		{
			name: "both empty clear after known identities",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"assistant","uuid":"","message":{"id":"","role":"assistant","content":[{"type":"text","text":"new"}]}}` + "\n",
			wantText: "new",
		},
		{
			name: "tool-only assistant replaces identity retaining text",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"retained"}]}}` + "\n" +
				`{"type":"assistant","uuid":"tool-uuid","message":{"id":"tool-msg","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"true"}}]}}` + "\n",
			wantUUID:      "tool-uuid",
			wantMessageID: "tool-msg",
			wantText:      "retained",
			wantReliable:  true,
		},
		{
			name: "empty-content assistant replaces identity retaining text",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"retained"}]}}` + "\n" +
				`{"type":"assistant","uuid":"empty-uuid","message":{"id":"empty-msg","role":"assistant","content":[]}}` + "\n",
			wantUUID:      "empty-uuid",
			wantMessageID: "empty-msg",
			wantText:      "retained",
			wantReliable:  true,
		},
		{
			name: "wrong typed identities clear while useful fields survive",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"assistant","uuid":42,"timestamp":"2026-07-05T02:00:00Z","message":{"id":{"bad":true},"role":"assistant","content":[{"type":"text","text":"useful"},{"type":"tool_use","id":"tool-2","name":"Bash","input":{"command":"sleep 1","description":"task","run_in_background":true}}]}}` + "\n" +
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-2","content":"running"}]},"toolUseResult":{"backgroundTaskId":"task-2"}}` + "\n",
			wantText: "useful",
		},
		{
			name: "array and null identities clear independently",
			transcript: `{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n" +
				`{"type":"assistant","uuid":["bad"],"message":{"id":null,"role":"assistant","content":[{"type":"text","text":"still useful"}]}}` + "\n",
			wantText: "still useful",
		},
		{
			name:          "malformed JSON skipped then valid assistant captured",
			transcript:    "{not json}\n" + `{"type":"assistant","uuid":"valid-uuid","message":{"id":"valid-msg","role":"assistant","content":[{"type":"text","text":"valid"}]}}` + "\n",
			wantUUID:      "valid-uuid",
			wantMessageID: "valid-msg",
			wantText:      "valid",
			wantReliable:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := ScanTranscript(strings.NewReader(tt.transcript))
			if err != nil {
				t.Fatalf("ScanTranscript returned error: %v", err)
			}
			if res.LastAssistantUUID != tt.wantUUID {
				t.Errorf("LastAssistantUUID = %q, want %q", res.LastAssistantUUID, tt.wantUUID)
			}
			if res.LastAssistantMessageID != tt.wantMessageID {
				t.Errorf(
					"LastAssistantMessageID = %q, want %q",
					res.LastAssistantMessageID,
					tt.wantMessageID,
				)
			}
			if res.LastAssistantText != tt.wantText {
				t.Errorf("LastAssistantText = %q, want %q", res.LastAssistantText, tt.wantText)
			}
			if res.AssistantIdentityReliable != tt.wantReliable {
				t.Errorf(
					"AssistantIdentityReliable = %t, want %t",
					res.AssistantIdentityReliable,
					tt.wantReliable,
				)
			}
		})
	}
}

func TestScanTranscriptEmptyInput(t *testing.T) {
	res, err := ScanTranscript(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ScanTranscript(empty) returned error: %v", err)
	}
	if res.Goal.Status != GoalNone {
		t.Errorf("Status = %v, want GoalNone", res.Goal.Status)
	}
	if res.LastUserMessage != "" {
		t.Errorf("LastUserMessage = %q, want empty", res.LastUserMessage)
	}
	if res.LastAssistantText != "" {
		t.Errorf("LastAssistantText = %q, want empty", res.LastAssistantText)
	}
	if res.LastAssistantUUID != "" {
		t.Errorf("LastAssistantUUID = %q, want empty", res.LastAssistantUUID)
	}
	if res.LastAssistantMessageID != "" {
		t.Errorf("LastAssistantMessageID = %q, want empty", res.LastAssistantMessageID)
	}
}

// TestScanTranscriptConversationCapture retains conversation filtering, goal
// status, and tail-truncated assistant text coverage from the mixed fixture.
func TestScanTranscriptConversationCapture(t *testing.T) {
	const fixtureName = "conversation.jsonl"
	f := openFixture(t, fixtureName)
	res, err := ScanTranscript(f)
	if err != nil {
		t.Fatalf("ScanTranscript(%s) returned error: %v", fixtureName, err)
	}

	// Goal: composes with existing scanning.
	if res.Goal.Status != GoalActive {
		t.Errorf("Goal.Status = %v, want GoalActive", res.Goal.Status)
	}
	// LastUserMessage: the reminder-appended message's
	// typed text wins over both the later reminder-only and isMeta records.
	wantLastUser := "Go ahead and add the byte offset tracking too, we need it for the watchdog."
	if res.LastUserMessage != wantLastUser {
		t.Errorf("LastUserMessage = %q, want %q", res.LastUserMessage, wantLastUser)
	}
	// LastAssistantText: the second (long) assistant message, tail-truncated
	// to exactly 2000 bytes, with the END surviving.
	if len(res.LastAssistantText) != 2000 {
		t.Errorf("len(LastAssistantText) = %d, want 2000", len(res.LastAssistantText))
	}
	wantSuffix := "This is the FINAL-MARKER-END that must survive tail truncation."
	if !strings.HasSuffix(res.LastAssistantText, wantSuffix) {
		t.Errorf("LastAssistantText does not end with %q; got tail %q", wantSuffix, lastN(res.LastAssistantText, 80))
	}
	if strings.Contains(res.LastAssistantText, "On it") {
		t.Errorf(
			"LastAssistantText contains the first (superseded) assistant message: %q",
			lastN(res.LastAssistantText, 80),
		)
	}
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestTruncateWords(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{name: "within budget passes through", s: "short text", maxLen: 20, want: "short text"},
		{name: "exact budget passes through", s: "abcdef", maxLen: 6, want: "abcdef"},
		{
			name: "cuts at word boundary with ellipsis",
			s:    "run the criteria sweep then review", maxLen: 20, want: "run the criteria…",
		},
		{
			name: "single long token cuts at rune boundary",
			s:    "abcdefghijklmnopqrstuvwxyz", maxLen: 10, want: "abcdefg…",
		},
		{name: "never splits a multibyte rune", s: "héllo wörld hére wé gö ãgain", maxLen: 12, want: "héllo…"},
		{name: "tiny budget yields ellipsis", s: "abcdef", maxLen: 2, want: "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateWords(tt.s, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateWords(%q, %d) = %q, want %q", tt.s, tt.maxLen, got, tt.want)
			}
			if len(tt.s) > tt.maxLen && tt.maxLen >= len(truncationEllipsis) && len(got) > tt.maxLen {
				t.Errorf("truncateWords(%q, %d) = %q (%d bytes), exceeds budget", tt.s, tt.maxLen, got, len(got))
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateWords(%q, %d) = %q, invalid UTF-8", tt.s, tt.maxLen, got)
			}
		})
	}
}

func TestTruncateHeadWords(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{name: "within budget passes through", s: "short text", maxLen: 20, want: "short text"},
		{
			name: "keeps tail at word boundary with ellipsis",
			s:    "the meaningful ask is at the end", maxLen: 16, want: "…at the end",
		},
		{name: "single long token keeps rune-safe tail", s: "abcdefghijklmnopqrstuvwxyz", maxLen: 10, want: "…tuvwxyz"},
		{name: "never splits a multibyte rune", s: "wörldwörldwörldwörldwörld", maxLen: 10, want: "…dwörld"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateHeadWords(tt.s, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateHeadWords(%q, %d) = %q, want %q", tt.s, tt.maxLen, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateHeadWords(%q, %d) = %q, invalid UTF-8", tt.s, tt.maxLen, got)
			}
		})
	}
}

func TestScanTranscriptAssistantIdentityRejectsRawInvalidUTF8(t *testing.T) {
	raw := []byte(
		`{"type":"assistant","uuid":"old-uuid","message":{"id":"old-msg","role":"assistant",` +
			`"content":[{"type":"text","text":"old"}]}}` + "\n" +
			`{"type":"assistant","uuid":"`,
	)
	raw = append(raw, 0xff)
	raw = append(
		raw,
		[]byte(`","message":{"id":"msg-new","role":"assistant","content":[{"type":"text","text":"kept"}]}}`+"\n")...,
	)
	res, err := ScanTranscript(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ScanTranscript: %v", err)
	}
	if res.AssistantIdentityReliable || res.LastAssistantUUID != "" || res.LastAssistantMessageID != "" {
		t.Fatalf("invalid raw identity was accepted: %+v", res)
	}
	if res.LastAssistantText != "kept" {
		t.Fatalf("useful decoded data was discarded: %+v", res)
	}
}

func TestScanTranscriptRawInvalidUTF8TailMakesIdentityUnreliable(t *testing.T) {
	raw := []byte(
		`{"type":"assistant","uuid":"uuid-1","message":` +
			`{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"kept"}]}}` + "\n" +
			`{"type":"user","message":{"role":"user","content":"bad`,
	)
	raw = append(raw, 0xff)
	raw = append(raw, []byte(` encoding"}}`+"\n")...)
	res, err := ScanTranscript(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ScanTranscript: %v", err)
	}
	if res.AssistantIdentityReliable {
		t.Fatalf("identity remained reliable after invalidly encoded tail: %+v", res)
	}
	if res.LastAssistantText != "kept" || res.LastAssistantUUID != "uuid-1" {
		t.Fatalf("useful assistant data lost: %+v", res)
	}
}

func TestScanTranscriptValidAssistantRestoresIdentityReliability(t *testing.T) {
	raw := []byte(`{"type":"assistant","uuid":"`)
	raw = append(raw, 0xff)
	raw = append(
		raw,
		[]byte(
			`","message":{"id":"bad-msg","role":"assistant","content":[]}}`+"\n"+
				`{"type":"assistant","uuid":"good-uuid","message":`+
				`{"id":"good-msg","role":"assistant","content":[]}}`+"\n",
		)...,
	)
	res, err := ScanTranscript(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ScanTranscript: %v", err)
	}
	if !res.AssistantIdentityReliable || res.LastAssistantUUID != "good-uuid" ||
		res.LastAssistantMessageID != "good-msg" {
		t.Fatalf("valid assistant did not restore identity reliability: %+v", res)
	}
}

func TestScanTranscriptAssistantIdentityReliability(t *testing.T) {
	transcript := `{"type":"assistant","uuid":"uuid-1","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"kept"}]}}` + "\n" + `{bad tail}` + "\n"
	res, err := ScanTranscript(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ScanTranscript: %v", err)
	}
	if res.AssistantIdentityReliable {
		t.Fatalf("identity remained reliable after malformed tail: %+v", res)
	}
	if res.LastAssistantText != "kept" || res.LastAssistantUUID != "uuid-1" {
		t.Fatalf("useful assistant data lost: %+v", res)
	}
}
