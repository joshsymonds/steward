// Package main implements the steward CLI application.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/joshsymonds/steward/internal/aliases"
	"github.com/joshsymonds/steward/internal/output"
	"github.com/joshsymonds/steward/internal/shared"
	"github.com/joshsymonds/steward/internal/statusline"
)

const (
	minArgs     = 2
	helpFlag    = "--help"
	helpCommand = "help"
)

// Build-time variables.
var version = "dev"

func main() {
	out := output.NewTerminal(os.Stdout, os.Stderr)

	// Debug logging — log all invocations to a file. Gated behind the
	// STEWARD_DEBUG env var so hot-path subcommands (statusline,
	// subagent-statusline) don't unconditionally touch disk + grow an
	// unbounded log on every tick. Set STEWARD_DEBUG=1 to capture
	// invocations for diagnosis.
	if fileDebugLoggingEnabled() {
		debugLog()
	}

	if len(os.Args) < minArgs {
		printUsage(out)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "statusline":
		runStatusline()
	case "debug":
		runDebugCommand()
	case "mcp":
		runMCPCommand()
	case "config":
		runConfigCommand()
	case "resolve":
		runResolveCommand()
	case "render-clouds":
		runRenderCloudsCommand()
	case "subagent-statusline":
		runSubagentStatuslineCommand()
	case "preview":
		runPreviewCommand()
	case "notify", "session-metadata":
		runNotifyOrSessionMetadataCommand(os.Args[1])
	case "notifyd":
		runNotifydCommand()
	case "version":
		// Print version to stdout as intended output
		out.Raw(fmt.Sprintf("steward %s\n", version))
	case helpCommand, "-h", helpFlag:
		printUsage(out)
	default:
		out.Errorf("Unknown command: %s", os.Args[1])
		printUsage(out)
		os.Exit(1)
	}
}

func fileDebugLoggingEnabled() bool {
	return os.Getenv("STEWARD_DEBUG") == "1" &&
		(len(os.Args) < minArgs || os.Args[1] != "session-metadata")
}

func runNotifyOrSessionMetadataCommand(command string) {
	if command == "notify" {
		runNotifyCommand()
		return
	}
	runSessionMetadataCommand()
}

func printUsage(out *output.Terminal) {
	out.RawError(`steward - terminal coding-agent tools

Usage:
  steward <command> [arguments]

Commands:
  statusline    Generate a Claude-compatible command status line
  debug         Configure debug logging for directories
  mcp           Manage Claude MCP servers
  config        Manage configuration settings
  resolve       Look up alias label + env for a host/aws/k8s/gcloud value
  render-clouds Emit AWS/gcloud/k8s chip chain as ANSI (for starship)
  subagent-statusline  Render per-row chip decorations for the claude agents view
  preview       Render every statusline/subagent-statusline scenario, labeled
  notify        Agent notifier (Claude or Pi JSON on stdin)
  notifyd       Long-running daemon that runs the notify pipeline over a control socket
  session-metadata  Read shared session naming metadata
  version       Print version information
  help          Show this help message

Examples:
  echo '{"cwd": "/path"}' | steward statusline
  steward statusline '{"cwd":"/path","columns":100}'
  steward mcp list
  steward mcp enable jira
  steward resolve --type=k8s --raw="arn:aws:eks:us-east-1:123:cluster/prod"
  echo '{"columns":80,"tasks":[...]}' | steward subagent-statusline
`)
}

func runStatusline() {
	out := output.NewTerminal(os.Stdout, os.Stderr)

	input, err := readStatuslineInput(os.Args[2:], os.Stdin)
	if err != nil {
		// Fallback prompt output to stdout
		out.Raw(" > ")
		os.Exit(0)
	}

	// Recreate stdin reader
	reader := bytes.NewReader(input)

	result, err := runStatuslineWithInput(reader)
	if err != nil {
		// Fallback prompt output to stdout
		out.Raw(" > ")
		os.Exit(0)
	}
	// Output statusline result to stdout
	out.Raw(result)
}

// readStatuslineInput preserves Claude's stdin command-hook contract while
// allowing harness extensions without stdin support to pass the same JSON as
// one positional argument.
func readStatuslineInput(args []string, stdin io.Reader) ([]byte, error) {
	switch len(args) {
	case 0:
		input, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("reading statusline stdin: %w", err)
		}
		return input, nil
	case 1:
		return []byte(args[0]), nil
	default:
		return nil, fmt.Errorf("statusline accepts at most one JSON argument")
	}
}

func runStatuslineWithInput(reader io.Reader) (string, error) {
	deps := &statusline.Dependencies{
		FileReader:    &statusline.DefaultFileReader{},
		CommandRunner: &statusline.DefaultCommandRunner{},
		EnvReader:     &statusline.DefaultEnvReader{},
		TerminalWidth: &statusline.DefaultTerminalWidth{},
		Resolver:      aliases.NewResolverFromDefaultPath(os.Stderr, "steward statusline"),
		CacheDir:      statusline.ResolveCacheDir(),
		CacheDuration: getCacheDuration(),
	}

	sl := statusline.CreateStatusline(deps)

	result, err := sl.Generate(reader)
	if err != nil {
		return "", fmt.Errorf("generating statusline: %w", err)
	}

	return result, nil
}

func getCacheDuration() time.Duration {
	if os.Getenv("DEBUG_CONTEXT") == "1" {
		return 0
	}
	seconds := os.Getenv("STEWARD_STATUSLINE_CACHE_SECONDS")
	if seconds != "" {
		if duration, err := time.ParseDuration(seconds + "s"); err == nil {
			return duration
		}
	}
	const defaultCacheSeconds = 20
	return defaultCacheSeconds * time.Second
}

func debugLog() {
	// Create or append to debug log file for current directory
	debugFile := getDebugLogPath()
	//nolint:gosec // Debug log file path is controlled
	f, err := os.OpenFile(debugFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return // Silently fail if we can't write debug log
	}
	defer func() { _ = f.Close() }()

	// Read stdin and save it for both debug and actual use
	// Only read stdin for commands that actually need it
	var stdinDebugData []byte
	needsStdin := len(os.Args) > 1 && os.Args[1] == "statusline"

	if needsStdin {
		if stat, _ := os.Stdin.Stat(); (stat.Mode() & os.ModeCharDevice) == 0 {
			// There's data in stdin
			stdinDebugData, _ = io.ReadAll(os.Stdin)
			// Create a new reader from the data we just read
			// This will be used by the actual commands
			// Actually, we need to pipe it back - create a temp file
			//nolint:forbidigo // Debug temp file
			if tmpFile, tmpErr := os.CreateTemp("", "steward-stdin-"); tmpErr == nil {
				_, _ = tmpFile.Write(stdinDebugData)
				_, _ = tmpFile.Seek(0, 0)
				os.Stdin = tmpFile //nolint:reassign // Resetting stdin for subsequent reads
			}
		}
	}

	// Log the invocation details
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	_, _ = fmt.Fprintf(f, "\n========================================\n")
	_, _ = fmt.Fprintf(f, "[%s] steward invoked\n", timestamp)
	_, _ = fmt.Fprintf(f, "Args: %v\n", os.Args)
	_, _ = fmt.Fprintf(f, "Environment:\n")
	_, _ = fmt.Fprintf(f, "  STEWARD_DEBUG: %s\n", os.Getenv("STEWARD_DEBUG"))
	_, _ = fmt.Fprintf(f, "  Working Dir: %s\n", func() string {
		if wd, wdErr := os.Getwd(); wdErr == nil {
			return wd
		}
		return "unknown"
	}())

	if len(stdinDebugData) > 0 {
		_, _ = fmt.Fprintf(f, "Stdin: %s\n", string(stdinDebugData))
	} else {
		_, _ = fmt.Fprintf(f, "Stdin: (no data available)\n")
	}

	_, _ = fmt.Fprintf(f, "Command: %s\n", func() string {
		if len(os.Args) > 1 {
			return os.Args[1]
		}
		return "(none)"
	}())
}

// getDebugLogPath returns the debug log path for the current directory.
func getDebugLogPath() string {
	wd, err := os.Getwd()
	if err != nil {
		// Fallback to generic log if we can't get working directory
		return "/tmp/steward.debug"
	}
	return shared.GetDebugLogPathForDir(wd)
}
