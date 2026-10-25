// Package diagnose prints everything the agent detects to the local
// terminal. This is the only place where a full argv may appear; the output
// is never sent to central.
package diagnose

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"gpumon/agent/internal/attrib"
	"gpumon/agent/internal/collect"
	"gpumon/agent/internal/payload"
)

// Section headings, also checked by tests.
const (
	HeadHost     = "== Host =="
	HeadFS       = "== Filesystems =="
	HeadGPUs     = "== GPUs and GPU processes =="
	HeadRoot     = "== Root processes using more than 1% CPU =="
	HeadQEMU     = "== QEMU processes (multipass detection) =="
	HeadOwners   = "== Owners =="
	HeadProblems = "== Problems =="
)

func opt[T any](p *T, unit string) string {
	if p == nil {
		return "n/a (not supported or failed)"
	}
	return fmt.Sprintf("%v%s", *p, unit)
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// Print writes the report.
func Print(w io.Writer, d *collect.Detail, inv *payload.InventoryMsg) {
	s := d.Sample
	fmt.Fprintln(w, "GPUMon agent diagnose")
	fmt.Fprintln(w, "This report is for local inspection only and is never sent to central.")
	fmt.Fprintln(w)

	fmt.Fprintln(w, HeadHost)
	in := inv.Inventory
	fmt.Fprintf(w, "Server ID:      %s\n", s.ServerID)
	fmt.Fprintf(w, "Hostname:       %s\n", in.Hostname)
	fmt.Fprintf(w, "OS release:     %s\n", in.OSRelease)
	fmt.Fprintf(w, "Kernel:         %s\n", in.Kernel)
	fmt.Fprintf(w, "CPU model:      %s (%d logical cores)\n", in.CPUModel, in.LogicalCores)
	fmt.Fprintf(w, "Memory:         %s used of %s (available %s), swap %s used of %s\n",
		gib(s.Host.MemUsedBytes), gib(s.Host.MemTotalBytes), gib(s.Host.MemAvailableBytes),
		gib(s.Host.SwapUsedBytes), gib(s.Host.SwapTotalBytes))
	fmt.Fprintf(w, "CPU usage:      %s, load %.2f %.2f %.2f\n", opt(s.Host.CPUPercent, " %"), s.Host.LoadAvg[0], s.Host.LoadAvg[1], s.Host.LoadAvg[2])
	fmt.Fprintf(w, "NVIDIA driver:  %s\n", opt(in.DriverVersion, ""))
	fmt.Fprintf(w, "CUDA driver:    %s\n", opt(in.CUDADriverVersion, ""))
	fmt.Fprintln(w)

	fmt.Fprintln(w, HeadFS)
	if len(s.Filesystems) == 0 {
		fmt.Fprintln(w, "(none)")
	}
	for _, f := range s.Filesystems {
		fmt.Fprintf(w, "%-12s size %s, used %s, free %s\n", f.Mount, gib(f.SizeBytes), gib(f.UsedBytes), gib(f.FreeBytes))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, HeadGPUs)
	if len(s.GPUs) == 0 {
		fmt.Fprintln(w, "(no GPUs detected)")
	}
	for _, g := range s.GPUs {
		fmt.Fprintf(w, "GPU %d  %s  %s\n", g.Index, g.UUID, opt(g.Name, ""))
		fmt.Fprintf(w, "  VRAM used %s of %s, utilization %s, temperature %s, power %s of %s, fan %s\n",
			opt(g.VRAMUsedMiB, " MiB"), opt(g.VRAMTotalMiB, " MiB"), opt(g.UtilizationPct, " %"),
			opt(g.TemperatureC, " C"), opt(g.PowerDrawW, " W"), opt(g.PowerLimitW, " W"), opt(g.FanPct, " %"))
		if len(g.Processes) == 0 {
			fmt.Fprintln(w, "  (no processes)")
		}
		for _, p := range g.Processes {
			fmt.Fprintf(w, "  PID %d %s [%s] VRAM %s, per-process utilization %s -> owner %s (method %s)\n",
				p.PID, p.Exe, p.Type, opt(p.VRAMMiB, " MiB"), opt(p.UtilizationPct, " %"), p.Owner, p.Attribution)
			printEvidence(w, d, p.PID, "    ")
		}
	}
	fmt.Fprintln(w)

	pids := make([]int, 0, len(d.Results))
	for pid := range d.Results {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	fmt.Fprintln(w, HeadRoot)
	n := 0
	for _, pid := range pids {
		p := d.Procs[pid]
		if p.UID == 0 && d.CPUCores[pid] > 0.01 {
			r := d.Results[pid]
			fmt.Fprintf(w, "PID %d %s using %.2f cores -> owner %s (method %s)\n", pid, p.Exe, d.CPUCores[pid], r.Owner, r.Method)
			printEvidence(w, d, pid, "  ")
			n++
		}
	}
	if n == 0 {
		fmt.Fprintln(w, "(none)")
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, HeadQEMU)
	n = 0
	for _, pid := range pids {
		p := d.Procs[pid]
		if !attrib.IsQEMU(p) {
			continue
		}
		r := d.Results[pid]
		fmt.Fprintf(w, "PID %d %s -> owner %s (method %s), vm_name %q\n", pid, p.Exe, r.Owner, r.Method, r.Ctx.VMName)
		printEvidence(w, d, pid, "  ")
		argv, err := d.Cmdline(pid)
		if err != nil {
			fmt.Fprintf(w, "  raw argv: (unreadable: %v)\n", err)
		} else {
			fmt.Fprintf(w, "  raw argv: %s\n", strings.Join(argv, " "))
		}
		n++
	}
	if n == 0 {
		fmt.Fprintln(w, "(none)")
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, HeadOwners)
	for _, u := range s.Users {
		fmt.Fprintf(w, "%-16s %7.2f cores  RSS %-10s %4d processes  methods %s\n",
			u.Owner, u.CPUCores, gib(u.RSSBytes), u.ProcessCount, strings.Join(u.Attributions, ","))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, HeadProblems)
	all := append(append([]string{}, s.Errors...), inv.Errors...)
	if len(all) == 0 {
		fmt.Fprintln(w, "(none)")
	}
	for _, e := range all {
		fmt.Fprintf(w, "- %s\n", e)
	}
}

func printEvidence(w io.Writer, d *collect.Detail, pid int, indent string) {
	r, ok := d.Results[pid]
	if !ok || r.Evidence == nil {
		fmt.Fprintf(w, "%sevidence: (process not readable)\n", indent)
		return
	}
	ev := r.Evidence
	fmt.Fprintf(w, "%sattribution chain:\n", indent)
	for _, step := range ev.Steps {
		fmt.Fprintf(w, "%s  - %s\n", indent, step)
	}
	cg := ev.CgroupLine
	if cg == "" {
		if p, ok := d.Procs[pid]; ok {
			cg = strings.TrimSpace(strings.SplitN(p.Cgroup, "\n", 2)[0])
		}
	}
	cid := ev.ContainerID
	if cid == "" {
		cid = "none"
	}
	fmt.Fprintf(w, "%sevidence: uid=%d (%s) loginuid=%s cgroup=%q container_id=%s rootless=%v\n",
		indent, ev.UID, ev.Username, ev.LoginUID, cg, cid, ev.Rootless)
	if ev.ContainerID != "" {
		keys := make([]string, 0, len(ev.ContainerLabels))
		for k := range ev.ContainerLabels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+"="+ev.ContainerLabels[k])
		}
		fmt.Fprintf(w, "%scontainer labels: %s\n", indent, strings.Join(parts, ", "))
		if ev.DockerError != "" {
			fmt.Fprintf(w, "%sdocker error: %s\n", indent, ev.DockerError)
		}
	}
	sub := ev.SubuidMatch
	if sub == "" {
		sub = "not evaluated (an earlier rule matched)"
	}
	fmt.Fprintf(w, "%ssubuid match: %s\n", indent, sub)
	if len(ev.ParentChain) > 0 {
		fmt.Fprintf(w, "%sparent chain: %s\n", indent, strings.Join(ev.ParentChain, " <- "))
	}
}
