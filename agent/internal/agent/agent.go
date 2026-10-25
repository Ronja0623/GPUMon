// Package agent runs the sampling loop: one sample per interval, pushed
// immediately; the inventory at startup, every few hours, and on change; and
// the daily disk scan.
package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/collect"
	"gpumon/agent/internal/disk"
	"gpumon/agent/internal/logx"
	"gpumon/agent/internal/payload"
	"gpumon/agent/internal/push"
)

// NewRunID returns a random run identifier.
func NewRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000")))[:16]
	}
	return hex.EncodeToString(b)
}

// Agent ties a collector to a pusher.
type Agent struct {
	Col            *collect.Collector
	Pusher         push.Pusher
	Log            *logx.Logger
	Clock          clock.Clock
	Interval       time.Duration
	InventoryEvery time.Duration
	PrimeDelay     time.Duration
	PushTimeout    time.Duration
	// BeforeSample is called before each sample (the simulator uses it to
	// advance its synthetic world).
	BeforeSample func()

	Scanner      *disk.Scanner // nil disables the disk scan
	ScanPaths    []string
	ScanTime     string
	ScanOnStart  bool
	StatfsMounts []string

	lastInvHash string
	lastInvSent time.Time
	invPending  bool
	pendingScan *payload.DiskScanMsg
}

func (a *Agent) push(ctx context.Context, what string, msg any) bool {
	pctx, cancel := context.WithTimeout(ctx, a.PushTimeout)
	defer cancel()
	if err := a.Pusher.Push(pctx, msg); err != nil {
		a.Log.Warn("push to central failed; the message is dropped", "kind", what, "error", err)
		return false
	}
	return true
}

// Run samples until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.PushTimeout <= 0 {
		a.PushTimeout = 5 * time.Second
	}
	a.invPending = true
	if a.BeforeSample != nil {
		a.BeforeSample()
	}
	a.Col.Prime()
	if a.PrimeDelay > 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.PrimeDelay):
		}
	}
	scans := make(chan *payload.DiskScanMsg, 1)
	if a.Scanner != nil {
		go a.scanLoop(ctx, scans)
	}
	a.Log.Info("agent started", "server_id", a.Col.ServerID, "run_id", a.Col.RunID, "interval_seconds", a.Interval.Seconds())
	a.tick(ctx)
	ticker := time.NewTicker(a.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.Log.Info("agent stopping")
			return nil
		case m := <-scans:
			a.pendingScan = m
			if a.push(ctx, "disk_scan", m) {
				a.pendingScan = nil
			}
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

// tick takes and pushes one sample, then the inventory and a pending disk
// scan result when needed.
func (a *Agent) tick(ctx context.Context) {
	if a.BeforeSample != nil {
		a.BeforeSample()
	}
	s := a.Col.Sample()
	a.push(ctx, "sample", s)

	inv := a.Col.Inventory()
	b, _ := json.Marshal(inv.Inventory)
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	now := a.Clock.Now()
	if a.invPending || h != a.lastInvHash || now.Sub(a.lastInvSent) >= a.InventoryEvery {
		if a.push(ctx, "inventory", inv) {
			a.lastInvHash, a.lastInvSent, a.invPending = h, now, false
		} else {
			a.invPending = true
		}
	}
	if a.pendingScan != nil && a.push(ctx, "disk_scan", a.pendingScan) {
		a.pendingScan = nil
	}
}

func (a *Agent) scanLoop(ctx context.Context, out chan<- *payload.DiskScanMsg) {
	if a.ScanOnStart {
		a.runScan(ctx, out)
	}
	for {
		next := disk.NextRun(a.Clock.Now(), a.ScanTime)
		t := time.NewTimer(next.Sub(a.Clock.Now()))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			a.runScan(ctx, out)
		}
	}
}

// runScan runs one scan in a dedicated goroutine, whose OS thread gets the
// lowest CPU and IO priority and is discarded afterwards.
func (a *Agent) runScan(ctx context.Context, out chan<- *payload.DiskScanMsg) {
	done := make(chan *payload.DiskScanMsg, 1)
	go func() {
		sc := *a.Scanner
		sc.Owner = a.Col.Accounts().FileOwner
		mountOf := func(p string) string { return disk.MountOf(p, a.StatfsMounts) }
		res, errs := sc.Scan(ctx, a.ScanPaths, mountOf)
		for _, e := range errs {
			a.Log.Warn("disk scan problem", "error", e)
		}
		done <- a.Col.DiskScanMessage(res, errs)
	}()
	m := <-done
	a.Log.Info("disk scan finished", "partial", m.Scan.Partial, "completed_at", m.Scan.CompletedAt)
	select {
	case out <- m:
	case <-ctx.Done():
	}
}
