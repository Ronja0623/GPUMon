// Package collect assembles samples and inventory messages from the host
// sources (procfs, NVML, Docker, statfs). Every source is an interface so
// that tests and the simulator can use fakes. A failing source adds an
// English message to errors[] and never stops the rest of the collection.
package collect

import (
	"context"
	"math"
	"sort"
	"time"

	"gpumon/agent/internal/accounts"
	"gpumon/agent/internal/attrib"
	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/config"
	"gpumon/agent/internal/disk"
	"gpumon/agent/internal/docker"
	"gpumon/agent/internal/gpu"
	"gpumon/agent/internal/hostfs"
	"gpumon/agent/internal/logx"
	"gpumon/agent/internal/payload"
	"gpumon/agent/internal/procfs"
)

// Deps are the data sources of a collector.
type Deps struct {
	FS     hostfs.FS
	GPU    gpu.Library
	Docker docker.Client // nil when Docker is disabled
	Statfs disk.StatfsFunc
	Clock  clock.Clock
	Log    *logx.Logger
}

// Collector produces samples. It is not safe for concurrent use.
type Collector struct {
	Deps
	Cfg      config.Config
	ServerID string
	RunID    string
	Version  string
	PageSize int
	ClkTck   float64

	seq        uint64
	gpuReady   bool
	prevCPU    *procfs.CPUTimes
	prevTicks  map[procKey]uint64
	prevTime   time.Time
	prevUptime float64
	procUtilTS map[string]uint64
	attribCfg  attrib.Config
}

type procKey struct {
	pid   int
	start uint64
}

// New returns a collector.
func New(cfg config.Config, d Deps, serverID, runID, version string) *Collector {
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	return &Collector{
		Deps:       d,
		Cfg:        cfg,
		ServerID:   serverID,
		RunID:      runID,
		Version:    version,
		PageSize:   4096,
		ClkTck:     100,
		prevTicks:  map[procKey]uint64{},
		procUtilTS: map[string]uint64{},
		attribCfg:  cfg.AttribConfig(),
	}
}

// Detail is everything gathered for one sample. Procs, Results, and
// Cmdline are only used locally (diagnose) and are never sent.
type Detail struct {
	Sample   *payload.Sample
	Procs    map[int]*procfs.Proc
	Results  map[int]attrib.Result
	CPUCores map[int]float64
	Cmdline  func(pid int) ([]string, error)
}

func (c *Collector) header(typ string, now time.Time) payload.Header {
	return payload.Header{
		SchemaVersion: payload.SchemaVersion,
		Type:          typ,
		ServerID:      c.ServerID,
		RunID:         c.RunID,
		Timestamp:     payload.FormatTime(now),
		AgentVersion:  c.Version,
	}
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// Prime takes the baseline CPU readings so that the first sample has
// meaningful CPU deltas.
func (c *Collector) Prime() {
	c.collect(false, false)
}

// Sample collects one sample and increments the sequence number.
func (c *Collector) Sample() *payload.Sample {
	return c.collect(false, true).Sample
}

// Collect collects one sample. With evidence, the attribution evidence is
// recorded for the diagnose command.
func (c *Collector) Collect(withEvidence bool) *Detail {
	return c.collect(withEvidence, true)
}

func (c *Collector) collect(withEvidence, count bool) *Detail {
	now := c.Clock.Now()
	errs := logx.NewErrorSet()
	s := &payload.Sample{
		Header:          c.header(payload.TypeSample, now),
		IntervalSeconds: c.Cfg.IntervalSeconds,
		GPUs:            []payload.GPU{},
		Users:           []payload.User{},
		Filesystems:     []payload.Filesystem{},
	}
	c.collectHost(s, errs)

	acc, aerrs, warnings := accounts.Load(c.FS, c.Cfg.UIDMin)
	for _, e := range aerrs {
		errs.Add("%s", e)
	}
	for _, w := range warnings {
		c.Log.Warn(w)
	}

	procs := map[int]*procfs.Proc{}
	pids, err := procfs.ListPIDs(c.FS)
	if err != nil {
		errs.Add("procfs: cannot list /proc: %v", err)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if p, err := procfs.ReadProc(c.FS, pid, c.PageSize); err == nil {
			procs[pid] = p
		}
	}
	cores := c.cpuDeltas(procs, pids, now)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmdline := func(pid int) ([]string, error) { return procfs.ReadCmdline(c.FS, pid) }
	eng := &attrib.Engine{
		Cfg: c.attribCfg, Acc: acc, Docker: c.Docker, Procs: procs,
		Cmdline: cmdline, Errs: errs, Ctx: ctx, WithEvidence: withEvidence,
	}
	results := map[int]attrib.Result{}
	for _, pid := range pids {
		if p, ok := procs[pid]; ok && !p.IsKernelThread() {
			results[pid] = eng.Attribute(p)
		}
	}
	s.Users = c.aggregateUsers(procs, pids, results, cores)
	s.GPUs = c.collectGPUs(errs, procs, results, eng)

	for _, m := range c.Cfg.Disk.StatfsMounts {
		if c.Statfs == nil {
			break
		}
		fs, err := c.Statfs(m)
		if err != nil {
			errs.Add("disk: statfs %s failed: %v", m, err)
			continue
		}
		fs.Mount = m
		s.Filesystems = append(s.Filesystems, fs)
	}

	s.Errors = errs.List()
	if count {
		for _, e := range s.Errors {
			c.Log.Warn("collection problem", "error", e)
		}
		c.seq++
	}
	s.Seq = c.seq
	return &Detail{Sample: s, Procs: procs, Results: results, CPUCores: cores, Cmdline: cmdline}
}

func (c *Collector) collectHost(s *payload.Sample, errs *logx.ErrorSet) {
	if ct, err := procfs.ReadCPUTimes(c.FS); err != nil {
		errs.Add("host: cannot read /proc/stat: %v", err)
	} else {
		s.Host.LogicalCores = ct.Cores
		if c.prevCPU != nil {
			if pct, ok := procfs.CPUPercent(*c.prevCPU, ct); ok {
				v := round(pct, 1)
				s.Host.CPUPercent = &v
			}
		}
		c.prevCPU = &ct
	}
	if m, err := procfs.ReadMemInfo(c.FS); err != nil {
		errs.Add("host: cannot read /proc/meminfo: %v", err)
	} else {
		s.Host.MemTotalBytes = m.Total
		s.Host.MemAvailableBytes = m.Available
		if m.Total > m.Available {
			s.Host.MemUsedBytes = m.Total - m.Available
		}
		s.Host.SwapTotalBytes = m.SwapTotal
		if m.SwapTotal > m.SwapFree {
			s.Host.SwapUsedBytes = m.SwapTotal - m.SwapFree
		}
	}
	if l, err := procfs.ReadLoadAvg(c.FS); err != nil {
		errs.Add("host: cannot read /proc/loadavg: %v", err)
	} else {
		s.Host.LoadAvg = l
	}
}

// cpuDeltas returns the CPU usage of each process in cores, computed from
// utime+stime deltas between samples.
func (c *Collector) cpuDeltas(procs map[int]*procfs.Proc, pids []int, now time.Time) map[int]float64 {
	uptime, _ := procfs.ReadUptime(c.FS)
	elapsed := 0.0
	if !c.prevTime.IsZero() {
		elapsed = now.Sub(c.prevTime).Seconds()
	}
	cores := map[int]float64{}
	next := make(map[procKey]uint64, len(procs))
	for _, pid := range pids {
		p, ok := procs[pid]
		if !ok || p.IsKernelThread() {
			continue
		}
		key := procKey{pid, p.StartTime}
		total := p.UTime + p.STime
		next[key] = total
		if elapsed <= 0 {
			continue
		}
		var delta uint64
		if prev, ok := c.prevTicks[key]; ok {
			if total > prev {
				delta = total - prev
			}
		} else if c.prevUptime > 0 && float64(p.StartTime)/c.ClkTck >= c.prevUptime {
			delta = total // started after the previous sample
		}
		cores[pid] = float64(delta) / c.ClkTck / elapsed
	}
	c.prevTicks, c.prevTime, c.prevUptime = next, now, uptime
	return cores
}

type ownerAgg struct {
	cores   float64
	rss     uint64
	n       int
	methods map[attrib.Method]struct{}
	procs   []payload.TopProcess
}

func (c *Collector) aggregateUsers(procs map[int]*procfs.Proc, pids []int, results map[int]attrib.Result, cores map[int]float64) []payload.User {
	aggs := map[string]*ownerAgg{}
	for _, pid := range pids {
		r, ok := results[pid]
		if !ok {
			continue
		}
		p := procs[pid]
		a := aggs[r.Owner]
		if a == nil {
			a = &ownerAgg{methods: map[attrib.Method]struct{}{}}
			aggs[r.Owner] = a
		}
		a.cores += cores[pid]
		a.rss += p.RSSBytes
		a.n++
		a.methods[r.Method] = struct{}{}
		a.procs = append(a.procs, payload.TopProcess{
			PID: pid, Exe: p.Exe, CPUCores: round(cores[pid], 3), RSSBytes: p.RSSBytes,
			ContainerName: r.Ctx.ContainerName, VMName: r.Ctx.VMName,
		})
	}
	owners := make([]string, 0, len(aggs))
	for o := range aggs {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	n := c.Cfg.TopProcessesPerOwner
	out := make([]payload.User, 0, len(owners))
	for _, o := range owners {
		a := aggs[o]
		byCPU := append([]payload.TopProcess(nil), a.procs...)
		sort.SliceStable(byCPU, func(i, j int) bool {
			if byCPU[i].CPUCores != byCPU[j].CPUCores {
				return byCPU[i].CPUCores > byCPU[j].CPUCores
			}
			return byCPU[i].RSSBytes > byCPU[j].RSSBytes
		})
		byRSS := append([]payload.TopProcess(nil), a.procs...)
		sort.SliceStable(byRSS, func(i, j int) bool { return byRSS[i].RSSBytes > byRSS[j].RSSBytes })
		out = append(out, payload.User{
			Owner:        o,
			Attributions: attrib.SortedMethods(a.methods),
			CPUCores:     round(a.cores, 3),
			RSSBytes:     a.rss,
			ProcessCount: a.n,
			TopCPU:       byCPU[:min(n, len(byCPU))],
			TopRSS:       byRSS[:min(n, len(byRSS))],
		})
	}
	return out
}

func (c *Collector) ensureGPU(errs *logx.ErrorSet) bool {
	if c.gpuReady {
		return true
	}
	if err := c.GPU.Init(); err != nil {
		errs.Add("nvml: %v; GPU metrics are unavailable", err)
		return false
	}
	c.gpuReady = true
	return true
}

func (c *Collector) collectGPUs(errs *logx.ErrorSet, procs map[int]*procfs.Proc, results map[int]attrib.Result, eng *attrib.Engine) []payload.GPU {
	out := []payload.GPU{}
	if c.GPU == nil || !c.ensureGPU(errs) {
		return out
	}
	devs, err := c.GPU.Devices()
	if err != nil {
		errs.Add("nvml: cannot list GPUs: %v", err)
	}
	for i, d := range devs {
		uuid, err := d.UUID()
		if err != nil {
			errs.Add("nvml: GPU %d: cannot read the UUID, skipping the device: %v", i, err)
			continue
		}
		ok := func(field string, err error) bool {
			if err == nil {
				return true
			}
			if !gpu.IsNotSupported(err) {
				errs.Add("nvml: GPU %d: reading %s failed: %v", i, field, err)
			}
			return false
		}
		g := payload.GPU{Index: i, UUID: uuid, Processes: []payload.GPUProcess{}}
		if v, err := d.Name(); ok("name", err) {
			g.Name = &v
		}
		if tot, used, err := d.Memory(); ok("memory", err) {
			g.VRAMTotalMiB, g.VRAMUsedMiB = &tot, &used
		}
		if v, err := d.Utilization(); ok("utilization", err) {
			g.UtilizationPct = &v
		}
		if v, err := d.Temperature(); ok("temperature", err) {
			g.TemperatureC = &v
		}
		if v, err := d.PowerUsageMilliW(); ok("power draw", err) {
			w := round(float64(v)/1000, 1)
			g.PowerDrawW = &w
		}
		if v, err := d.PowerLimitMilliW(); ok("power limit", err) {
			w := round(float64(v)/1000, 1)
			g.PowerLimitW = &w
		}
		if v, err := d.FanSpeed(); ok("fan speed", err) {
			g.FanPct = &v
		}
		g.Processes = c.gpuProcesses(d, uuid, ok, procs, results, eng)
		out = append(out, g)
	}
	return out
}

func (c *Collector) gpuProcesses(d gpu.Device, uuid string, ok func(string, error) bool, procs map[int]*procfs.Proc, results map[int]attrib.Result, eng *attrib.Engine) []payload.GPUProcess {
	type entry struct {
		pid  uint32
		typ  string
		vram *uint64
	}
	var list []*entry
	byPID := map[uint32]*entry{}
	add := func(ps []gpu.ProcInfo, typ string) {
		for _, p := range ps {
			if e, dup := byPID[p.PID]; dup {
				if p.UsedMiB != nil && (e.vram == nil || *p.UsedMiB > *e.vram) {
					e.vram = p.UsedMiB
				}
				continue
			}
			e := &entry{pid: p.PID, typ: typ, vram: p.UsedMiB}
			byPID[p.PID] = e
			list = append(list, e)
		}
	}
	if ps, err := d.ComputeProcesses(); ok("compute processes", err) {
		add(ps, "compute")
	}
	if ps, err := d.GraphicsProcesses(); ok("graphics processes", err) {
		add(ps, "graphics")
	}

	// Per-process utilization is only reported when NVML provides it.
	utils := map[uint32]uint32{}
	if samples, err := d.ProcessUtilization(c.procUtilTS[uuid]); ok("per-process utilization", err) {
		sum, n := map[uint32]uint64{}, map[uint32]uint64{}
		for _, s := range samples {
			sum[s.PID] += uint64(s.SMUtil)
			n[s.PID]++
			if s.TimeStamp > c.procUtilTS[uuid] {
				c.procUtilTS[uuid] = s.TimeStamp
			}
		}
		for pid, total := range sum {
			utils[pid] = uint32(total / n[pid])
		}
	}

	out := make([]payload.GPUProcess, 0, len(list))
	for _, e := range list {
		gp := payload.GPUProcess{PID: int(e.pid), Type: e.typ, VRAMMiB: e.vram, Exe: "unknown",
			Owner: attrib.OwnerUnattributed, Attribution: string(attrib.MethodUnattributed)}
		pid := int(e.pid)
		p, known := procs[pid]
		if !known {
			if rp, err := procfs.ReadProc(c.FS, pid, c.PageSize); err == nil && !rp.IsKernelThread() {
				procs[pid], p, known = rp, rp, true
				results[pid] = eng.Attribute(rp)
			}
		}
		if known {
			r := results[pid]
			gp.Exe, gp.Owner, gp.Attribution = p.Exe, r.Owner, string(r.Method)
			gp.ProcessContext = payload.ProcessContext{
				ContainerID: r.Ctx.ContainerID, ContainerName: r.Ctx.ContainerName,
				ContainerImage: r.Ctx.ContainerImage, VMName: r.Ctx.VMName, Account: r.Ctx.Account,
			}
		}
		if u, has := utils[e.pid]; has {
			v := u
			gp.UtilizationPct = &v
		}
		out = append(out, gp)
	}
	return out
}

// Inventory collects the hardware inventory.
func (c *Collector) Inventory() *payload.InventoryMsg {
	now := c.Clock.Now()
	errs := logx.NewErrorSet()
	inv := payload.Inventory{
		Hostname:    procfs.ReadTrimmed(c.FS, "/proc/sys/kernel/hostname"),
		OSRelease:   procfs.ReadOSRelease(c.FS),
		Kernel:      procfs.ReadTrimmed(c.FS, "/proc/sys/kernel/osrelease"),
		CPUModel:    procfs.ReadCPUModel(c.FS),
		GPUs:        []payload.InventoryGPU{},
		Filesystems: []payload.InventoryFS{},
	}
	if ct, err := procfs.ReadCPUTimes(c.FS); err == nil {
		inv.LogicalCores = ct.Cores
	} else {
		errs.Add("host: cannot read /proc/stat: %v", err)
	}
	if m, err := procfs.ReadMemInfo(c.FS); err == nil {
		inv.MemTotalBytes = m.Total
	} else {
		errs.Add("host: cannot read /proc/meminfo: %v", err)
	}
	if c.GPU != nil && c.ensureGPU(errs) {
		if v, err := c.GPU.DriverVersion(); err == nil {
			inv.DriverVersion = &v
		} else {
			errs.Add("nvml: cannot read the driver version: %v", err)
		}
		if v, err := c.GPU.CUDADriverVersion(); err == nil {
			s := gpu.FormatCUDAVersion(v)
			inv.CUDADriverVersion = &s
		} else {
			errs.Add("nvml: cannot read the CUDA driver version: %v", err)
		}
		devs, err := c.GPU.Devices()
		if err != nil {
			errs.Add("nvml: cannot list GPUs: %v", err)
		}
		for i, d := range devs {
			uuid, err := d.UUID()
			if err != nil {
				continue
			}
			g := payload.InventoryGPU{Index: i, UUID: uuid}
			if v, err := d.Name(); err == nil {
				g.Name = &v
			}
			if tot, _, err := d.Memory(); err == nil {
				g.VRAMTotalMiB = &tot
			}
			inv.GPUs = append(inv.GPUs, g)
		}
	}
	for _, m := range c.Cfg.Disk.StatfsMounts {
		if c.Statfs == nil {
			break
		}
		if fs, err := c.Statfs(m); err == nil {
			inv.Filesystems = append(inv.Filesystems, payload.InventoryFS{Mount: m, SizeBytes: fs.SizeBytes})
		} else {
			errs.Add("disk: statfs %s failed: %v", m, err)
		}
	}
	return &payload.InventoryMsg{Header: c.header(payload.TypeInventory, now), Inventory: inv, Errors: errs.List()}
}

// DiskScanMessage wraps a scan result in a message.
func (c *Collector) DiskScanMessage(scan payload.DiskScan, errs []string) *payload.DiskScanMsg {
	if errs == nil {
		errs = []string{}
	}
	return &payload.DiskScanMsg{Header: c.header(payload.TypeDiskScan, c.Clock.Now()), Scan: scan, Errors: errs}
}

// Accounts loads the account database (used for disk scan owner mapping).
func (c *Collector) Accounts() *accounts.Accounts {
	acc, _, _ := accounts.Load(c.FS, c.Cfg.UIDMin)
	return acc
}
