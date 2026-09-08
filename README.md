# steward

High-performance Go utilities for terminal coding agents. Steward integrates
with Claude Code and Pi. `steward` and `steward-statusline` are the canonical
executable names.

## Features

### 📊 Rich Statusline
- **Model & cost tracking** - Current model, token usage, running costs
- **Git awareness** - Branch, dirty status, uncommitted file count
- **Environment context** - Kubernetes cluster, AWS profile, custom workspace
- **Visual indicators** - Token usage bars, color-coded states
- **Performance** - Cached results with 20-second refresh

### 🎛️ Development Controls
- **MCP management** - Enable/disable context servers per-project
- **Debug logging** - Detailed execution logs for troubleshooting
- **No daemon required** - Direct execution, no background processes

### 🔔 Turn Notifications

- **Deterministic root delivery** - Claude Stop and Pi TurnComplete events
  always deliver with done urgency; child and internal events stay silent
- **Immediate input delivery** - Claude permission, elicitation, and
  agent-needs-input events deliver with blocked urgency without model latency
- **Daemon-only Pi composition** - `notifyd` may improve a completion body using
  the configured Pi helper, but composition cannot veto delivery or change urgency
- **Canonical configuration** - `STEWARD_NTFY_*` environment variables only

## Agent compatibility

| Capability | Claude Code | Pi |
|---|---|---|
| External notification command | Yes, JSON on stdin | Yes, canonical JSON on stdin |
| Root turn-complete ntfy delivery | Yes | Yes |
| Permission/external approval delivery | Yes | No |
| Daemon-side Pi body composition and shared session labels | Yes, using reliable transcript identity | Yes, using native turn identity |
| Steward-rendered in-app statusline | Yes, through the configured command | Yes, through the packaged extension |

## Installation

### Claude Code Hooks

steward provides the statusline hook that you can use in Claude Code itself.

- **`steward-statusline`** - Generates the rich statusline

### Download Pre-built Binaries

Download the latest release from [GitHub Releases](https://github.com/joshsymonds/steward/releases):

```bash
# Download and extract binaries
wget https://github.com/joshsymonds/steward/releases/latest/download/steward-linux-amd64.tar.gz
tar -xzf steward-linux-amd64.tar.gz

# Move to ~/.claude/bin/ (or any directory in your PATH)
mkdir -p ~/.claude/bin
mv steward-statusline ~/.claude/bin/
chmod +x ~/.claude/bin/steward-statusline
```

### Build from Source (NixOS)

```bash
# Build with Nix
nix-build

# Copy the required binaries
cp ./result/bin/steward-statusline ~/.claude/bin/
```

### Build from Source (Go)

```bash
# Build all binaries
make build

# Copy the required binaries
cp build/steward-statusline ~/.claude/bin/
```

### Claude Code Configuration

Add to your `~/.claude/settings.json`:

```json
{
  "statusLine": {
    "type": "command",
    "command": "~/.claude/bin/steward-statusline",
    "padding": 0
  }
}
```

### Shared notification configuration

The daemon uses `STEWARD_NTFY_URL` and optional `STEWARD_NTFY_TOKEN`.
Their `_FILE` variants read runtime secret files; `STEWARD_NTFY_DISABLED=true`
disables sending. Former environment namespaces are ignored.

Sessionless Pi composition is configured centrally in the daemon environment:

```bash
export STEWARD_HELPER_BIN="steward-pi-helper"
export STEWARD_MODEL_PROVIDER="openai-codex"
export STEWARD_MODEL_ID="gpt-5.6-luna"
export STEWARD_MODEL_THINKING="low"
```

Defaults apply only when absent. An invalid setting, unavailable helper,
authentication failure, malformed result, or timeout retains deterministic
notification delivery. Composition cannot veto completion or change urgency.
See [the Pi helper protocol](docs/pi-helper.md) for bounds and failure behavior.

For an eligible native identity pair, the daemon requests a label on the first
completed exchange and after four additional completed exchanges with changed
material (normally 1, 5, 9).
Minimal metadata persists in `session-labels`; a known label replaces the cwd
project title. Inspect one scope without generation or notification:

```bash
steward session-metadata --harness pi --session-id native-id
```

Metadata is not a delivery receipt. See [shared session labels](docs/session-labels.md)
for generation, retry, and ownership semantics.

Daemon acknowledgement is non-durable local admission: `accepted` or `duplicate`
suppresses inline work. Rejection, timeout, or malformed acknowledgement uses
one deterministic inline fallback from the same prepared snapshot. Claims cover native
IDs for 24 hours, capped at 10,000; restarts clear them. Ambiguity can duplicate
notifications, and crashes can lose accepted work. See [the notification
protocol](docs/notify-protocol.md).

## Control Commands

The `steward` binary provides control commands for managing your development workflow:

### Debug Logging

Enable detailed debug logging to troubleshoot hook behavior:

```bash
# Enable debug logging for current directory
steward debug enable

# Check debug status
steward debug status

# View log file path
steward debug filename

# List all directories with debug enabled
steward debug list

# Disable debug logging
steward debug disable
```

### MCP Server Management

Control which MCP (Model Context Protocol) servers are active per-project:

```bash
# List all MCP servers and their status
steward mcp list

# Enable specific MCP server
steward mcp enable jira
steward mcp enable playwright

# Disable specific MCP server
steward mcp disable targetprocess

# Bulk operations
steward mcp enable-all    # Enable all configured MCPs
steward mcp disable-all   # Disable all MCPs (reduce context)
```

MCP names support flexible matching (e.g., 'target' matches 'targetprocess').

MCP management reads your existing MCP configurations from `~/.claude/settings.json`. Example configuration:

```json
{
  "mcpServers": {
    "playwright": {
      "type": "stdio",
      "command": "~/.claude/playwright-mcp-wrapper.sh",
      "args": [],
      "env": {}
    },
    "targetprocess": {
      "type": "stdio",
      "command": "~/.claude/bin/targetprocess-mcp",
      "args": [],
      "env": {}
    },
    "jira": {
      "type": "stdio",
      "command": "~/.claude/jira-mcp-wrapper.sh",
      "args": [],
      "env": {}
    }
  }
}
```

## Behavior

### Statusline

Generates a rich statusline for Claude Code prompts:

```bash
echo '{"cwd": "/path/to/project", "model": {"display_name": "Claude 3.5"}, "cost": {"input_tokens": 1000}}' | steward statusline
```

Example output: image

The cache variables are `STEWARD_STATUSLINE_CACHE_DIR` and
`STEWARD_STATUSLINE_CACHE_SECONDS`.

When `PATCHBAY_CALLER_KEY_FILE` is set, the statusline asks Patchbay's
`/_patchbay/usage/summary` API for the local-midnight-to-now daily total. Set
`STEWARD_PATCHBAY_URL` to Patchbay's root URL, or omit it to use
`http://127.0.0.1:4100`; leave both variables unset to retain transcript-based
cost display. The URL must use `https`, except `http` is allowed for
`127.0.0.0/8`, `::1`, and `localhost`; URL userinfo is rejected. Costs stay integer nano-USD through half-even cent rounding. A
trailing `~` means Patchbay was unreachable and the chip fell back wholly to
legacy transcript data. `ERR` means the configured API, caller key, or response
is broken, so no cost number is shown. A Patchbay-backed rate-limit alarm carries
no dollar figure because its subscription-capacity signal has no shared monetary
basis with the day summary.

## Configuration

All configuration is managed through the `steward config` command. Settings are stored in `~/.config/steward/config.json` and are automatically created with defaults on first use.

### Viewing Configuration

```bash
# List all settings with current values and defaults
steward config list

# Example output:
# Configuration:
#   statusline:
#     - statusline.cache_dir = /dev/shm (default)
#     - statusline.cache_seconds = 20 (default)
#     - statusline.workspace =  (default)

# View the raw JSON config file
steward config show

# Get a specific value
steward config get statusline.cache_seconds
```

### Setting Configuration

```bash
# Set custom workspace label for statusline
steward config set statusline.workspace "my-project"

# Set cache directory (e.g., for systems without /dev/shm)
steward config set statusline.cache_dir "/tmp"
```

### Resetting to Defaults

```bash
# Reset a specific setting to its default
steward config reset statusline.cache_seconds

# Reset all settings to defaults
steward config reset
```

### Available Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `statusline.workspace` | "" | Custom label shown in statusline (e.g., project name) |
| `statusline.cache_dir` | /dev/shm | Directory for statusline cache files (fast tmpfs recommended) |
| `statusline.cache_seconds` | 20 | How long to cache statusline data before refreshing |

The `config list` command clearly shows which values are customized vs defaults, making it easy to see what you've changed from the standard configuration.

## Development

### Building

```bash
# Run tests
make test

# Run lints
make lint

# Build binary
make build

# Run all checks
make check
```

### Testing

```bash
# Unit tests
go test ./...

# With race detection
go test -race ./...

# Specific package
go test ./internal/statusline/...

# Verbose output
go test -v ./...
```

## License

MIT

## Author

Josh Symonds ([@joshsymonds](https://github.com/joshsymonds))
