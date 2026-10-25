// Package sim builds a synthetic host (procfs, NVML, Docker, disk) from an
// inventory file so that the real collector and attribution code can run
// without hardware. It is used by "gpumon-agent simulate", the E2E test, and
// the contract tests.
package sim

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/disk"
	"gpumon/agent/internal/docker"
	"gpumon/agent/internal/gpu"
	"gpumon/agent/internal/hostfs"
	"gpumon/agent/internal/payload"
)

// Job kinds. Each one exercises one attribution scenario.
const (
	KindUID             = "uid"
	KindSudo            = "sudo"
	KindRootLogin       = "root_login"
	KindDaemon          = "daemon"
	KindDockerLabeled   = "docker_labeled"
	KindDockerUnlabeled = "docker_unlabeled"
	KindRootlessRoot    = "rootless_root"
	KindRootlessSubuid  = "rootless_subuid"
	KindMultipass       = "multipass"
	KindUnknownUID      = "unknown_uid"
)

// GPU states.
const (
	StateFree = "free"
	StateBusy = "busy"
	StateIdle = "idle"
)

// Inventory is the simulate --inventory file.
type Inventory struct {
	Servers []Server `yaml:"servers"`
}

// Server describes one simulated host.
type Server struct {
	ID                 string   `yaml:"id"`
	OSRelease          string   `yaml:"os_release"`
	Kernel             string   `yaml:"kernel"`
	CPUModel           string   `yaml:"cpu_model"`
	LogicalCores       int      `yaml:"logical_cores"`
	MemTotalGiB        float64  `yaml:"mem_total_gib"`
	DriverVersion      string   `yaml:"driver_version"`
	CUDADriverVersion  int      `yaml:"cuda_driver_version"`
	DockerDefaultOwner string   `yaml:"docker_default_owner"`
	UIDOffset          int      `yaml:"uid_offset"`
	FanNotSupported    bool     `yaml:"fan_not_supported"`
	Users              []string `yaml:"users"`
	GPUs               []GPU    `yaml:"gpus"`
	Jobs               []Job    `yaml:"jobs"`
}

// GPU describes one simulated GPU.
type GPU struct {
	Name         string `yaml:"name"`
	VRAMTotalMiB uint64 `yaml:"vram_total_mib"`
	State        string `yaml:"state"`
	Jobs         []Job  `yaml:"jobs"`
}

// Job is one simulated process.
type Job struct {
	Kind    string   `yaml:"kind"`
	User    string   `yaml:"user"`
	UID     uint32   `yaml:"uid"`
	VMName  string   `yaml:"vm_name"`
	Exe     string   `yaml:"exe"`
	Cores   float64  `yaml:"cores"`
	RSSMiB  uint64   `yaml:"rss_mib"`
	VRAMMiB uint64   `yaml:"vram_mib"`
	Args    []string `yaml:"args"`
}

// LoadInventory reads an inventory file.
func LoadInventory(path string) (*Inventory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inv Inventory
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&inv); err != nil {
		return nil, fmt.Errorf("invalid inventory file %s: %v", path, err)
	}
	return &inv, nil
}

// Find returns the server with the given ID.
func (inv *Inventory) Find(id string) (*Server, error) {
	for i := range inv.Servers {
		if inv.Servers[i].ID == id {
			return &inv.Servers[i], nil
		}
	}
	return nil, fmt.Errorf("server %q is not in the inventory file", id)
}

type proc struct {
	pid, ppid int
	comm, exe string
	uid       uint32
	loginuid  uint32
	cgroup    string
	argv      []string
	cores     float64
	rss       uint64
	ticks     float64
	start     uint64
	gpu       int
	gpuType   string
	vram      uint64
	jitter    bool
}

// World is the synthetic host.
type World struct {
	Srv    *Server
	FS     *hostfs.MapFS
	GPU    *gpu.FakeLibrary
	Docker *docker.Fake
	DiskFS *disk.MapFS

	rng      *rand.Rand
	procs    []*proc
	uids     map[string]uint32
	last     time.Time
	uptime   float64
	cpuTotal float64
	cpuIdle  float64
	nextPID  int
	homeUsed uint64
}

const clkTck = 100

// UserUID returns the simulated UID of a user on this server. UIDs differ
// between servers through uid_offset, while usernames stay the same.
func (w *World) UserUID(name string) uint32 { return w.uids[name] }

func (w *World) addUser(name string) {
	if _, ok := w.uids[name]; ok {
		return
	}
	w.uids[name] = uint32(1001 + len(w.uids) + w.Srv.UIDOffset)
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// NewWorld builds the synthetic host. Background users "user01".."userNN"
// are added when users > 0. The world is deterministic for a given seed.
func NewWorld(srv *Server, users int, seed int64, now time.Time) *World {
	w := &World{
		Srv: srv, FS: hostfs.NewMapFS(), Docker: &docker.Fake{Containers: map[string]*docker.Container{}},
		DiskFS: disk.NewMapFS(), rng: rand.New(rand.NewSource(seed)), uids: map[string]uint32{},
		last: now, uptime: 86400, nextPID: 1000,
	}
	for _, u := range srv.Users {
		w.addUser(u)
	}
	for _, g := range srv.GPUs {
		for _, j := range g.Jobs {
			if j.User != "" {
				w.addUser(j.User)
			}
		}
	}
	for _, j := range srv.Jobs {
		if j.User != "" {
			w.addUser(j.User)
		}
	}
	for i := 1; i <= users; i++ {
		w.addUser(fmt.Sprintf("user%02d", i))
	}
	w.writeStatic()
	w.buildProcs(users)
	w.buildGPUs()
	w.buildDisk()
	w.Advance(now)
	return w
}

func (w *World) sortedUsers() []string {
	names := make([]string, 0, len(w.uids))
	for n := range w.uids {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return w.uids[names[i]] < w.uids[names[j]] })
	return names
}

func (w *World) subuidStart(name string) uint32 {
	return 100000 + (w.uids[name]-1001-uint32(w.Srv.UIDOffset))*65536
}

func (w *World) writeStatic() {
	fs, s := w.FS, w.Srv
	fs.WriteString("/proc/sys/kernel/hostname", s.ID+"\n")
	fs.WriteString("/proc/sys/kernel/osrelease", s.Kernel+"\n")
	fs.WriteString("/etc/os-release", fmt.Sprintf("NAME=\"Ubuntu\"\nPRETTY_NAME=%q\n", s.OSRelease))
	var cpuinfo strings.Builder
	for i := 0; i < s.LogicalCores; i++ {
		fmt.Fprintf(&cpuinfo, "processor\t: %d\nmodel name\t: %s\n\n", i, s.CPUModel)
	}
	fs.WriteString("/proc/cpuinfo", cpuinfo.String())
	fs.WriteString("/etc/login.defs", "UID_MIN\t\t\t 1000\nUID_MAX\t\t\t60000\n")

	var passwd, subuid strings.Builder
	passwd.WriteString("root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\n")
	passwd.WriteString("messagebus:x:101:102::/nonexistent:/usr/sbin/nologin\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n")
	for i, n := range w.sortedUsers() {
		uid := w.uids[n]
		fmt.Fprintf(&passwd, "%s:x:%d:%d:%s:/home/%s:/bin/bash\n", n, uid, uid, n, n)
		if i%2 == 0 {
			fmt.Fprintf(&subuid, "%s:%d:65536\n", n, w.subuidStart(n))
		} else {
			fmt.Fprintf(&subuid, "%d:%d:65536\n", uid, w.subuidStart(n))
		}
	}
	fs.WriteString("/etc/passwd", passwd.String())
	fs.WriteString("/etc/subuid", subuid.String())
}

func (w *World) newProc(p *proc) *proc {
	if p.pid == 0 {
		w.nextPID += 1 + w.rng.Intn(7)
		p.pid = w.nextPID
	}
	if p.exe == "" {
		p.exe = "/usr/bin/" + p.comm
	}
	if len(p.argv) == 0 {
		p.argv = []string{p.exe}
	}
	if p.cgroup == "" {
		if p.uid >= 1000 {
			p.cgroup = fmt.Sprintf("0::/user.slice/user-%d.slice/session-%d.scope\n", p.uid, 1+p.pid%50)
		} else {
			p.cgroup = "0::/system.slice/" + p.comm + ".service\n"
		}
	}
	p.gpu = -1
	p.start = uint64((w.uptime - 3600 - float64(w.rng.Intn(80000))) * clkTck)
	p.ticks = float64(w.rng.Intn(500000))
	w.procs = append(w.procs, p)
	return p
}

func (w *World) containerID(tag string) string {
	return shortHash(w.Srv.ID + "/" + tag)
}

// jobProc creates the process for one job.
func (w *World) jobProc(j Job, tag string) *proc {
	uid := w.uids[j.User]
	exe := j.Exe
	if exe == "" {
		exe = "python3"
	}
	p := &proc{comm: exe, uid: uid, loginuid: uid, cores: j.Cores, rss: j.RSSMiB << 20, jitter: true}
	p.argv = append([]string{"/usr/bin/" + exe, "train.py"}, j.Args...)
	switch j.Kind {
	case KindUID:
	case KindSudo:
		p.uid = 0
	case KindRootLogin:
		p.uid, p.loginuid = 0, 0
		p.cgroup = fmt.Sprintf("0::/user.slice/user-0.slice/session-%d.scope\n", 3+len(w.procs))
	case KindDaemon:
		p.uid, p.loginuid = 0, procLoginUnset
	case KindDockerLabeled, KindDockerUnlabeled:
		id := w.containerID(tag)
		p.uid, p.loginuid = 0, procLoginUnset
		p.cgroup = "0::/system.slice/docker-" + id + ".scope\n"
		c := &docker.Container{ID: id, Name: tag, Image: "pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime", Labels: map[string]string{"com.example.project": "sim"}}
		if j.Kind == KindDockerLabeled {
			c.Labels["owner"] = j.User
		}
		w.Docker.Containers[id] = c
	case KindRootlessRoot, KindRootlessSubuid:
		id := w.containerID(tag)
		p.loginuid = procLoginUnset
		p.cgroup = fmt.Sprintf("0::/user.slice/user-%d.slice/user@%d.service/user.slice/docker-%s.scope\n", uid, uid, id)
		if j.Kind == KindRootlessSubuid {
			p.uid = w.subuidStart(j.User) + 999
		}
	case KindMultipass:
		p.comm = "qemu-system-x86"
		p.exe = "/snap/multipass/19230/usr/bin/qemu-system-x86_64"
		p.uid, p.loginuid = 0, procLoginUnset
		p.cgroup = "0::/system.slice/snap.multipass.multipassd.service\n"
		dir := "/var/snap/multipass/common/data/multipassd/vault/instances/" + j.VMName
		p.argv = []string{p.exe, "-bios", "OVMF.fd", "--enable-kvm", "-cpu", "host", "-smp", "4", "-m", "8192M",
			"-drive", "file=" + dir + "/ubuntu-24.04-server-cloudimg-amd64.img,if=none,format=qcow2,id=hda",
			"-cdrom", dir + "/cloud-init-config.iso", "-nographic"}
		p.ppid = w.multipassd().pid
	case KindUnknownUID:
		p.uid, p.loginuid = j.UID, j.UID
	}
	return w.newProc(p)
}

const procLoginUnset = 4294967295

func (w *World) multipassd() *proc {
	for _, p := range w.procs {
		if p.comm == "multipassd" {
			return p
		}
	}
	return w.newProc(&proc{comm: "multipassd", exe: "/snap/multipass/19230/bin/multipassd", ppid: 1, loginuid: procLoginUnset, cores: 0.02, rss: 60 << 20,
		cgroup: "0::/system.slice/snap.multipass.multipassd.service\n"})
}

func (w *World) buildProcs(users int) {
	w.newProc(&proc{pid: 1, comm: "systemd", exe: "/usr/lib/systemd/systemd", loginuid: procLoginUnset, rss: 12 << 20, cores: 0.01, cgroup: "0::/init.scope\n"})
	w.newProc(&proc{pid: 2, comm: "kthreadd", loginuid: procLoginUnset})
	for i := 0; i < 6; i++ {
		w.newProc(&proc{comm: fmt.Sprintf("kworker/%d:1", i), ppid: 2, loginuid: procLoginUnset})
	}
	w.newProc(&proc{comm: "sshd", exe: "/usr/sbin/sshd", ppid: 1, loginuid: procLoginUnset, rss: 8 << 20})
	w.newProc(&proc{comm: "dbus-daemon", ppid: 1, uid: 101, loginuid: procLoginUnset, rss: 5 << 20})
	w.newProc(&proc{comm: "dockerd", exe: "/usr/bin/dockerd", ppid: 1, loginuid: procLoginUnset, rss: 90 << 20, cores: 0.03})
	for gi, g := range w.Srv.GPUs {
		if g.State == StateFree && gi == 0 {
			x := w.newProc(&proc{comm: "Xorg", exe: "/usr/lib/xorg/Xorg", ppid: 1, loginuid: procLoginUnset, rss: 80 << 20, cores: 0.01})
			x.gpu, x.gpuType, x.vram = gi, "graphics", 4
		}
		for ji, j := range g.Jobs {
			p := w.jobProc(j, fmt.Sprintf("gpu%d-job%d", gi, ji))
			p.gpu, p.gpuType, p.vram = gi, "compute", j.VRAMMiB
		}
	}
	for ji, j := range w.Srv.Jobs {
		w.jobProc(j, fmt.Sprintf("cpu-job%d", ji))
	}
	exes := []string{"python3", "bash", "jupyter-lab", "code-server", "node", "R", "matlab"}
	for i := 1; i <= users; i++ {
		name := fmt.Sprintf("user%02d", i)
		if w.rng.Intn(2) == 0 {
			continue // not active on this server
		}
		uid := w.uids[name]
		shell := w.newProc(&proc{comm: "bash", ppid: 1, uid: uid, loginuid: uid, rss: 4 << 20})
		for k := 0; k < 1+w.rng.Intn(3); k++ {
			w.newProc(&proc{comm: exes[w.rng.Intn(len(exes))], ppid: shell.pid, uid: uid, loginuid: uid,
				cores: float64(w.rng.Intn(300)) / 100, rss: uint64(100+w.rng.Intn(8000)) << 20, jitter: true})
		}
	}
}

func (w *World) buildGPUs() {
	w.GPU = &gpu.FakeLibrary{Driver: w.Srv.DriverVersion, CUDA: w.Srv.CUDADriverVersion}
	for i, g := range w.Srv.GPUs {
		h := shortHash(w.Srv.ID + fmt.Sprint(i))
		d := &gpu.FakeDevice{
			UUIDVal:     fmt.Sprintf("GPU-%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]),
			NameVal:     g.Name,
			TotalMiB:    g.VRAMTotalMiB,
			LimitMilliW: 320000,
			Errs:        map[string]error{"ProcessUtilization": gpu.ErrNotSupported},
		}
		if w.Srv.FanNotSupported {
			d.Errs["FanSpeed"] = gpu.ErrNotSupported
		}
		w.GPU.Devs = append(w.GPU.Devs, d)
	}
}

func (w *World) buildDisk() {
	const dev = 2049
	w.DiskFS.Add("/home", disk.FileInfo{Dev: dev, Ino: 2, Nlink: 3, IsDir: true, Blocks: 8})
	ino := uint64(100)
	for _, name := range w.sortedUsers() {
		uid := w.uids[name]
		home := "/home/" + name
		ino++
		w.DiskFS.Add(home, disk.FileInfo{Dev: dev, Ino: ino, Nlink: 2, UID: uid, IsDir: true, Blocks: 8})
		for k := 0; k < 3; k++ {
			ino++
			gibs := int64(1 + w.rng.Intn(200))
			w.DiskFS.Add(fmt.Sprintf("%s/data%d.bin", home, k), disk.FileInfo{Dev: dev, Ino: ino, Nlink: 1, UID: uid, Blocks: gibs << 21})
		}
		ino++
		w.DiskFS.Add(home+"/link-to-data", disk.FileInfo{Dev: dev, Ino: ino, Nlink: 1, UID: uid, IsSymlink: true})
		ino++
		w.DiskFS.Add(home+"/container-layer", disk.FileInfo{Dev: dev, Ino: ino, Nlink: 1, UID: w.subuidStart(name) + 999, Blocks: 1 << 21})
		ino++
		w.DiskFS.Add(home+"/model-a.pt", disk.FileInfo{Dev: dev, Ino: ino, Nlink: 2, UID: uid, Blocks: 1 << 20})
		w.DiskFS.Add(home+"/model-b.pt", disk.FileInfo{Dev: dev, Ino: ino, Nlink: 2, UID: uid, Blocks: 1 << 20})
	}
	ino++
	w.DiskFS.Add("/home/shared-orphan.tar", disk.FileInfo{Dev: dev, Ino: ino, Nlink: 1, UID: 4242, Blocks: 5 << 21})
	var total uint64
	sc := disk.Scanner{FS: w.DiskFS, Owner: func(uint32) string { return "x" }, Clock: clock.Real{}}
	res, _ := sc.Scan(context.Background(), []string{"/home"}, func(string) string { return "/home" })
	for _, p := range res.Paths {
		total += p.TotalBytes
	}
	w.homeUsed = total + 150<<30
}

// Statfs returns synthetic statfs totals.
func (w *World) Statfs(mount string) (payload.Filesystem, error) {
	switch mount {
	case "/":
		return payload.Filesystem{Mount: mount, SizeBytes: 1 << 40, UsedBytes: 400 << 30, FreeBytes: (1 << 40) - (400 << 30)}, nil
	case "/home":
		size := uint64(8 << 40)
		return payload.Filesystem{Mount: mount, SizeBytes: size, UsedBytes: w.homeUsed, FreeBytes: size - w.homeUsed}, nil
	}
	return payload.Filesystem{}, fmt.Errorf("no such mount point in the simulation: %s", mount)
}

// Advance moves the synthetic host to time now: CPU counters grow,
// utilization and VRAM fluctuate, and procfs files are rewritten.
func (w *World) Advance(now time.Time) {
	dt := now.Sub(w.last).Seconds()
	if dt < 0 {
		dt = 0
	}
	w.last = now
	w.uptime += dt
	busy := 0.0
	var rssTotal uint64
	for _, p := range w.procs {
		c := p.cores
		if p.jitter && c > 0 {
			c *= 0.85 + 0.3*w.rng.Float64()
		}
		p.ticks += c * dt * clkTck
		busy += c
		rssTotal += p.rss
	}
	cores := float64(w.Srv.LogicalCores)
	if busy > cores {
		busy = cores
	}
	w.cpuTotal += cores * dt * clkTck
	w.cpuIdle += (cores - busy) * dt * clkTck
	w.writeDynamic(rssTotal, busy)
	w.updateGPUs()
}

func (w *World) writeDynamic(rssTotal uint64, busy float64) {
	fs := w.FS
	user := uint64(w.cpuTotal - w.cpuIdle)
	var stat strings.Builder
	fmt.Fprintf(&stat, "cpu  %d 0 0 %d 0 0 0 0 0 0\n", user, uint64(w.cpuIdle))
	for i := 0; i < w.Srv.LogicalCores; i++ {
		fmt.Fprintf(&stat, "cpu%d 0 0 0 0 0 0 0 0 0 0\n", i)
	}
	fs.WriteString("/proc/stat", stat.String())
	fs.WriteString("/proc/uptime", fmt.Sprintf("%.2f %.2f\n", w.uptime, w.uptime*2))
	fs.WriteString("/proc/loadavg", fmt.Sprintf("%.2f %.2f %.2f 2/%d %d\n", busy, busy*0.9, busy*0.8, len(w.procs), w.nextPID))
	total := uint64(w.Srv.MemTotalGiB * (1 << 30))
	used := rssTotal + 3<<30
	if used > total {
		used = total
	}
	fs.WriteString("/proc/meminfo", fmt.Sprintf("MemTotal: %d kB\nMemFree: %d kB\nMemAvailable: %d kB\nSwapTotal: %d kB\nSwapFree: %d kB\n",
		total>>10, (total-used)>>10, (total-used)>>10, uint64(8<<30)>>10, uint64(7<<30)>>10))
	for _, p := range w.procs {
		dir := fmt.Sprintf("/proc/%d", p.pid)
		flags := uint64(0x400000)
		if p.ppid == 2 || p.pid == 2 {
			flags |= 0x00200000 // PF_KTHREAD
		}
		t := uint64(p.ticks)
		fs.WriteString(dir+"/stat", fmt.Sprintf("%d (%s) S %d %d %d 0 -1 %d 100 0 0 0 %d %d 0 0 20 0 1 0 %d %d %d 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n",
			p.pid, p.comm, p.ppid, p.pid, p.pid, flags, t*9/10, t/10, p.start, p.rss*2, p.rss/4096))
		if _, err := fs.ReadFile(dir + "/status"); err == nil || p.ppid == 2 || p.pid == 2 {
			continue // the remaining files never change
		}
		fs.WriteString(dir+"/status", fmt.Sprintf("Name:\t%s\nUid:\t%d\t%d\t%d\t%d\n", p.comm, p.uid, p.uid, p.uid, p.uid))
		fs.WriteString(dir+"/loginuid", fmt.Sprint(p.loginuid))
		fs.WriteString(dir+"/cgroup", p.cgroup)
		fs.WriteString(dir+"/cmdline", strings.Join(p.argv, "\x00")+"\x00")
		fs.Symlink(dir+"/exe", p.exe)
	}
}

func (w *World) updateGPUs() {
	for i, g := range w.Srv.GPUs {
		d := w.GPU.Devs[i]
		d.Compute, d.Graphics = nil, nil
		used := uint64(250)
		for _, p := range w.procs {
			if p.gpu != i {
				continue
			}
			v := p.vram
			if p.gpuType == "compute" && v > 0 {
				v = v - v/20 + uint64(w.rng.Intn(int(v/10)+1))
			}
			used += v
			vv := v
			pi := gpu.ProcInfo{PID: uint32(p.pid), UsedMiB: &vv}
			if p.gpuType == "graphics" {
				d.Graphics = append(d.Graphics, pi)
			} else {
				d.Compute = append(d.Compute, pi)
			}
		}
		if used > g.VRAMTotalMiB {
			used = g.VRAMTotalMiB
		}
		d.UsedMiB = used
		switch g.State {
		case StateBusy:
			d.Util = uint32(60 + w.rng.Intn(36))
			d.PowerMilliW = uint32(180000 + w.rng.Intn(100000))
			d.Temp = uint32(70 + w.rng.Intn(10))
		case StateIdle:
			d.Util = uint32(w.rng.Intn(3))
			d.PowerMilliW = uint32(30000 + w.rng.Intn(10000))
			d.Temp = uint32(40 + w.rng.Intn(5))
		default:
			d.Util = 0
			d.PowerMilliW = uint32(15000 + w.rng.Intn(5000))
			d.Temp = uint32(32 + w.rng.Intn(4))
		}
		d.Fan = 30 + d.Util/2
	}
}
