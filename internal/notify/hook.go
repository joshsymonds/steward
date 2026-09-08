package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// eventNotification is the HookEventName of a Notification event, named
// because several call sites branch on it (Decide's event switch, the
// broadcast-facts gate, and the pipeline's lazy SinceLastNotifySame
// computation).
const eventNotification = "Notification"
const eventStop = "Stop"
const eventSessionEnd = "SessionEnd"

// eventTurnComplete is the provider-neutral event emitted by adapters whose
// notification API reports only that an agent turn finished. Unlike Claude's
// Stop hook it carries no transcript path to inspect, so Decide handles it
// without the Claude-specific transcript/judge pipeline.
const eventTurnComplete = "TurnComplete"

const (
	harnessClaude = "claude-code"
	harnessPi     = "pi"
)

// HookInput is the notifier's canonical provider-neutral event. Native
// provider adapters normalize into this shape. Field meaning varies by
// HookEventName: NotificationType/Message apply to Notification events,
// LastAssistantMessage applies to Stop/TurnComplete, and AgentID/AgentType
// identify a subagent/teammate context.
type HookInput struct {
	SchemaVersion  int    `json:"schema_version"`
	Harness        string `json:"harness"`
	SessionID      string `json:"session_id"`
	CompletionID   string `json:"completion_id,omitempty"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	// NotificationType and Message apply to Notification events.
	NotificationType string `json:"notification_type"`
	Message          string `json:"message"`
	// LastAssistantMessage applies to Stop/TurnComplete events.
	LastAssistantMessage string `json:"last_assistant_message"`
	// AgentID and AgentType are set only inside a subagent/teammate context.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
}

// ParseHookInput decodes exactly one provider payload from r and normalizes
// it into HookInput, retaining automatic native Claude detection for callers.
// Unknown fields are ignored. Empty input
// or malformed JSON is an error rather than a zero-value HookInput. When r
// contains more than one JSON value, only the first is decoded.
func ParseHookInput(r io.Reader) (HookInput, error) { return ParseHookInputForHarness(r, "") }

// ParseHookInputForHarness decodes one hook payload, making source authoritative
// when harness is supplied.
func ParseHookInputForHarness(r io.Reader, harness string) (HookInput, error) {
	if harness != "" && !knownHarness(harness) {
		return HookInput{}, parseHookError("unknown harness")
	}
	raw, err := decodeHookJSON(r)
	if err != nil {
		return HookInput{}, err
	}
	fields, err := hookFields(raw)
	if err != nil {
		return HookInput{}, err
	}
	if _, canonical := fields["schema_version"]; canonical {
		return parseCanonicalHook(raw, fields, harness)
	}
	if _, canonical := fields["harness"]; canonical {
		return parseCanonicalHook(raw, fields, harness)
	}
	return parseNativeHook(raw, fields, harness)
}

func decodeHookJSON(r io.Reader) (json.RawMessage, error) {
	dec := json.NewDecoder(r)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, parseHookError("empty input")
		}
		return nil, parseHookError("invalid JSON")
	}
	if !utf8.Valid(raw) {
		return nil, parseHookError("invalid JSON")
	}
	return raw, nil
}

func hookFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, parseHookError("invalid JSON")
	}
	return fields, nil
}

func parseCanonicalHook(
	raw json.RawMessage,
	fields map[string]json.RawMessage,
	source string,
) (HookInput, error) {
	version, ok := canonicalVersion(fields["schema_version"])
	if !ok || version != 1 {
		return HookInput{}, parseHookError("unsupported canonical schema")
	}
	harness, ok := canonicalHarness(fields["harness"])
	if !ok {
		return HookInput{}, parseHookError("unsupported canonical schema")
	}
	if source != "" && source != harness {
		return HookInput{}, parseHookError("harness mismatch")
	}
	var in HookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return HookInput{}, parseHookError("invalid canonical payload")
	}
	// encoding/json matches object keys case-insensitively after preferring
	// exact matches, so later aliases must not overwrite the exact canonical
	// discriminator values validated above.
	in.SchemaVersion = version
	in.Harness = harness
	if !canonicalEvent(harness, in.HookEventName) {
		return HookInput{}, parseHookError("invalid canonical event")
	}
	if err := validateInput(
		in,
		in.Harness != harnessClaude || in.HookEventName == eventTurnComplete,
	); err != nil {
		return HookInput{}, err
	}
	return in, nil
}

func canonicalVersion(raw json.RawMessage) (int, bool) {
	var version int
	err := json.Unmarshal(raw, &version)
	return version, err == nil
}

func canonicalHarness(raw json.RawMessage) (string, bool) {
	var harness string
	if json.Unmarshal(raw, &harness) != nil || !knownHarness(harness) {
		return "", false
	}
	return harness, true
}

func canonicalEvent(harness, event string) bool {
	if harness == harnessPi {
		return event == eventTurnComplete
	}
	return event == eventStop || event == eventNotification || event == eventSessionEnd
}

func parseNativeHook(
	raw json.RawMessage,
	fields map[string]json.RawMessage,
	harness string,
) (HookInput, error) {
	if harness == harnessPi {
		return HookInput{}, parseHookError("Pi payload must be canonical")
	}
	if harness == "" || harness == harnessClaude {
		if stringField(fields, "hook_event_name") == "" && stringField(fields, "session_id") == "" {
			return HookInput{}, parseHookError("unsupported notification type")
		}
		var in HookInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return HookInput{}, parseHookError("invalid Claude payload")
		}
		in.SchemaVersion, in.Harness = 1, harnessClaude
		if err := validateInput(in, false); err != nil {
			return HookInput{}, err
		}
		return in, nil
	}
	return HookInput{}, parseHookError("unknown harness")
}

func stringField(fields map[string]json.RawMessage, name string) string {
	var value string
	_ = json.Unmarshal(fields[name], &value)
	return value
}

func validateInput(in HookInput, completionRequired bool) error {
	// SessionID keys the daemon's in-memory dedupe state and is written
	// verbatim into every decision-log record. Reject anything that could
	// carry traversal into a future filesystem-path use.
	if !validSessionID(in.SessionID) {
		return parseHookError("invalid session_id")
	}
	if completionRequired && !validCompletionID(in.CompletionID) {
		return parseHookError("invalid completion_id")
	}
	if in.CompletionID != "" && !validCompletionID(in.CompletionID) {
		return parseHookError("invalid completion_id")
	}
	return nil
}

func parseHookError(reason string) error { return fmt.Errorf("parsing hook input: %s", reason) }

func knownHarness(h string) bool {
	return h == harnessClaude || h == harnessPi
}

func validCompletionID(id string) bool {
	return id != "" && len(id) <= 256 && utf8.ValidString(id) &&
		!strings.ContainsFunc(id, unicode.IsControl)
}

// validSessionID reports whether id is safe to use as a map key and to join
// onto a filesystem path, should some future call site need to. An empty id
// or "." would collapse filepath.Join(base, id) to base itself; a path
// separator or ".." could steer it outside base.
func validSessionID(id string) bool {
	return id != "" && id != "." &&
		!strings.ContainsRune(id, filepath.Separator) &&
		!strings.Contains(id, "..")
}
