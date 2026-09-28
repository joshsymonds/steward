package cost_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshsymonds/steward/internal/cost"
)

// rowOpts describes one synthetic transcript JSONL line for a test fixture.
// Zero-valued fields take the described defaults; see buildRow.
type rowOpts struct {
	rowType   string // defaults to "assistant"
	msgID     string // omitted from message when ""
	model     string
	requestID string // top-level requestId; omitted from JSON when "" and !nullReqID
	nullReqID bool   // emit `"requestId":null` explicitly
	sidechain bool
	timestamp string
	noMessage bool // emit `"message":null`
	noUsage   bool // omit message.usage entirely

	input, output, cacheRead, cacheWrite int
}

// buildRow renders one transcript JSONL line matching the real Claude Code
// shape described in the task brief, with just enough knobs to exercise the
// row filter, dedup, billing, and date-bucketing rules under test.
func buildRow(o rowOpts) string {
	rowType := o.rowType
	if rowType == "" {
		rowType = "assistant"
	}

	var sb strings.Builder
	sb.WriteString("{")
	fmt.Fprintf(&sb, `"type":%q,`, rowType)
	fmt.Fprintf(&sb, `"isSidechain":%t,`, o.sidechain)
	fmt.Fprintf(&sb, `"timestamp":%q,`, o.timestamp)
	switch {
	case o.nullReqID:
		sb.WriteString(`"requestId":null,`)
	case o.requestID != "":
		fmt.Fprintf(&sb, `"requestId":%q,`, o.requestID)
	}
	sb.WriteString(`"sessionId":"sess1",`)

	if o.noMessage {
		sb.WriteString(`"message":null`)
	} else {
		sb.WriteString(`"message":{`)
		if o.msgID != "" {
			fmt.Fprintf(&sb, `"id":%q,`, o.msgID)
		}
		fmt.Fprintf(&sb, `"model":%q`, o.model)
		if !o.noUsage {
			fmt.Fprintf(
				&sb,
				`,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}`,
				o.input,
				o.output,
				o.cacheRead,
				o.cacheWrite,
			)
		}
		sb.WriteString("}")
	}
	sb.WriteString("}")
	return sb.String()
}

// writeJSONL writes lines (already-built JSONL rows, or raw garbage strings
// for malformed-line tests) to path, one per line, terminated by a trailing
// newline.
func writeJSONL(t *testing.T, path string, lines []string) {
	t.Helper()
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture %s: %v", path, err)
	}
}

// fableBedrockCost hand-computes, from the bedrock pricing table in the task
// brief, the USD cost of one claude-fable-5 bedrock row with the given token
// counts.
func fableBedrockCost(input, output, cacheRead, cacheWrite int) float64 {
	const (
		in  = 0.000011
		out = 0.000055
		cr  = 0.0000011
		cw  = 0.00001375
	)
	return float64(input)*in + float64(output)*out + float64(cacheRead)*cr + float64(cacheWrite)*cw
}

// fableListCost hand-computes, from the list (anthropic, unsubscribed)
// pricing table in the task brief, the USD cost of one claude-fable-5 row
// with the given token counts.
func fableListCost(input, output, cacheRead, cacheWrite int) float64 {
	const (
		in  = 0.00001
		out = 0.00005
		cr  = 0.000001
		cw  = 0.0000125
	)
	return float64(input)*in + float64(output)*out + float64(cacheRead)*cr + float64(cacheWrite)*cw
}

const floatTolerance = 1e-9

func assertWithinTolerance(t *testing.T, got, want float64, msg string) {
	t.Helper()
	if diff := got - want; diff > floatTolerance || diff < -floatTolerance {
		t.Fatalf("%s: got %v, want %v (diff %v)", msg, got, want, diff)
	}
}

// sessionOnly calls cost.Costs against transcriptPath with a distinct,
// otherwise-empty sibling projectsDir, so the walk side of Costs
// contributes nothing and only sessionUSD is meaningful -- the session-scan
// equivalent of the old standalone cost.Session for tests that have no
// daily/projectsDir behavior to exercise.
func sessionOnly(t *testing.T, transcriptPath string, subscribed bool) (float64, error) {
	t.Helper()
	projectsDir := t.TempDir()
	sessionUSD, _, err := cost.Costs(transcriptPath, projectsDir, time.Now(), subscribed)
	return sessionUSD, err
}

// --- Row filter ---

func TestRowFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	kept := buildRow(rowOpts{
		msgID: "msg_bdrk_kept", model: "claude-fable-5", sidechain: true,
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})
	userRow := buildRow(rowOpts{
		rowType: "user", msgID: "msg_bdrk_user", model: "claude-fable-5",
		timestamp: "2026-07-06T16:00:01.000Z",
		input:     100, output: 100,
	})
	syntheticRow := buildRow(rowOpts{
		msgID: "msg_bdrk_synth", model: "<synthetic>",
		timestamp: "2026-07-06T16:00:02.000Z",
		input:     100, output: 100,
	})
	noUsageRow := buildRow(rowOpts{
		msgID: "msg_bdrk_nousage", model: "claude-fable-5", noUsage: true,
		timestamp: "2026-07-06T16:00:03.000Z",
	})

	writeJSONL(t, path, []string{kept, userRow, syntheticRow, noUsageRow})

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	want := fableBedrockCost(10, 5, 1, 2)
	assertWithinTolerance(t, got, want, "row filter should keep only the billable sidechain row")
}

// --- Dedup ---

func TestDedupSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	row1 := buildRow(rowOpts{
		msgID: "msg_bdrk_dup", model: "claude-fable-5", requestID: "req_1",
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})
	// Same message id + requestId, as when a turn's text part and tool_use
	// part are logged as two separate rows.
	row2 := buildRow(rowOpts{
		msgID: "msg_bdrk_dup", model: "claude-fable-5", requestID: "req_1",
		timestamp: "2026-07-06T16:00:00.500Z",
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})

	writeJSONL(t, path, []string{row1, row2})

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	want := fableBedrockCost(10, 5, 1, 2)
	assertWithinTolerance(t, got, want, "duplicate id+requestId within one file should be counted once")
}

func TestDedupAcrossFilesWithNullRequestID(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	row := buildRow(rowOpts{
		msgID: "msg_bdrk_resumed", model: "claude-fable-5", nullReqID: true,
		timestamp: now.UTC().Format(time.RFC3339),
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})

	// Simulate a resumed session: the parent session's row is re-logged into
	// a new file under a different sessionId, but with the same message.id
	// and a null requestId. session-a is scanned as "the session"
	// (transcriptPath); session-b is found by the projectsDir walk. Both
	// must be scanned for Costs' daily total to find today's rows across
	// all live transcripts, but the shared dedup set must prevent double
	// counting.
	transcriptPath := filepath.Join(dir, "session-a.jsonl")
	writeJSONL(t, transcriptPath, []string{row})
	writeJSONL(t, filepath.Join(dir, "session-b.jsonl"), []string{row})

	_, dailyUSD, err := cost.Costs(transcriptPath, dir, now, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	want := fableBedrockCost(10, 5, 1, 2)
	assertWithinTolerance(t, dailyUSD, want, "cross-file dedup on null requestId should count the resumed row once")
}

func TestDedupNoMessageIDCountsEachRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Two rows with no message.id at all: never deduped, so both must count
	// even though they are otherwise identical.
	row := buildRow(rowOpts{
		model:     "claude-fable-5",
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})

	writeJSONL(t, path, []string{row, row})

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	// No message.id means backend defaults to anthropic (no "msg_bdrk"
	// prefix); subscribed=false so list rates apply, and both rows count.
	want := 2 * fableListCost(10, 5, 1, 2)
	assertWithinTolerance(t, got, want, "rows with no message.id should never be deduped")
}

// --- Byte-level pre-filter (perf-2) ---

func TestByteFilterFalsePositiveContentNotCounted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// A user row (never billable) whose own content happens to contain the
	// literal `"type":"assistant"` byte sequence unescaped in the raw
	// JSON bytes -- via a nested object field (as real transcripts do,
	// e.g. a toolUseResult echoing the assistant turn's own shape) rather
	// than as an escaped substring inside a string value, which the
	// pre-filter's plain bytes.Contains would never see. The gate is a
	// false positive here -- it can't tell the difference from bytes
	// alone -- but the full unmarshal + isBillableRow that always follows
	// it correctly reads this row's real top-level "type":"user" and
	// rejects it, so it must still not be counted.
	line := `{"type":"user","isSidechain":false,"timestamp":"2026-07-06T16:00:00.000Z",` +
		`"message":{"id":"msg_bdrk_shouldnotcount","model":"claude-fable-5",` +
		`"usage":{"input_tokens":999,"output_tokens":999,"cache_read_input_tokens":999,"cache_creation_input_tokens":999}},` +
		`"toolUseResult":{"type":"assistant","stuff":123}}`

	writeJSONL(t, path, []string{line})

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	assertWithinTolerance(
		t,
		got,
		0,
		"a non-assistant row whose content contains the assistant marker must not be counted",
	)
}

// --- Oversized line skip (perf-3) ---

func TestOversizedLineSkippedRestOfFileStillCounted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	before := buildRow(rowOpts{
		msgID: "msg_bdrk_before", model: "claude-fable-5",
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})
	after := buildRow(rowOpts{
		msgID: "msg_bdrk_after", model: "claude-fable-5",
		timestamp: "2026-07-06T16:00:01.000Z",
		input:     20, output: 10, cacheRead: 2, cacheWrite: 4,
	})
	// An oversized "line" with no embedded newline, well past the 10MB
	// per-line cap: it must be discarded without erroring or aborting the
	// scan, and the valid rows before and after it must both still count.
	const oversizedLineBytes = 11 * 1024 * 1024
	junk := strings.Repeat("x", oversizedLineBytes)

	writeJSONL(t, path, []string{before, junk, after})

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error scanning past an oversized line: %v", err)
	}
	want := fableBedrockCost(10, 5, 1, 2) + fableBedrockCost(20, 10, 2, 4)
	assertWithinTolerance(t, got, want, "rows before and after an oversized line must both count, with no error")
}

// --- Billing ---

func TestBillingBedrockRegardlessOfSubscribed(t *testing.T) {
	dir := t.TempDir()

	row := buildRow(rowOpts{
		msgID: "msg_bdrk_01abc", model: "claude-fable-5",
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     100, output: 50, cacheRead: 10, cacheWrite: 20,
	})

	pathSubscribed := filepath.Join(dir, "subscribed.jsonl")
	writeJSONL(t, pathSubscribed, []string{row})
	gotSubscribed, err := sessionOnly(t, pathSubscribed, true)
	if err != nil {
		t.Fatalf("Costs (subscribed): unexpected error: %v", err)
	}

	pathUnsubscribed := filepath.Join(dir, "unsubscribed.jsonl")
	writeJSONL(t, pathUnsubscribed, []string{row})
	gotUnsubscribed, err := sessionOnly(t, pathUnsubscribed, false)
	if err != nil {
		t.Fatalf("Costs (unsubscribed): unexpected error: %v", err)
	}

	want := fableBedrockCost(100, 50, 10, 20)
	assertWithinTolerance(t, gotSubscribed, want, "bedrock row must be priced when subscribed=true")
	assertWithinTolerance(t, gotUnsubscribed, want, "bedrock row must be priced when subscribed=false")
}

func TestBillingAnthropicSubscribedIsFree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	row := buildRow(rowOpts{
		msgID: "msg_01xyz", model: "claude-fable-5",
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     100, output: 50, cacheRead: 10, cacheWrite: 20,
	})
	writeJSONL(t, path, []string{row})

	got, err := sessionOnly(t, path, true)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	assertWithinTolerance(t, got, 0, "subscribed anthropic row must cost $0")
}

func TestBillingAnthropicUnsubscribedUsesListRates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	row := buildRow(rowOpts{
		msgID: "msg_01xyz", model: "claude-fable-5",
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     100, output: 50, cacheRead: 10, cacheWrite: 20,
	})
	writeJSONL(t, path, []string{row})

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	want := fableListCost(100, 50, 10, 20)
	assertWithinTolerance(t, got, want, "unsubscribed anthropic row must use list rates")
}

func TestBillingUnknownModelIsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	row := buildRow(rowOpts{
		msgID: "msg_bdrk_unknown", model: "claude-unheard-of-9",
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     100, output: 50, cacheRead: 10, cacheWrite: 20,
	})
	writeJSONL(t, path, []string{row})

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	assertWithinTolerance(t, got, 0, "unknown model id must never be guessed at, must cost $0")
}

// TestBillingClaude5Models pins the per-token rates of the Claude 5-family
// ids an Attain session reports: each one is priced (never the unknown-model
// $0) on both the bedrock and the unsubscribed list table.
func TestBillingClaude5Models(t *testing.T) {
	// rates are USD per token: input, output, cache read, 5m cache write.
	tests := []struct {
		model   string
		list    [4]float64
		bedrock [4]float64
	}{
		{
			model:   "claude-fable-5-1",
			list:    [4]float64{0.00001, 0.00005, 0.00000025, 0.0000125},
			bedrock: [4]float64{0.000011, 0.000055, 0.000000275, 0.00001375},
		},
		{
			model:   "claude-opus-5-5",
			list:    [4]float64{0.000004, 0.00002, 0.0000002, 0.000005},
			bedrock: [4]float64{0.0000044, 0.000022, 0.00000022, 0.0000055},
		},
		{
			model:   "claude-opus-5",
			list:    [4]float64{0.000005, 0.000025, 0.0000005, 0.00000625},
			bedrock: [4]float64{0.0000055, 0.0000275, 0.00000055, 0.000006875},
		},
		{
			model:   "claude-sonnet-5-5",
			list:    [4]float64{0.000002, 0.00001, 0.0000002, 0.0000025},
			bedrock: [4]float64{0.0000022, 0.000011, 0.00000022, 0.00000275},
		},
	}
	const input, output, cacheRead, cacheWrite = 100, 50, 10, 20
	for _, test := range tests {
		for _, backend := range []struct {
			name  string
			msgID string
			rates [4]float64
		}{
			{name: "bedrock", msgID: "msg_bdrk_01abc", rates: test.bedrock},
			{name: "list", msgID: "msg_01xyz", rates: test.list},
		} {
			t.Run(test.model+"/"+backend.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "session.jsonl")
				writeJSONL(t, path, []string{buildRow(rowOpts{
					msgID: backend.msgID, model: test.model,
					timestamp: "2026-07-06T16:00:00.000Z",
					input:     input, output: output, cacheRead: cacheRead, cacheWrite: cacheWrite,
				})})
				got, err := sessionOnly(t, path, false)
				if err != nil {
					t.Fatalf("Costs: unexpected error: %v", err)
				}
				r := backend.rates
				want := input*r[0] + output*r[1] + cacheRead*r[2] + cacheWrite*r[3]
				assertWithinTolerance(t, got, want, test.model+" "+backend.name+" rates")
			})
		}
	}
}

// --- Local-date bucketing ---

func TestDailyLocalDateBucketing(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("loading America/Los_Angeles: %v", err)
	}

	// now is 2026-07-05 23:00 in Los Angeles.
	now := time.Date(2026, time.July, 5, 23, 0, 0, 0, la)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// This row's timestamp is 2026-07-06T02:00:00Z, which converts to
	// 2026-07-05 19:00 in Los Angeles -- the same local date as now, so it
	// must count for "today".
	inBucket := buildRow(rowOpts{
		msgID: "msg_bdrk_today", model: "claude-fable-5",
		timestamp: "2026-07-06T02:00:00.000Z",
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})
	// This row is a full day earlier in Los Angeles local time and must be
	// excluded, proving the bucketing filter actually excludes something.
	outOfBucket := buildRow(rowOpts{
		msgID: "msg_bdrk_yesterday", model: "claude-fable-5",
		timestamp: "2026-07-04T10:00:00.000Z",
		input:     999, output: 999, cacheRead: 999, cacheWrite: 999,
	})

	writeJSONL(t, path, []string{inBucket, outOfBucket})

	// path is itself the session (transcriptPath): its own today-matching
	// rows feed the daily total directly, with no separate walk
	// contribution needed (dir contains nothing else).
	_, dailyUSD, err := cost.Costs(path, dir, now, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	want := fableBedrockCost(10, 5, 1, 2)
	assertWithinTolerance(t, dailyUSD, want, "only the row whose local date matches now's local date should count")
}

func TestSessionTotalHasNoDateFilter(t *testing.T) {
	// The session total (sessionUSD) must include every billable row in
	// transcriptPath regardless of date -- only dailyUSD applies the
	// today-only filter.
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("loading America/Los_Angeles: %v", err)
	}
	now := time.Date(2026, time.July, 5, 23, 0, 0, 0, la)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	today := buildRow(rowOpts{
		msgID: "msg_bdrk_today", model: "claude-fable-5",
		timestamp: "2026-07-06T02:00:00.000Z",
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})
	yesterday := buildRow(rowOpts{
		msgID: "msg_bdrk_yesterday", model: "claude-fable-5",
		timestamp: "2026-07-04T10:00:00.000Z",
		input:     20, output: 10, cacheRead: 2, cacheWrite: 4,
	})
	writeJSONL(t, path, []string{today, yesterday})

	sessionUSD, _, err := cost.Costs(path, dir, now, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	want := fableBedrockCost(10, 5, 1, 2) + fableBedrockCost(20, 10, 2, 4)
	assertWithinTolerance(t, sessionUSD, want, "session total must include rows from every date")
}

// --- mtime skip ---

func TestDailyMtimeSkip(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()

	// transcriptPath is a separate, empty session file -- the mtime skip
	// under test applies only to the projectsDir walk, not to the
	// unconditionally-scanned session file.
	transcriptPath := filepath.Join(dir, "current.jsonl")
	writeJSONL(t, transcriptPath, nil)

	path := filepath.Join(dir, "old.jsonl")
	// Content, if scanned, would count -- but the file's mtime predates
	// local midnight of now, so the walk must skip it without ever
	// opening it.
	row := buildRow(rowOpts{
		msgID: "msg_bdrk_old", model: "claude-fable-5",
		timestamp: now.UTC().Format(time.RFC3339),
		input:     100, output: 100, cacheRead: 100, cacheWrite: 100,
	})
	writeJSONL(t, path, []string{row})

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	oldTime := midnight.Add(-24 * time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	_, dailyUSD, err := cost.Costs(transcriptPath, dir, now, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	assertWithinTolerance(t, dailyUSD, 0, "a file with mtime before local midnight must be skipped")
}

// --- Malformed lines / empty files ---

func TestMalformedLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	good := buildRow(rowOpts{
		msgID: "msg_bdrk_good", model: "claude-fable-5",
		timestamp: "2026-07-06T16:00:00.000Z",
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})

	writeJSONL(t, path, []string{
		good,
		"not json at all {{{",
		"",
		`{"type":"assistant","message":`, // truncated/partial line
	})

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error on malformed lines: %v", err)
	}
	want := fableBedrockCost(10, 5, 1, 2)
	assertWithinTolerance(t, got, want, "malformed lines must be skipped silently")
}

func TestEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	writeJSONL(t, path, nil)

	got, err := sessionOnly(t, path, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error on empty file: %v", err)
	}
	assertWithinTolerance(t, got, 0, "empty file must cost 0")
}

func TestDailyEmptyDir(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	transcriptPath := filepath.Join(dir, "session.jsonl")
	writeJSONL(t, transcriptPath, nil)

	_, dailyUSD, err := cost.Costs(transcriptPath, dir, now, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error on empty projects dir: %v", err)
	}
	assertWithinTolerance(t, dailyUSD, 0, "empty projects dir must cost 0")
}

// --- Missing / unreadable paths ---

func TestMissingTranscriptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.jsonl")

	if _, _, err := cost.Costs(path, dir, time.Now(), false); err == nil {
		t.Fatal("Costs: expected error for missing transcript file, got nil")
	}
}

func TestMissingProjectsDir(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "session.jsonl")
	writeJSONL(t, transcriptPath, nil)

	missingDir := filepath.Join(dir, "does-not-exist")

	if _, _, err := cost.Costs(transcriptPath, missingDir, time.Now(), false); err == nil {
		t.Fatal("Costs: expected error for missing projects dir, got nil")
	}
}

func TestDailyUnreadableFileAmongReadableOnes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are bypassed, cannot exercise unreadable-file skip")
	}

	dir := t.TempDir()
	now := time.Now()

	goodRow := buildRow(rowOpts{
		msgID: "msg_bdrk_readable", model: "claude-fable-5",
		timestamp: now.UTC().Format(time.RFC3339),
		input:     10, output: 5, cacheRead: 1, cacheWrite: 2,
	})
	badRow := buildRow(rowOpts{
		msgID: "msg_bdrk_unreadable", model: "claude-fable-5",
		timestamp: now.UTC().Format(time.RFC3339),
		input:     999, output: 999, cacheRead: 999, cacheWrite: 999,
	})

	goodPath := filepath.Join(dir, "readable.jsonl")
	badPath := filepath.Join(dir, "unreadable.jsonl")
	writeJSONL(t, goodPath, []string{goodRow})
	writeJSONL(t, badPath, []string{badRow})

	if err := os.Chmod(badPath, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer func() { _ = os.Chmod(badPath, 0o600) }()

	_, dailyUSD, err := cost.Costs(goodPath, dir, now, false)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}
	want := fableBedrockCost(10, 5, 1, 2)
	assertWithinTolerance(t, dailyUSD, want, "an unreadable file must be skipped, not fatal, among readable ones")
}

// --- End-to-end fixture ---

func TestDailyEndToEndFixture(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	ts := now.UTC().Format(time.RFC3339)

	bedrockRow1 := buildRow(rowOpts{
		msgID: "msg_bdrk_e2e1", model: "claude-fable-5", requestID: "req_1",
		timestamp: ts, input: 100, output: 50, cacheRead: 10, cacheWrite: 20,
	})
	bedrockRow2 := buildRow(rowOpts{
		msgID: "msg_bdrk_e2e2", model: "claude-fable-5", requestID: "req_2",
		timestamp: ts, input: 200, output: 60, cacheRead: 0, cacheWrite: 5,
	})
	bedrockRow3 := buildRow(rowOpts{
		msgID: "msg_bdrk_e2e3", model: "claude-fable-5", requestID: "req_3",
		timestamp: ts, input: 50, output: 25, cacheRead: 2, cacheWrite: 0,
	})
	subscribedAnthropicRow := buildRow(rowOpts{
		msgID: "msg_01e2e4", model: "claude-fable-5", requestID: "req_4",
		timestamp: ts, input: 1000, output: 1000, cacheRead: 1000, cacheWrite: 1000,
	})

	path := filepath.Join(dir, "e2e.jsonl")
	writeJSONL(t, path, []string{bedrockRow1, bedrockRow2, bedrockRow3, subscribedAnthropicRow})

	// path is itself the session (transcriptPath); dir contains nothing
	// else, so dailyUSD comes entirely from path's own today-matching
	// rows, and sessionUSD (no date filter) must equal the same total
	// since every row's timestamp is "today".
	sessionUSD, dailyUSD, err := cost.Costs(path, dir, now, true)
	if err != nil {
		t.Fatalf("Costs: unexpected error: %v", err)
	}

	want := fableBedrockCost(100, 50, 10, 20) +
		fableBedrockCost(200, 60, 0, 5) +
		fableBedrockCost(50, 25, 2, 0)
	// subscribedAnthropicRow costs $0 (subscribed=true, anthropic backend).
	assertWithinTolerance(t, dailyUSD, want, "end-to-end fixture: 3 bedrock rows + 1 free subscribed row")
	assertWithinTolerance(t, sessionUSD, want, "session total must match daily when every row is from today")
}

// --- Non-circular pricing validation (conf-3) ---
//
// TestRealShapeFixtureMatchesIndependentTotal validates cost.Costs against
// testdata/real_shape.jsonl using dollar totals computed independently of
// this package: by hand, from decimal arithmetic against pricing.go's
// documented per-token rates, NOT by calling any of pricing.go's own
// functions or constants (fableBedrockCost/fableListCost above are
// deliberately not used here — those helpers duplicate pricing.go's own
// constants and so can't catch a bug shared between pricing.go and the
// helper). If pricing.go's rate tables ever drift from the documented
// rates, this is the test that notices.
func TestRealShapeFixtureMatchesIndependentTotal(t *testing.T) {
	// Row 1: bedrock claude-fable-5, msg_bdrk_fixture1,
	// input=7546 output=22 cache_read=0 cache_creation=43994.
	//   7546*0.000011 + 22*0.000055 + 0*0.0000011 + 43994*0.00001375
	// = 0.083006 + 0.001210 + 0 + 0.60491750 = 0.68913350
	//
	// Row 2: bedrock claude-sonnet-5, msg_bdrk_fixture2,
	// input=1000 output=500 cache_read=20000 cache_creation=3000.
	//   1000*0.0000022 + 500*0.000011 + 20000*0.00000022 + 3000*0.00000275
	// = 0.0022 + 0.0055 + 0.0044 + 0.00825 = 0.02035
	//
	// Row 3: anthropic claude-opus-4-8 (list rates, subscribed=false),
	// msg_01fixture3, input=2000 output=100 cache_read=50000
	// cache_creation=0.
	//   2000*0.000005 + 100*0.000025 + 50000*0.0000005 + 0
	// = 0.01 + 0.0025 + 0.025 = 0.0375
	//
	// TOTAL (subscribed=false) = 0.68913350 + 0.02035 + 0.0375 = 0.74698350
	// TOTAL (subscribed=true), row 3 (anthropic) is free:
	//   0.68913350 + 0.02035 = 0.70948350
	const (
		wantUnsubscribed = 0.74698350
		wantSubscribed   = 0.70948350
	)

	got, err := sessionOnly(t, "testdata/real_shape.jsonl", false)
	if err != nil {
		t.Fatalf("Costs (unsubscribed): unexpected error: %v", err)
	}
	assertWithinTolerance(
		t,
		got,
		wantUnsubscribed,
		"real_shape.jsonl total (subscribed=false) must match the independently computed literal",
	)

	got, err = sessionOnly(t, "testdata/real_shape.jsonl", true)
	if err != nil {
		t.Fatalf("Costs (subscribed): unexpected error: %v", err)
	}
	assertWithinTolerance(
		t,
		got,
		wantSubscribed,
		"real_shape.jsonl total (subscribed=true) must match the independently computed literal",
	)
}
