// Package notify parses Claude Code session transcripts (JSONL) into a
// ScanResult: goal state, live background task information, recent
// conversation text, session timing, and bytes consumed. It is the
// ground-truth layer a notification pipeline builds on: deterministic
// gates, digests, and a watchdog all consume ScanTranscript's output.
package notify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Scanner tuning: real transcript lines can run multi-hundred-KB and files
// reach tens of MB, so we stream line-by-line with a generous max line size
// rather than loading the whole file.
const (
	initialScanBufferSize = 64 * 1024
	maxLineBufferSize     = 10 * 1024 * 1024
	maxDetailLen          = 200
	// substantiveUserMinLen is the trimmed length (in bytes) a human-typed
	// user message must reach to update LastSubstantiveUser rather than only
	// LastUserMessage. Chosen to exclude one-word acks ("yes", "ok", "go
	// ahead") while still capturing short-but-real asks.
	substantiveUserMinLen = 40
	// maxAssistantTextLen bounds LastAssistantText via truncateHead, which
	// keeps the tail: the end of an assistant turn carries the ask or
	// conclusion, unlike the user-message truncation elsewhere in this
	// package which keeps the head.
	maxAssistantTextLen = 2000
)

var (
	taskIDPattern = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)

	// systemReminderPattern matches an embedded <system-reminder>...</system-reminder>
	// span so it can be stripped from a user message before the human-typed-text
	// checks run: a real prompt often carries one of these appended by the CLI,
	// and a reminder-only record must not be mistaken for typed text. Non-greedy
	// and dot-matches-newline since reminders are frequently multiline.
	systemReminderPattern = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

	// bashOutputFilePattern extracts the on-disk output path out of a
	// background-bash launch's tool_result content text: the structured
	// toolUseResult for that launch (backgroundTaskId) carries no path, but
	// the human-readable ack alongside it reads "...Output is being written
	// to: /tmp/claude-1000/<proj>/<session>/tasks/<id>.output. You will be
	// notified...". \S+ is deliberate over a charset like [^\s.]+: real
	// paths contain dots (session UUIDs, the .output suffix) and dashes
	// (anonymized project segments), so excluding "." would truncate the
	// match. The only punctuation to strip is the sentence-final period the
	// ack appends after ".output" — see extractOutputFile. An async-agent
	// launch needs no such extraction: its toolUseResult carries the path
	// directly as the structured outputFile field.
	bashOutputFilePattern = regexp.MustCompile(`Output is being written to: (\S+)`)

	// resumeOutputFilePattern extracts the on-disk output path out of an
	// agent-resume toolUseResult's message text, which reads "...You'll be
	// notified when it finishes. Output: /tmp/claude-1000/<proj>/<session>/
	// tasks/<id>.output" — the resume shape carries no separate structured
	// path field the way an async launch's outputFile does.
	resumeOutputFilePattern = regexp.MustCompile(`Output: (\S+)`)

	// agentStoppedByUserPattern extracts the agent ID out of a user-stopped
	// agent's toolUseResult message text ("Agent <id> was stopped by the
	// user and won't be resumed..."). The ID is not a separate structured
	// field on this shape, unlike a launch or resume — text extraction is
	// unavoidable here, but it is only ever attempted on a message already
	// gated by toolUseResult.success == false (see processToolUseResult),
	// never on raw line bytes.
	agentStoppedByUserPattern = regexp.MustCompile(`^Agent (\S+) was stopped by the user`)

	// teammateMessageTagPattern isolates a <teammate-message ...> tag's
	// attribute text so teammateIDAttrPattern and summaryAttrPattern can
	// pull specific attributes out of it regardless of attribute order.
	teammateMessageTagPattern = regexp.MustCompile(`<teammate-message([^>]*)>`)
	teammateIDAttrPattern     = regexp.MustCompile(`teammate_id="([^"]*)"`)
	summaryAttrPattern        = regexp.MustCompile(`summary="([^"]*)"`)
)

// teammateMessagePrefix is the literal prefix the CLI puts on a plain-string
// user message that relays another session's SendMessage — the structural
// gate that must hold before teammateMessageTagPattern is trusted to mean
// anything (ordinary prose could otherwise mention the tag by coincidence).
const teammateMessagePrefix = "Another Claude session sent a message:"

// GoalStatus represents the current state of a /goal condition as recorded
// in a session transcript.
type GoalStatus int

const (
	// GoalNone means no goal_status record was found in the transcript.
	GoalNone GoalStatus = iota
	// GoalActive means a goal is set and has not yet been met, cleared, or failed.
	GoalActive
	// GoalMet means the goal condition was satisfied.
	GoalMet
	// GoalCleared means the user cleared the goal before it was met.
	GoalCleared
	// GoalFailed means goal evaluation gave up without the condition being met.
	GoalFailed
)

// GoalState captures the most recent goal_status verdict found in a
// transcript. The last matching record in the transcript wins.
type GoalState struct {
	Status     GoalStatus
	Condition  string
	Iterations int
}

// TaskKind identifies whether a LiveTask is a background Bash command or an
// async Agent launch.
type TaskKind string

const (
	// TaskBash is a background Bash command launch.
	TaskBash TaskKind = "bash"
	// TaskAgent is an async Agent launch.
	TaskAgent TaskKind = "agent"
)

// LiveTask describes a background bash command or agent launch that has not
// yet received a matching completion notification in the transcript.
type LiveTask struct {
	ID          string
	Kind        TaskKind
	Description string
	Detail      string
	LaunchedAt  time.Time
	// OutputFile is the on-disk path the launch acknowledgment said output
	// is being written to, taken verbatim from that ack text (never derived
	// from cwd or session ID: a real bg-bash task has been observed writing
	// under a different session ID than the transcript's own). Empty when
	// the ack carried no path; this never affects liveness.
	OutputFile string
}

// transcriptRecord is the subset of a transcript JSONL line's shape that
// ScanTranscript cares about. Unknown fields are ignored by encoding/json.
type transcriptRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Content   string          `json:"content"`
	IsMeta    bool            `json:"isMeta"`
	UUID      json.RawMessage `json:"uuid"`
	Message   *rawMessage     `json:"message"`
	// ToolUseResult is the harness's own typed record of what a tool call
	// did, attached to a user record alongside its tool_result content
	// block. It is authoritative over ack prose for task liveness: a
	// backgroundTaskId/agentId/task_id field means exactly what it says,
	// unlike text a session could coincidentally quote (e.g. a grep whose
	// own output mentions a launch pattern). It is a plain string rather
	// than an object for some tool errors (e.g. "Error: No task found with
	// ID: ..."), so processToolUseResult tries a string decode first.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Attachment    json.RawMessage `json:"attachment"`
}

// TeammateActivity describes a teammate agent spawned during the session:
// when it was spawned, and — once it has sent one — the timing and summary
// of its most recent SendMessage back to this session. Teammates are never
// part of LiveTasks or any liveness gate; they are surfaced in the digest
// purely as context for the judge.
type TeammateActivity struct {
	Name          string
	ID            string
	SpawnedAt     time.Time
	LastMessageAt time.Time
	LastSummary   string
}

// rawMessage defers decoding of Content: it is a plain string for
// human-typed messages and system reminders, and an array of contentBlock
// for assistant tool_use turns and user tool_result turns.
type rawMessage struct {
	ID      json.RawMessage `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlock covers tool_use, tool_result, and text block shapes found in
// message.content arrays. Its own Content field defers decoding the same
// way: a tool_result's content is a plain string for Bash results and an
// array of text blocks for Agent results.
type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Text      string          `json:"text"`
}

// contentBlockTypeText is the contentBlock.Type discriminator for a plain
// text block, as distinct from tool_use/tool_result blocks.
const contentBlockTypeText = "text"

type bashInput struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	// RunInBackground defaults to false (foreground) when absent, which is
	// the Bash tool's own default.
	RunInBackground bool `json:"run_in_background"`
}

type agentInput struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	// RunInBackground is a pointer because absent must resolve to true
	// (background is the Agent tool's default), unlike Bash's false default.
	RunInBackground *bool `json:"run_in_background"`
}

// attachmentEnvelope is decoded first to route to the right richer struct,
// since "attachment" holds either a goal_status or a queued_command payload.
type attachmentEnvelope struct {
	Type string `json:"type"`
}

type goalAttachment struct {
	Type       string `json:"type"`
	Met        bool   `json:"met"`
	Sentinel   bool   `json:"sentinel"`
	Failed     bool   `json:"failed"`
	Condition  string `json:"condition"`
	Iterations int    `json:"iterations"`
}

type queuedCommandAttachment struct {
	Type   string `json:"type"`
	Prompt string `json:"prompt"`
}

// toolUseInfo is what we remember about a Bash or Agent tool_use record
// until its matching tool_result announces the launch's background/agent ID.
// name and runInBackground are what let processUserMessage kind-check a
// pairing instead of trusting pattern text alone: name must match the
// pattern being registered (Bash for a background-bash ID, Agent for an
// agent ID), and runInBackground must resolve true, before a launch fires.
// detail is truncated to maxDetailLen at store time (in
// processAssistantMessage) rather than deferred to addLiveTask: a session
// can accumulate far more tool_use records awaiting a result than ever
// become live tasks, so truncating here bounds this map's per-entry memory
// immediately instead of holding a full untruncated command/prompt for the
// entry's whole lifetime.
type toolUseInfo struct {
	description     string
	detail          string
	name            string
	runInBackground bool
}

// Tool names as they appear in a transcript's tool_use blocks.
const (
	toolNameBash  = "Bash"
	toolNameAgent = "Agent"
)

// toolUseResult status values that route processToolUseResult. asyncLaunched
// marks a genuine background-agent launch; completed marks a delivered or
// synchronous agent result (which removes a live task if its agentId happens
// to be one — otherwise a no-op); teammateSpawned marks a teammate agent
// launch, which registers a TeammateActivity rather than a LiveTask.
const (
	toolUseStatusAsyncLaunched = "async_launched"
	toolUseStatusCompleted     = "completed"
	toolUseStatusTeammateSpawn = "teammate_spawned"
)

// toolUseResultEnvelope decodes the fields processToolUseResult needs out of
// a user record's structured toolUseResult, across every shape it can take:
// a background-bash launch, an async-agent launch, a delivered/synchronous
// agent completion, a TaskStop terminal, an agent resume, a user-stopped
// agent, or a teammate spawn. Only one group of fields is populated on any
// given real record; which group decides which case in the switch fires.
// toolUseResult can also be a plain string for some tool errors (e.g. "Error:
// No task found with ID: ..."), which fails to decode into this struct
// entirely — see processToolUseResult's string-decode attempt first.
type toolUseResultEnvelope struct {
	// BackgroundTaskID marks a background-bash launch.
	BackgroundTaskID string `json:"backgroundTaskId"`

	// Status, AgentID, and OutputFile are shared by an async-agent launch
	// (Status == toolUseStatusAsyncLaunched) and a delivered/synchronous
	// agent completion (Status == toolUseStatusCompleted); OutputFile is
	// only populated on the launch shape.
	Status     string `json:"status"`
	AgentID    string `json:"agentId"`
	OutputFile string `json:"outputFile"`

	// TaskID marks a TaskStop terminal for a background-bash task.
	TaskID string `json:"task_id"`

	// Success and Message are shared by an agent resume (Success == true,
	// ResumedAgentID != "") and a user-stopped agent (Success == false, ID
	// embedded in Message text — see agentStoppedByUserPattern).
	Success        bool   `json:"success"`
	Message        string `json:"message"`
	ResumedAgentID string `json:"resumedAgentId"`

	// TeammateID and Name mark a teammate spawn (Status ==
	// toolUseStatusTeammateSpawn).
	TeammateID string `json:"teammate_id"`
	Name       string `json:"name"`
}

// ScanResult is the full result of a single streaming pass over a session
// transcript: goal state, live background tasks, the most recent
// conversation turns, session timing, and how many bytes were consumed.
// This is the ground-truth layer a notification pipeline builds on:
// deterministic gates, digests, and a watchdog all consume it.
type ScanResult struct {
	Goal      GoalState
	LiveTasks []LiveTask

	// Teammates lists every teammate agent spawned during the session, in
	// spawn order. It is informational only — never consulted by any
	// liveness gate — so a teammate that never sent a message back still
	// appears here with a zero LastMessageAt.
	Teammates []TeammateActivity

	// LastUserMessage is the most recently human-typed user text found in
	// the transcript, of any length (e.g. a bare "yes").
	LastUserMessage string
	// LastSubstantiveUser is the most recently human-typed user text whose
	// trimmed/stripped length is at least substantiveUserMinLen; short acks
	// never update it.
	LastSubstantiveUser string
	// LastAssistantText is the most recent non-empty concatenation of an
	// assistant record's text blocks, tail-truncated to maxAssistantTextLen
	// via truncateHead so the end of the message (the ask or conclusion)
	// survives truncation.
	LastAssistantText string
	// LastAssistantUUID and LastAssistantMessageID name the most recent
	// decoded assistant record. That record can differ from the one retained
	// in LastAssistantText when its content has no text. Malformed trailing
	// lines remain skipped and therefore do not provide terminal-transcript
	// reliability by themselves.
	LastAssistantUUID      string
	LastAssistantMessageID string
	// AssistantIdentityReliable reports whether the final decoded assistant
	// record supplied at least one valid source identity without a later
	// malformed record or invalidly encoded line.
	AssistantIdentityReliable bool

	// FirstTimestamp and LastTimestamp are the first and last top-level
	// "timestamp" values that parsed successfully, across every record type
	// in the transcript.
	FirstTimestamp time.Time
	LastTimestamp  time.Time

	// UserTurns counts human-typed user records (the same records that can
	// update LastUserMessage/LastSubstantiveUser).
	UserTurns int

	// BytesScanned is the number of input bytes consumed, including the line
	// terminator per line: bufio.Scanner strips terminators, so this
	// accumulates len(line)+1 for every line read (blank lines included). A
	// final unterminated line still counts +1 under this convention, which
	// undercounts the true file size by one byte in that one case; ordinary
	// JSONL files end with a trailing newline, so the count is exact for
	// them and remains a monotone growth baseline otherwise (the only use a
	// watchdog needs).
	BytesScanned int64
}

// scanState is the mutable state threaded through a single streaming pass
// over a transcript.
type scanState struct {
	goal     GoalState
	toolUses map[string]toolUseInfo
	live     map[string]*LiveTask
	order    []string

	teammates     map[string]*TeammateActivity
	teammateOrder []string

	lastUserMessage           string
	lastSubstantiveUser       string
	lastAssistantText         string
	lastAssistantUUID         string
	lastAssistantMessageID    string
	assistantIdentityReliable bool

	firstTimestamp time.Time
	lastTimestamp  time.Time

	userTurns    int
	bytesScanned int64
}

func newScanState() *scanState {
	return &scanState{
		goal:      GoalState{Status: GoalNone},
		toolUses:  make(map[string]toolUseInfo),
		live:      make(map[string]*LiveTask),
		teammates: make(map[string]*TeammateActivity),
	}
}

// ScanTranscript reads a Claude Code session transcript in JSONL format and
// returns a ScanResult combining goal state, live background/agent tasks,
// the most recent conversation turns, session timing, and bytes consumed.
// Malformed or unparseable lines are skipped rather than treated as fatal;
// an empty transcript yields a zero-value ScanResult.
func ScanTranscript(r io.Reader) (ScanResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, initialScanBufferSize), maxLineBufferSize)

	state := newScanState()
	for scanner.Scan() {
		line := scanner.Bytes()
		state.bytesScanned += int64(len(line)) + 1
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		state.processLine(line)
	}
	if err := scanner.Err(); err != nil {
		return state.result(), fmt.Errorf("scanning transcript: %w", err)
	}

	return state.result(), nil
}

// result assembles the accumulated scan state into a ScanResult.
func (s *scanState) result() ScanResult {
	return ScanResult{
		Goal:                      s.goal,
		LiveTasks:                 s.tasks(),
		Teammates:                 s.teammateActivities(),
		LastUserMessage:           s.lastUserMessage,
		LastSubstantiveUser:       s.lastSubstantiveUser,
		LastAssistantText:         s.lastAssistantText,
		LastAssistantUUID:         s.lastAssistantUUID,
		LastAssistantMessageID:    s.lastAssistantMessageID,
		AssistantIdentityReliable: s.assistantIdentityReliable,
		FirstTimestamp:            s.firstTimestamp,
		LastTimestamp:             s.lastTimestamp,
		UserTurns:                 s.userTurns,
		BytesScanned:              s.bytesScanned,
	}
}

func (s *scanState) processLine(line []byte) {
	rawUTF8Valid := utf8.Valid(line)
	var rec transcriptRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		s.assistantIdentityReliable = false
		return
	}
	if !rawUTF8Valid {
		s.assistantIdentityReliable = false
	}

	s.recordTimestamp(rec.Timestamp)

	switch rec.Type {
	case "attachment":
		s.processAttachment(rec.Attachment)
	case "queue-operation":
		s.extractCompletions(rec.Content)
	}

	if rec.Message == nil {
		return
	}
	switch rec.Message.Role {
	case "assistant":
		s.lastAssistantUUID = ""
		s.lastAssistantMessageID = ""
		s.assistantIdentityReliable = false
		if rawUTF8Valid {
			s.lastAssistantUUID = optionalString(rec.UUID)
			s.lastAssistantMessageID = optionalString(rec.Message.ID)
			s.assistantIdentityReliable = validCompletionID(s.lastAssistantUUID) ||
				validCompletionID(s.lastAssistantMessageID)
		}
		s.processAssistantMessage(rec.Message.Content)
	case "user":
		s.assistantIdentityReliable = false
		s.processUserMessage(rec.Message.Content, rec.ToolUseResult, rec.Timestamp)
		// message.content decodes as a plain string for human-typed text,
		// task-notifications, and teammate relays alike — decode it exactly
		// once here and share the result, rather than letting each consumer
		// re-unmarshal the same bytes on every user record of the scan.
		text, isPlain := plainStringContent(rec.Message.Content)
		if isPlain {
			s.processPlainUserText(text, rec.Timestamp)
		}
		s.captureUserText(rec.Message.Content, text, isPlain, rec.IsMeta)
	}
}

// optionalString decodes optional identity metadata without making a
// wrong-typed value invalidate the rest of its transcript record.
func optionalString(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// recordTimestamp updates the running first/last timestamps from a
// top-level "timestamp" value, across every record type. Unparseable or
// empty timestamps are ignored rather than resetting the running values.
func (s *scanState) recordTimestamp(ts string) {
	t := parseTimestamp(ts)
	if t.IsZero() {
		return
	}
	if s.firstTimestamp.IsZero() {
		s.firstTimestamp = t
	}
	s.lastTimestamp = t
}

// captureUserText updates LastUserMessage/LastSubstantiveUser/UserTurns from
// a user-role message.content payload, applying each exclusion as its own
// documented check:
//   - isMeta:true records never count, regardless of content.
//   - a payload that isn't a plain string, or an array of blocks including a
//     tool_result, is not human-typed text.
//   - any embedded <system-reminder>...</system-reminder> span is stripped
//     before the remaining checks, so a real prompt with an appended
//     reminder still counts on its typed portion, and a reminder-only
//     record (which strips down to nothing) does not count at all.
//   - text whose trimmed, stripped form starts with "<" (task-notification
//     and local-command wrappers) is excluded.
func (s *scanState) captureUserText(raw json.RawMessage, text string, isPlain, isMeta bool) {
	if isMeta {
		return
	}
	if !isPlain {
		var ok bool
		if text, ok = humanTypedBlockText(raw); !ok {
			return
		}
	}
	text = systemReminderPattern.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "<") {
		return
	}

	s.userTurns++
	s.lastUserMessage = text
	if len(text) >= substantiveUserMinLen {
		s.lastSubstantiveUser = text
	}
}

// plainStringContent decodes a user message.content payload as a plain JSON
// string. processLine calls it exactly once per user record and shares the
// result with processPlainUserText and captureUserText — the two consumers
// that would otherwise each re-unmarshal the same bytes. A plain string is
// always human-typed; array-of-blocks payloads return ok=false and go
// through humanTypedBlockText instead.
func plainStringContent(raw json.RawMessage) (string, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

// humanTypedBlockText extracts the candidate human-typed text from an
// array-of-blocks user message.content payload (the non-plain-string shape).
// ok is false when the payload is structurally not human-typed text: an
// array containing a tool_result block, or not an array of blocks at all.
// An array is human-typed only if it contains at least one text block and
// no tool_result block, with all text blocks concatenated.
func humanTypedBlockText(raw json.RawMessage) (string, bool) {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false
	}
	var sb strings.Builder
	hasText := false
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			return "", false
		case contentBlockTypeText:
			hasText = true
			sb.WriteString(b.Text)
		}
	}
	if !hasText {
		return "", false
	}
	return sb.String(), true
}

// processPlainUserText handles a user message.content payload that decoded
// as a plain string (see plainStringContent in processLine), checking it
// against the two structural shapes that matter beyond ordinary human-typed
// text:
//   - a task-notification delivered inline as the message's entire content
//     (rather than via a queue-operation record or a queued_command
//     attachment, the two shapes extractCompletions's callers already cover)
//     removes the notified task from live.
//   - a relayed teammate SendMessage (gated on teammateMessagePrefix, never
//     on the embedded tag text alone) updates that teammate's last-message
//     time and summary.
//
// Text matching neither shape is a no-op here; captureUserText separately
// decides whether the same payload counts as human-typed text.
func (s *scanState) processPlainUserText(text, timestamp string) {
	if strings.HasPrefix(text, "<task-notification>") {
		s.extractCompletions(text)
		return
	}
	if name, summary, ok := extractTeammateMessage(text); ok {
		s.updateTeammateMessage(name, summary, timestamp)
	}
}

// extractTeammateMessage pulls the teammate's short name and the summary
// attribute out of a relayed teammate SendMessage's <teammate-message ...>
// tag. The tag's teammate_id attribute carries the SHORT name in relay
// format ("worker-wire", not "worker-wire@session-id") — the same key
// registerTeammate stores under, which is what lets updateTeammateMessage
// index the teammates map with it directly. ok is false unless text is
// structurally gated by teammateMessagePrefix AND carries a well-formed tag
// with a teammate_id attribute — ordinary prose can never be mistaken for a
// real relay. summary is "" (still ok) when the tag carries no summary
// attribute.
func extractTeammateMessage(text string) (string, string, bool) {
	if !strings.HasPrefix(text, teammateMessagePrefix) {
		return "", "", false
	}
	tag := teammateMessageTagPattern.FindStringSubmatch(text)
	if tag == nil {
		return "", "", false
	}
	attrs := tag[1]
	nameMatch := teammateIDAttrPattern.FindStringSubmatch(attrs)
	if nameMatch == nil {
		return "", "", false
	}
	var summary string
	if summaryMatch := summaryAttrPattern.FindStringSubmatch(attrs); summaryMatch != nil {
		summary = summaryMatch[1]
	}
	return nameMatch[1], summary, true
}

// processAttachment handles the top-level "attachment" field, which carries
// either a goal_status verdict or a queued_command task-notification.
func (s *scanState) processAttachment(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var envelope attachmentEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return
	}
	switch envelope.Type {
	case "goal_status":
		var att goalAttachment
		if err := json.Unmarshal(raw, &att); err == nil {
			s.applyGoalStatus(att)
		}
	case "queued_command":
		var qc queuedCommandAttachment
		if err := json.Unmarshal(raw, &qc); err == nil {
			s.extractCompletions(qc.Prompt)
		}
	}
}

// applyGoalStatus fully replaces the goal state from the fields on the
// current record: the last matching goal_status record in the transcript
// wins outright, it is not merged with whatever came before.
func (s *scanState) applyGoalStatus(att goalAttachment) {
	s.goal.Condition = att.Condition
	s.goal.Iterations = att.Iterations
	switch {
	case att.Sentinel && !att.Met:
		s.goal.Status = GoalActive
	case att.Sentinel && att.Met:
		s.goal.Status = GoalCleared
	case !att.Met && att.Failed:
		s.goal.Status = GoalFailed
	case !att.Met:
		s.goal.Status = GoalActive
	default:
		s.goal.Status = GoalMet
	}
}

// extractCompletions removes any live task whose ID appears in a
// <task-id>...</task-id> wrapper within text. text must come from a record
// already known structurally to be a task-notification (a queue-operation's
// content, a queued_command attachment's prompt, or a plain-string user
// message whose content itself starts with "<task-notification>") — never
// from raw line bytes — so that ordinary prose mentioning "<task-id>" (e.g. a
// transcript where the assistant is discussing this very format) can never be
// mistaken for a real completion.
func (s *scanState) extractCompletions(text string) {
	for _, m := range taskIDPattern.FindAllStringSubmatch(text, -1) {
		delete(s.live, m[1])
	}
}

func (s *scanState) processAssistantMessage(raw json.RawMessage) {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return
	}
	s.captureAssistantText(blocks)
	for _, block := range blocks {
		if block.Type != "tool_use" || block.ID == "" {
			continue
		}
		switch block.Name {
		case toolNameBash:
			var in bashInput
			if err := json.Unmarshal(block.Input, &in); err == nil {
				s.toolUses[block.ID] = toolUseInfo{
					description:     in.Description,
					detail:          truncate(in.Command, maxDetailLen),
					name:            toolNameBash,
					runInBackground: in.RunInBackground,
				}
			}
		case toolNameAgent:
			var in agentInput
			if err := json.Unmarshal(block.Input, &in); err == nil {
				runInBackground := true
				if in.RunInBackground != nil {
					runInBackground = *in.RunInBackground
				}
				s.toolUses[block.ID] = toolUseInfo{
					description:     in.Description,
					detail:          truncate(in.Prompt, maxDetailLen),
					name:            toolNameAgent,
					runInBackground: runInBackground,
				}
			}
		}
	}
}

// captureAssistantText concatenates an assistant record's "text" blocks and,
// if the result is non-empty, replaces LastAssistantText with it
// (tail-truncated). A record with no text blocks (a pure tool_use turn)
// leaves the previous LastAssistantText in place rather than clearing it.
func (s *scanState) captureAssistantText(blocks []contentBlock) {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == contentBlockTypeText {
			sb.WriteString(b.Text)
		}
	}
	text := sb.String()
	if text == "" {
		return
	}
	s.lastAssistantText = truncateHead(text, maxAssistantTextLen)
}

// processUserMessage looks for a tool_result block paired with a structured
// toolUseResult (see transcriptRecord.ToolUseResult) and routes it to
// processToolUseResult. A record with no toolUseResult (an ordinary
// human-typed message, a goal_status/queue-operation record, etc.) is a
// no-op — most user records never carry one.
func (s *scanState) processUserMessage(raw json.RawMessage, toolUseResult json.RawMessage, timestamp string) {
	if len(toolUseResult) == 0 {
		return
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return
	}
	for _, block := range blocks {
		if block.Type != "tool_result" {
			continue
		}
		s.processToolUseResult(block.ToolUseID, extractBlockText(block.Content), toolUseResult, timestamp)
		return
	}
}

// processToolUseResult decodes a user record's structured toolUseResult and
// routes it by shape — a background-bash launch, an async-agent launch, a
// delivered/synchronous agent completion, a TaskStop terminal, an agent
// resume, a user-stopped agent, or a teammate spawn — updating live tasks or
// teammates accordingly. toolUseResult is authoritative over ack prose: a
// backgroundTaskId/agentId/task_id field means exactly what it says, unlike
// text a session could coincidentally quote (e.g. a grep whose own stdout
// mentions a launch pattern, which carries none of these structured fields
// and therefore matches no case below).
//
// toolUseResult is a plain string rather than an object for some tool errors
// (e.g. "Error: No task found with ID: ..."); such a record decodes into
// none of toolUseResultEnvelope's fields as a live JSON object, so the
// string-decode attempt below returns immediately — a "no task found" is
// already a no-op (the task was never live to begin with, or is already
// gone) and requires no further handling.
//
// Launch registration pairs blockText (a bash launch's tool_result content,
// which carries the human-readable output-file sentence the structured
// record itself omits) and the toolUses entry for tool_use_id, deleting that
// entry once consumed: a given tool_use_id is produced once by the assistant
// and consumed at most once by its own matching tool_result, so the entry
// has no further use.
func (s *scanState) processToolUseResult(toolUseID, blockText string, raw json.RawMessage, timestamp string) {
	var opaque string
	if err := json.Unmarshal(raw, &opaque); err == nil {
		return
	}

	var res toolUseResultEnvelope
	if err := json.Unmarshal(raw, &res); err != nil {
		return
	}

	switch {
	case res.BackgroundTaskID != "":
		s.registerLaunch(toolUseID, toolNameBash, res.BackgroundTaskID, TaskBash, timestamp,
			extractOutputFile(bashOutputFilePattern, blockText))
	case res.Status == toolUseStatusAsyncLaunched && res.AgentID != "":
		s.registerLaunch(toolUseID, toolNameAgent, res.AgentID, TaskAgent, timestamp, res.OutputFile)
	case res.Status == toolUseStatusTeammateSpawn:
		s.registerTeammate(res.TeammateID, res.Name, timestamp)
		delete(s.toolUses, toolUseID)
	case res.TaskID != "":
		delete(s.live, res.TaskID)
	case res.Status == toolUseStatusCompleted && res.AgentID != "":
		delete(s.live, res.AgentID)
	case res.ResumedAgentID != "":
		s.addLiveTask(res.ResumedAgentID, TaskAgent, timestamp, toolUseInfo{},
			extractOutputFile(resumeOutputFilePattern, res.Message))
	case !res.Success && res.Message != "":
		if m := agentStoppedByUserPattern.FindStringSubmatch(res.Message); m != nil {
			delete(s.live, m[1])
		}
	}
}

// registerLaunch registers a background-bash or async-agent launch after
// confirming the tool_result's tool_use_id pairs to a remembered tool_use of
// the matching kind (wantName) with run_in_background resolving true —
// never from the launch ID alone. See addLiveTask and toolUseInfo for the
// rationale; a tool_use_id that fails to pair (or pairs to the wrong kind)
// is left in s.toolUses untouched, matching the pre-structured behavior.
func (s *scanState) registerLaunch(toolUseID, wantName, id string, kind TaskKind, timestamp, outputFile string) {
	info, paired := s.toolUses[toolUseID]
	if !paired || info.name != wantName || !info.runInBackground {
		return
	}
	s.addLiveTask(id, kind, timestamp, info, outputFile)
	delete(s.toolUses, toolUseID)
}

// registerTeammate records a newly-spawned teammate, keyed by name: a later
// relayed SendMessage (see extractTeammateMessage) addresses the teammate by
// this same short name, not the fuller "name@session-id" teammate_id the
// spawn record itself carries in TeammateID. A duplicate spawn of the same
// name is a no-op, mirroring addLiveTask's idempotency.
func (s *scanState) registerTeammate(teammateID, name, timestamp string) {
	if name == "" {
		return
	}
	if _, exists := s.teammates[name]; exists {
		return
	}
	s.teammates[name] = &TeammateActivity{
		Name:      name,
		ID:        teammateID,
		SpawnedAt: parseTimestamp(timestamp),
	}
	s.teammateOrder = append(s.teammateOrder, name)
}

// updateTeammateMessage records the timing and summary of a teammate's most
// recent relayed SendMessage, keyed by the same short name registerTeammate
// stores under (a relay's teammate_id attribute IS that short name — see
// extractTeammateMessage). A message from a name with no matching spawn in
// this scan (e.g. the spawn record fell outside the scanned range) is a
// no-op: there is nowhere to attach the update.
func (s *scanState) updateTeammateMessage(name, summary, timestamp string) {
	tm, ok := s.teammates[name]
	if !ok {
		return
	}
	tm.LastMessageAt = parseTimestamp(timestamp)
	tm.LastSummary = summary
}

// addLiveTask records a newly-launched task, keyed by the ID a launch's
// structured toolUseResult carries (backgroundTaskId, agentId, or
// resumedAgentId) — never from ack prose alone. A duplicate ID (e.g. a
// completed task's ID later reappearing) is a no-op; tasks returns only the
// first registration per ID.
func (s *scanState) addLiveTask(id string, kind TaskKind, timestamp string, info toolUseInfo, outputFile string) {
	if _, exists := s.live[id]; exists {
		return
	}
	s.live[id] = &LiveTask{
		ID:          id,
		Kind:        kind,
		Description: info.description,
		Detail:      info.detail, // already truncated at store time; see toolUseInfo.detail
		LaunchedAt:  parseTimestamp(timestamp),
		OutputFile:  outputFile,
	}
	s.order = append(s.order, id)
}

// extractOutputFile pulls the on-disk output path out of ack/resume text
// using pattern (bashOutputFilePattern for a bash launch's tool_result
// content, or resumeOutputFilePattern for an agent-resume toolUseResult's
// message), returning "" when the text carries no path. The bash sentence
// form's match includes a trailing "." (the path is immediately followed by
// ". You will be notified..." with no space before the period), which
// TrimSuffix strips; the resume form's path sits at the end of the message
// with no trailing punctuation, so the trim is a no-op there.
func extractOutputFile(pattern *regexp.Regexp, text string) string {
	m := pattern.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSuffix(m[1], ".")
}

// tasks returns the still-live tasks in launch order. An ID can appear more
// than once in order (e.g. a completed task whose ID later reappears in a
// paired launch), so seen guards against emitting the same live task twice.
func (s *scanState) tasks() []LiveTask {
	if len(s.order) == 0 {
		return nil
	}
	result := make([]LiveTask, 0, len(s.order))
	seen := make(map[string]bool, len(s.order))
	for _, id := range s.order {
		if seen[id] {
			continue
		}
		seen[id] = true
		if lt, ok := s.live[id]; ok {
			result = append(result, *lt)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// teammateActivities returns every registered teammate in spawn order.
func (s *scanState) teammateActivities() []TeammateActivity {
	if len(s.teammateOrder) == 0 {
		return nil
	}
	result := make([]TeammateActivity, 0, len(s.teammateOrder))
	for _, name := range s.teammateOrder {
		if tm, ok := s.teammates[name]; ok {
			result = append(result, *tm)
		}
	}
	return result
}

// extractBlockText handles a tool_result's content field, which is a plain
// string for Bash results and an array of {"type":"text",...} blocks for
// Agent results.
func extractBlockText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// truncate returns s cut to at most maxLen bytes. The truncated branch
// returns strings.Clone(s[:maxLen]) rather than the bare substring: callers
// (processAssistantMessage's toolUseInfo.detail, and the goal-condition
// fallback text in watchdog.go) store the result long-term, and a plain
// substring slice would still alias — and therefore pin in memory — the
// entire source backing array (a full command or prompt) for as long as the
// truncated copy lives.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return strings.Clone(s[:maxLen])
}

// truncateHead mirrors truncate but keeps the END of s rather than the
// start, for text where the tail carries the meaningful content (an
// assistant turn's ask or conclusion is at the end, not the beginning).
func truncateHead(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}

// truncationEllipsis marks a word-boundary cut in human-visible text.
const truncationEllipsis = "…"

// truncateWords bounds s to maxLen bytes for text a person (or the session
// model) will read: unlike truncate's raw byte chop, it never splits a
// UTF-8 rune, prefers cutting at the last word boundary that fits, and
// appends an ellipsis to mark the cut. Strings within budget pass through
// unchanged.
func truncateWords(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	budget := maxLen - len(truncationEllipsis)
	if budget < 1 {
		return truncationEllipsis
	}
	for budget > 0 && !utf8.RuneStart(s[budget]) {
		budget--
	}
	cut := s[:budget]
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	cut = strings.TrimRight(cut, " ")
	if cut == "" {
		cut = s[:budget]
	}
	return strings.Clone(cut) + truncationEllipsis
}

// truncateHeadWords mirrors truncateWords but keeps the END of s, for
// human-visible text whose tail carries the meaning; the ellipsis goes in
// front. Strings within budget pass through unchanged.
func truncateHeadWords(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	budget := maxLen - len(truncationEllipsis)
	if budget < 1 {
		return truncationEllipsis
	}
	tail := s[len(s)-budget:]
	for tail != "" && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	if i := strings.IndexByte(tail, ' '); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	tail = strings.TrimLeft(tail, " ")
	return truncationEllipsis + strings.Clone(tail)
}

func parseTimestamp(ts string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}
	}
	return t
}
