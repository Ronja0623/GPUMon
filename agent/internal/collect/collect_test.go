package collect_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/collect"
	"gpumon/agent/internal/config"
	"gpumon/agent/internal/docker"
	"gpumon/agent/internal/fixture"
	"gpumon/agent/internal/gpu"
	"gpumon/agent/internal/hostfs"
	"gpumon/agent/internal/logx"
	"gpumon/agent/internal/payload"
)

var t0 = time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC)

const (
	labeledID   = "4f2a9c3e5b7d1f0a8c6e4b2d9f7a5c3e1b0d8f6a4c2e0b9d7f5a3c1e8b6d4f2a"
	unlabeledID = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3c2b1a0f9e8d"
)

func u64(v uint64) *uint64 { return &v }

type harness struct {
	fs   *hostfs.MapFS
	lib  *gpu.FakeLibrary
	dc   *docker.Fake
	clk  *clock.Fake
	log  *bytes.Buffer
	col  *collect.Collector
	cfg  config.Config
	stat map[string]payload.Filesystem
}

func fixtureDevices() []*gpu.FakeDevice {
	return []*gpu.FakeDevice{
		{
			UUIDVal: "GPU-11111111-2222-3333-4444-555555555555", NameVal: "Fake GPU A", TotalMiB: 10240, UsedMiB: 4500,
			Util: 77, Temp: 65, PowerMilliW: 150250, LimitMilliW: 320000, Fan: 40,
			Compute:  []gpu.ProcInfo{{PID: 400, UsedMiB: u64(1000)}, {PID: 700, UsedMiB: nil}},
			Graphics: []gpu.ProcInfo{{PID: 402, UsedMiB: u64(12)}},
			Errs: map[string]error{
				"FanSpeed":           gpu.ErrNotSupported,
				"Temperature":        errors.New("GPU is lost"),
				"ProcessUtilization": gpu.ErrNotSupported,
			},
		},
		{
			UUIDVal: "GPU-66666666-7777-8888-9999-000000000000", NameVal: "Fake GPU B", TotalMiB: 24576, UsedMiB: 9000,
			Util: 50, Temp: 55, PowerMilliW: 200000, LimitMilliW: 350000, Fan: 30,
			Compute:   []gpu.ProcInfo{{PID: 200, UsedMiB: u64(8000)}, {PID: 501, UsedMiB: u64(500)}},
			ProcUtils: []gpu.ProcUtil{{PID: 200, SMUtil: 40, TimeStamp: 10}, {PID: 200, SMUtil: 60, TimeStamp: 20}},
		},
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		fs:  fixture.LoadSet("procs.yaml").Build(),
		lib: &gpu.FakeLibrary{Driver: "570.86.15", CUDA: 12080, Devs: fixtureDevices()},
		dc: &docker.Fake{Containers: map[string]*docker.Container{
			labeledID:   {ID: labeledID, Name: "carol-train", Image: "pytorch/pytorch:2.4", Labels: map[string]string{"owner": "carol"}},
			unlabeledID: {ID: unlabeledID, Name: "nightly-eval", Image: "nvcr.io/nvidia/cuda:12.4.1"},
		}},
		clk: clock.NewFake(t0),
		log: &bytes.Buffer{},
		cfg: config.Default(),
		stat: map[string]payload.Filesystem{
			"/":     {SizeBytes: 1000, UsedBytes: 400, FreeBytes: 550},
			"/home": {SizeBytes: 5000, UsedBytes: 3000, FreeBytes: 1900},
		},
	}
	h.cfg.Docker.DefaultOwner = "dave"
	h.build()
	return h
}

func (h *harness) build() {
	statfs := func(m string) (payload.Filesystem, error) {
		fs, ok := h.stat[m]
		if !ok {
			return fs, fmt.Errorf("no such file or directory")
		}
		return fs, nil
	}
	var dc docker.Client
	if h.dc != nil {
		dc = h.dc
	}
	h.col = collect.New(h.cfg, collect.Deps{
		FS: h.fs, GPU: h.lib, Docker: dc, Statfs: statfs, Clock: h.clk,
		Log: logx.New(h.log, logx.Debug, h.clk),
	}, "srv-test", "0123456789abcdef", "test")
}

// sample primes, advances the clock, and takes one sample.
func (h *harness) sample() *payload.Sample {
	h.col.Prime()
	h.clk.Advance(10 * time.Second)
	return h.col.Sample()
}

func findUser(s *payload.Sample, owner string) *payload.User {
	for i := range s.Users {
		if s.Users[i].Owner == owner {
			return &s.Users[i]
		}
	}
	return nil
}

func TestREQ_AGT_002_GPUFieldsNullOnFailure(t *testing.T) {
	h := newHarness(t)
	s := h.sample()
	if len(s.GPUs) != 2 {
		t.Fatalf("got %d GPUs", len(s.GPUs))
	}
	g := s.GPUs[0]
	if g.UUID != "GPU-11111111-2222-3333-4444-555555555555" || g.Index != 0 || *g.Name != "Fake GPU A" {
		t.Fatalf("identity fields: %+v", g)
	}
	if g.FanPct != nil || g.TemperatureC != nil {
		t.Fatalf("fan and temperature should be null, got %v %v", g.FanPct, g.TemperatureC)
	}
	if *g.VRAMTotalMiB != 10240 || *g.VRAMUsedMiB != 4500 || *g.UtilizationPct != 77 || *g.PowerDrawW != 150.2 && *g.PowerDrawW != 150.3 || *g.PowerLimitW != 320 {
		t.Fatalf("other fields must survive a failing field: %+v", g)
	}
	// A real error is reported; NOT_SUPPORTED is not an error.
	joined := strings.Join(s.Errors, "\n")
	if !strings.Contains(joined, "GPU 0: reading temperature failed: GPU is lost") {
		t.Fatalf("missing temperature error in %v", s.Errors)
	}
	if strings.Contains(joined, "fan") {
		t.Fatalf("NOT_SUPPORTED fan speed must not be an error: %v", s.Errors)
	}
	b, _ := json.Marshal(g)
	if !strings.Contains(string(b), `"fan_pct":null`) || !strings.Contains(string(b), `"temperature_c":null`) {
		t.Fatalf("JSON must carry explicit nulls: %s", b)
	}
}

func TestREQ_AGT_003_GPUProcesses(t *testing.T) {
	h := newHarness(t)
	s := h.sample()
	procs := map[int]payload.GPUProcess{}
	for _, g := range s.GPUs {
		for _, p := range g.Processes {
			procs[p.PID] = p
		}
	}
	want := map[int][4]string{
		400: {"compute", "python3.12", "alice", "loginuid"},
		700: {"compute", "python3", "bob", "uid"},
		402: {"graphics", "cron", "system", "system"},
		200: {"compute", "python3.11", "carol", "docker_label"},
		501: {"compute", "qemu-system-x86_64", "frank", "multipass_name"},
	}
	for pid, w := range want {
		p, ok := procs[pid]
		if !ok {
			t.Fatalf("pid %d missing", pid)
		}
		if p.Type != w[0] || p.Exe != w[1] || p.Owner != w[2] || p.Attribution != w[3] {
			t.Errorf("pid %d: got %+v, want %v", pid, p, w)
		}
	}
	if procs[700].VRAMMiB != nil || *procs[400].VRAMMiB != 1000 {
		t.Error("VRAM must be null when NVML does not report it")
	}
	if procs[200].ContainerName != "carol-train" || procs[200].ContainerImage != "pytorch/pytorch:2.4" {
		t.Errorf("container context missing: %+v", procs[200])
	}
	if procs[501].VMName != "frank-dev" {
		t.Errorf("vm name missing: %+v", procs[501])
	}
}

func TestREQ_AGT_004_NoFabricatedUtilization(t *testing.T) {
	h := newHarness(t)
	s := h.sample()
	// GPU 0 is shared by alice and bob and NVML has no per-process data.
	for _, p := range s.GPUs[0].Processes {
		if p.UtilizationPct != nil {
			t.Fatalf("per-process utilization invented for pid %d", p.PID)
		}
	}
	b, _ := json.Marshal(s.GPUs[0].Processes)
	if strings.Contains(string(b), "utilization") {
		t.Fatalf("processes JSON must not contain utilization: %s", b)
	}
	if *s.GPUs[0].UtilizationPct != 77 {
		t.Fatal("device utilization must be reported for the device")
	}
	// GPU 1 has real per-process samples for pid 200 only.
	for _, p := range s.GPUs[1].Processes {
		switch p.PID {
		case 200:
			if p.UtilizationPct == nil || *p.UtilizationPct != 50 {
				t.Fatalf("pid 200 utilization = %v, want 50", p.UtilizationPct)
			}
		default:
			if p.UtilizationPct != nil {
				t.Fatalf("pid %d has invented utilization", p.PID)
			}
		}
	}
	for _, u := range s.Users {
		b, _ := json.Marshal(u)
		if strings.Contains(string(b), "utilization") {
			t.Fatalf("user entries must not carry GPU utilization: %s", b)
		}
	}
}

func TestREQ_AGT_005_HostCPUAndMemory(t *testing.T) {
	h := newHarness(t)
	h.col.Prime() // /proc/stat total=1000 idle=500
	fixture.WriteHost(h.fs, 2000, 700, 8)
	h.clk.Advance(10 * time.Second)
	s := h.col.Sample()
	if s.Host.CPUPercent == nil || *s.Host.CPUPercent != 80 {
		t.Fatalf("cpu_percent = %v, want 80", s.Host.CPUPercent)
	}
	if s.Host.LogicalCores != 8 {
		t.Fatalf("logical_cores = %d", s.Host.LogicalCores)
	}
	if s.Host.LoadAvg != [3]float64{1.5, 1.25, 1.0} {
		t.Fatalf("load = %v", s.Host.LoadAvg)
	}
	kb := uint64(1024)
	if s.Host.MemTotalBytes != 16384000*kb || s.Host.MemAvailableBytes != 8192000*kb || s.Host.MemUsedBytes != (16384000-8192000)*kb {
		t.Fatalf("memory = %+v", s.Host)
	}
	if s.Host.SwapTotalBytes != 2048000*kb || s.Host.SwapUsedBytes != 1024000*kb {
		t.Fatalf("swap = %+v", s.Host)
	}
}

func TestREQ_AGT_006_PerUserCPUAndMemory(t *testing.T) {
	h := newHarness(t)
	p := fixture.Proc{PID: 701, PPID: 1, Comm: "python3", Exe: "/usr/bin/python3", UID: 1001, LoginUID: "1001", UTime: 100, Start: 100, RSSPages: 1000}
	fixture.WriteProc(h.fs, p)
	h.col.Prime()
	h.clk.Advance(10 * time.Second)
	p.UTime, p.STime = 2100, 500 // 2500 ticks in 10 s = 2.5 cores
	fixture.WriteProc(h.fs, p)
	s := h.col.Sample()
	alice := findUser(s, "alice")
	if alice == nil {
		t.Fatal("alice missing")
	}
	if alice.CPUCores != 2.5 {
		t.Fatalf("alice cpu_cores = %v, want 2.5", alice.CPUCores)
	}
	// alice owns pid 701 (uid) and pid 400 (sudo); pid 503 is her VM.
	if alice.ProcessCount != 3 || alice.RSSBytes != 1000*4096 {
		t.Fatalf("alice = %+v", alice)
	}
	if strings.Join(alice.Attributions, ",") != "loginuid,multipass_name,uid" {
		t.Fatalf("attributions = %v", alice.Attributions)
	}
}

func TestREQ_AGT_007_TopProcesses(t *testing.T) {
	h := newHarness(t)
	h.cfg.TopProcessesPerOwner = 2
	h.build()
	for i := 0; i < 5; i++ {
		fixture.WriteProc(h.fs, fixture.Proc{PID: 800 + i, PPID: 1, Comm: "worker", Exe: "/usr/bin/worker", UID: 1006, LoginUID: "1006",
			RSSPages: uint64(100 * (i + 1)), Argv: []string{"worker", "--token=SECRET123"}})
	}
	s := h.sample()
	dave := findUser(s, "dave")
	// Five workers plus the unlabeled Docker container (docker_default_owner).
	if dave == nil || dave.ProcessCount != 6 {
		t.Fatalf("dave = %+v", dave)
	}
	if len(dave.TopCPU) != 2 || len(dave.TopRSS) != 2 {
		t.Fatalf("top lists must be limited to 2: %+v", dave)
	}
	if dave.TopRSS[0].PID != 804 || dave.TopRSS[1].PID != 803 || dave.TopRSS[0].RSSBytes != 500*4096 || dave.TopRSS[0].Exe != "worker" {
		t.Fatalf("top_rss = %+v", dave.TopRSS)
	}
	b, _ := json.Marshal(s)
	for _, bad := range []string{"SECRET123", "--token", "cmdline", "environ"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("payload contains %q", bad)
		}
	}
	// Default is 5.
	if config.Default().TopProcessesPerOwner != 5 {
		t.Fatal("default top_processes_per_owner must be 5")
	}
}

func TestREQ_AGT_008_InventoryFields(t *testing.T) {
	h := newHarness(t)
	inv := h.col.Inventory()
	in := inv.Inventory
	if inv.Type != "inventory" || inv.SchemaVersion != 1 {
		t.Fatalf("header = %+v", inv.Header)
	}
	if in.Hostname != "fixture-host" || in.OSRelease != "Ubuntu 24.04.1 LTS" || in.Kernel != "6.8.0-45-generic" ||
		in.CPUModel != "Fixture CPU @ 3.00GHz" || in.LogicalCores != 8 || in.MemTotalBytes != 16384000*1024 {
		t.Fatalf("host fields = %+v", in)
	}
	if *in.DriverVersion != "570.86.15" || *in.CUDADriverVersion != "12.8" {
		t.Fatalf("driver fields = %v %v", *in.DriverVersion, *in.CUDADriverVersion)
	}
	if len(in.GPUs) != 2 || in.GPUs[1].UUID != "GPU-66666666-7777-8888-9999-000000000000" || *in.GPUs[1].VRAMTotalMiB != 24576 || *in.GPUs[1].Name != "Fake GPU B" {
		t.Fatalf("gpus = %+v", in.GPUs)
	}
	if len(in.Filesystems) != 2 || in.Filesystems[1].Mount != "/home" || in.Filesystems[1].SizeBytes != 5000 {
		t.Fatalf("filesystems = %+v", in.Filesystems)
	}
}

// Verifies: REQ-AGT-009
func TestStatfsTotalsInEverySample(t *testing.T) {
	h := newHarness(t)
	h.cfg.Disk.StatfsMounts = []string{"/", "/home", "/missing"}
	h.build()
	for i := 0; i < 2; i++ {
		s := h.sample()
		if len(s.Filesystems) != 2 || s.Filesystems[1] != (payload.Filesystem{Mount: "/home", SizeBytes: 5000, UsedBytes: 3000, FreeBytes: 1900}) {
			t.Fatalf("filesystems = %+v", s.Filesystems)
		}
		if !strings.Contains(strings.Join(s.Errors, " "), "disk: statfs /missing failed") {
			t.Fatalf("missing statfs error: %v", s.Errors)
		}
	}
}

func TestREQ_AGT_010_GracefulDegradation(t *testing.T) {
	h := newHarness(t)
	h.lib.InitErr = gpu.ErrLibraryNotFound
	h.dc.Err = errors.New("dial unix /var/run/docker.sock: connect: connection refused")
	h.fs.RemoveAll("/etc/subuid")
	h.fs.RemoveAll("/proc/meminfo")
	h.build()
	var s *payload.Sample
	h.col.Prime()
	for i := 0; i < 3; i++ {
		h.clk.Advance(30 * time.Second)
		fixture.WriteHost(h.fs, uint64(2000+1000*i), uint64(700+500*i), 8)
		h.fs.RemoveAll("/proc/meminfo")
		s = h.col.Sample()
	}
	if len(s.GPUs) != 0 {
		t.Fatal("no GPUs expected without NVML")
	}
	if findUser(s, "alice") == nil || findUser(s, "dave") == nil || s.Host.CPUPercent == nil || s.Host.LoadAvg[0] != 1.5 {
		t.Fatalf("everything else must still be reported: %+v", s)
	}
	if len(s.Filesystems) != 2 {
		t.Fatal("statfs must still be reported")
	}
	errs := strings.Join(s.Errors, "\n")
	for _, want := range []string{"nvml: NVML library not found", "subuid: cannot read /etc/subuid", "host: cannot read /proc/meminfo", "docker: cannot inspect container"} {
		if !strings.Contains(errs, want) {
			t.Errorf("errors[] lacks %q: %v", want, s.Errors)
		}
	}
	// Each distinct error is logged once per hour.
	countLog := func(sub string) int { return strings.Count(h.log.String(), sub) }
	if n := countLog("NVML library not found"); n != 1 {
		t.Fatalf("NVML error logged %d times in 90 s, want 1", n)
	}
	h.clk.Advance(61 * time.Minute)
	h.col.Sample()
	if n := countLog("NVML library not found"); n != 2 {
		t.Fatalf("NVML error logged %d times after an hour, want 2", n)
	}
	if !strings.Contains(h.log.String(), "suppressed=") {
		t.Fatal("the repeated line should report how many were suppressed")
	}
}

// Verifies: REQ-ATT-007
func TestDockerFailureWarningIsRateLimited(t *testing.T) {
	h := newHarness(t)
	h.dc.Err = errors.New("connection refused")
	h.build()
	h.col.Prime()
	for i := 0; i < 5; i++ {
		h.clk.Advance(30 * time.Second)
		s := h.col.Sample()
		for _, g := range s.GPUs[1].Processes {
			if g.PID == 200 && (g.Owner != "dave" || g.Attribution != "docker_default_owner") {
				t.Fatalf("fallback owner wrong: %+v", g)
			}
		}
	}
	if n := strings.Count(h.log.String(), "level=warn msg=\"collection problem\" error=\"docker: cannot inspect container 4f2a9c3e5b7d"); n != 1 {
		t.Fatalf("docker warning logged %d times, want 1:\n%s", n, h.log.String())
	}
}

func TestREQ_ATT_004_KernelThreadsExcluded(t *testing.T) {
	h := newHarness(t)
	d := h.col.Collect(false)
	for _, pid := range []int{2, 3, 50} {
		if _, ok := d.Results[pid]; ok {
			t.Errorf("kernel thread %d was attributed", pid)
		}
	}
	total := 0
	for _, u := range d.Sample.Users {
		total += u.ProcessCount
	}
	if want := len(d.Procs) - 3; total != want {
		t.Fatalf("users count %d processes, want %d", total, want)
	}
}

func TestREQ_SEC_007_NoSecretInPayloadOrLogs(t *testing.T) {
	h := newHarness(t)
	h.dc.Err = errors.New("connection refused")
	h.build()
	s := h.sample()
	inv := h.col.Inventory()
	for _, v := range []any{s, inv} {
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), "SECRET123") || strings.Contains(string(b), "train.py") {
			t.Fatalf("argv leaked into the payload: %s", b)
		}
	}
	if strings.Contains(h.log.String(), "SECRET123") {
		t.Fatal("argv leaked into the logs")
	}
	// The fixture really contains the secret, so the test is meaningful.
	if b, _ := h.fs.ReadFile("/proc/700/cmdline"); !bytes.Contains(b, []byte("SECRET123")) {
		t.Fatal("fixture lost its secret argv")
	}
}

// buildLargeFS creates a fake procfs with n processes for the benchmark.
func buildLargeFS(n int) *hostfs.MapFS {
	fs := fixture.LoadSet("procs.yaml").Build()
	for i := 0; i < n; i++ {
		p := fixture.Proc{PID: 10000 + i, PPID: 1, Comm: "python3", Exe: "/usr/bin/python3",
			UID: uint32(1001 + i%6), LoginUID: fmt.Sprint(1001 + i%6), UTime: uint64(i * 3), STime: uint64(i), RSSPages: uint64(1000 + i)}
		switch i % 10 {
		case 0:
			p.UID, p.LoginUID = 0, "1001"
		case 1:
			p.UID, p.LoginUID = 0, "unset"
		case 2:
			p.UID = 232071
		}
		fixture.WriteProc(fs, p)
	}
	return fs
}

func TestREQ_AGT_012_Collect2000ProcessesUnder200ms(t *testing.T) {
	fs := buildLargeFS(2000)
	clk := clock.NewFake(t0)
	col := collect.New(config.Default(), collect.Deps{
		FS: fs, GPU: &gpu.FakeLibrary{Devs: fixtureDevices()}, Docker: &docker.Fake{}, Clock: clk,
		Statfs: func(m string) (payload.Filesystem, error) { return payload.Filesystem{Mount: m}, nil },
		Log:    logx.New(&bytes.Buffer{}, logx.Error, clk),
	}, "bench", "r", "t")
	col.Prime()
	res := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			clk.Advance(30 * time.Second)
			s := col.Sample()
			if len(s.Users) == 0 {
				b.Fatal("no users")
			}
		}
	})
	ms := float64(res.NsPerOp()) / 1e6
	fmt.Printf("METRIC collect_2000_processes_ms=%.2f\n", ms)
	if ms >= 200 {
		t.Fatalf("collecting one sample of 2000 processes took %.1f ms, budget is 200 ms", ms)
	}
}

func BenchmarkCollect2000(b *testing.B) {
	fs := buildLargeFS(2000)
	clk := clock.NewFake(t0)
	col := collect.New(config.Default(), collect.Deps{FS: fs, GPU: gpu.Absent{}, Clock: clk, Log: logx.New(&bytes.Buffer{}, logx.Error, clk)}, "bench", "r", "t")
	col.Prime()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clk.Advance(30 * time.Second)
		col.Sample()
	}
}
