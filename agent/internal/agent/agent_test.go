package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/collect"
	"gpumon/agent/internal/config"
	"gpumon/agent/internal/disk"
	"gpumon/agent/internal/docker"
	"gpumon/agent/internal/fixture"
	"gpumon/agent/internal/gpu"
	"gpumon/agent/internal/logx"
	"gpumon/agent/internal/payload"
	"gpumon/agent/internal/push"
)

type capture struct {
	mu    sync.Mutex
	msgs  []any
	fail  int // fail this many pushes first
	calls int
}

func (c *capture) Push(_ context.Context, m any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= c.fail {
		return errors.New("cannot reach central: connection refused")
	}
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *capture) samples() []*payload.Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*payload.Sample
	for _, m := range c.msgs {
		if s, ok := m.(*payload.Sample); ok {
			out = append(out, s)
		}
	}
	return out
}

func (c *capture) count(typ string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.msgs {
		b, _ := json.Marshal(m)
		if strings.Contains(string(b), `"type":"`+typ+`"`) {
			n++
		}
	}
	return n
}

func newTestAgent(clk clock.Clock, lib *gpu.FakeLibrary, p push.Pusher, logBuf io.Writer) *Agent {
	fs := fixture.LoadSet("procs.yaml").Build()
	cfg := config.Default()
	log := logx.New(logBuf, logx.Debug, clk)
	col := collect.New(cfg, collect.Deps{FS: fs, GPU: lib, Docker: &docker.Fake{}, Clock: clk, Log: log,
		Statfs: func(m string) (payload.Filesystem, error) { return payload.Filesystem{Mount: m, SizeBytes: 10}, nil }}, "srv-t", NewRunID(), "test")
	return &Agent{Col: col, Pusher: p, Log: log, Clock: clk, Interval: 40 * time.Millisecond,
		InventoryEvery: 6 * time.Hour, PushTimeout: time.Second}
}

var tsPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func TestREQ_AGT_001_SamplingLoop(t *testing.T) {
	if config.Default().IntervalSeconds != 30 {
		t.Fatal("default interval must be 30 s")
	}
	c := &capture{}
	a := newTestAgent(clock.Real{}, &gpu.FakeLibrary{}, c, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	ss := c.samples()
	if len(ss) < 5 {
		t.Fatalf("only %d samples in %v with a 40 ms interval", len(ss), time.Since(start))
	}
	if len(ss) > 13 {
		t.Fatalf("%d samples is more than one per interval", len(ss))
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(ss[0].RunID) {
		t.Fatalf("run_id %q is not random hex", ss[0].RunID)
	}
	for i, s := range ss {
		if s.Seq != uint64(i+1) {
			t.Fatalf("sample %d has seq %d", i, s.Seq)
		}
		if s.RunID != ss[0].RunID {
			t.Fatal("run_id changed within a run")
		}
		if !tsPattern.MatchString(s.Timestamp) {
			t.Fatalf("timestamp %q is not UTC RFC 3339 with milliseconds", s.Timestamp)
		}
		if s.SchemaVersion != payload.SchemaVersion {
			t.Fatal("schema_version missing")
		}
	}
	if NewRunID() == NewRunID() {
		t.Fatal("run ids must be random")
	}
}

func TestREQ_AGT_008_InventorySchedule(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	lib := &gpu.FakeLibrary{Driver: "570.86.15", CUDA: 12080, Devs: []*gpu.FakeDevice{{UUIDVal: "GPU-1", NameVal: "G", TotalMiB: 1000}}}
	c := &capture{}
	a := newTestAgent(clk, lib, c, io.Discard)
	a.invPending = true
	ctx := context.Background()
	a.tick(ctx)
	if c.count("inventory") != 1 {
		t.Fatal("inventory must be sent at startup")
	}
	clk.Advance(time.Minute)
	a.tick(ctx)
	if c.count("inventory") != 1 {
		t.Fatal("an unchanged inventory must not be resent before 6 h")
	}
	lib.Driver = "575.57.08"
	clk.Advance(time.Minute)
	a.tick(ctx)
	if c.count("inventory") != 2 {
		t.Fatal("a changed inventory must be sent")
	}
	clk.Advance(6 * time.Hour)
	a.tick(ctx)
	if c.count("inventory") != 3 {
		t.Fatal("the inventory must be resent every 6 h")
	}
	// A failed inventory push is retried on the next tick.
	c.fail = c.calls + 2 // fail the next sample and inventory pushes
	lib.Devs = append(lib.Devs, &gpu.FakeDevice{UUIDVal: "GPU-2", NameVal: "G2", TotalMiB: 2000})
	a.tick(ctx)
	a.tick(ctx)
	if c.count("inventory") != 4 {
		t.Fatalf("failed inventory push must be retried, got %d", c.count("inventory"))
	}
}

func TestREQ_AGT_011_PushHTTP(t *testing.T) {
	const tok = "tok-very-secret-value"
	var got struct {
		method, auth, ctype string
		body                map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.auth, got.ctype = r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	p := push.NewHTTP(srv.URL+"/api/v1/ingest", tok, 5*time.Second)
	if p.Client.Timeout != 5*time.Second || config.Default().PushTimeoutSeconds != 5 {
		t.Fatal("the request timeout must be 5 s")
	}
	a := newTestAgent(clock.Real{}, &gpu.FakeLibrary{}, p, io.Discard)
	a.Col.Prime()
	if err := p.Push(context.Background(), a.Col.Sample()); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.auth != "Bearer "+tok || got.ctype != "application/json" {
		t.Fatalf("request = %+v", got)
	}
	if got.body["schema_version"] != float64(1) || got.body["type"] != "sample" {
		t.Fatalf("body = %v", got.body)
	}

	// A slow central makes the push fail at the timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	if err := push.NewHTTP(slow.URL, tok, 100*time.Millisecond).Push(context.Background(), map[string]int{"x": 1}); err == nil {
		t.Fatal("expected a timeout")
	}

	// A rejected push reports the status but never the token.
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid or missing bearer token", http.StatusUnauthorized)
	}))
	defer deny.Close()
	err := push.NewHTTP(deny.URL, tok, time.Second).Push(context.Background(), map[string]int{"x": 1})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), tok) {
		t.Fatalf("err = %v", err)
	}
}

// Verifies: REQ-AGT-011
func TestFailedPushesAreDroppedNotBuffered(t *testing.T) {
	var logBuf bytes.Buffer
	c := &capture{fail: 3}
	a := newTestAgent(clock.Real{}, &gpu.FakeLibrary{}, c, &logBuf)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)
	ss := c.samples()
	if len(ss) == 0 {
		t.Fatal("no samples delivered after recovery")
	}
	// The first samples were dropped: delivered sequence numbers never repeat
	// and the first delivered one is greater than 1.
	seen := map[uint64]bool{}
	for _, s := range ss {
		if seen[s.Seq] {
			t.Fatalf("seq %d delivered twice", s.Seq)
		}
		seen[s.Seq] = true
	}
	if ss[0].Seq == 1 {
		t.Fatal("the failed first sample must have been dropped, not replayed")
	}
	if n := strings.Count(logBuf.String(), "push to central failed"); n < 1 || n > 2 {
		t.Fatalf("push failure warning logged %d times; it must be rate-limited", n)
	}
	if !strings.Contains(logBuf.String(), "level=warn") {
		t.Fatal("push failures are warnings")
	}
}

// Verifies: REQ-AGT-009
func TestDiskScanResultIsSent(t *testing.T) {
	c := &capture{}
	a := newTestAgent(clock.Real{}, &gpu.FakeLibrary{}, c, io.Discard)
	fs := disk.NewMapFS()
	fs.Add("/home", disk.FileInfo{Dev: 1, Ino: 2, IsDir: true, Blocks: 8})
	fs.Add("/home/f", disk.FileInfo{Dev: 1, Ino: 3, Nlink: 1, UID: 1001, Blocks: 2048})
	a.Scanner = &disk.Scanner{FS: fs, Clock: clock.Real{}, Timeout: time.Minute}
	a.ScanPaths, a.ScanTime, a.ScanOnStart, a.StatfsMounts = []string{"/home"}, "03:30", true, []string{"/", "/home"}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)
	var scan *payload.DiskScanMsg
	for _, m := range c.msgs {
		if s, ok := m.(*payload.DiskScanMsg); ok {
			scan = s
		}
	}
	if scan == nil {
		t.Fatal("no disk_scan message was sent")
	}
	if scan.Scan.CompletedAt == "" || scan.Scan.Paths[0].Mount != "/home" || scan.Scan.Paths[0].Owners[0].Owner != "alice" {
		t.Fatalf("scan = %+v", scan.Scan)
	}
}
