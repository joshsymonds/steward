package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joshsymonds/steward/internal/notify"
)

// notifyHookTimeout bounds the hook client, including its inline delivery
// fallback. Daemon-side Pi composition is asynchronous to the client.
const notifyHookTimeout = 80 * time.Second

// decisionLogName is the decision log's filename inside the notify state base.
const decisionLogName = "notify-decisions.jsonl"

// clientDialTimeout bounds how long the hook client waits to reach notifyd
// before giving up and running its own inline fallback: short enough that
// an unreachable or overloaded daemon never meaningfully delays the hook
// (which must return well inside its own settings.json timeout), long
// enough to survive a momentary scheduling delay on a live daemon.
const clientDialTimeout = 250 * time.Millisecond

func runNotifyCommand() {
	if exitCode := runNotifyCommandWithIO(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); exitCode != 0 {
		os.Exit(exitCode)
	}
}

// runNotifyCommandWithIO parses notify's command-line flags and runs the hook
// client against explicit streams, returning the process exit code.
func runNotifyCommandWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("notify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dryRun := flags.Bool("dry-run", false, "print what would be sent instead of sending it")
	harness := flags.String("harness", "", "explicit hook harness (claude-code or pi)")
	stateBase := flags.String("state-base", defaultNotifyStateBase(), "root directory for notify state")
	if err := flags.Parse(args); err != nil {
		return exitUsageError
	}

	log := notify.DecisionLog{Path: filepath.Join(*stateBase, decisionLogName)}
	sender, senderOK := notify.ResolveSenderEnv(os.Environ())
	sender.Host = notify.ShortHostname()

	if !senderOK && !*dryRun {
		_, _ = fmt.Fprintln(stderr, "steward notify: no ntfy URL configured, skipping")
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), notifyHookTimeout)
	defer cancel()
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "steward notify: does not accept positional arguments")
		return 0
	}

	dispatchNotify(ctx, notifyClientConfig{
		DryRun:      *dryRun,
		Harness:     *harness,
		Sender:      sender,
		Log:         log,
		Environ:     os.Environ(),
		SockPath:    notify.SocketPath(),
		DialTimeout: clientDialTimeout,
	}, stdin, stdout, stderr)
	return 0
}

// notifyClientConfig groups the dependencies dispatchNotify needs, resolved
// by runNotifyCommand from flags/env — kept as an explicit struct (rather
// than reading os.Args/os.Environ directly inside dispatchNotify) so tests
// can drive the socket-vs-fallback dispatch logic without touching process
// globals.
type notifyClientConfig struct {
	DryRun      bool
	Harness     string
	Sender      notify.Sender
	Log         notify.DecisionLog
	Environ     []string
	SockPath    string
	DialTimeout time.Duration
}

// dispatchNotify parses and prepares one hook payload exactly once, then sends
// that immutable snapshot to notifyd. Every ambiguous daemon outcome uses the
// same snapshot for one deterministic model-free inline fallback; dry runs
// bypass the socket so their rehearsal reaches this invocation's stdout.
func dispatchNotify(ctx context.Context, cfg notifyClientConfig, stdin io.Reader, stdout, stderr io.Writer) {
	input, err := notify.ParseHookInputForHarness(stdin, cfg.Harness)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "steward notify: %v\n", err)
		return
	}
	prepared, err := notify.PrepareEvent(input)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "steward notify: invalid event")
		return
	}

	workspace := notify.WorkspaceName(cfg.Environ, notify.RunCommand)
	frame := notify.Frame{Event: prepared, Workspace: workspace, DryRun: cfg.DryRun}
	if !cfg.DryRun && sendFrame(ctx, cfg.SockPath, frame, cfg.DialTimeout) {
		return
	}

	pipeline := notify.Pipeline{
		DryRun:    cfg.DryRun,
		Sender:    cfg.Sender,
		Log:       cfg.Log,
		Stdout:    stdout,
		Workspace: workspace,
	}
	if runErr := pipeline.RunPrepared(ctx, prepared); runErr != nil {
		_, _ = fmt.Fprintln(stderr, "steward notify: invalid event")
	}
}

// sendFrame uses one dial/write/read budget, bounded by timeout (250ms by
// default) and any earlier caller deadline. Only a strict accepted or duplicate
// acknowledgement transfers delivery ownership to the daemon; every other
// result is ambiguous and returns false for exactly one inline fallback.
func sendFrame(ctx context.Context, sockPath string, frame notify.Frame, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = clientDialTimeout
	}
	budgetContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(budgetContext, "unix", sockPath)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	stopOnCancellation := context.AfterFunc(budgetContext, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopOnCancellation()
	deadline, ok := budgetContext.Deadline()
	if !ok || conn.SetDeadline(deadline) != nil {
		return false
	}
	if notify.EncodeFrame(conn, frame) != nil {
		return false
	}
	ack, err := notify.DecodeAck(conn)
	if err != nil {
		return false
	}
	return ack.Status == "accepted" || ack.Status == "duplicate"
}

// defaultNotifyStateBase resolves the default notify state directory:
// ${XDG_STATE_HOME:-~/.local/state}/steward/notify.
func defaultNotifyStateBase() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return filepath.Join(os.TempDir(), "steward", "notify")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "steward", "notify")
}

// notifydRequiresSender reports whether notifyd must refuse to start
// because no ntfy delivery is configured and it isn't a dry run. This
// differs from the hook client's identical-looking gate: a single hook
// invocation can safely skip (exit 0, try again next event), but a
// long-running daemon with no delivery config would silently accept every
// frame and fail every send from then on — coverage loss the user has no
// way to notice. notifyd fails fast at startup instead, with a nonzero
// exit a service supervisor can see and report.
func notifydRequiresSender(senderOK, dryRun bool) bool {
	return !senderOK && !dryRun
}

func newNotifydPipeline(
	dryRun bool,
	sender notify.Sender,
	log notify.DecisionLog,
	environ []string,
	stateBase string,
) notify.Pipeline {
	pipeline := notify.Pipeline{
		DryRun: dryRun,
		Sender: sender,
		Log:    log,
		Stdout: os.Stdout,
	}
	if !dryRun {
		pipeline.LabelStore = notify.NewLabelStore(stateBase)
	}
	composer, err := notify.NewPiComposer(environ)
	if err != nil {
		pipeline.CompositionError = err
		return pipeline
	}
	pipeline.Composer = composer
	return pipeline
}

// runNotifydCommand runs the notifyd daemon: it constructs the real
// Pipeline dependencies once (unlike the hook client, which resolves them
// fresh on every invocation) and serves the control socket until SIGTERM or
// SIGINT, then removes the socket file before exiting.
func runNotifydCommand() {
	flags := flag.NewFlagSet("notifyd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dryRun := flags.Bool("dry-run", false, "print what would be sent instead of sending it")
	stateBase := flags.String("state-base", defaultNotifyStateBase(), "root directory for notify state")
	if err := flags.Parse(os.Args[2:]); err != nil {
		os.Exit(exitUsageError)
	}

	log := notify.DecisionLog{Path: filepath.Join(*stateBase, decisionLogName)}
	sender, senderOK := notify.ResolveSenderEnv(os.Environ())
	sender.Host = notify.ShortHostname()

	if notifydRequiresSender(senderOK, *dryRun) {
		_, _ = fmt.Fprintln(os.Stderr, "steward notifyd: no ntfy URL configured")
		os.Exit(1)
	}

	d := notify.Daemon{
		Pipeline: newNotifydPipeline(*dryRun, sender, log, os.Environ(), *stateBase),
	}

	sockPath := notify.SocketPath()
	ln, listenErr := notify.Listen(sockPath)
	if listenErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "steward notifyd: %v\n", listenErr)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	serveErr := d.Serve(ctx, ln)
	stop()
	if serveErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "steward notifyd: %v\n", serveErr)
	}
	_ = os.Remove(sockPath)
	if serveErr != nil {
		os.Exit(1)
	}
}
