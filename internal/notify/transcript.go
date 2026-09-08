// Package notify parses Claude Code session transcripts for notification inputs.
package notify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	initialScanBufferSize = 64 * 1024
	maxLineBufferSize     = 10 * 1024 * 1024
	maxAssistantTextLen   = 2000
)

var systemReminderPattern = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// GoalStatus represents the latest structural /goal state.
type GoalStatus int

// Goal statuses emitted by Claude Code goal_status attachments.
const (
	GoalNone GoalStatus = iota
	GoalActive
	GoalMet
	GoalCleared
	GoalFailed
)

// GoalState captures the latest goal_status verdict.
type GoalState struct{ Status GoalStatus }

type transcriptRecord struct {
	Type       string          `json:"type"`
	IsMeta     bool            `json:"isMeta"`
	UUID       json.RawMessage `json:"uuid"`
	Message    *rawMessage     `json:"message"`
	Attachment json.RawMessage `json:"attachment"`
}

type rawMessage struct {
	ID      json.RawMessage `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const contentBlockTypeText = "text"

type attachmentEnvelope struct {
	Type string `json:"type"`
}
type goalAttachment struct {
	Met      bool `json:"met"`
	Sentinel bool `json:"sentinel"`
	Failed   bool `json:"failed"`
}

// ScanResult contains notification-relevant transcript state.
type ScanResult struct {
	Goal                      GoalState
	LastUserMessage           string
	LastAssistantText         string
	LastAssistantUUID         string
	LastAssistantMessageID    string
	AssistantIdentityReliable bool
}

type scanState struct {
	goal                      GoalState
	lastUserMessage           string
	lastAssistantText         string
	lastAssistantUUID         string
	lastAssistantMessageID    string
	assistantIdentityReliable bool
}

// ScanTranscript scans JSONL records, skipping malformed records.
func ScanTranscript(r io.Reader) (ScanResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, initialScanBufferSize), maxLineBufferSize)
	state := &scanState{goal: GoalState{Status: GoalNone}}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) != 0 {
			state.processLine(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return state.result(), fmt.Errorf("scanning transcript: %w", err)
	}
	return state.result(), nil
}

func (s *scanState) result() ScanResult {
	return ScanResult{
		Goal: s.goal, LastUserMessage: s.lastUserMessage,
		LastAssistantText:         s.lastAssistantText,
		LastAssistantUUID:         s.lastAssistantUUID,
		LastAssistantMessageID:    s.lastAssistantMessageID,
		AssistantIdentityReliable: s.assistantIdentityReliable,
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
	if rec.Type == "attachment" {
		s.processAttachment(rec.Attachment)
	}
	if rec.Message == nil {
		return
	}
	switch rec.Message.Role {
	case "assistant":
		s.lastAssistantUUID, s.lastAssistantMessageID = "", ""
		s.assistantIdentityReliable = false
		if rawUTF8Valid {
			s.lastAssistantUUID = optionalString(rec.UUID)
			s.lastAssistantMessageID = optionalString(rec.Message.ID)
			s.assistantIdentityReliable = validCompletionID(s.lastAssistantUUID) ||
				validCompletionID(s.lastAssistantMessageID)
		}
		s.captureAssistantText(rec.Message.Content)
	case "user":
		s.assistantIdentityReliable = false
		s.captureUserText(rec.Message.Content, rec.IsMeta)
	}
}

func optionalString(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func (s *scanState) captureUserText(raw json.RawMessage, isMeta bool) {
	if isMeta {
		return
	}
	text, plain := plainStringContent(raw)
	if !plain {
		var ok bool
		text, ok = humanTypedBlockText(raw)
		if !ok {
			return
		}
	}
	text = strings.TrimSpace(systemReminderPattern.ReplaceAllString(text, ""))
	if text == "" || strings.HasPrefix(text, "<") {
		return
	}
	s.lastUserMessage = text
}

func plainStringContent(raw json.RawMessage) (string, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

func humanTypedBlockText(raw json.RawMessage) (string, bool) {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false
	}
	var sb strings.Builder
	hasText := false
	for _, block := range blocks {
		if block.Type == "tool_result" {
			return "", false
		}
		if block.Type == contentBlockTypeText {
			hasText = true
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), hasText
}

func (s *scanState) processAttachment(raw json.RawMessage) {
	var envelope attachmentEnvelope
	if len(raw) == 0 || json.Unmarshal(raw, &envelope) != nil || envelope.Type != "goal_status" {
		return
	}
	var attachment goalAttachment
	if json.Unmarshal(raw, &attachment) == nil {
		s.applyGoalStatus(attachment)
	}
}

func (s *scanState) applyGoalStatus(att goalAttachment) {
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

func (s *scanState) captureAssistantText(raw json.RawMessage) {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	var sb strings.Builder
	for _, block := range blocks {
		if block.Type == contentBlockTypeText {
			sb.WriteString(block.Text)
		}
	}
	if text := sb.String(); text != "" {
		s.lastAssistantText = truncateHead(text, maxAssistantTextLen)
	}
}

func truncateHead(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}

const truncationEllipsis = "…"

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
	return truncationEllipsis + strings.Clone(strings.TrimLeft(tail, " "))
}
