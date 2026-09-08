package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxNotificationFallbackBytes = 160
	maxNotificationBodyBytes     = 200
	turnCompleteLabel            = "turn complete"

	compositionComposed = "composed"
	compositionFallback = "fallback"

	compositionErrorIdentityUnavailable  = "completion identity unavailable"
	compositionErrorUnavailable          = "composer unavailable"
	compositionErrorLabelsUnavailable    = "labels unavailable"
	compositionErrorDryRun               = "dry run"
	compositionErrorInvalidConfiguration = "invalid configuration"
	compositionErrorInvalidRequest       = "invalid compose request"
	compositionErrorHelperUnavailable    = "helper unavailable"
	compositionErrorHelperExecution      = "helper execution failed"
	compositionErrorHelperTimeout        = "helper timed out"
	compositionErrorHelperCanceled       = "helper canceled"
	compositionErrorInvalidProtocol      = "invalid helper protocol"
	compositionErrorRejectedRequest      = "helper rejected request"
	compositionErrorModelUnavailable     = "helper model unavailable"
	compositionErrorGenerationFailed     = "helper generation failed"
	compositionErrorReportedTimeout      = "helper reported timeout"
	compositionErrorInvalidOutput        = "helper output invalid"
	compositionErrorInvalidResult        = "invalid compose result"
	compositionErrorFailed               = "composition failed"

	deliveryTimeout              = 11 * time.Second
	deliveryFailureReason        = "send failed"
	decisionLogFailureDiagnostic = "steward: decision log append failed"
)

// Composer is the sole inference seam used by Pipeline. PiComposer is the
// only production implementation; tests inject this minimal interface to
// observe composition without invoking a model.
type Composer interface {
	Compose(context.Context, ComposeInput, ComposeLabel) (ComposeResult, error)
}

// Pipeline processes one normalized hook event. Completion eligibility and
// urgency are deterministic; Composer may enrich the notification body and a
// requested shared session label.
type Pipeline struct {
	DryRun bool

	Composer Composer
	// CompositionError records why daemon startup could not configure its
	// composer. It is categorized before being logged and never exposed raw.
	CompositionError error

	Sender     Sender
	Log        DecisionLog
	Stdout     io.Writer
	LabelStore *LabelStore

	// Workspace is the calling hook's tmux locator, snapshotted from Frame.
	Workspace string
	// Host is the short hostname used when Workspace is unavailable.
	Host string
}

// Run prepares one native event exactly once, delegates to RunPrepared, and
// preserves the hook's exit-zero contract even when preparation is invalid.
func (pipeline Pipeline) Run(ctx context.Context, input HookInput) error {
	prepared, ok := preparePipelineEvent(input)
	if !ok {
		return nil
	}
	_ = pipeline.RunPrepared(ctx, prepared)
	return nil
}

func preparePipelineEvent(input HookInput) (PreparedEvent, bool) {
	prepared, err := PrepareEvent(input)
	return prepared, err == nil
}

// RunPrepared processes one immutable event snapshot. Callers that need claim
// ownership use processPrepared's delivery result; the exported API reports
// only invalid prepared events and never a raw sender or composer error.
func (pipeline Pipeline) RunPrepared(ctx context.Context, event PreparedEvent) error {
	_, err := pipeline.processPrepared(ctx, event)
	return err
}

// processPrepared returns whether the final deterministic notification was
// delivered. That private result lets notifyd retain or release a claim while
// preserving RunPrepared's error contract.
func (pipeline Pipeline) processPrepared(ctx context.Context, event PreparedEvent) (bool, error) {
	if err := validatePreparedEvent(event); err != nil {
		return false, err
	}
	now := time.Now()
	project := filepath.Base(event.CWD)
	host := pipeline.Host
	if host == "" {
		host = ShortHostname()
	}
	locus := pipeline.Workspace
	if locus == "" {
		locus = host
	}

	decision := decidePreparedEvent(event)
	if decision.Outcome == OutcomeSilent {
		pipeline.logRecord(event, now, DecisionRecord{
			Outcome: decision.Outcome.String(), Reason: decision.Reason,
		})
		return true, nil
	}
	if event.Kind == eventKindCompletion {
		return pipeline.handleCompletion(ctx, event, now, project, locus, decision), nil
	}
	return pipeline.handleInput(ctx, event, now, project, locus, host, decision), nil
}

func defaultHarness(harness string) string {
	if harness != "" {
		return harness
	}
	return harnessClaude
}

// claudeCompletionID preserves the reliable native identity order: a valid
// terminal assistant UUID wins, then message.id. Claude-supplied IDs are
// never trusted when the transcript cannot establish terminal identity.
func claudeCompletionID(input HookInput, scan ScanResult, scanErr error) string {
	if input.Harness != harnessClaude || input.HookEventName != eventStop {
		return input.CompletionID
	}
	if scanErr != nil || !scan.AssistantIdentityReliable {
		return ""
	}
	if validCompletionID(scan.LastAssistantUUID) {
		return scan.LastAssistantUUID
	}
	if validCompletionID(scan.LastAssistantMessageID) {
		return scan.LastAssistantMessageID
	}
	return ""
}

func decidePreparedEvent(event PreparedEvent) Decision {
	input := HookInput{
		Harness:          event.Harness,
		HookEventName:    event.SourceEvent,
		NotificationType: event.NotificationType,
		Message:          event.Message,
		AgentID:          event.AgentID,
		AgentType:        event.AgentType,
	}
	scan := ScanResult{}
	if event.GoalActive {
		scan.Goal.Status = GoalActive
	}
	return Decide(input, scan)
}

func (pipeline Pipeline) handleCompletion(
	ctx context.Context,
	event PreparedEvent,
	now time.Time,
	project string,
	locus string,
	decision Decision,
) bool {
	eligible := !pipeline.DryRun && event.SessionID != "" && validCompletionID(event.CompletionID)
	plan, request, title, labelUnavailable := pipeline.planCompletionLabel(event, project, eligible)
	result, body, compositionOutcome, compositionError := pipeline.composeCompletion(ctx, event, request, eligible)
	if compositionOutcome == compositionComposed && plan.refresh {
		title, labelUnavailable = pipeline.publishCompletionLabel(plan, result, title, labelUnavailable)
	}
	if labelUnavailable && compositionOutcome == compositionComposed {
		compositionError = compositionErrorLabelsUnavailable
	}

	notification := Notification{
		Title:   title + " · " + locus,
		Body:    body,
		Urgency: UrgencyDone,
	}
	reason := decision.Reason
	if labelUnavailable {
		reason += " (" + compositionErrorLabelsUnavailable + ")"
	}
	reason += compositionReason(compositionOutcome, compositionError)
	deliveryReason, delivered := pipeline.deliver(ctx, notification)
	reason += deliveryReason
	pipeline.logRecord(event, now, DecisionRecord{
		Outcome: decision.Outcome.String(), Reason: reason,
		Urgency: notification.Urgency, Title: notification.Title, Body: notification.Body,
		CompositionOutcome: compositionOutcome, CompositionError: compositionError,
	})
	return delivered
}

func (pipeline Pipeline) planCompletionLabel(
	event PreparedEvent,
	fallback string,
	eligible bool,
) (labelCompositionPlan, ComposeLabel, string, bool) {
	if !eligible || pipeline.LabelStore == nil {
		return labelCompositionPlan{}, ComposeLabel{}, fallback, false
	}
	plan, err := pipeline.LabelStore.planCompletion(event)
	if err != nil {
		return labelCompositionPlan{}, ComposeLabel{}, fallback, true
	}
	title := fallback
	if plan.current != "" {
		title = plan.current
	}
	return plan, ComposeLabel{Current: plan.current, Refresh: plan.refresh}, title, false
}

func (pipeline Pipeline) composeCompletion(
	ctx context.Context,
	event PreparedEvent,
	label ComposeLabel,
	eligible bool,
) (ComposeResult, string, string, string) {
	fallback := completionFallbackBody(event.Assistant)
	switch {
	case pipeline.DryRun:
		return ComposeResult{}, fallback, compositionFallback, compositionErrorDryRun
	case !eligible:
		return ComposeResult{}, fallback, compositionFallback, compositionErrorIdentityUnavailable
	case pipeline.Composer == nil && pipeline.CompositionError != nil:
		return ComposeResult{}, fallback, compositionFallback, safeCompositionError(pipeline.CompositionError)
	case pipeline.Composer == nil:
		return ComposeResult{}, fallback, compositionFallback, compositionErrorUnavailable
	}
	result, err := pipeline.Composer.Compose(
		ctx,
		ComposeInput{User: event.User, Assistant: event.Assistant},
		label,
	)
	if err != nil {
		return ComposeResult{}, fallback, compositionFallback, safeCompositionError(err)
	}
	if !validPipelineComposeResult(result, label) {
		return ComposeResult{}, fallback, compositionFallback, compositionErrorInvalidResult
	}
	return result, truncateWords(result.Body, maxNotificationBodyBytes), compositionComposed, ""
}

func (pipeline Pipeline) publishCompletionLabel(
	plan labelCompositionPlan,
	result ComposeResult,
	title string,
	unavailable bool,
) (string, bool) {
	label := result.Label
	if label == "" {
		label = plan.current
	}
	if err := pipeline.LabelStore.finishCompletion(plan, label); err != nil {
		return title, true
	}
	return label, unavailable
}

func validPipelineComposeResult(result ComposeResult, label ComposeLabel) bool {
	if !validPiBody(result.Body) {
		return false
	}
	if !label.Refresh {
		return result.Label == ""
	}
	if result.Label == "" {
		return label.Current != ""
	}
	return validPiGeneratedLabel(result.Label)
}

func compositionReason(outcome, category string) string {
	if outcome == compositionComposed {
		return " (enriched)"
	}
	switch category {
	case compositionErrorDryRun, compositionErrorIdentityUnavailable:
		return " (enrichment skipped: " + category + ")"
	case compositionErrorUnavailable, compositionErrorInvalidConfiguration:
		return " (enrichment disabled: " + category + ")"
	default:
		return " (enrichment failed: " + category + ")"
	}
}

// safeCompositionError consumes only PiComposer's fixed, secret-safe errors.
// An injected, wrapped, or future error is deliberately collapsed so raw
// subprocess/configuration text can never enter the decision log.
func safeCompositionError(err error) string {
	if err == nil {
		return ""
	}
	switch err.Error() {
	case "pi composer: invalid configuration":
		return compositionErrorInvalidConfiguration
	case "pi composer: invalid compose request":
		return compositionErrorInvalidRequest
	case "pi composer: helper unavailable":
		return compositionErrorHelperUnavailable
	case "pi composer: helper execution failed":
		return compositionErrorHelperExecution
	case "pi composer: helper timed out":
		return compositionErrorHelperTimeout
	case "pi composer: helper canceled":
		return compositionErrorHelperCanceled
	case "pi composer: invalid helper protocol":
		return compositionErrorInvalidProtocol
	case "pi composer: helper rejected request":
		return compositionErrorRejectedRequest
	case "pi composer: helper model unavailable":
		return compositionErrorModelUnavailable
	case "pi composer: helper generation failed":
		return compositionErrorGenerationFailed
	case "pi composer: helper reported timeout":
		return compositionErrorReportedTimeout
	case "pi composer: helper output invalid":
		return compositionErrorInvalidOutput
	default:
		return compositionErrorFailed
	}
}

func (pipeline Pipeline) handleInput(
	ctx context.Context,
	event PreparedEvent,
	now time.Time,
	project string,
	locus string,
	host string,
	decision Decision,
) bool {
	body := decision.Message
	if body == "" {
		body = inputFallbackLabel(event.NotificationType)
	}
	title := project
	labelUnavailable := false
	if !pipeline.DryRun && pipeline.LabelStore != nil && event.SessionID != "" {
		label, err := pipeline.LabelStore.lookupLabel(event.Harness, event.SessionID)
		if err != nil {
			labelUnavailable = true
		} else if label != "" {
			title = label
		}
	}
	where := locus
	if event.NotificationType == notifTypeAgentNeedsInput {
		where = host
	}
	notification := Notification{
		Title:   title + " · " + where,
		Body:    body,
		Urgency: decision.Urgency,
	}
	reason := decision.Reason
	if labelUnavailable {
		reason += " (" + compositionErrorLabelsUnavailable + ")"
	}
	deliveryReason, delivered := pipeline.deliver(ctx, notification)
	reason += deliveryReason
	pipeline.logRecord(event, now, DecisionRecord{
		Outcome: decision.Outcome.String(), Reason: reason,
		Urgency: notification.Urgency, Title: notification.Title, Body: notification.Body,
	})
	return delivered
}

func inputFallbackLabel(notificationType string) string {
	switch notificationType {
	case "permission_prompt":
		return "needs permission"
	case "elicitation_dialog", "elicitation_url_dialog", notifTypeAgentNeedsInput:
		return "needs input"
	default:
		return "notification"
	}
}

// deliver either prints a rehearsal or performs the existing Sender call.
// It returns only a success bit and a fixed safe reason, never a raw Sender
// error.
func (pipeline Pipeline) deliver(ctx context.Context, notification Notification) (string, bool) {
	if pipeline.DryRun {
		writer := pipeline.Stdout
		if writer == nil {
			writer = io.Discard
		}
		_, _ = fmt.Fprintf(
			writer,
			"DRY RUN: [%s] %s — %s\n",
			notification.Urgency,
			notification.Title,
			notification.Body,
		)
		return "", true
	}
	deliveryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryTimeout)
	defer cancel()
	if err := pipeline.Sender.Send(deliveryContext, notification); err != nil {
		return " (" + deliveryFailureReason + ")", false
	}
	return "", true
}

func (pipeline Pipeline) logRecord(event PreparedEvent, now time.Time, record DecisionRecord) {
	record.Time = now
	record.SessionID = event.SessionID
	record.Event = event.SourceEvent
	record.Harness = event.Harness
	record.CompletionID = event.CompletionID
	if err := pipeline.Log.Append(record); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, decisionLogFailureDiagnostic)
	}
}

func completionFallbackBody(raw string) string {
	body := truncateHeadWords(normalizeCompletionPlainText(raw), maxNotificationFallbackBytes)
	if body == "" {
		return turnCompleteLabel
	}
	return body
}

func normalizeCompletionPlainText(raw string) string {
	raw = strings.ToValidUTF8(raw, "")
	if completionRawJSON(raw) {
		return ""
	}
	lines := strings.Split(raw, "\n")
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			continue
		}
		line = stripCompletionMarkdownPrefix(line)
		line = unwrapCompletionMarkdownLinks(line)
		line = strings.ReplaceAll(line, "`", "")
		for _, marker := range []string{"**", "__", "~~", "*", "_"} {
			line = unwrapCompletionMarkdownMarker(line, marker)
		}
		line = strings.Map(func(character rune) rune {
			if unicode.IsControl(character) {
				return ' '
			}
			return character
		}, line)
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	plain := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if completionRawJSON(plain) {
		return ""
	}
	return plain
}

func completionRawJSON(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 2 && (value[0] == '{' || value[0] == '[') && json.Valid([]byte(value))
}

func stripCompletionMarkdownPrefix(line string) string {
	for {
		before := line
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, ">") {
			line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
		}
		if width := completionHeadingPrefixLen(line); width > 0 {
			line = strings.TrimSpace(line[width:])
		}
		if width := completionListPrefixLen(line); width > 0 {
			line = strings.TrimSpace(line[width:])
		}
		if line == before {
			return line
		}
	}
}

func completionHeadingPrefixLen(line string) int {
	count := 0
	for count < len(line) && count < 6 && line[count] == '#' {
		count++
	}
	if count > 0 && count < len(line) && unicode.IsSpace(rune(line[count])) {
		return count + 1
	}
	return 0
}

const completionMarkdownPairWidth = 2

func completionListPrefixLen(line string) int {
	if len(line) >= completionMarkdownPairWidth && strings.ContainsRune("-*+", rune(line[0])) &&
		unicode.IsSpace(rune(line[1])) {
		return completionMarkdownPairWidth
	}
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits+1 < len(line) && (line[digits] == '.' || line[digits] == ')') &&
		unicode.IsSpace(rune(line[digits+1])) {
		return digits + completionMarkdownPairWidth
	}
	return 0
}

func unwrapCompletionMarkdownLinks(value string) string {
	var result strings.Builder
	for {
		open := strings.IndexByte(value, '[')
		if open < 0 {
			result.WriteString(value)
			return result.String()
		}
		labelEnd := strings.Index(value[open+1:], "](")
		if labelEnd < 0 {
			result.WriteString(value)
			return result.String()
		}
		labelEnd += open + 1
		urlEnd := strings.IndexByte(value[labelEnd+completionMarkdownPairWidth:], ')')
		if urlEnd < 0 {
			result.WriteString(value)
			return result.String()
		}
		urlEnd += labelEnd + completionMarkdownPairWidth
		result.WriteString(strings.TrimSuffix(value[:open], "!"))
		result.WriteString(value[open+1 : labelEnd])
		value = value[urlEnd+1:]
	}
}

func unwrapCompletionMarkdownMarker(value string, marker string) string {
	for searchFrom := 0; searchFrom < len(value); {
		openOffset := strings.Index(value[searchFrom:], marker)
		if openOffset < 0 {
			return value
		}
		open := searchFrom + openOffset
		if completionMarkerStartsTechnicalToken(value, open, marker) ||
			!completionMarkerCanOpen(value, open, len(marker)) {
			searchFrom = open + len(marker)
			continue
		}
		closeFrom := open + len(marker)
		for closeFrom < len(value) {
			closeOffset := strings.Index(value[closeFrom:], marker)
			if closeOffset < 0 {
				return value
			}
			closeAt := closeFrom + closeOffset
			if completionMarkerCanClose(value, closeAt, len(marker)) {
				value = value[:open] + value[open+len(marker):closeAt] + value[closeAt+len(marker):]
				searchFrom = open
				break
			}
			closeFrom = closeAt + len(marker)
		}
	}
	return value
}

func completionMarkerStartsTechnicalToken(value string, at int, marker string) bool {
	token := strings.TrimRight(completionTokenAt(value, at), ".,;:!?)]}\"'")
	paired := strings.HasPrefix(token, marker) && strings.HasSuffix(token, marker) && len(token) > 2*len(marker)
	if paired {
		content := token[len(marker) : len(token)-len(marker)]
		return strings.IndexFunc(content, func(character rune) bool {
			return unicode.IsLetter(character) || unicode.IsDigit(character)
		}) < 0
	}
	return strings.ContainsAny(token, `/\.`)
}

func completionTokenAt(value string, at int) string {
	start := at
	for start > 0 {
		previous, size := utf8.DecodeLastRuneInString(value[:start])
		if unicode.IsSpace(previous) {
			break
		}
		start -= size
	}
	end := at
	for end < len(value) {
		next, size := utf8.DecodeRuneInString(value[end:])
		if unicode.IsSpace(next) {
			break
		}
		end += size
	}
	return value[start:end]
}

func completionMarkerCanOpen(value string, at int, markerLength int) bool {
	if at+markerLength >= len(value) {
		return false
	}
	next, _ := utf8.DecodeRuneInString(value[at+markerLength:])
	if unicode.IsSpace(next) {
		return false
	}
	if at == 0 {
		return true
	}
	previous, _ := utf8.DecodeLastRuneInString(value[:at])
	return unicode.IsSpace(previous) || strings.ContainsRune("([{>\"'", previous)
}

func completionMarkerCanClose(value string, at int, markerLength int) bool {
	if at == 0 {
		return false
	}
	previous, _ := utf8.DecodeLastRuneInString(value[:at])
	if unicode.IsSpace(previous) {
		return false
	}
	if at+markerLength == len(value) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(value[at+markerLength:])
	return unicode.IsSpace(next) || unicode.IsPunct(next)
}

// ShortHostname strips the domain suffix from os.Hostname().
func ShortHostname() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	short, _, _ := strings.Cut(host, ".")
	return short
}
