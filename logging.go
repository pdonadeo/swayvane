package main

import (
	"context"
	"fmt"
	"log/slog"
	"log/syslog"
	"os"
	"strings"
)

// logTag identifies the daemon in the system log: `journalctl -t swayvane`.
const logTag = "swayvane"

// newLogger builds the logger for the whole daemon. Records go to the system
// log, which on a systemd machine means journald.
//
// Writing to syslog rather than to stdout is deliberate: the daemon is started
// by the compositor through `exec`, so it inherits whatever stdout the
// compositor itself was given — a systemd user session, a display manager, a
// bare tty — and that differs from machine to machine. The syslog socket is
// always in the same place.
//
// If the socket isn't there (a system without syslog at all) there would be no
// destination left, so we fall back to stderr. In daemon mode stderr is
// /dev/null, so that fallback is only really useful in the foreground — which
// is also what --stderr forces, to follow the log in a terminal without
// journalctl.
func newLogger(level slog.Level, forceStderr bool) *slog.Logger {
	if !forceStderr {
		if w, err := syslog.New(syslog.LOG_DAEMON|syslog.LOG_INFO, logTag); err == nil {
			return slog.New(&syslogHandler{w: w, level: level})
		}
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// syslogHandler writes slog records to the system log.
type syslogHandler struct {
	w     *syslog.Writer
	level slog.Level
	attrs []slog.Attr
}

func (h *syslogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *syslogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	// Copy instead of appending in place: several loggers derive from the same
	// parent (one per handler, via dispatcher.runOne) and would otherwise share
	// — and overwrite — the same backing array.
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &syslogHandler{w: h.w, level: h.level, attrs: merged}
}

// WithGroup is not supported: nothing in this program uses attribute groups,
// and syslog lines are flat by nature.
func (h *syslogHandler) WithGroup(_ string) slog.Handler { return h }

// Handle emits "message key=value ..." with no timestamp and no level prefix:
// the journal records both by itself, and repeating them would only clutter
// the output of journalctl.
func (h *syslogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	line := b.String()

	// Typed methods rather than Write, so that journalctl -p filters by
	// priority as expected.
	switch {
	case r.Level >= slog.LevelError:
		return h.w.Err(line)
	case r.Level >= slog.LevelWarn:
		return h.w.Warning(line)
	case r.Level >= slog.LevelInfo:
		return h.w.Info(line)
	default:
		return h.w.Debug(line)
	}
}
