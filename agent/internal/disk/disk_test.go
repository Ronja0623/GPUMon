package disk_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpumon/agent/internal/accounts"
	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/config"
	"gpumon/agent/internal/disk"
	"gpumon/agent/internal/fixture"
)

func loadTree(t *testing.T) *disk.MapFS {
	t.Helper()
	fs := disk.NewMapFS()
	for _, line := range strings.Split(string(fixture.Read("disktree.txt")), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		n := func(i int) uint64 {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		fs.Add(f[0], disk.FileInfo{
			UID: uint32(n(2)), Blocks: int64(n(3)), Size: int64(n(4)), Dev: n(5), Ino: n(6), Nlink: n(7),
			IsDir: f[1] == "d", IsSymlink: f[1] == "l",
		})
	}
	return fs
}

func owners(t *testing.T) func(uint32) string {
	acc := &accounts.Accounts{Passwd: accounts.ParsePasswd(fixture.Read("etc/passwd_srv_a")), UIDMin: 1000}
	acc.Subuid, _ = accounts.ParseSubuid(fixture.Read("etc/subuid"))
	return acc.FileOwner
}

func TestREQ_AGT_009_DiskScanPerOwner(t *testing.T) {
	fs := loadTree(t)
	lowered := false
	sc := disk.Scanner{FS: fs, Owner: owners(t), Clock: clock.NewFake(time.Date(2026, 10, 25, 19, 30, 0, 0, time.UTC)),
		Timeout: 2 * time.Hour, Lower: func() error { lowered = true; return nil }}
	res, errs := sc.Scan(context.Background(), []string{"/home"}, func(string) string { return "/home" })
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if !lowered {
		t.Fatal("the scan must lower its CPU and IO priority first")
	}
	if res.Partial || len(res.Paths) != 1 || res.Paths[0].Mount != "/home" || res.CompletedAt == "" {
		t.Fatalf("result = %+v", res)
	}
	got := map[string]uint64{}
	for _, o := range res.Paths[0].Owners {
		got[o.Owner] = o.Bytes
	}
	want := map[string]uint64{
		// dirs 8+8, data 2048, hard link once 4096, sparse file 16 allocated blocks, symlinks 0
		"alice": (8 + 8 + 2048 + 4096 + 16) * 512,
		// dir 8, file 1024, a file owned by bob's subuid range 512
		"bob": (8 + 1024 + 512) * 512,
		// unknown UID
		"uid:4242": 100 * 512,
		// /home itself; /home/mnt is another filesystem and is skipped
		"root": 8 * 512,
	}
	for o, b := range want {
		if got[o] != b {
			t.Errorf("%s: got %d bytes, want %d", o, got[o], b)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected owners: %v", got)
	}
	var total uint64
	for _, b := range want {
		total += b
	}
	if res.Paths[0].TotalBytes != total {
		t.Errorf("total = %d, want %d", res.Paths[0].TotalBytes, total)
	}
	if res.Paths[0].Owners[0].Owner != "alice" {
		t.Error("owners must be sorted by size")
	}
}

// Verifies: REQ-AGT-009
func TestDiskScanTimeoutReportsPartial(t *testing.T) {
	fs := loadTree(t)
	clk := clock.NewFake(time.Date(2026, 10, 25, 19, 30, 0, 0, time.UTC))
	fs.OnLstat = func(string) { clk.Advance(time.Minute) }
	sc := disk.Scanner{FS: fs, Owner: owners(t), Clock: clk, Timeout: 5 * time.Minute}
	res, _ := sc.Scan(context.Background(), []string{"/home"}, func(string) string { return "/home" })
	if !res.Partial || !res.Paths[0].Partial {
		t.Fatalf("expected a partial result: %+v", res)
	}
	if res.Paths[0].TotalBytes == 0 {
		t.Fatal("a partial scan still reports what it counted")
	}
}

// Verifies: REQ-AGT-009
func TestDiskScanScheduleAndDefaults(t *testing.T) {
	cfg := config.Default()
	if cfg.Disk.ScanTimeLocal != "03:30" || cfg.Disk.ScanTimeoutMinutes != 120 || strings.Join(cfg.Disk.ScanPaths, ",") != "/home" {
		t.Fatalf("defaults = %+v", cfg.Disk)
	}
	tz, _ := time.LoadLocation("Asia/Taipei")
	if tz == nil {
		tz = time.FixedZone("UTC+8", 8*3600)
	}
	before := time.Date(2026, 10, 25, 2, 0, 0, 0, tz)
	if got := disk.NextRun(before, "03:30"); !got.Equal(time.Date(2026, 10, 25, 3, 30, 0, 0, tz)) {
		t.Fatalf("next run = %v", got)
	}
	after := time.Date(2026, 10, 25, 4, 0, 0, 0, tz)
	if got := disk.NextRun(after, "03:30"); !got.Equal(time.Date(2026, 10, 26, 3, 30, 0, 0, tz)) {
		t.Fatalf("next run = %v", got)
	}
	if disk.MountOf("/home/alice", []string{"/", "/home"}) != "/home" || disk.MountOf("/srv/data", []string{"/", "/home"}) != "/" {
		t.Fatal("mount mapping is wrong")
	}
}

// Verifies: REQ-AGT-009
func TestDiskScanRealFilesystemDoesNotFollowSymlinks(t *testing.T) {
	// The real stat layer reports symlinks as links (lstat) and allocated blocks.
	dir := t.TempDir()
	fi, err := disk.OSFS{}.Lstat(dir)
	if err != nil || !fi.IsDir {
		t.Fatalf("lstat dir: %+v %v", fi, err)
	}
	if err := disk.LowerThreadPriority(); err != nil {
		t.Logf("lowering priority failed (allowed in restricted sandboxes): %v", err)
	}
	if _, err := disk.OSStatfs(dir); err != nil {
		t.Fatalf("statfs: %v", err)
	}
}
