package notify

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		fixture       string
		wantStatus    GoalStatus
		wantCondition string
		wantIters     int
	}{
		{
			fixture:       "goal_none.jsonl",
			wantStatus:    GoalNone,
			wantCondition: "",
			wantIters:     0,
		},
		{
			fixture:       "goal_active_set.jsonl",
			wantStatus:    GoalActive,
			wantCondition: "Continue using gambit:executing-plans until we run into a decision point you need my input on. Do not pose false choices; take the next task and iterate on it unless there is a meaningful blockage.",
			wantIters:     0,
		},
		{
			fixture:       "goal_active_iterating.jsonl",
			wantStatus:    GoalActive,
			wantCondition: "Continue using gambit:executing-plans until we run into a decision point you need my input on. Do not pose false choices; take the next task and iterate on it unless there is a meaningful blockage.",
			wantIters:     2,
		},
		{
			fixture:       "goal_met.jsonl",
			wantStatus:    GoalMet,
			wantCondition: "Continue using gambit:executing-plans until we run into a decision point you need my input on. Do not pose false choices; take the next task and iterate on it unless there is a meaningful blockage.",
			wantIters:     3,
		},
		{
			fixture:       "goal_cleared.jsonl",
			wantStatus:    GoalCleared,
			wantCondition: "Continue executing plan until ALL components of the epic are complete. Do not stop until ALL components of the epic are complete robustly and completely.",
			wantIters:     0,
		},
		{
			fixture:       "goal_failed.jsonl",
			wantStatus:    GoalFailed,
			wantCondition: "Continue executing plan until ALL components of the epic are complete. Do not stop until ALL components of the epic are complete robustly and completely.",
			wantIters:     0,
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
			if res.Goal.Condition != c.wantCondition {
				t.Errorf("Condition = %q, want %q", res.Goal.Condition, c.wantCondition)
			}
			if res.Goal.Iterations != c.wantIters {
				t.Errorf("Iterations = %d, want %d", res.Goal.Iterations, c.wantIters)
			}
			if len(res.LiveTasks) != 0 {
				t.Errorf("tasks = %v, want none", res.LiveTasks)
			}
		})
	}
}

// assertLiveTask checks every field of a LiveTask against its expected
// value in one call, keeping per-field branching out of the calling test
// function (which otherwise trips cyclop's complexity ceiling once several
// full-field assertions accumulate across subtests).
func assertLiveTask(
	t *testing.T,
	task LiveTask,
	wantID string,
	wantKind TaskKind,
	wantDescription string,
	wantDetail string,
	wantLaunchedAt time.Time,
	wantOutputFile string,
) {
	t.Helper()
	if task.ID != wantID {
		t.Errorf("ID = %q, want %q", task.ID, wantID)
	}
	if task.Kind != wantKind {
		t.Errorf("Kind = %v, want %v", task.Kind, wantKind)
	}
	if task.Description != wantDescription {
		t.Errorf("Description = %q, want %q", task.Description, wantDescription)
	}
	if task.Detail != wantDetail {
		t.Errorf("Detail = %q, want %q", task.Detail, wantDetail)
	}
	if !task.LaunchedAt.Equal(wantLaunchedAt) {
		t.Errorf("LaunchedAt = %v, want %v", task.LaunchedAt, wantLaunchedAt)
	}
	if task.OutputFile != wantOutputFile {
		t.Errorf("OutputFile = %q, want %q", task.OutputFile, wantOutputFile)
	}
}

func parseTestTimestamp(t *testing.T, ts string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("parsing test timestamp %q: %v", ts, err)
	}
	return parsed
}

func TestScanTranscriptLiveTasks(t *testing.T) {
	wantLaunchedAt := func(ts string) time.Time {
		return parseTestTimestamp(t, ts)
	}

	t.Run("tasks_live.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_live.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if res.Goal.Status != GoalNone {
			t.Errorf("Status = %v, want GoalNone", res.Goal.Status)
		}
		if len(res.LiveTasks) != 2 {
			t.Fatalf("len(tasks) = %d, want 2: %+v", len(res.LiveTasks), res.LiveTasks)
		}

		wantPrompt := "Investigate the current notification hook setup and summarize how Stop and Notification hooks interact with subagents, citing official docs."
		assertLiveTask(
			t, res.LiveTasks[0],
			"agenttestid01", TaskAgent, "Research task placeholder", wantPrompt,
			wantLaunchedAt("2026-07-05T05:25:54.323Z"),
			"/tmp/claude-1000/-home-user-project/anon-session-0001/tasks/agenttestid01.output",
		)

		assertLiveTask(
			t, res.LiveTasks[1],
			"bgtaskid001", TaskBash, "Run full build in background", "long-running-build.sh --target all --verbose",
			wantLaunchedAt("2026-07-05T05:47:58.730Z"),
			"/tmp/claude-1000/-home-user-project/anon-session-0099/tasks/bgtaskid001.output",
		)
	})

	t.Run("tasks_completed.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_completed.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf("tasks = %+v, want none (both launches completed)", res.LiveTasks)
		}
	})

	t.Run("tasks_mixed.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_mixed.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 1 {
			t.Fatalf(
				"len(tasks) = %d, want 1 (bash still live, agent completed): %+v",
				len(res.LiveTasks), res.LiveTasks,
			)
		}
		if res.LiveTasks[0].ID != "bgtaskid001" {
			t.Errorf("live task ID = %q, want bgtaskid001", res.LiveTasks[0].ID)
		}
		if res.LiveTasks[0].Kind != TaskBash {
			t.Errorf("live task Kind = %v, want TaskBash", res.LiveTasks[0].Kind)
		}
		wantOutputFile := "/tmp/claude-1000/-home-user-project/anon-session-0099/tasks/bgtaskid001.output"
		if res.LiveTasks[0].OutputFile != wantOutputFile {
			t.Errorf("live task OutputFile = %q, want %q", res.LiveTasks[0].OutputFile, wantOutputFile)
		}
	})

	// tasks_prose_mention.jsonl is a real false-positive case: a plain
	// (non-background) Bash tool_use whose own grep output happens to quote
	// both the "Command running in background with ID:" and "agentId:"
	// patterns literally. Without structural pairing to a backgrounded
	// tool_use, neither pattern should register a launch.
	t.Run("tasks_prose_mention.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_prose_mention.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf("tasks = %+v, want none (prose mention of launch patterns is not a launch)", res.LiveTasks)
		}
	})

	// tasks_sync_agent.jsonl is a synchronous Agent call (run_in_background:
	// false) whose final report embeds "agentId: ..." as part of the
	// SendMessage-to-continue text. The task already finished inline; it
	// must not register as a live agent task.
	t.Run("tasks_sync_agent.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_sync_agent.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf("tasks = %+v, want none (synchronous agent report is not a live launch)", res.LiveTasks)
		}
	})

	// tasks_sync_agent_no_flag.jsonl is a synchronous Agent call with NO
	// run_in_background key at all (absent, not false) — real halmasuit
	// sessions dispatch review/research agents this way. Its tool_result is
	// a short final report ending in the SendMessage-to-continue text, with
	// no "Async agent launched successfully" acknowledgment anywhere. Absent
	// run_in_background resolves to true (Agent's own default), so kind
	// pairing and run_in_background alone would misclassify this as a live
	// launch; only the missing async marker correctly excludes it.
	t.Run("tasks_sync_agent_no_flag.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_sync_agent_no_flag.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf(
				"tasks = %+v, want none (synchronous agent report with run_in_background absent is not a live launch)",
				res.LiveTasks,
			)
		}
	})

	// tasks_readd_after_completion.jsonl exercises a real background launch
	// that completes via a queue-operation task-notification, followed by a
	// later prose mention (an unrelated Bash tool_use's own grep output)
	// that quotes the same ID's launch pattern again. The ID must not come
	// back to life, and specifically must not be emitted twice.
	t.Run("tasks_readd_after_completion.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_readd_after_completion.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf("tasks = %+v, want none (completed task must not be re-added or duplicated)", res.LiveTasks)
		}
	})

	// tasks_taskstop_removes_live.jsonl reproduces a real leak: a TaskStop of
	// a background-bash launch never emits a <task-id> completion
	// notification, so the old regex scanner (which only ever removed a live
	// task on that notification) held it live indefinitely. The structured
	// TaskStop toolUseResult (task_id) must remove it directly.
	t.Run("tasks_taskstop_removes_live.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_taskstop_removes_live.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf("tasks = %+v, want none (TaskStop must remove the launch)", res.LiveTasks)
		}
	})

	// tasks_agent_stopped_by_user.jsonl reproduces the other half of the same
	// leak class: a user-stopped async agent never emits a <task-id>
	// notification either. Its toolUseResult carries success:false with the
	// agent ID embedded only in message text.
	t.Run("tasks_agent_stopped_by_user.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_agent_stopped_by_user.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf("tasks = %+v, want none (user-stopped agent must remove the launch)", res.LiveTasks)
		}
	})

	// tasks_no_task_found.jsonl is a TaskStop attempt on an ID that was never
	// live. Its toolUseResult is a plain STRING ("Error: No task found with
	// ID: ..."), not an object — this must not panic and must leave no live
	// tasks behind.
	t.Run("tasks_no_task_found.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_no_task_found.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf("tasks = %+v, want none", res.LiveTasks)
		}
	})

	// tasks_sync_completion_removes_live.jsonl launches an agent and later
	// delivers a synchronous/delivered completion record for the SAME agent
	// ID (status:"completed" + agentId, distinct from the async-launch
	// status). That shape must remove the live task, not just fail to
	// register one.
	t.Run("tasks_sync_completion_removes_live.jsonl", func(t *testing.T) {
		f := openFixture(t, "tasks_sync_completion_removes_live.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf("tasks = %+v, want none (delivered completion must remove the launch)", res.LiveTasks)
		}
	})

	// task_notification_plain_string.jsonl completes a live bash launch via a
	// <task-notification> delivered as the ENTIRE plain-string content of a
	// user message, rather than via a queue-operation record or a
	// queued_command attachment (the two shapes extractCompletions's other
	// callers already cover).
	t.Run("task_notification_plain_string.jsonl", func(t *testing.T) {
		f := openFixture(t, "task_notification_plain_string.jsonl")
		res, err := ScanTranscript(f)
		if err != nil {
			t.Fatalf("ScanTranscript returned error: %v", err)
		}
		if len(res.LiveTasks) != 0 {
			t.Errorf(
				"tasks = %+v, want none (plain-string task-notification must remove the launch)",
				res.LiveTasks,
			)
		}
	})
}

// TestScanTranscriptResumeReregisters exercises a SendMessage resume of an
// agent with no prior launch pairing in this scan (e.g. it was launched,
// completed, and is now being resumed later): resumedAgentId must
// re-register it as live, with Description/Detail empty (no toolUseInfo
// pairing exists for a resume — it fires from a SendMessage tool_use, not a
// Bash/Agent one) and OutputFile pulled from the resume message's
// "Output: ..." text.
func TestScanTranscriptResumeReregisters(t *testing.T) {
	f := openFixture(t, "tasks_resume_reregisters.jsonl")
	res, err := ScanTranscript(f)
	if err != nil {
		t.Fatalf("ScanTranscript returned error: %v", err)
	}
	if len(res.LiveTasks) != 1 {
		t.Fatalf("len(LiveTasks) = %d, want 1: %+v", len(res.LiveTasks), res.LiveTasks)
	}
	assertLiveTask(
		t, res.LiveTasks[0],
		"agenttestid03", TaskAgent, "", "",
		parseTestTimestamp(t, "2026-07-06T04:00:00.100Z"),
		"/tmp/claude-1000/-home-user-project/anon-session-0100/tasks/agenttestid03.output",
	)
}

// TestScanTranscriptTeammates exercises a teammate spawn (via the Agent
// tool, distinguished from an async launch by toolUseResult.status ==
// "teammate_spawned") followed by a relayed SendMessage from that teammate.
// The teammate must appear in ScanResult.Teammates with its spawn/
// last-message timing and summary, and must NOT appear in LiveTasks —
// teammates are never part of the liveness gate.
func TestScanTranscriptTeammates(t *testing.T) {
	f := openFixture(t, "teammates.jsonl")
	res, err := ScanTranscript(f)
	if err != nil {
		t.Fatalf("ScanTranscript returned error: %v", err)
	}
	if len(res.LiveTasks) != 0 {
		t.Errorf("LiveTasks = %+v, want none (a teammate spawn is never a live task)", res.LiveTasks)
	}
	if len(res.Teammates) != 1 {
		t.Fatalf("len(Teammates) = %d, want 1: %+v", len(res.Teammates), res.Teammates)
	}
	tm := res.Teammates[0]
	if tm.Name != "worker-wire" {
		t.Errorf("Name = %q, want %q", tm.Name, "worker-wire")
	}
	if tm.ID != "worker-wire@anon-session-0100" {
		t.Errorf("ID = %q, want %q", tm.ID, "worker-wire@anon-session-0100")
	}
	wantSpawnedAt := parseTestTimestamp(t, "2026-07-06T06:00:00.100Z")
	if !tm.SpawnedAt.Equal(wantSpawnedAt) {
		t.Errorf("SpawnedAt = %v, want %v", tm.SpawnedAt, wantSpawnedAt)
	}
	wantLastMessageAt := parseTestTimestamp(t, "2026-07-06T06:20:00.000Z")
	if !tm.LastMessageAt.Equal(wantLastMessageAt) {
		t.Errorf("LastMessageAt = %v, want %v", tm.LastMessageAt, wantLastMessageAt)
	}
	wantSummary := "DONE: TagGranted/Revoked/Transformed wired"
	if tm.LastSummary != wantSummary {
		t.Errorf("LastSummary = %q, want %q", tm.LastSummary, wantSummary)
	}
}

// TestScanTranscriptTeammateMessageWithoutSummary exercises a relayed
// teammate message whose <teammate-message> tag carries no summary attribute
// (e.g. an idle notification): the relay must still update LastMessageAt,
// with LastSummary left empty.
func TestScanTranscriptTeammateMessageWithoutSummary(t *testing.T) {
	f := openFixture(t, "teammates_no_summary.jsonl")
	res, err := ScanTranscript(f)
	if err != nil {
		t.Fatalf("ScanTranscript returned error: %v", err)
	}
	if len(res.Teammates) != 1 {
		t.Fatalf("len(Teammates) = %d, want 1: %+v", len(res.Teammates), res.Teammates)
	}
	tm := res.Teammates[0]
	wantLastMessageAt := parseTestTimestamp(t, "2026-07-06T06:20:00.000Z")
	if !tm.LastMessageAt.Equal(wantLastMessageAt) {
		t.Errorf("LastMessageAt = %v, want %v (summary-less relay must still update timing)",
			tm.LastMessageAt, wantLastMessageAt)
	}
	if tm.LastSummary != "" {
		t.Errorf("LastSummary = %q, want empty for a tag with no summary attribute", tm.LastSummary)
	}
}

// TestScanTranscriptTeammateMessageWithoutSpawn exercises a relayed teammate
// message whose sender has no matching spawn in the scanned range: the
// update has nowhere to attach and must be a clean no-op — no teammate
// invented, no live task, no panic.
func TestScanTranscriptTeammateMessageWithoutSpawn(t *testing.T) {
	f := openFixture(t, "teammates_no_spawn.jsonl")
	res, err := ScanTranscript(f)
	if err != nil {
		t.Fatalf("ScanTranscript returned error: %v", err)
	}
	if len(res.Teammates) != 0 {
		t.Errorf("Teammates = %+v, want none (a relay without a spawn has nowhere to attach)", res.Teammates)
	}
	if len(res.LiveTasks) != 0 {
		t.Errorf("LiveTasks = %+v, want none", res.LiveTasks)
	}
}

// TestScanTranscriptDuplicateCompletionNoDoubleSubtract exercises the case
// spelled out in the notify package brief: a single task-notification event
// materializes as two separate transcript lines (a queue-operation record
// and an attachment record) that both carry the same <task-id>. Removing a
// completed task twice must not panic or otherwise misbehave, and the task
// must end up absent exactly once.
func TestScanTranscriptDuplicateCompletionNoDoubleSubtract(t *testing.T) {
	f := openFixture(t, "tasks_completed.jsonl")
	res, err := ScanTranscript(f)
	if err != nil {
		t.Fatalf("ScanTranscript returned error: %v", err)
	}
	if len(res.LiveTasks) != 0 {
		t.Fatalf("tasks = %+v, want none", res.LiveTasks)
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
	if res.Goal.Condition != "Valid record after garbage lines." {
		t.Errorf("Condition = %q, want %q", res.Goal.Condition, "Valid record after garbage lines.")
	}
	if len(res.LiveTasks) != 0 {
		t.Errorf("tasks = %+v, want none", res.LiveTasks)
	}
}

func TestScanTranscriptAssistantIdentity(t *testing.T) {
	tests := []struct {
		name          string
		transcript    string
		wantUUID      string
		wantMessageID string
		wantText      string
		wantUserTurns int
		wantLiveTasks int
		wantLastTime  string
		wantReliable  bool
	}{
		{name: "empty input"},
		{
			name:          "distinct identities preserved",
			transcript:    `{"type":"assistant","uuid":"uuid-1","timestamp":"2026-07-05T01:00:00Z","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n",
			wantUUID:      "uuid-1",
			wantMessageID: "msg-1",
			wantText:      "done",
			wantLastTime:  "2026-07-05T01:00:00Z",
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
			wantUserTurns: 1,
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
			wantUserTurns: 1,
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
			wantText:      "useful",
			wantLiveTasks: 1,
			wantLastTime:  "2026-07-05T02:00:00Z",
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
			if res.UserTurns != tt.wantUserTurns {
				t.Errorf("UserTurns = %d, want %d", res.UserTurns, tt.wantUserTurns)
			}
			if len(res.LiveTasks) != tt.wantLiveTasks {
				t.Errorf("len(LiveTasks) = %d, want %d", len(res.LiveTasks), tt.wantLiveTasks)
			}
			if res.AssistantIdentityReliable != tt.wantReliable {
				t.Errorf(
					"AssistantIdentityReliable = %t, want %t",
					res.AssistantIdentityReliable,
					tt.wantReliable,
				)
			}
			if tt.wantLastTime != "" &&
				!res.LastTimestamp.Equal(parseTestTimestamp(t, tt.wantLastTime)) {
				t.Errorf("LastTimestamp = %v, want %s", res.LastTimestamp, tt.wantLastTime)
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
	if res.Goal.Condition != "" {
		t.Errorf("Condition = %q, want empty", res.Goal.Condition)
	}
	if res.Goal.Iterations != 0 {
		t.Errorf("Iterations = %d, want 0", res.Goal.Iterations)
	}
	if res.LiveTasks != nil {
		t.Errorf("tasks = %+v, want nil", res.LiveTasks)
	}
	if res.LastUserMessage != "" {
		t.Errorf("LastUserMessage = %q, want empty", res.LastUserMessage)
	}
	if res.LastSubstantiveUser != "" {
		t.Errorf("LastSubstantiveUser = %q, want empty", res.LastSubstantiveUser)
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
	if !res.FirstTimestamp.IsZero() {
		t.Errorf("FirstTimestamp = %v, want zero", res.FirstTimestamp)
	}
	if !res.LastTimestamp.IsZero() {
		t.Errorf("LastTimestamp = %v, want zero", res.LastTimestamp)
	}
	if res.UserTurns != 0 {
		t.Errorf("UserTurns = %d, want 0", res.UserTurns)
	}
	if res.BytesScanned != 0 {
		t.Errorf("BytesScanned = %d, want 0", res.BytesScanned)
	}
}

// TestScanTranscriptConversationCapture exercises the conversation-capture
// fields (LastUserMessage, LastSubstantiveUser, LastAssistantText,
// FirstTimestamp, LastTimestamp, UserTurns, BytesScanned) against
// testdata/conversation.jsonl, a fixture that interleaves a goal_status
// attachment and a live Bash background-task launch/ack pair (both from the
// existing structural-gating vocabulary) with conversation records:
//
//  1. a typed user string message >= 40 chars
//  2. a short ack ("yes")
//  3. a Bash background launch + its tool_result ack (registers a live task)
//  4. a user message whose real typed text is followed by an appended
//     <system-reminder>...</system-reminder> block (the typed part must win)
//  5. a reminder-ONLY user record (must not count at all)
//  6. an isMeta:true user record carrying real-looking prose (must not count)
//  7. two assistant text messages, the second over 2000 chars (tail-truncated,
//     keeping the END)
func TestScanTranscriptConversationCapture(t *testing.T) {
	const fixtureName = "conversation.jsonl"
	fixturePath := filepath.Join("testdata", fixtureName)

	info, err := os.Stat(fixturePath)
	if err != nil {
		t.Fatalf("stat fixture %s: %v", fixtureName, err)
	}

	f := openFixture(t, fixtureName)
	res, err := ScanTranscript(f)
	if err != nil {
		t.Fatalf("ScanTranscript(%s) returned error: %v", fixtureName, err)
	}

	// Goal: composes with existing scanning.
	if res.Goal.Status != GoalActive {
		t.Errorf("Goal.Status = %v, want GoalActive", res.Goal.Status)
	}
	wantCondition := "Continue restructuring the transcript scanner until ScanResult lands with conversation capture and tests are green."
	if res.Goal.Condition != wantCondition {
		t.Errorf("Goal.Condition = %q, want %q", res.Goal.Condition, wantCondition)
	}

	// LiveTasks: the Bash background launch/ack pair.
	if len(res.LiveTasks) != 1 {
		t.Fatalf("len(LiveTasks) = %d, want 1: %+v", len(res.LiveTasks), res.LiveTasks)
	}
	assertLiveTask(
		t, res.LiveTasks[0],
		"convtaskid001", TaskBash,
		"Run conversation capture tests in background",
		"go test ./internal/notify/... -run Conversation -v",
		parseTestTimestamp(t, "2026-07-05T06:00:20.000Z"),
		"/tmp/claude-1000/-home-user-project/anon-session-0200/tasks/convtaskid001.output",
	)

	// LastUserMessage / LastSubstantiveUser: the reminder-appended message's
	// typed text wins over both the later reminder-only and isMeta records.
	wantLastUser := "Go ahead and add the byte offset tracking too, we need it for the watchdog."
	if res.LastUserMessage != wantLastUser {
		t.Errorf("LastUserMessage = %q, want %q", res.LastUserMessage, wantLastUser)
	}
	if res.LastSubstantiveUser != wantLastUser {
		t.Errorf("LastSubstantiveUser = %q, want %q", res.LastSubstantiveUser, wantLastUser)
	}

	// UserTurns: the >=40-char typed message, the short "yes" ack, and the
	// reminder-appended message each count once; the reminder-only and
	// isMeta records do not.
	if res.UserTurns != 3 {
		t.Errorf("UserTurns = %d, want 3", res.UserTurns)
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

	// Timestamps: first and last top-level timestamps across all record
	// types (the goal attachment is first, the final assistant message is
	// last).
	wantFirst := parseTestTimestamp(t, "2026-07-05T06:00:00.000Z")
	wantLast := parseTestTimestamp(t, "2026-07-05T06:10:00.000Z")
	if !res.FirstTimestamp.Equal(wantFirst) {
		t.Errorf("FirstTimestamp = %v, want %v", res.FirstTimestamp, wantFirst)
	}
	if !res.LastTimestamp.Equal(wantLast) {
		t.Errorf("LastTimestamp = %v, want %v", res.LastTimestamp, wantLast)
	}

	// BytesScanned: exact against the fixture's true byte size (computed via
	// os.Stat, never hardcoded).
	if res.BytesScanned != info.Size() {
		t.Errorf("BytesScanned = %d, want %d (fixture size)", res.BytesScanned, info.Size())
	}
}

// TestScanTranscriptLiveTaskDetailTruncated exercises a Bash background
// launch whose command exceeds maxDetailLen (200 bytes): LiveTask.Detail
// must come back truncated to exactly maxDetailLen bytes, matching the
// command's prefix. This is coverage for the truncation contract itself
// (previously untested by any fixture, all of which use short commands),
// independent of exactly where in the scan pipeline the truncation is
// applied.
func TestScanTranscriptLiveTaskDetailTruncated(t *testing.T) {
	longCommand := strings.Repeat("a", maxDetailLen+50)
	transcript := `{"type":"assistant","timestamp":"2026-07-05T00:00:00.000Z","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_longcmd001","name":"Bash","input":{"command":"` + longCommand +
		`","description":"Long detail truncation test","run_in_background":true}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-07-05T00:00:01.000Z","message":{"role":"user","content":[` +
		`{"tool_use_id":"toolu_longcmd001","type":"tool_result","content":"Command running in background with ID: ` +
		`longcmdtask01. Output is being written to: /tmp/claude-1000/proj/sess/tasks/longcmdtask01.output. ` +
		`You will be notified when it completes."}]},` +
		`"toolUseResult":{"stdout":"","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false,` +
		`"backgroundTaskId":"longcmdtask01"}}` + "\n"

	res, err := ScanTranscript(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("ScanTranscript returned error: %v", err)
	}
	if len(res.LiveTasks) != 1 {
		t.Fatalf("len(LiveTasks) = %d, want 1: %+v", len(res.LiveTasks), res.LiveTasks)
	}
	wantDetail := longCommand[:maxDetailLen]
	if res.LiveTasks[0].Detail != wantDetail {
		t.Errorf("Detail = %q (len %d), want %q (len %d)",
			res.LiveTasks[0].Detail, len(res.LiveTasks[0].Detail), wantDetail, len(wantDetail))
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
		[]byte(
			`","message":{"id":"msg-new","role":"assistant","content":[`+
				`{"type":"text","text":"kept"},`+
				`{"type":"tool_use","id":"tool-1","name":"Bash",`+
				`"input":{"command":"sleep 1","run_in_background":true}}]}}`+"\n"+
				`{"type":"user","message":{"role":"user","content":[`+
				`{"type":"tool_result","tool_use_id":"tool-1","content":"running"}]},`+
				`"toolUseResult":{"backgroundTaskId":"task-1"}}`+"\n",
		)...,
	)
	res, err := ScanTranscript(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ScanTranscript: %v", err)
	}
	if res.AssistantIdentityReliable || res.LastAssistantUUID != "" || res.LastAssistantMessageID != "" {
		t.Fatalf("invalid raw identity was accepted: %+v", res)
	}
	if res.LastAssistantText != "kept" || len(res.LiveTasks) != 1 {
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
