package sdk

import (
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Level orders log messages; a logger prints its level and above.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarning
	LevelError
	LevelSilent
)

var levelNames = map[Level]string{LevelDebug: "debug", LevelInfo: "info", LevelWarning: "warning", LevelError: "error"}

// ParseLevel reads the run request's logLevel (mihomo's names); unknown values mean info.
func ParseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	case "warning", "warn":
		return LevelWarning
	case "error":
		return LevelError
	case "silent":
		return LevelSilent
	}
	return LevelInfo
}

// minSecret is the shortest string redacted; shorter ones would blank out ordinary words.
const minSecret = 4

const redacted = "[redacted]"

// Logger writes text lines to stderr with every registered secret replaced by [redacted]. The SDK registers the
// link, the egress credentials and the front credentials; an extension adds what it decodes from the link.
type Logger struct {
	mu       sync.Mutex
	w        io.Writer
	level    Level
	secrets  []string
	replacer *strings.Replacer
	now      func() time.Time
}

// NewLogger writes lines at level and above to w.
func NewLogger(w io.Writer, level Level) *Logger {
	return &Logger{w: w, level: level, replacer: strings.NewReplacer(), now: time.Now}
}

// AddSecret redacts s, and its URL-escaped forms, from every later line.
func (l *Logger) AddSecret(secrets ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range secrets {
		for _, v := range []string{s, url.QueryEscape(s), url.PathEscape(s)} {
			if len(v) >= minSecret && !slices.Contains(l.secrets, v) {
				l.secrets = append(l.secrets, v)
			}
		}
	}
	// strings.Replacer tries its pairs in order at each position: longest first, so a secret that contains
	// another is redacted whole.
	slices.SortFunc(l.secrets, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(l.secrets))
	for _, s := range l.secrets {
		pairs = append(pairs, s, redacted)
	}
	l.replacer = strings.NewReplacer(pairs...)
}

// Redact returns s with every registered secret replaced.
func (l *Logger) Redact(s string) string {
	l.mu.Lock()
	r := l.replacer
	l.mu.Unlock()
	return r.Replace(s)
}

// Enabled reports whether a message at level is printed.
func (l *Logger) Enabled(level Level) bool { return level >= l.level && level < LevelSilent }

func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarning, format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }

func (l *Logger) logf(level Level, format string, args ...any) {
	if !l.Enabled(level) {
		return
	}
	l.line(level, fmt.Sprintf(format, args...))
}

func (l *Logger) line(level Level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg = l.replacer.Replace(strings.TrimRight(msg, "\r\n"))
	for _, part := range strings.Split(msg, "\n") {
		_, _ = fmt.Fprintf(l.w, "%s [%s] %s\n", l.now().Format("15:04:05.000"), levelNames[level], part)
	}
}

// Writer returns an io.Writer that logs each line written to it at level, for libraries that log to a writer.
// Partial lines wait for their newline, so a secret split across writes is still redacted.
func (l *Logger) Writer(level Level) io.Writer { return &lineWriter{l: l, level: level} }

// maxPending flushes a line that never ends.
const maxPending = 64 << 10

type lineWriter struct {
	l       *Logger
	level   Level
	mu      sync.Mutex
	pending []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		i := slices.Index(w.pending, '\n')
		if i < 0 {
			break
		}
		w.emit(w.pending[:i])
		w.pending = w.pending[i+1:]
	}
	if len(w.pending) > maxPending {
		w.emit(w.pending)
		w.pending = nil
	}
	if len(w.pending) == 0 {
		w.pending = nil
	}
	return len(p), nil
}

func (w *lineWriter) emit(b []byte) {
	if w.l.Enabled(w.level) {
		w.l.line(w.level, string(b))
	}
}
