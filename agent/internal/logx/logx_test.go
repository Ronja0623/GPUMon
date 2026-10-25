package logx_test

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/logx"
)

var logfmtLine = regexp.MustCompile(`^ts=\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z level=(debug|info|warn|error) msg=("([^"\\]|\\.)*"|\S+)( [A-Za-z0-9_.]+=("([^"\\]|\\.)*"|\S+))*$`)

func TestREQ_LOG_001_StructuredASCII(t *testing.T) {
	var buf bytes.Buffer
	l := logx.New(&buf, logx.Debug, clock.NewFake(time.Date(2026, 10, 25, 1, 2, 3, 4e6, time.UTC)))
	l.Info("agent started", "server_id", "srv-40", "interval_seconds", 30)
	l.Warn("odd input", "value", "caf\u00e9 \u2603 \"quoted\" a=b\nnext")
	l.Error("plain")
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !logfmtLine.MatchString(line) {
			t.Errorf("not logfmt: %q", line)
		}
		for _, r := range line {
			if r > 127 {
				t.Fatalf("non-ASCII output: %q", line)
			}
		}
	}
	if !strings.Contains(buf.String(), `ts=2026-10-25T01:02:03.004Z level=info msg="agent started" server_id=srv-40 interval_seconds=30`) {
		t.Fatalf("unexpected output:\n%s", buf.String())
	}
}

func TestREQ_LOG_002_LevelsAndRateLimit(t *testing.T) {
	for _, n := range []string{"debug", "info", "warn", "error"} {
		if _, err := logx.ParseLevel(n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := logx.ParseLevel("trace"); err == nil {
		t.Fatal("unknown levels must be rejected")
	}
	var buf bytes.Buffer
	clk := clock.NewFake(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	l := logx.New(&buf, logx.Warn, clk)
	l.Info("hidden")
	l.Debug("hidden")
	for i := 0; i < 10; i++ {
		l.Warn("push failed", "error", "connection refused")
		l.Error("disk full")
		clk.Advance(time.Minute)
	}
	l.Warn("push failed", "error", "timeout") // a different message is not suppressed
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatal("lines below the configured level must be dropped")
	}
	if strings.Count(out, "connection refused") != 1 || strings.Count(out, "disk full") != 1 || strings.Count(out, "timeout") != 1 {
		t.Fatalf("rate limiting failed:\n%s", out)
	}
	clk.Advance(time.Hour)
	l.Warn("push failed", "error", "connection refused")
	if !strings.Contains(buf.String(), "suppressed=9") {
		t.Fatalf("expected suppressed count:\n%s", buf.String())
	}
	// Info lines are never rate-limited.
	buf.Reset()
	l2 := logx.New(&buf, logx.Info, clk)
	l2.Info("tick")
	l2.Info("tick")
	if strings.Count(buf.String(), "tick") != 2 {
		t.Fatal("info lines must not be rate-limited")
	}
}
