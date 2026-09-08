package statusline

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
)

const (
	piQuotaProvider = "openai-codex"
	piQuotaBaseURL  = "https://chatgpt.com/backend-api"
)

func quotaValue[T any](v T) *T {
	return &v
}

func validStewardQuota(now time.Time) *StewardQuotaInput {
	return &StewardQuotaInput{
		Provider:  piQuotaProvider,
		BaseURL:   piQuotaBaseURL,
		FetchedAt: quotaValue(now.UnixMilli()),
		Stale:     quotaValue(false),
		Windows: StewardQuotaWindowsInput{
			FiveHour: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(75.0),
				ResetAt:          quotaValue(now.Add(2 * time.Hour).UnixMilli()),
			},
			Weekly: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(20.0),
				ResetAt:          quotaValue(now.Add(3 * 24 * time.Hour).UnixMilli()),
			},
		},
	}
}

func piQuotaDeps(width int, now time.Time) *Dependencies {
	return &Dependencies{
		FileReader:    newFixedFileReader(),
		CommandRunner: newFixedCommandRunner(),
		EnvReader:     newFixedEnvReader(map[string]string{scenarioHomeEnvKey: scenarioHome}),
		TerminalWidth: fixedTerminalWidth(width),
		IconIndex:     func(int) int { return 0 },
		Now:           func() time.Time { return now },
	}
}

func generatePiQuota(t *testing.T, input Input, deps *Dependencies) string {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	got, err := CreateStatusline(deps).Generate(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return got
}

func basePiQuotaInput(now time.Time) Input {
	input := Input{
		Harness:      "pi",
		StewardQuota: validStewardQuota(now),
		Columns:      200,
	}
	input.Model.Provider = piQuotaProvider
	input.Model.DisplayName = scenarioModelDisplay
	input.Workspace.ProjectDir = scenarioProjectDir
	input.ContextWindow.UsedPercentage = 42
	return input
}

func TestPiStatuslineRenderingDoesNotCallPatchbayTransport(t *testing.T) {
	for _, harness := range []string{"pi", "claude-code"} {
		t.Run(harness, func(t *testing.T) {
			now := scenarioFixedNow()
			deps := piQuotaDeps(200, now)
			deps.CacheDir = t.TempDir() // New empty cache: no usable Patchbay snapshot.
			deps.CacheDuration = 20 * time.Second
			deps.EnvReader = patchbayEnv("http://127.0.0.1:1", writePatchbayKey(t))
			calls := 0
			deps.PatchbayClient = &http.Client{
				Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.Header.Get(patchbayCallerKeyHeader) != "caller-key" {
						t.Error("synthetic caller key missing")
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body: io.NopCloser(strings.NewReader(
							patchbaySummaryJSON(1000000000, 0),
						)),
						Request: req,
					}, nil
				}),
			}
			input := basePiQuotaInput(now)
			input.Harness = harness
			rendered := stripAnsi(generatePiQuota(t, input, deps))
			if harness == "pi" {
				for _, want := range []string{"5h", "75%", "7d", "20%"} {
					if !strings.Contains(rendered, want) {
						t.Errorf("Pi quota missing %q from %q", want, rendered)
					}
				}
				if calls != 0 {
					t.Errorf("Pi footer transport calls = %d, want 0", calls)
				}
			} else if calls != 1 {
				t.Errorf("Claude accounting control transport calls = %d, want 1", calls)
			}
			t.Logf("harness=%s transport_calls=%d", harness, calls)
		})
	}
}

func TestPiQuotaGenerateRequiresExactScope(t *testing.T) {
	now := scenarioFixedNow()
	deps := piQuotaDeps(200, now)

	baselineInput := basePiQuotaInput(now)
	baselineInput.Harness = ""
	baselineInput.StewardQuota = nil
	baseline := generatePiQuota(t, baselineInput, deps)

	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{name: "absent namespace", mutate: func(in *Input) { in.StewardQuota = nil }},
		{name: "other harness", mutate: func(in *Input) { in.Harness = "claude-code" }},
		{name: "other model provider", mutate: func(in *Input) { in.Model.Provider = "anthropic" }},
		{name: "other quota provider", mutate: func(in *Input) { in.StewardQuota.Provider = "anthropic" }},
		{name: "other base URL", mutate: func(in *Input) {
			in.StewardQuota.BaseURL = piQuotaBaseURL + "/"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := basePiQuotaInput(now)
			tt.mutate(&input)
			got := generatePiQuota(t, input, deps)
			if got != baseline {
				t.Errorf("out-of-scope quota changed legacy render\ngot:  %q\nwant: %q", got, baseline)
			}
		})
	}

	scoped := stripAnsi(generatePiQuota(t, basePiQuotaInput(now), deps))
	for _, want := range []string{"5h", "75%", "2h", "7d", "20%", "3d", piQuotaFreshLabel} {
		if !strings.Contains(scoped, want) {
			t.Errorf("scoped Pi quota output missing %q: %q", want, scoped)
		}
	}
}

func TestPiQuotaDoesNotChangeNativeClaudeRateLimits(t *testing.T) {
	now := scenarioFixedNow()
	deps := piQuotaDeps(200, now)
	input := basePiQuotaInput(now)
	input.Harness = ""
	input.Model.Provider = "anthropic"
	input.StewardQuota = nil
	input.RateLimits = &RateLimitsInput{
		FiveHour: &RateLimitWindow{UsedPercentage: 23, ResetsAt: now.Add(time.Hour).Unix()},
		SevenDay: &RateLimitWindow{UsedPercentage: 41, ResetsAt: now.Add(48 * time.Hour).Unix()},
	}
	want := generatePiQuota(t, input, deps)

	input.Harness = "pi"
	input.StewardQuota = validStewardQuota(now)
	got := generatePiQuota(t, input, deps)
	if got != want {
		t.Errorf("non-Codex Pi namespace changed native Claude rate-limit render\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPiQuotaGenerateNormalizesAfterCostLookupAtRenderEntry(t *testing.T) {
	fetchedAt := scenarioFixedNow()
	const syntheticTranscriptPath = "/synthetic/pi-quota/session.jsonl"

	for _, width := range []int{40, 60, 80, 200} {
		t.Run(quotaWidthName(width), func(t *testing.T) {
			now := fetchedAt.Add(15*time.Minute - time.Millisecond)
			deps := piQuotaDeps(width, now)
			deps.Now = func() time.Time { return now }
			deps.CostSource = func(transcriptPath string) (float64, float64, bool) {
				if transcriptPath != syntheticTranscriptPath {
					t.Errorf("CostSource path = %q, want %q", transcriptPath, syntheticTranscriptPath)
				}
				now = fetchedAt.Add(15 * time.Minute)
				return 1.23, 4.56, true
			}

			input := basePiQuotaInput(fetchedAt)
			input.Columns = width
			input.TranscriptPath = syntheticTranscriptPath
			plain := stripAnsi(generatePiQuota(t, input, deps))
			assertPiQuotaUnknownWithoutMetrics(t, plain)
		})
	}
}

func TestPiQuotaDirectRenderRenormalizesAcrossFreshnessBoundaries(t *testing.T) {
	fetchedAt := scenarioFixedNow()
	now := fetchedAt.Add(5*time.Minute - time.Millisecond)
	deps := piQuotaDeps(200, now)
	deps.Now = func() time.Time { return now }
	s := CreateStatusline(deps)

	quota := validStewardQuota(fetchedAt)
	initialState := normalizePiQuota(requiredPiHarness, requiredPiProvider, quota, now)
	data := &CachedData{
		Harness:        requiredPiHarness,
		ModelProvider:  requiredPiProvider,
		ModelDisplay:   scenarioModelDisplay,
		CurrentDir:     scenarioProjectDir,
		TermWidth:      200,
		StewardQuota:   quota,
		PiQuota:        initialState,
		UsedPercentage: piQuotaScenarioContext,
	}

	fresh := stripAnsi(s.Render(data))
	if !strings.Contains(fresh, piQuotaFreshLabel) || !strings.Contains(fresh, "75%") {
		t.Fatalf("render before five minutes did not show fresh metrics: %q", fresh)
	}

	now = fetchedAt.Add(5 * time.Minute)
	stale := stripAnsi(s.Render(data))
	if !strings.Contains(stale, piQuotaStaleLabel) || strings.Contains(stale, piQuotaFreshLabel) {
		t.Errorf("render at five minutes did not revalidate as stale: %q", stale)
	}
	if !strings.Contains(stale, "75%") || !strings.Contains(stale, "20%") {
		t.Errorf("stale render lost still-retained metrics: %q", stale)
	}

	now = fetchedAt.Add(15 * time.Minute)
	unknown := stripAnsi(s.Render(data))
	assertPiQuotaUnknownWithoutMetrics(t, unknown)

	if data.PiQuota != initialState || data.PiQuota.freshness != piQuotaFresh {
		t.Errorf("Render mutated caller-owned cached state: got %+v, want original %+v", data.PiQuota, initialState)
	}
}

func assertPiQuotaUnknownWithoutMetrics(t *testing.T, plain string) {
	t.Helper()
	for _, want := range []string{"5h?", "7d?", unknownLabel} {
		if !strings.Contains(plain, want) {
			t.Errorf("unknown quota render missing %q: %q", want, plain)
		}
	}
	for _, old := range []string{"75%", "20%", "resets", "@"} {
		if strings.Contains(plain, old) {
			t.Errorf("unknown quota render retained old metric/reset %q: %q", old, plain)
		}
	}
}

func TestPiQuotaFreshnessBoundaries(t *testing.T) {
	now := scenarioFixedNow()
	tests := []struct {
		name      string
		fetchedAt *int64
		stale     *bool
		want      piQuotaFreshness
	}{
		{
			name: "missing fetched_at", fetchedAt: nil,
			stale: quotaValue(false), want: piQuotaUnknown,
		},
		{
			name: "zero fetched_at", fetchedAt: quotaValue(int64(0)),
			stale: quotaValue(false), want: piQuotaUnknown,
		},
		{
			name: "negative fetched_at", fetchedAt: quotaValue(int64(-1)),
			stale: quotaValue(false), want: piQuotaUnknown,
		},
		{
			name: "missing stale flag", fetchedAt: quotaValue(now.UnixMilli()),
			stale: nil, want: piQuotaUnknown,
		},
		{
			name:      "future fetched_at",
			fetchedAt: quotaValue(now.Add(time.Millisecond).UnixMilli()),
			stale:     quotaValue(false), want: piQuotaUnknown,
		},
		{
			name:      "fresh before five minutes",
			fetchedAt: quotaValue(now.Add(-5*time.Minute + time.Millisecond).UnixMilli()),
			stale:     quotaValue(false), want: piQuotaFresh,
		},
		{
			name:      "exactly five minutes stale",
			fetchedAt: quotaValue(now.Add(-5 * time.Minute).UnixMilli()),
			stale:     quotaValue(false), want: piQuotaStale,
		},
		{
			name: "early stale flag", fetchedAt: quotaValue(now.UnixMilli()),
			stale: quotaValue(true), want: piQuotaStale,
		},
		{
			name:      "stale before fifteen minutes",
			fetchedAt: quotaValue(now.Add(-15*time.Minute + time.Millisecond).UnixMilli()),
			stale:     quotaValue(false), want: piQuotaStale,
		},
		{
			name:      "exactly fifteen minutes unknown",
			fetchedAt: quotaValue(now.Add(-15 * time.Minute).UnixMilli()),
			stale:     quotaValue(true), want: piQuotaUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quota := validStewardQuota(now)
			quota.FetchedAt = tt.fetchedAt
			quota.Stale = tt.stale
			state := normalizePiQuota("pi", piQuotaProvider, quota, now)
			if state == nil {
				t.Fatal("exact Pi/Codex scope must produce an explicit quota state")
			}
			if state.freshness != tt.want {
				t.Errorf("freshness = %v, want %v", state.freshness, tt.want)
			}
			if tt.want == piQuotaUnknown && (state.fiveHour.known || state.weekly.known) {
				t.Errorf("unknown freshness retained stale metrics: %+v", state)
			}
		})
	}
}

func TestPiQuotaWindowValidationAndPercentages(t *testing.T) {
	now := scenarioFixedNow()
	tests := []struct {
		name      string
		window    *StewardQuotaWindowInput
		wantKnown bool
		wantToken string
	}{
		{name: "missing window", window: nil, wantToken: "5h?"},
		{
			name:      "missing percentage",
			window:    &StewardQuotaWindowInput{ResetAt: quotaValue(now.UnixMilli())},
			wantToken: "5h?",
		},
		{
			name:      "missing reset",
			window:    &StewardQuotaWindowInput{RemainingPercent: quotaValue(50.0)},
			wantToken: "5h?",
		},
		{
			name: "negative reset",
			window: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(50.0), ResetAt: quotaValue(int64(-1)),
			},
			wantToken: "5h?",
		},
		{
			name: "negative percentage",
			window: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(-0.1), ResetAt: quotaValue(now.UnixMilli()),
			},
			wantToken: "5h?",
		},
		{
			name: "over one hundred",
			window: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(100.1), ResetAt: quotaValue(now.UnixMilli()),
			},
			wantToken: "5h?",
		},
		{
			name: "NaN",
			window: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(math.NaN()), ResetAt: quotaValue(now.UnixMilli()),
			},
			wantToken: "5h?",
		},
		{
			name: "positive infinity",
			window: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(math.Inf(1)), ResetAt: quotaValue(now.UnixMilli()),
			},
			wantToken: "5h?",
		},
		{
			name: "zero remaining",
			window: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(0.0), ResetAt: quotaValue(now.UnixMilli()),
			},
			wantKnown: true, wantToken: "5h0%@now",
		},
		{
			name: "one hundred remaining",
			window: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(100.0),
				ResetAt:          quotaValue(now.Add(time.Hour).UnixMilli()),
			},
			wantKnown: true, wantToken: "5h100%@1h",
		},
		{
			name: "fractional remaining",
			window: &StewardQuotaWindowInput{
				RemainingPercent: quotaValue(74.6),
				ResetAt:          quotaValue(now.Add(90 * time.Minute).UnixMilli()),
			},
			wantKnown: true, wantToken: "5h75%@1h30m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quota := validStewardQuota(now)
			quota.Windows.FiveHour = tt.window
			state := normalizePiQuota("pi", piQuotaProvider, quota, now)
			if state.fiveHour.known != tt.wantKnown {
				t.Fatalf("five-hour known = %v, want %v", state.fiveHour.known, tt.wantKnown)
			}
			body := buildPiQuotaCompactBody(state, now)
			if !strings.Contains(body, tt.wantToken) {
				t.Errorf("compact body missing %q: %q", tt.wantToken, body)
			}
		})
	}
}

func TestPiQuotaUnknownFreshnessCannotReappearInAnyBody(t *testing.T) {
	now := scenarioFixedNow()
	quota := validStewardQuota(now)
	quota.FetchedAt = quotaValue(now.Add(-15 * time.Minute).UnixMilli())
	state := normalizePiQuota("pi", piQuotaProvider, quota, now)

	for name, body := range map[string]string{
		"full":    buildPiQuotaFullBody(state, now),
		"compact": buildPiQuotaCompactBody(state, now),
		"summary": buildPiQuotaSummaryBody(state),
	} {
		if strings.Contains(body, "75%") || strings.Contains(body, "20%") {
			t.Errorf("%s body resurrected expired metrics: %q", name, body)
		}
		if !strings.Contains(body, unknownLabel) {
			t.Errorf("%s body does not explicitly report unknown freshness: %q", name, body)
		}
	}
}

func TestPiQuotaResetTokensAreBoundedAndUseMilliseconds(t *testing.T) {
	now := scenarioFixedNow()
	const day = 24 * time.Hour
	tests := []struct {
		name    string
		resetAt int64
		want    string
	}{
		{name: "past", resetAt: now.Add(-time.Hour).UnixMilli(), want: "now"},
		{name: "exactly now", resetAt: now.UnixMilli(), want: "now"},
		{name: "under one minute", resetAt: now.Add(30 * time.Second).UnixMilli(), want: "<1m"},
		{name: "minutes", resetAt: now.Add(59 * time.Minute).UnixMilli(), want: "59m"},
		{name: "hours and minutes", resetAt: now.Add(time.Hour + 2*time.Minute).UnixMilli(), want: "1h2m"},
		{name: "days and hours", resetAt: now.Add(3*day + 4*time.Hour).UnixMilli(), want: "3d4h"},
		{name: "over 999 days", resetAt: now.Add(1000 * day).UnixMilli(), want: ">999d"},
		{name: "maximum millisecond timestamp", resetAt: math.MaxInt64, want: ">999d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatPiQuotaReset(tt.resetAt, now); got != tt.want {
				t.Errorf("formatPiQuotaReset() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPiQuotaGeneratePreservesMaxMetricsAcrossWidths(t *testing.T) {
	now := scenarioFixedNow()
	const day = 24 * time.Hour
	resetCases := []struct {
		name        string
		resetIn     time.Duration
		wantCompact string
		wantFull    string
	}{
		{name: "hours and minutes", resetIn: 23*time.Hour + 59*time.Minute, wantCompact: "23h", wantFull: "23h59m"},
		{name: "days and hours", resetIn: 999*day + 23*time.Hour, wantCompact: "999d", wantFull: "999d23h"},
		{name: "over maximum days", resetIn: 1000 * day, wantCompact: ">999d", wantFull: ">999d"},
	}
	freshnessCases := []struct {
		name  string
		stale bool
	}{
		{name: piQuotaFreshLabel, stale: false},
		{name: piQuotaStaleLabel, stale: true},
	}

	for _, resetCase := range resetCases {
		for _, freshnessCase := range freshnessCases {
			for _, width := range []int{40, 60, 80, 200} {
				name := resetCase.name + "/" + freshnessCase.name + "/" + quotaWidthName(width)
				t.Run(name, func(t *testing.T) {
					input := basePiQuotaInput(now)
					input.Columns = width
					input.StewardQuota.Stale = quotaValue(freshnessCase.stale)
					resetAt := now.Add(resetCase.resetIn).UnixMilli()
					for _, window := range []*StewardQuotaWindowInput{
						input.StewardQuota.Windows.FiveHour,
						input.StewardQuota.Windows.Weekly,
					} {
						window.RemainingPercent = quotaValue(100.0)
						window.ResetAt = quotaValue(resetAt)
					}

					plain := stripAnsi(generatePiQuota(t, input, piQuotaDeps(width, now)))
					if width == 40 {
						t.Logf("stripped width-40 render: %q", plain)
					}
					if got := runewidth.StringWidth(plain); got != width-4 {
						t.Errorf("rendered width = %d, want %d; output=%q", got, width-4, plain)
					}

					windowTokens := []string{
						"5h100%@" + resetCase.wantCompact + " ",
						"7d100%@" + resetCase.wantCompact + " " + freshnessCase.name,
					}
					if width > narrowWidthThreshold {
						windowTokens = []string{
							"5h 100% resets " + resetCase.wantFull,
							"7d 100% resets " + resetCase.wantFull,
						}
					}
					for _, want := range windowTokens {
						if !strings.Contains(plain, want) {
							t.Errorf("render missing complete window %q: %q", want, plain)
						}
					}
					if !strings.Contains(plain, freshnessCase.name) {
						t.Errorf("render missing freshness %q: %q", freshnessCase.name, plain)
					}
					if width == 40 && resetCase.wantCompact == ">999d" && strings.Contains(plain, RateLimitIcon) {
						t.Errorf("width-40 semantic compact tier retained decorative icon: %q", plain)
					}
				})
			}
		}
	}
}

func TestPiQuotaWideSqueezeUsesSemanticCompactTier(t *testing.T) {
	now := scenarioFixedNow()
	quota := validStewardQuota(now)
	quota.Stale = quotaValue(true)
	resetAt := now.Add(1000 * 24 * time.Hour).UnixMilli()
	for _, window := range []*StewardQuotaWindowInput{quota.Windows.FiveHour, quota.Windows.Weekly} {
		window.RemainingPercent = quotaValue(100.0)
		window.ResetAt = quotaValue(resetAt)
	}
	state := normalizePiQuota(requiredPiHarness, requiredPiProvider, quota, now)
	s := CreateStatusline(piQuotaDeps(200, now))
	s.colors = CatppuccinMocha{}

	const width = 36
	plain := stripAnsi(s.buildSqueezedPiQuotaChip(state, now, width))
	if got := runewidth.StringWidth(plain); got != width {
		t.Errorf("squeezed quota width = %d, want %d; output=%q", got, width, plain)
	}
	for _, want := range []string{"5h100%@>999d", "7d100%@>999d", piQuotaStaleLabel} {
		if !strings.Contains(plain, want) {
			t.Errorf("squeezed quota missing %q: %q", want, plain)
		}
	}
	if strings.Contains(plain, RateLimitIcon) {
		t.Errorf("squeezed semantic compact tier retained decorative icon: %q", plain)
	}
}

func TestPiQuotaZeroRemainingIsNotClaudeMonetaryAlarm(t *testing.T) {
	now := scenarioFixedNow()
	input := basePiQuotaInput(now)
	input.StewardQuota.Windows.FiveHour.RemainingPercent = quotaValue(0.0)
	input.Cost.TotalCostUSD = 9.99
	plain := stripAnsi(generatePiQuota(t, input, piQuotaDeps(200, now)))
	if !strings.Contains(plain, "5h 0%") {
		t.Errorf("zero remaining quota not rendered: %q", plain)
	}
	if strings.Contains(plain, "EXTRA") || strings.Contains(plain, "$9.99") {
		t.Errorf("Pi remaining quota was treated as Claude monetary alarm: %q", plain)
	}
}

func TestPiQuotaRichRenderPreservesQuotaAndWidth(t *testing.T) {
	now := scenarioFixedNow()
	for _, width := range []int{40, 60, 80, 200} {
		t.Run(quotaWidthName(width), func(t *testing.T) {
			input := basePiQuotaInput(now)
			input.Columns = width
			input.Workspace.ProjectDir = "/workspace/a-very-long/team/repository/with-a-long-leaf-directory"
			input.ContextWindow.UsedPercentage = 97
			input.Cost.TotalCostUSD = 1234.56

			fr := newFixedFileReader()
			gitDir := input.Workspace.ProjectDir + "/.git"
			fr.setExists(gitDir, true)
			fr.setFile(gitDir+"/HEAD", []byte("ref: refs/heads/feature/a-very-long-branch-name-for-quota-layout\n"))
			env := newFixedEnvReader(map[string]string{
				"HOME":        scenarioHome,
				"AWS_PROFILE": "production-account-with-a-very-long-name",
			})
			deps := piQuotaDeps(width, now)
			deps.FileReader = fr
			deps.EnvReader = env

			got := generatePiQuota(t, input, deps)
			plain := stripAnsi(got)
			if measured := runewidth.StringWidth(plain); measured != width-4 {
				t.Errorf("rendered width = %d, want %d; output=%q", measured, width-4, plain)
			}
			for _, want := range []string{"5h", "75%", "2h", "7d", "20%", "3d", piQuotaFreshLabel} {
				if !strings.Contains(plain, want) {
					t.Errorf("protected quota missing %q at width %d: %q", want, width, plain)
				}
			}
			if strings.Contains(plain, "$1234.56") {
				t.Errorf("optional cost outranked scoped quota at width %d: %q", width, plain)
			}
		})
	}
}

func TestPiQuotaScenariosRenderAllStatesAtAllWidths(t *testing.T) {
	wantNames := make(map[string]bool)
	for _, state := range []string{piQuotaFreshLabel, piQuotaStaleLabel, unknownLabel, piQuotaPartialState} {
		for _, width := range []int{40, 60, 80, 200} {
			wantNames["quota_"+state+"_"+quotaWidthName(width)] = false
		}
	}

	for _, scenario := range Scenarios() {
		seen, quotaScenario := wantNames[scenario.Name]
		if !quotaScenario {
			continue
		}
		if seen {
			t.Fatalf("duplicate quota scenario %q", scenario.Name)
		}
		wantNames[scenario.Name] = true
		plain := stripAnsi(renderScenario(t, scenario))
		t.Logf("%s stripped: %q", scenario.Name, plain)
		if width := runewidth.StringWidth(plain); width != scenario.Width-4 {
			t.Errorf("%s width = %d, want %d; output=%q", scenario.Name, width, scenario.Width-4, plain)
		}
		for _, label := range []string{"5h", "7d"} {
			if !strings.Contains(plain, label) {
				t.Errorf("%s missing %s window: %q", scenario.Name, label, plain)
			}
		}
		switch {
		case strings.Contains(scenario.Name, "_fresh_"):
			assertPiQuotaScenarioText(t, scenario.Name, plain, []string{"75%", "2h", "20%", "3d", piQuotaFreshLabel})
		case strings.Contains(scenario.Name, "_stale_"):
			assertPiQuotaScenarioText(t, scenario.Name, plain, []string{"75%", "2h", "20%", "3d", piQuotaStaleLabel})
		case strings.Contains(scenario.Name, "_unknown_"):
			assertPiQuotaScenarioText(t, scenario.Name, plain, []string{"5h?", "7d?", unknownLabel})
			if strings.Contains(plain, "75%") || strings.Contains(plain, "20%") {
				t.Errorf("%s unknown output contains quota values: %q", scenario.Name, plain)
			}
		case strings.Contains(scenario.Name, "_partial_"):
			assertPiQuotaScenarioText(t, scenario.Name, plain, []string{"75%", "2h", "7d?", piQuotaFreshLabel})
		}
	}

	for name, seen := range wantNames {
		if !seen {
			t.Errorf("missing quota scenario %q", name)
		}
	}
}

func quotaWidthName(width int) string {
	switch width {
	case 40:
		return "40"
	case 60:
		return "60"
	case 80:
		return "80"
	case 200:
		return "200"
	default:
		return "invalid"
	}
}

func assertPiQuotaScenarioText(t *testing.T, name, plain string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(plain, want) {
			t.Errorf("%s missing %q: %q", name, want, plain)
		}
	}
}
