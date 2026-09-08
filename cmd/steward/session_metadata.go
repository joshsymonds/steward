package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/joshsymonds/steward/internal/notify"
)

const (
	sessionMetadataResponseVersion        = 1
	sessionMetadataOutputFailureExitCode  = 1
	sessionMetadataInvalidRequestExitCode = 2
	maximumSessionMetadataIDBytes         = 256
	maximumSessionMetadataOutputBytes     = 2048
)

type sessionMetadataQuery struct {
	harness   string
	sessionID string
	stateBase string
}

type sessionMetadataResponse struct {
	Version          int    `json:"version"`
	Status           string `json:"status"`
	Harness          string `json:"harness"`
	SessionID        string `json:"session_id"`
	Label            string `json:"label"`
	CompletionID     string `json:"completion_id"`
	SourceGeneration string `json:"source_generation"`
	LabelGeneration  string `json:"label_generation"`
}

type invalidSessionMetadataResponse struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
}

func runSessionMetadataCommand() {
	exitCode := runSessionMetadataCommandWithIO(os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func runSessionMetadataCommandWithIO(args []string, _ io.Reader, stdout, _ io.Writer) int {
	query, help, valid := parseSessionMetadataQuery(args)
	if help {
		if err := printSessionMetadataUsage(stdout); err != nil {
			return sessionMetadataOutputFailureExitCode
		}
		return 0
	}
	if !valid {
		if err := writeSessionMetadataJSON(stdout, invalidSessionMetadataResponse{
			Version: sessionMetadataResponseVersion,
			Status:  "invalid_request",
		}); err != nil {
			return sessionMetadataOutputFailureExitCode
		}
		return sessionMetadataInvalidRequestExitCode
	}

	metadata, present, err := notify.ReadLabelMetadata(query.stateBase, query.harness, query.sessionID)
	response := sessionMetadataResponse{
		Version:          sessionMetadataResponseVersion,
		Status:           "missing",
		Harness:          query.harness,
		SessionID:        query.sessionID,
		SourceGeneration: "0",
		LabelGeneration:  "0",
	}
	if err != nil {
		response.Status = "unavailable"
	} else if present {
		response.Status = "known"
		response.Label = metadata.Label
		response.CompletionID = metadata.CompletionID
		response.SourceGeneration = strconv.FormatUint(metadata.SourceGeneration, 10)
		response.LabelGeneration = strconv.FormatUint(metadata.LabelGeneration, 10)
	}
	if err = writeSessionMetadataJSON(stdout, response); err != nil {
		return sessionMetadataOutputFailureExitCode
	}
	return 0
}

func parseSessionMetadataQuery(args []string) (sessionMetadataQuery, bool, bool) {
	if len(args) == 1 && (args[0] == helpFlag || args[0] == "-h") {
		return sessionMetadataQuery{}, true, true
	}

	var query sessionMetadataQuery
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		name, value, hasInlineValue := strings.Cut(args[index], "=")
		if !hasInlineValue {
			index++
			if index >= len(args) {
				return sessionMetadataQuery{}, false, false
			}
			value = args[index]
		}
		if !setSessionMetadataQueryValue(&query, seen, name, value) {
			return sessionMetadataQuery{}, false, false
		}
	}

	if !seen["--harness"] || !seen["--session-id"] ||
		!validSessionMetadataHarness(query.harness) || !validSessionMetadataID(query.sessionID) {
		return sessionMetadataQuery{}, false, false
	}
	if !seen["--state-base"] {
		query.stateBase = defaultNotifyStateBase()
	}
	return query, false, true
}

func setSessionMetadataQueryValue(
	query *sessionMetadataQuery,
	seen map[string]bool,
	name string,
	value string,
) bool {
	if seen[name] {
		return false
	}
	switch name {
	case "--harness":
		query.harness = value
	case "--session-id":
		query.sessionID = value
	case "--state-base":
		query.stateBase = value
	default:
		return false
	}
	seen[name] = true
	return true
}

func validSessionMetadataHarness(harness string) bool {
	switch harness {
	case "claude-code", "pi":
		return true
	default:
		return false
	}
}

func validSessionMetadataID(sessionID string) bool {
	return sessionID != "" && len(sessionID) <= maximumSessionMetadataIDBytes &&
		utf8.ValidString(sessionID) && !strings.ContainsFunc(sessionID, unicode.IsControl)
}

func writeSessionMetadataJSON(stdout io.Writer, response any) error {
	var wire bytes.Buffer
	encoder := json.NewEncoder(&wire)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return fmt.Errorf("encoding session metadata response: %w", err)
	}
	if wire.Len() > maximumSessionMetadataOutputBytes {
		return errors.New("session metadata output exceeds limit")
	}
	written, err := stdout.Write(wire.Bytes())
	if err != nil {
		return fmt.Errorf("writing session metadata response: %w", err)
	}
	if written != wire.Len() {
		return io.ErrShortWrite
	}
	return nil
}

func printSessionMetadataUsage(stdout io.Writer) error {
	const usage = `Usage:
  steward session-metadata --harness <claude-code|pi> --session-id <native-id> [--state-base <path>]

Read validated shared session naming metadata for one exact harness/session pair.
The command never reads stdin or modifies notification state.
`
	written, err := io.WriteString(stdout, usage)
	if err != nil {
		return fmt.Errorf("writing session metadata usage: %w", err)
	}
	if written != len(usage) {
		return io.ErrShortWrite
	}
	return nil
}
