package diagnose_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/collect"
	"gpumon/agent/internal/config"
	"gpumon/agent/internal/diagnose"
	"gpumon/agent/internal/docker"
	"gpumon/agent/internal/fixture"
	"gpumon/agent/internal/gpu"
	"gpumon/agent/internal/logx"
)

func TestREQ_AGT_013_DiagnoseSections(t *testing.T) {
	fs := fixture.LoadSet("procs.yaml").Build()
	clk := clock.NewFake(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	v := uint64(2000)
	lib := &gpu.FakeLibrary{Driver: "570.86.15", CUDA: 12080, Devs: []*gpu.FakeDevice{{
		UUIDVal: "GPU-abc", NameVal: "Fake GPU", TotalMiB: 10240, UsedMiB: 2500, Util: 90,
		Compute: []gpu.ProcInfo{{PID: 200, UsedMiB: &v}}, Errs: map[string]error{"FanSpeed": gpu.ErrNotSupported},
	}}}
	dc := &docker.Fake{Containers: map[string]*docker.Container{
		"4f2a9c3e5b7d1f0a8c6e4b2d9f7a5c3e1b0d8f6a4c2e0b9d7f5a3c1e8b6d4f2a": {Name: "carol-train", Image: "pytorch", Labels: map[string]string{"owner": "carol"}},
	}}
	col := collect.New(config.Default(), collect.Deps{FS: fs, GPU: lib, Docker: dc, Clock: clk,
		Log: logx.New(&bytes.Buffer{}, logx.Error, clk)}, "srv-x", "diagnose", "test")
	col.Prime()
	// pid 400 (sudo, uid 0) uses 0.5 cores between the two readings.
	fixture.WriteProc(fs, fixture.Proc{PID: 400, PPID: 1, Comm: "python3", Exe: "/usr/bin/python3.12", UID: 0, LoginUID: "1001", UTime: 500})
	clk.Advance(10 * time.Second)
	d := col.Collect(true)
	var out bytes.Buffer
	diagnose.Print(&out, d, col.Inventory())
	text := out.String()
	for _, want := range []string{
		diagnose.HeadHost, diagnose.HeadFS, diagnose.HeadGPUs, diagnose.HeadRoot, diagnose.HeadQEMU, diagnose.HeadOwners, diagnose.HeadProblems,
		"NVIDIA driver:  570.86.15", "CUDA driver:    12.8",
		// GPU process evidence: cgroup path, container id and labels.
		"PID 200 python3.11 [compute] VRAM 2000 MiB", "owner carol (method docker_label)",
		"container_id=4f2a9c3e5b7d1f0a8c6e4b2d9f7a5c3e1b0d8f6a4c2e0b9d7f5a3c1e8b6d4f2a", "container labels: owner=carol",
		"rule 1 (rootful Docker): container 4f2a9c3e5b7d has label owner=carol",
		// Root process above 1 % CPU with uid and loginuid evidence.
		"PID 400 python3.12 using 0.50 cores -> owner alice (method loginuid)", "uid=0 (root) loginuid=1001",
		"rule 4 (sudo/su): uid 0 with loginuid 1001 (alice)",
		// QEMU raw argv for manual multipass checks.
		"PID 501 qemu-system-x86_64 -> owner frank (method multipass_name), vm_name \"frank-dev\"",
		"raw argv: /snap/multipass/19230/usr/bin/qemu-system-x86_64 -bios OVMF.fd",
		"parent chain: multipassd <- systemd",
		"fan n/a (not supported or failed)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnose output lacks %q", want)
		}
	}
	for _, r := range text {
		if r > 127 {
			t.Fatal("diagnose output must be ASCII")
		}
	}
	// The argv shown locally never reaches the payload.
	b, _ := json.Marshal(d.Sample)
	if strings.Contains(string(b), "OVMF.fd") || strings.Contains(string(b), "rule 1") {
		t.Fatal("diagnose details leaked into the payload")
	}
	if testing.Verbose() {
		t.Log("\n" + text)
	}
}
