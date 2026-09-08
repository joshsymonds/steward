package notify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	preparedEventVersion      = 1
	maximumPreparedTextBytes  = 8192
	reservedPreparedTextBytes = 4096
	maximumPreparedInputBytes = 160

	maximumPreparedIDBytes     = 256
	maximumPreparedCWDBytes    = 4096
	maximumPreparedSourceBytes = 128

	eventKindCompletion = "completion"
	eventKindInput      = "input"
	eventKindCleanup    = "cleanup"
	eventKindIgnored    = "ignored"
)

// PreparedEvent is the immutable, provider-neutral snapshot handed from the
// native hook adapter to either notifyd or the model-free inline path. It
// deliberately contains no transcript path, process context, credentials, or
// mutable scanner state.
type PreparedEvent struct {
	Version          int    `json:"version"`
	Harness          string `json:"harness"`
	SessionID        string `json:"session_id"`
	Kind             string `json:"kind"`
	SourceEvent      string `json:"source_event"`
	CWD              string `json:"cwd"`
	CompletionID     string `json:"completion_id"`
	User             string `json:"user"`
	Assistant        string `json:"assistant"`
	NotificationType string `json:"notification_type"`
	Message          string `json:"message"`
	AgentID          string `json:"agent_id"`
	AgentType        string `json:"agent_type"`
	GoalActive       bool   `json:"goal_active"`
}

// PrepareEvent performs the only source-side transcript scan and freezes all
// facts needed by later delivery. A Claude Stop trusts only a reliable final
// assistant UUID (then message.id); failure to establish that identity clears
// any hook-supplied ID and keeps a deterministic hook-text fallback.
func PrepareEvent(input HookInput) (PreparedEvent, error) {
	harness := defaultHarness(input.Harness)
	input.Harness = harness
	if !validPreparationInput(input, harness) {
		return PreparedEvent{}, invalidPreparedEventError()
	}

	event := PreparedEvent{
		Version: preparedEventVersion, Harness: harness, SessionID: input.SessionID,
		Kind:        preparedEventKind(harness, input.HookEventName, input.NotificationType),
		SourceEvent: input.HookEventName, CWD: input.CWD,
		CompletionID: input.CompletionID,
		AgentID:      input.AgentID, AgentType: input.AgentType,
	}

	switch event.Kind {
	case eventKindCompletion:
		event.User, event.Assistant = input.Message, input.LastAssistantMessage
		if harness == harnessClaude && input.HookEventName == eventStop {
			event.CompletionID = ""
			scan, scanErr := scanPreparedTranscript(input.TranscriptPath)
			event.GoalActive = scan.Goal.Status == GoalActive
			if scanErr == nil && scan.AssistantIdentityReliable {
				event.CompletionID = claudeCompletionID(input, scan, nil)
				event.User = scan.LastUserMessage
				event.Assistant = scan.LastAssistantText
			}
		}
		event.User, event.Assistant = boundPreparedText(event.User, event.Assistant)
	case eventKindInput, eventKindIgnored:
		event.NotificationType = input.NotificationType
		event.Message = truncateWords(normalizeCompletionPlainText(input.Message), maximumPreparedInputBytes)
	}

	if err := validatePreparedEvent(event); err != nil {
		return PreparedEvent{}, err
	}
	return event, nil
}

func preparedEventKind(harness, sourceEvent, notificationType string) string {
	switch sourceEvent {
	case eventStop:
		if harness == harnessClaude {
			return eventKindCompletion
		}
	case eventTurnComplete:
		if harness == harnessPi {
			return eventKindCompletion
		}
	case eventNotification:
		if harness == harnessClaude && recognizedInputType(notificationType) {
			return eventKindInput
		}
	case eventSessionEnd:
		if harness == harnessClaude {
			return eventKindCleanup
		}
	}
	return eventKindIgnored
}

func validPreparationInput(input HookInput, harness string) bool {
	if !knownHarness(harness) || !validPreparedMetadata(input.HookEventName, maximumPreparedSourceBytes, false) ||
		!validPreparedMetadata(input.SessionID, maximumPreparedIDBytes, true) ||
		!validPreparedMetadata(input.CWD, maximumPreparedCWDBytes, true) ||
		!validPreparedMetadata(input.NotificationType, maximumPreparedSourceBytes, true) ||
		!validPreparedMetadata(input.AgentID, maximumPreparedIDBytes, true) ||
		!validPreparedMetadata(input.AgentType, maximumPreparedIDBytes, true) ||
		!utf8.ValidString(input.Message) || !utf8.ValidString(input.LastAssistantMessage) ||
		!utf8.ValidString(input.TranscriptPath) {
		return false
	}
	// A Claude Stop never trusts the hook's completion ID, so even stale or
	// malformed supplied data is discarded rather than propagated. Every
	// other source must have valid metadata whenever it supplies an ID.
	return harness == harnessClaude && input.HookEventName == eventStop ||
		validPreparedMetadata(input.CompletionID, maximumPreparedIDBytes, true)
}

func scanPreparedTranscript(path string) (ScanResult, error) {
	if path == "" {
		return ScanResult{}, errors.New("transcript unavailable")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return ScanResult{}, errors.New("transcript unavailable")
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return ScanResult{}, errors.New("transcript unavailable")
	}
	defer func() { _ = file.Close() }()
	result, err := ScanTranscript(file)
	if err != nil {
		return result, errors.New("transcript unavailable")
	}
	return result, nil
}

func boundPreparedText(user, assistant string) (string, string) {
	userBudget := maximumPreparedTextBytes - min(len(assistant), reservedPreparedTextBytes)
	assistantBudget := maximumPreparedTextBytes - min(len(user), reservedPreparedTextBytes)
	return piUTF8Tail(user, userBudget), piUTF8Tail(assistant, assistantBudget)
}

func validPreparedMetadata(value string, maximum int, emptyOK bool) bool {
	if value == "" {
		return emptyOK
	}
	return len(value) <= maximum && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func validPreparedText(event PreparedEvent) bool {
	return utf8.ValidString(event.User) && utf8.ValidString(event.Assistant) &&
		len(event.User)+len(event.Assistant) <= maximumPreparedTextBytes &&
		utf8.ValidString(event.Message) && len(event.Message) <= maximumPreparedInputBytes
}

// validatePreparedEvent applies the strict semantic shape shared by socket
// ingress and RunPrepared. Its fixed error never includes caller data.
func validatePreparedEvent(event PreparedEvent) error {
	if !validPreparedEventMetadata(event) || !validPreparedEventShape(event) {
		return invalidPreparedEventError()
	}
	return nil
}

func validPreparedEventMetadata(event PreparedEvent) bool {
	if event.Version != preparedEventVersion || !knownHarness(event.Harness) {
		return false
	}
	checks := []struct {
		value   string
		maximum int
		emptyOK bool
	}{
		{event.SessionID, maximumPreparedIDBytes, true},
		{event.CWD, maximumPreparedCWDBytes, true},
		{event.SourceEvent, maximumPreparedSourceBytes, false},
		{event.CompletionID, maximumPreparedIDBytes, true},
		{event.NotificationType, maximumPreparedSourceBytes, true},
		{event.AgentID, maximumPreparedIDBytes, true},
		{event.AgentType, maximumPreparedIDBytes, true},
	}
	for _, check := range checks {
		if !validPreparedMetadata(check.value, check.maximum, check.emptyOK) {
			return false
		}
	}
	return validPreparedText(event)
}

func validPreparedEventShape(event PreparedEvent) bool {
	switch event.Kind {
	case eventKindCompletion:
		return event.Message == "" && event.NotificationType == "" && validCompletionEventShape(event)
	case eventKindInput:
		return event.Harness == harnessClaude && event.SourceEvent == eventNotification &&
			validPreparedNotificationShape(event) && recognizedInputType(event.NotificationType)
	case eventKindCleanup:
		return event.Harness == harnessClaude && event.SourceEvent == eventSessionEnd &&
			validNonCompletionTextShape(event) && event.NotificationType == "" && event.Message == ""
	case eventKindIgnored:
		return validPreparedNotificationShape(event) &&
			preparedEventKind(event.Harness, event.SourceEvent, event.NotificationType) == eventKindIgnored
	default:
		return false
	}
}

func validPreparedNotificationShape(event PreparedEvent) bool {
	return validNonCompletionTextShape(event) &&
		event.Message == truncateWords(normalizeCompletionPlainText(event.Message), maximumPreparedInputBytes)
}

func validCompletionEventShape(event PreparedEvent) bool {
	switch event.Harness {
	case harnessClaude:
		return event.SourceEvent == eventStop
	case harnessPi:
		return event.SourceEvent == eventTurnComplete && !event.GoalActive
	default:
		return false
	}
}

func validNonCompletionTextShape(event PreparedEvent) bool {
	return event.CompletionID == "" && event.User == "" && event.Assistant == "" && !event.GoalActive
}

func recognizedInputType(notificationType string) bool {
	return decideNotification(HookInput{NotificationType: notificationType}).Outcome == OutcomeSend
}

func invalidPreparedEventError() error { return errors.New("notify: invalid prepared event") }
