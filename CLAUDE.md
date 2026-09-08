# steward Development Guide

Go implementation of terminal coding-agent utilities for Claude Code and Pi.
Claude Code provides the rich statusline and hook surface.

## Project Structure

```
cmd/
├── steward/             # Main CLI with subcommands (debug, mcp, config)
└── steward-statusline/  # Standalone statusline binary for Claude Code

internal/
├── config/       # JSON config management (~/.config/steward/config.json)
├── debug/        # Debug logging control
├── mcp/          # MCP server enable/disable management
├── output/       # Terminal output formatting and tables
├── statusline/   # Statusline generation with Catppuccin colors
└── shared/       # Project detection, colors, debug paths
```

## Key Design Patterns

### Dependency Injection

All major components use constructor-injected dependencies for testability:

```go
// Statusline uses Dependencies struct
type Dependencies struct {
    FileReader    FileReader
    CommandRunner CommandRunner
    EnvReader     EnvReader
    TerminalWidth TerminalWidth
    CacheDir      string
    CacheDuration time.Duration
}
```

### Exit Code Protocol

Claude Code hooks use specific exit codes:
- `0`: Silent success
- `2`: Show message to user (success or failure with feedback)

## Configuration

Config stored at `~/.config/steward/config.json`:

```json
{
  "statusline": {
    "workspace": "",
    "cache_dir": "/dev/shm",
    "cache_seconds": 20
  }
}
```

## Testing Locally

```bash
# Test statusline
echo '{"cwd": "'$(pwd)'", "model": {"display_name": "Claude"}}' | ./build/steward-statusline

# Enable debug logging
./build/steward debug enable
./build/steward debug filename  # Shows log path
```

## Build Commands

```bash
make build    # Build all binaries to build/
make test     # Run tests with coverage
make lint     # Run gofmt, golangci-lint, deadcode
make install  # Copy binaries to ~/bin
```
