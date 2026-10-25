// Package logx writes structured logfmt lines (ASCII only) and rate-limits
// repeated identical warnings and errors.
package logx

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"gpumon/agent/internal/clock"
)

// Level is a log severity.
type Level int

// Log levels, lowest first.
const (
	Debug Level = iota
	Info
	Warn
	Error
)

var levelNames = map[Level]string{Debug: "debug", Info: "info", Warn: "warn", Error: "error"}

// String returns the level name.
func (l Level) String() string { return levelNames[l] }

// ParseLevel converts a level name to a Level.
func ParseLevel(s string) (Level, error) {
	for l, n := range levelNames {
		if n == s {
			return l, nil
		}
	}
	return Info, fmt.Errorf("unknown log level %q (expected debug, info, warn, or error)", s)
}

// DefaultWindow is the minimum time between two identical warn/error lines.
const DefaultWindow = time.Hour

type seenEntry struct {
	last       time.Time
	suppressed int
}

// Logger is a leveled, rate-limited logfmt logger. It is safe for concurrent use.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	min    Level
	clock  clock.Clock
	window time.Duration
	seen   map[string]*seenEntry
}

// New returns a logger writing to w.
func New(w io.Writer, min Level, c clock.Clock) *Logger {
	if c == nil {
		c = clock.Real{}
	}
	return &Logger{w: w, min: min, clock: c, window: DefaultWindow, seen: map[string]*seenEntry{}}
}

// SetWindow changes the rate-limit window for identical warn/error lines.
func (l *Logger) SetWindow(d time.Duration) {
	l.mu.Lock()
	l.window = d
	l.mu.Unlock()
}

// Debug logs at debug level.
func (l *Logger) Debug(msg string, kv ...any) { l.log(Debug, msg, kv) }

// Info logs at info level.
func (l *Logger) Info(msg string, kv ...any) { l.log(Info, msg, kv) }

// Warn logs at warn level; identical lines are rate-limited.
func (l *Logger) Warn(msg string, kv ...any) { l.log(Warn, msg, kv) }

// Error logs at error level; identical lines are rate-limited.
func (l *Logger) Error(msg string, kv ...any) { l.log(Error, msg, kv) }

func (l *Logger) log(lv Level, msg string, kv []any) {
	if l == nil || lv < l.min {
		return
	}
	body := "msg=" + quote(msg) + formatKV(kv)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	if lv >= Warn {
		key := lv.String() + "|" + body
		if e, ok := l.seen[key]; ok && now.Sub(e.last) < l.window {
			e.suppressed++
			return
		} else if ok {
			if e.suppressed > 0 {
				body += fmt.Sprintf(" suppressed=%d", e.suppressed)
			}
			e.last, e.suppressed = now, 0
		} else {
			l.seen[key] = &seenEntry{last: now}
		}
	}
	line := "ts=" + now.UTC().Format("2006-01-02T15:04:05.000Z") + " level=" + lv.String() + " " + body + "\n"
	_, _ = io.WriteString(l.w, line)
}

func formatKV(kv []any) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(" ")
		b.WriteString(sanitizeKey(fmt.Sprint(kv[i])))
		b.WriteString("=")
		b.WriteString(quote(fmt.Sprint(kv[i+1])))
	}
	if len(kv)%2 == 1 {
		b.WriteString(" extra=" + quote(fmt.Sprint(kv[len(kv)-1])))
	}
	return b.String()
}

func sanitizeKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// ASCII escapes every non-printable or non-ASCII rune so the result is
// printable ASCII.
func ASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0x7e:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func quote(v string) string {
	v = ASCII(v)
	if v == "" || strings.ContainsAny(v, " =\"\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	return v
}

// ErrorSet collects distinct English error messages for one payload.
type ErrorSet struct {
	m map[string]struct{}
}

// NewErrorSet returns an empty set.
func NewErrorSet() *ErrorSet { return &ErrorSet{m: map[string]struct{}{}} }

// Add records a message.
func (e *ErrorSet) Add(format string, args ...any) {
	e.m[ASCII(fmt.Sprintf(format, args...))] = struct{}{}
}

// List returns the messages sorted.
func (e *ErrorSet) List() []string {
	out := make([]string, 0, len(e.m))
	for k := range e.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
