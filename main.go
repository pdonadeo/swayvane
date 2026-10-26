package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// daemonEnvVar is set in the environment of the re-exec'd daemon child so it
// knows not to daemonize again.
const daemonEnvVar = "_WS_RENAME_DAEMON"

// ── Daemonization ─────────────────────────────────────────────────────────────

func daemonize(wd string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemonize: %v\n", err)
		os.Exit(1)
	}
	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemonize: open /dev/null: %v\n", err)
		os.Exit(1)
	}
	null := devNull.Fd()

	env := append(os.Environ(), daemonEnvVar+"=1")
	_, err = syscall.ForkExec(exe, os.Args, &syscall.ProcAttr{
		Dir:   wd,
		Env:   env,
		Files: []uintptr{null, null, null},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemonize: forkexec: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// ── Event loop ────────────────────────────────────────────────────────────────

// eventLoop returns true if the caller should reconnect (Sway restart or
// unexpected disconnect), false if it should exit cleanly.
func eventLoop(disp *dispatcher, conn *Conn, sigCh <-chan os.Signal, log *slog.Logger) bool {
	shutdownReason := ShutdownNone
	for {
		select {
		case ev, ok := <-conn.eventCh:
			if !ok {
				// Connection closed. Reconnect unless we got a clean exit.
				return shutdownReason != ShutdownExit
			}
			switch e := ev.(type) {
			case WindowEvent:
				log.Debug("EVENT", "change", e.Info.Change)
				disp.dispatch(conn, e.Info)
			case BindingEvent:
				log.Debug("BINDING", "command", e.Command)
				disp.dispatchBinding(conn, e.Command)
			case ShutdownEvent:
				shutdownReason = e.Reason
				if e.Reason == ShutdownExit {
					log.Info("Sway shutdown, exiting")
					return false
				}
				// ShutdownRestart: keep looping until the socket closes.
			}
		case sig := <-sigCh:
			log.Info("Signal received, shutting down", "signal", sig)
			return false
		}
	}
}

func protectedLoop(disp *dispatcher, sigCh <-chan os.Signal, log *slog.Logger) error {
	for {
		conn, err := Connect()
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		if err := conn.Subscribe(disp.subscriptions()); err != nil {
			conn.Close()
			return fmt.Errorf("subscribe: %w", err)
		}
		log.Debug("connected to Sway")
		disp.dispatchConnect(conn)

		reconnect := eventLoop(disp, conn, sigCh, log)
		conn.Close()

		if !reconnect {
			return nil
		}
		log.Info("Sway is restarting, wait a second...")
		time.Sleep(1 * time.Second)
		log.Debug("reconnecting to Sway")
	}
}

// ── CLI & main ────────────────────────────────────────────────────────────────

func absPath(path, wd string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(wd, path)
}

// longFlagNames are the multi-letter flag names that must be given with a
// "--" prefix (GNU/Go convention), never a single "-". Single-letter
// shorthands (e.g. -c, -d) may keep a single dash.
var longFlagNames = map[string]bool{
	"daemon":           true,
	"verbose":          true,
	"conf":             true,
	"uniq":             true,
	"version":          true,
	"autotiling":       true,
	"maximize":         true,
	"expose":           true,
	"scatter":          true,
	"split-ratio":      true,
	"tab-min-width":    true,
	"stack-min-height": true,
	"firefox-rules":    true,
	"stderr":           true,
}

// usageWithDoubleDash returns a flag.Usage replacement that fixes up the
// output of flag.PrintDefaults(), which always prints "-name" no matter how
// many dashes the flag actually requires. Without this, --help contradicts
// rejectSingleDashLongFlags by advertising forms like "-daemon" that are
// rejected at parse time.
func usageWithDoubleDash(longNames map[string]bool) func() {
	names := make([]string, 0, len(longNames))
	for n := range longNames {
		names = append(names, regexp.QuoteMeta(n))
	}
	re := regexp.MustCompile(`(?m)^(  -)(` + strings.Join(names, "|") + `)(\s|$)`)
	return func() {
		out := flag.CommandLine.Output()
		_, _ = fmt.Fprintf(out, "Usage of %s:\n", os.Args[0])
		var buf strings.Builder
		flag.CommandLine.SetOutput(&buf)
		flag.PrintDefaults()
		flag.CommandLine.SetOutput(out)
		_, _ = fmt.Fprint(out, re.ReplaceAllString(buf.String(), "$1-$2$3"))
	}
}

// rejectSingleDashLongFlags exits with an error if a multi-letter flag was
// given with a single dash (e.g. -daemon instead of --daemon).
func rejectSingleDashLongFlags(args []string) {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			continue
		}
		name := strings.TrimPrefix(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if longFlagNames[name] {
			fmt.Fprintf(os.Stderr, "ws-rename: unknown flag -%s (did you mean --%s?)\n", name, name)
			os.Exit(2)
		}
	}
}

func main() {
	defaultConf := getDefaultConfFname("app-icons.json")
	// Empty when the file isn't there: the Firefox title rules are enabled by
	// the presence of their config file, nothing else to switch on.
	defaultFirefoxRules := getDefaultConfFname(firefoxTitleRulesFile)

	var (
		uniq           bool
		daemon         bool
		verbose        bool
		showVersion    bool
		autotiling     bool
		maximize       bool
		expose         bool
		scatter        bool
		toStderr       bool
		splitRatio     int
		tabMinWidth    int
		stackMinHeight int
		confFile       string
		firefoxRules   string
	)

	flag.BoolVar(&uniq, "u", false, "Remove duplicate icons in case the same application")
	flag.BoolVar(&uniq, "uniq", false, "Remove duplicate icons in case the same application")
	flag.BoolVar(&daemon, "d", false, "Daemon mode: send the application to background")
	flag.BoolVar(&daemon, "daemon", false, "Daemon mode: send the application to background")
	flag.BoolVar(&verbose, "v", false, "Log debug information too")
	flag.BoolVar(&verbose, "verbose", false, "Log debug information too")
	flag.StringVar(&confFile, "c", defaultConf, "Configuration file path")
	flag.StringVar(&confFile, "conf", defaultConf, "Configuration file path")
	flag.BoolVar(&showVersion, "version", false, "Print version information and exit")
	flag.BoolVar(&autotiling, "autotiling", false,
		"Set the split direction of the focused window to match its shape")
	flag.IntVar(&splitRatio, "split-ratio", 30,
		"Percentage of the container given to a newly opened window, 1 to 99 "+
			"(default 30: the existing window keeps 70%; 50 splits evenly). "+
			"Requires --autotiling")
	flag.BoolVar(&maximize, "maximize", false,
		"Give the window marked "+maximizeMark+" the whole workspace until the "+
			"mark is removed, bar and gaps respected, parking the other windows "+
			"on workspace "+maximizeStash+" meanwhile "+
			"(bind it with: bindsym $mod+z mark --add --toggle "+maximizeMark+")")
	flag.BoolVar(&expose, "expose", false,
		"Show every window of the focused workspace side by side, tabbed and "+
			"stacked ones included, until the binding is pressed again "+
			"(bind it with: bindsym <key> nop "+exposeCommand+")")
	flag.BoolVar(&scatter, "scatter", false,
		"Take every window of the focused workspace out of the layout and deal "+
			"them out side by side as floating windows, until one is clicked or "+
			"the binding is pressed again "+
			"(bind it with: bindsym <key> nop "+scatterCommand+", or nop "+
			scatterUniformCommand+" to shrink them all alike)")
	flag.IntVar(&tabMinWidth, "tab-min-width", 500,
		"Minimum width in pixels a new window may get out of a horizontal "+
			"split: below it the container is made tabbed instead, so both "+
			"windows keep the full width (0 disables). Requires --autotiling")
	flag.IntVar(&stackMinHeight, "stack-min-height", 300,
		"Same as --tab-min-width for a vertical split: below this height the "+
			"container is stacked instead of split (0 disables). "+
			"Requires --autotiling")
	flag.StringVar(&firefoxRules, "firefox-rules", defaultFirefoxRules,
		"Path to the Firefox title rules file (empty: feature disabled)")
	flag.BoolVar(&toStderr, "stderr", false,
		"Log to stderr instead of the system log")

	flag.Usage = usageWithDoubleDash(longFlagNames)

	rejectSingleDashLongFlags(os.Args[1:])
	flag.Parse()

	if showVersion {
		fmt.Println("ws-rename " + version)
		os.Exit(0)
	}

	_ = uniq // accepted but unused, like the OCaml version

	if splitRatio < 1 || splitRatio > 99 {
		fmt.Fprintf(os.Stderr,
			"ws-rename: --split-ratio must be between 1 and 99, got %d\n", splitRatio)
		os.Exit(2)
	}

	if tabMinWidth < 0 || stackMinHeight < 0 {
		fmt.Fprintf(os.Stderr,
			"ws-rename: --tab-min-width and --stack-min-height cannot be negative\n")
		os.Exit(2)
	}

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
		os.Exit(1)
	}

	if confFile != "" {
		confFile = absPath(confFile, wd)
		info, statErr := os.Stat(confFile)
		if statErr != nil || info.IsDir() {
			fmt.Fprintf(os.Stderr, "ERROR: file %q does not exist or not readable.\n", confFile)
			os.Exit(1)
		}
	}

	if daemon && os.Getenv(daemonEnvVar) == "" {
		daemonize(wd)
		// parent exits inside daemonize(); never reached
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	log := newLogger(level, toStderr)
	log.Info("started", "version", version, "pid", os.Getpid())

	conf, err := readConf(confFile)
	if err != nil {
		log.Error("failed to read configuration", "file", confFile, "error", err)
		os.Exit(1)
	}
	if confFile != "" {
		log.Debug("configuration loaded", "file", confFile)
	}

	// Handlers run in registration order, and the one renaming workspaces goes
	// last on purpose: the others may move windows around, and the icons must
	// reflect the layout as it ends up, not as it was when the event arrived.
	var handlers []WindowHandler

	if firefoxRules != "" {
		firefoxRules = absPath(firefoxRules, wd)
		rules, loadErr := loadFirefoxRules(firefoxRules, log)
		switch {
		case loadErr != nil:
			log.Error("failed to read Firefox title rules, feature disabled",
				"file", firefoxRules, "error", loadErr)
		case len(rules) == 0:
			log.Warn("no usable Firefox title rules, feature disabled", "file", firefoxRules)
		default:
			handlers = append(handlers, newFirefoxHandler(rules))
			log.Info("Firefox title rules enabled", "file", firefoxRules, "rules", len(rules))
		}
	}

	// The exposés reshape the layout for a while and the other layout
	// handlers must leave it alone until they are over, so they are told about
	// it before they are created.
	var exposeH *exposeHandler
	var scatterH *scatterHandler
	var exposeBindings []string
	if expose {
		exposeH = newExposeHandler()
		exposeBindings = append(exposeBindings, exposeCommand)
	}
	if scatter {
		scatterH = newScatterHandler()
		exposeBindings = append(exposeBindings, scatterCommand, scatterUniformCommand)
	}
	paused := func() bool {
		return (exposeH != nil && exposeH.active()) || (scatterH != nil && scatterH.active())
	}
	if exposeH != nil && scatterH != nil {
		exposeH.paused = scatterH.active
		scatterH.paused = exposeH.active
	}

	if autotiling {
		h := newAutotileHandler(splitRatio, tabMinWidth, stackMinHeight)
		h.paused = paused
		handlers = append(handlers, h)
		log.Info("autotiling enabled", "split_ratio", splitRatio,
			"tab_min_width", tabMinWidth, "stack_min_height", stackMinHeight)
	}

	if maximize {
		h := newMaximizeHandler()
		h.paused = paused
		h.endOn = exposeBindings
		handlers = append(handlers, h)
		log.Info("maximize enabled", "mark", maximizeMark, "stash", maximizeStash)
	}

	// Either exposé's key ends the other one, which stands aside meanwhile:
	// the exposé is asked to by the time its key reaches the scattered one,
	// and the scattered one does not start in the same go (see
	// scatterHandler.HandleBinding).
	if exposeH != nil {
		handlers = append(handlers, exposeH)
		log.Info("exposé enabled", "binding", "nop "+exposeCommand)
	}
	if scatterH != nil {
		handlers = append(handlers, scatterH)
		log.Info("scattered exposé enabled",
			"binding", "nop "+scatterCommand, "uniform", "nop "+scatterUniformCommand)
	}

	handlers = append(handlers, newIconHandler(conf))
	disp := newDispatcher(log, handlers...)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	if err := protectedLoop(disp, sigCh, log); err != nil {
		log.Error("fatal error", "error", err)
		os.Exit(1)
	}
}
