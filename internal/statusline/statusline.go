package statusline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshsymonds/steward/internal/aliases"
)

// Input represents the JSON input from stdin.
type Input struct {
	// Harness identifies the caller. Pi quota rendering is intentionally
	// opt-in and only applies when this is exactly "pi".
	Harness string `json:"harness"`
	// Columns is an optional caller-supplied render width. Claude Code exports
	// COLUMNS for its command hook; other harnesses can only pass structured
	// input, so a positive value here takes precedence over the environment.
	Columns int `json:"columns"`
	// SessionID keys the agents-state file the subagent hook writes
	// (agents.go); empty or malformed ids simply disable the
	// running-agents model-chip swap.
	SessionID string `json:"session_id"`
	Model     struct {
		ID          string `json:"id"`
		Provider    string `json:"provider"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Cost          CostInput `json:"cost"`
	ContextWindow struct {
		UsedPercentage    float64 `json:"used_percentage"`
		ContextWindowSize int     `json:"context_window_size"`
	} `json:"context_window"`
	GitInfo struct {
		Branch       string `json:"branch"`
		IsGitRepo    bool   `json:"is_git_repo"`
		HasUntracked bool   `json:"has_untracked"`
		HasModified  bool   `json:"has_modified"`
	} `json:"git_info"`
	Workspace struct {
		ProjectDir string `json:"project_dir"`
		CurrentDir string `json:"current_dir"`
		CWD        string `json:"cwd"`
	} `json:"workspace"`
	TranscriptPath string `json:"transcript_path"`
	// CWD is the top-level "cwd" field Claude Code sends as a baseline on
	// every payload. It's the last directory fallback before "~" — the
	// richer workspace object wins when present (see getCurrentDir).
	CWD string `json:"cwd"`
	// RateLimits is nil when Claude Code sends no rate_limits object,
	// meaning the session is not a subscription session — that nil/non-nil
	// distinction drives whether rate-limit chips or a cost chip render,
	// and (see costSource) whether transcript-derived anthropic-backend
	// rows price at $0.
	RateLimits   *RateLimitsInput   `json:"rate_limits"`
	StewardQuota *StewardQuotaInput `json:"steward_quota"`
	Effort       *EffortInput       `json:"effort"`
	PR           *PRInput           `json:"pr"`
}

// CostInput is the session cost summary from stdin's "cost" object.
// Absence leaves the zero value (no pointer needed — chip visibility
// keys off RateLimits, not Cost).
type CostInput struct {
	TotalCostUSD      float64 `json:"total_cost_usd"`
	TotalLinesAdded   int     `json:"total_lines_added"`
	TotalLinesRemoved int     `json:"total_lines_removed"`
}

// RateLimitsInput is stdin's "rate_limits" object. Each window is
// individually optional; nil means Claude Code didn't report it.
type RateLimitsInput struct {
	FiveHour *RateLimitWindow `json:"five_hour"`
	SevenDay *RateLimitWindow `json:"seven_day"`
}

// RateLimitWindow is one rolling rate-limit window (five-hour or
// seven-day) with its usage percentage and Unix reset timestamp.
type RateLimitWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}

// StewardQuotaInput is the normalized, renderer-owned Pi quota namespace.
// Pointer scalar fields preserve the distinction between an explicit zero and
// a missing/null value during validation.
type StewardQuotaInput struct {
	Provider  string                   `json:"provider"`
	BaseURL   string                   `json:"base_url"`
	FetchedAt *int64                   `json:"fetched_at"`
	Stale     *bool                    `json:"stale"`
	Windows   StewardQuotaWindowsInput `json:"windows"`
}

// StewardQuotaWindowsInput contains the two upstream Codex quota windows.
type StewardQuotaWindowsInput struct {
	FiveHour *StewardQuotaWindowInput `json:"five_hour"`
	Weekly   *StewardQuotaWindowInput `json:"weekly"`
}

// StewardQuotaWindowInput is one normalized remaining-capacity window. Pi
// timestamps are Unix milliseconds, unlike Claude's second-based rate limits.
type StewardQuotaWindowInput struct {
	RemainingPercent *float64 `json:"remaining_percent"`
	ResetAt          *int64   `json:"reset_at"`
}

// EffortInput is stdin's "effort" object. Level is one of
// low|medium|high|xhigh|max.
type EffortInput struct {
	Level string `json:"level"`
}

// PRInput is stdin's "pr" object. ReviewState may be empty even when
// the PR is present (values: approved|pending|changes_requested|draft).
type PRInput struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	ReviewState string `json:"review_state"`
}

// CachedData represents cached statusline data.
type CachedData struct {
	ModelID       string
	ModelProvider string
	ModelDisplay  string
	Harness       string
	// AgentsDisplay is the running-agents summary ("2×O5 · sol ×XH")
	// from ReadAgentsDisplay; "" when no agents are running. Non-empty
	// swaps the model chip's text and icon (see Render).
	AgentsDisplay  string
	CurrentDir     string
	TranscriptPath string
	GitBranch      string
	GitDirtyCount  int
	GitAhead       int
	GitBehind      int
	K8sContext     string
	GcloudProject  string
	UsedPercentage float64
	Hostname       string
	Devspace       string
	TermWidth      int
	RateLimits     *RateLimitsInput
	StewardQuota   *StewardQuotaInput
	PiQuota        *piQuotaState
	Cost           CostInput
	Effort         *EffortInput
	PR             *PRInput

	// SessionCostUSD and DailyCostUSD are transcript-derived costs
	// (internal/cost, via a TTL cache) for the current session and for
	// today respectively. Only meaningful when CostFromTranscript is
	// true; otherwise the cost chip falls back to Cost.TotalCostUSD.
	SessionCostUSD float64
	DailyCostUSD   float64
	// CostFromTranscript is true when SessionCostUSD/DailyCostUSD were
	// successfully computed from the transcript (cache hit or a fresh
	// compute that succeeded). False means transcript-derived cost is
	// unavailable — TranscriptPath was empty, or the compute/cache path
	// failed — and the cost chip renders CostInput.TotalCostUSD
	// instead.
	CostFromTranscript bool

	// Patchbay is today's gateway-accounted usage summary when Patchbay is
	// configured. Unconfigured and unavailable states retain the legacy
	// transcript path; an API error renders a visible error chip instead.
	Patchbay patchbayResult
}

// Dependencies contains all external dependencies.
type Dependencies struct {
	FileReader    FileReader
	CommandRunner CommandRunner
	EnvReader     EnvReader
	TerminalWidth TerminalWidth
	Resolver      *aliases.Resolver
	CacheDir      string
	CacheDuration time.Duration
	// PatchbayClient performs the optional local Patchbay usage request.
	// nil uses a 500ms-bounded default client.
	PatchbayClient *http.Client

	// IconIndex selects which rune index into ModelIcons is displayed
	// by selectModelIcon, given the icon count. nil (the production
	// default) preserves the existing math/rand/v2-based behavior;
	// golden/scenario tests inject a fixed function (e.g. always 0)
	// so rendered output is byte-for-byte reproducible.
	IconIndex func(n int) int

	// Now returns the current time, used by the rate-limit chip's pace
	// arrow and countdown math (comparing a window's resets_at against
	// "now"). nil (the production default) uses time.Now() at the
	// point of use; scenarios/golden tests inject a fixed function so
	// rendered output never depends on wall-clock time.
	Now func() time.Time

	// CostSource returns transcript-derived (session, daily) USD for
	// the given transcript path. nil (the production default) computes
	// from the real filesystem through the TTL cache; scenarios/golden
	// tests inject a fixed function so rendered output never touches
	// disk.
	CostSource func(transcriptPath string) (sessionUSD, dailyUSD float64, ok bool)
}

// FileReader interface for reading files.
type FileReader interface {
	ReadFile(path string) ([]byte, error)
	Exists(path string) bool
}

// CommandRunner interface for executing commands.
type CommandRunner interface {
	Run(command string, args ...string) ([]byte, error)

	// RunContext behaves like Run but honors ctx's deadline/cancellation,
	// letting a caller bound a subprocess's runtime explicitly (e.g. the
	// git-status chip's ≤500ms budget) instead of relying on Run's own
	// fixed internal timeout.
	RunContext(ctx context.Context, command string, args ...string) ([]byte, error)
}

// EnvReader interface for reading environment variables.
type EnvReader interface {
	Get(key string) string
}

// TerminalWidth interface for getting terminal width.
type TerminalWidth interface {
	GetWidth() int
}

// Config contains configuration for the statusline.
type Config struct {
	// LeftSpacerWidth is the width of the left spacer (default: 2)
	LeftSpacerWidth  int
	RightSpacerWidth int
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	const (
		defaultLeftSpacerWidth  = 2
		defaultRightSpacerWidth = 2
	)
	return &Config{
		LeftSpacerWidth:  defaultLeftSpacerWidth,
		RightSpacerWidth: defaultRightSpacerWidth,
	}
}

// Statusline is the main statusline generator.
type Statusline struct {
	deps   *Dependencies
	colors CatppuccinMocha
	input  *Input
	config *Config
}

// CreateStatusline creates a new Statusline instance.
func CreateStatusline(deps *Dependencies) *Statusline {
	return NewWithConfig(deps, DefaultConfig())
}

// NewWithConfig creates a new Statusline instance with custom configuration.
func NewWithConfig(deps *Dependencies, config *Config) *Statusline {
	if config == nil {
		config = DefaultConfig()
	}
	if deps != nil && deps.Resolver == nil {
		// Zero-value resolver: behaves identically to a missing alias file —
		// raw labels, default env patterns. Keeps test ergonomics simple.
		deps.Resolver, _ = aliases.NewResolver("")
	}
	return &Statusline{
		deps:   deps,
		config: config,
		colors: CatppuccinMocha{},
	}
}

// Generate generates the statusline from JSON input.
func (s *Statusline) Generate(reader io.Reader) (string, error) {
	// Read and parse JSON input
	if err := s.parseInput(reader); err != nil {
		return "", fmt.Errorf("parsing input: %w", err)
	}

	// Get current directory
	currentDir := s.getCurrentDir()

	// Always compute data fresh (no caching)
	data := s.computeData(currentDir)

	// Build and return the statusline with guaranteed fixed width
	return s.Render(data), nil
}

func (s *Statusline) parseInput(reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	s.input = &Input{}
	if err := decoder.Decode(s.input); err != nil {
		return fmt.Errorf("decoding JSON: %w", err)
	}
	return nil
}

func (s *Statusline) getCurrentDir() string {
	if s.input.Workspace.ProjectDir != "" {
		return s.input.Workspace.ProjectDir
	}
	if s.input.Workspace.CurrentDir != "" {
		return s.input.Workspace.CurrentDir
	}
	if s.input.Workspace.CWD != "" {
		return s.input.Workspace.CWD
	}
	if s.input.CWD != "" {
		return s.input.CWD
	}
	return "~"
}

func (s *Statusline) computeData(currentDir string) *CachedData {
	termWidth := s.input.Columns
	if termWidth <= 0 {
		termWidth = s.deps.TerminalWidth.GetWidth()
	}

	data := &CachedData{
		CurrentDir:     currentDir,
		TranscriptPath: s.input.TranscriptPath,
		TermWidth:      termWidth,
		ModelID:        s.input.Model.ID,
		ModelProvider:  s.input.Model.Provider,
		Harness:        s.input.Harness,
		StewardQuota:   s.input.StewardQuota,
	}

	// Model display name (abbreviated to first-letter + version, e.g.
	// "Sonnet 4.6 (1M Context)" → "S4.6").
	data.ModelDisplay = abbreviateModel(s.input.Model.DisplayName)

	// Running-agents summary from the subagent hook's state file.
	// Non-empty means agents are running right now and the model chip
	// should show their models instead of the session's (Claude never
	// tells this hook about agents directly — see agents.go).
	data.AgentsDisplay = ReadAgentsDisplay(
		s.deps.FileReader, s.deps.CacheDir, s.input.SessionID, data.ModelDisplay, s.now())

	// Git information. Branch comes from the zero-subprocess .git/HEAD
	// file-read path (getGitInfo/readGitInfo); dirty/ahead/behind state
	// comes from one cached, timeout-guarded `git status` subprocess
	// (gitStatus), only attempted when a branch was actually found. A
	// failed/timed-out gitStatus call (OK: false) leaves these fields at
	// their zero value, so the chip degrades to branch-only.
	gitInfo := s.getGitInfo(currentDir)
	data.GitBranch = gitInfo.Branch
	if gitInfo.Branch != "" {
		gs := gitStatus(s.deps.CommandRunner, s.deps.CacheDir, s.deps.CacheDuration, currentDir, s.now())
		if gs.OK {
			data.GitDirtyCount = gs.DirtyCount
			data.GitAhead = gs.Ahead
			data.GitBehind = gs.Behind
		}
	}

	// Kubernetes context
	data.K8sContext = s.getK8sContext()

	// Gcloud project
	data.GcloudProject = s.getGcloudProject()

	// Context window percentage (directly from input)
	data.UsedPercentage = s.input.ContextWindow.UsedPercentage

	// Hostname
	data.Hostname = s.getHostname()

	// Devspace
	data.Devspace = s.getDevspace()

	// Rate limits, cost, effort, and PR pass straight through from the
	// parsed input; the pointer fields stay nil when absent from stdin.
	data.RateLimits = s.input.RateLimits
	data.Cost = s.input.Cost
	data.Effort = s.input.Effort
	data.PR = s.input.PR

	// A configured Patchbay is the accounting source of truth for non-Pi
	// harnesses. Pi supplies its quota in the render input, so rendering it
	// must not perform a gateway lookup. Only an unreachable gateway falls
	// through to the legacy transcript path; an authentication, server, or
	// response-shape failure stays visible as an error indicator and never
	// mixes accounting bases.
	if s.input.Harness != requiredPiHarness {
		data.Patchbay = patchbayCost(
			s.deps.CacheDir, s.deps.CacheDuration, s.now(), s.deps.EnvReader, s.deps.PatchbayClient,
		)
		if data.Patchbay.Status == patchbayAvailable || data.Patchbay.Status == patchbayError {
			return data
		}
	}

	// Transcript-derived session+daily cost. Only attempted when stdin
	// reported a transcript path; a failure (or no path) leaves
	// CostFromTranscript false so the cost chip falls back to
	// data.Cost.TotalCostUSD.
	if s.input.TranscriptPath != "" {
		sessionUSD, dailyUSD, ok := s.costSource()(s.input.TranscriptPath)
		if ok {
			data.SessionCostUSD = sessionUSD
			data.DailyCostUSD = dailyUSD
			data.CostFromTranscript = true
		}
	}

	return data
}

// costSource returns deps.CostSource when injected (scenarios/tests),
// else the production transcriptCosts path through the TTL cache.
func (s *Statusline) costSource() func(transcriptPath string) (float64, float64, bool) {
	if s.deps != nil && s.deps.CostSource != nil {
		return s.deps.CostSource
	}
	// RateLimits is stdin's ground truth for a flat-subscription session
	// (see Input.RateLimits' doc comment): present means anthropic-backend
	// transcript rows price at $0, absent means real dollars.
	subscribed := s.input.RateLimits != nil
	return func(transcriptPath string) (float64, float64, bool) {
		state, ok := transcriptCosts(s.deps.CacheDir, s.deps.CacheDuration, transcriptPath, s.now(), subscribed)
		return state.SessionUSD, state.DailyUSD, ok
	}
}

func (s *Statusline) getGitInfo(dir string) GitInfo {
	// Walk up the directory tree to find .git
	current := dir
	for current != "/" && current != "." {
		gitPath := filepath.Join(current, ".git")
		if s.deps.FileReader.Exists(gitPath) {
			// Check if it's a directory or file (worktree)
			if content, err := s.deps.FileReader.ReadFile(gitPath); err == nil {
				// It's a file (worktree) - extract actual git dir
				contentStr := string(content)
				if strings.HasPrefix(contentStr, "gitdir:") {
					gitDir := strings.TrimSpace(strings.TrimPrefix(contentStr, "gitdir:"))
					return s.readGitInfo(gitDir)
				}
			}
			// Assume it's a directory
			return s.readGitInfo(gitPath)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return GitInfo{}
}

func (s *Statusline) readGitInfo(gitDir string) GitInfo {
	info := GitInfo{}

	// Read HEAD file for branch. This is the zero-subprocess fallback
	// path: it's always consulted for the branch name (including the
	// detached-HEAD short-SHA case), independent of whether the
	// gitStatus subprocess in git.go succeeds, times out, or is skipped
	// entirely.
	headPath := filepath.Join(gitDir, "HEAD")
	if content, err := s.deps.FileReader.ReadFile(headPath); err == nil {
		head := strings.TrimSpace(string(content))
		if strings.HasPrefix(head, "ref: refs/heads/") {
			info.Branch = strings.TrimPrefix(head, "ref: refs/heads/")
		} else if len(head) >= 7 {
			// Detached HEAD - show short hash
			info.Branch = head[:7]
		}
	}

	return info
}

func (s *Statusline) getK8sContext() string {
	// Check for test override
	if override := s.deps.EnvReader.Get("STEWARD_STATUSLINE_KUBECONFIG"); override != "" {
		if override == devNullOverride {
			return ""
		}
	}

	kubeconfig := s.deps.EnvReader.Get("KUBECONFIG")
	if kubeconfig == "" {
		home := s.deps.EnvReader.Get("HOME")
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	if !s.deps.FileReader.Exists(kubeconfig) {
		return ""
	}

	content, err := s.deps.FileReader.ReadFile(kubeconfig)
	if err != nil {
		return ""
	}

	// Scan line-by-line with early exit. The file is read whole via the
	// FileReader contract, but we avoid allocating a slice of every line
	// via strings.Split — meaningful on hot-path renders with long
	// kubeconfigs.
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "current-context:") {
			context := strings.TrimSpace(strings.TrimPrefix(line, "current-context:"))
			return strings.Trim(context, "\"")
		}
	}

	return ""
}

const unknownLabel = "unknown"

func (s *Statusline) getHostname() string {
	// Check for test override
	if override := s.deps.EnvReader.Get("STEWARD_STATUSLINE_HOSTNAME"); override != "" {
		return override
	}

	if hostname := s.deps.EnvReader.Get("HOSTNAME"); hostname != "" {
		return hostname
	}

	// Try to get hostname from command
	output, err := s.deps.CommandRunner.Run("hostname", "-s")
	if err == nil && len(output) > 0 {
		if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
			return trimmed
		}
	}

	output, err = s.deps.CommandRunner.Run("hostname")
	if err == nil && len(output) > 0 {
		if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
			return trimmed
		}
	}

	return unknownLabel
}

const devspacePlanetMercury = "mercury"

// devNullOverride is the sentinel override value (STEWARD_STATUSLINE_KUBECONFIG,
// STEWARD_STATUSLINE_GCLOUD) that suppresses a chip entirely — useful for
// tests and for users who don't want that chip shown.
const devNullOverride = "/dev/null"

func (s *Statusline) getDevspace() string {
	// Check for test override
	var tmuxDevspace string
	if override := s.deps.EnvReader.Get("STEWARD_STATUSLINE_DEVSPACE"); override != "" {
		tmuxDevspace = override
	} else {
		tmuxDevspace = s.deps.EnvReader.Get("TMUX_DEVSPACE")
	}

	if tmuxDevspace == "" || tmuxDevspace == "-TMUX_DEVSPACE" {
		return ""
	}

	// Known planets render with their astronomical glyph and a 3-char
	// label — the glyph carries identity, the text just confirms it.
	// Arbitrary devspace names (e.g. branch names) use the generic ●
	// glyph and keep their full label since the name itself is the only
	// identifier.
	var symbol string
	knownPlanet := true
	switch tmuxDevspace {
	case devspacePlanetMercury:
		symbol = "☿"
	case "venus":
		symbol = "♀"
	case "earth":
		symbol = "♁"
	case "mars":
		symbol = "♂"
	case "jupiter":
		symbol = "♃"
	default:
		symbol = "●"
		knownPlanet = false
	}

	label := tmuxDevspace
	if knownPlanet {
		const devspaceLabelMax = 3
		if len(label) > devspaceLabelMax {
			label = label[:devspaceLabelMax]
		}
	}
	return symbol + " " + label
}

func (s *Statusline) getColorBG(color string) string {
	switch color {
	case colorMauve:
		return s.colors.MauveBG()
	case colorRosewater:
		return s.colors.RosewaterBG()
	case colorSky:
		return s.colors.SkyBG()
	case colorPeach:
		return s.colors.PeachBG()
	case colorTeal:
		return s.colors.TealBG()
	case colorRed:
		return s.colors.RedBG()
	case colorMaroon:
		return s.colors.MaroonBG()
	case colorYellow:
		return s.colors.YellowBG()
	case colorGreen:
		return s.colors.GreenBG()
	case colorLavender:
		return s.colors.LavenderBG()
	case colorPink:
		return s.colors.PinkBG()
	case colorSapphire:
		return s.colors.SapphireBG()
	default:
		return ""
	}
}

func (s *Statusline) getColorFG(color string) string {
	switch color {
	case colorMauve:
		return s.colors.MauveFG()
	case colorRosewater:
		return s.colors.RosewaterFG()
	case colorSky:
		return s.colors.SkyFG()
	case colorPeach:
		return s.colors.PeachFG()
	case colorTeal:
		return s.colors.TealFG()
	case colorRed:
		return s.colors.RedFG()
	case colorMaroon:
		return s.colors.MaroonFG()
	case colorYellow:
		return s.colors.YellowFG()
	case colorGreen:
		return s.colors.GreenFG()
	case colorLavender:
		return s.colors.LavenderFG()
	case colorPink:
		return s.colors.PinkFG()
	case colorSapphire:
		return s.colors.SapphireFG()
	case colorOverlay:
		return s.colors.OverlayFG()
	default:
		return ""
	}
}

// GitInfo contains git repository information from the zero-subprocess
// .git/HEAD file-read path. Dirty/ahead/behind state comes separately
// from gitStatus (git.go), which runs a real `git status` subprocess.
type GitInfo struct {
	Branch string
}

// Component represents a statusline component.
type Component struct {
	Color string
	Text  string
}
