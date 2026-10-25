// Package procfs parses the parts of /proc that the agent needs.
package procfs

import (
	"bytes"
	"fmt"
	"path"
	"strconv"
	"strings"

	"gpumon/agent/internal/hostfs"
)

// PFKthread is the PF_KTHREAD flag in /proc/<pid>/stat.
const PFKthread = 0x00200000

// LoginUIDUnset is the value of /proc/<pid>/loginuid when no login UID is set.
const LoginUIDUnset = 4294967295

// Proc holds what the agent reads about one process. The command line is
// deliberately not part of it; see ReadCmdline.
type Proc struct {
	PID       int
	PPID      int
	Comm      string
	Exe       string // basename of the executable, falls back to Comm
	ExePath   string // full executable path when readable (diagnose only)
	Flags     uint64
	UID       uint32
	LoginUID  *uint32 // nil when unreadable
	UTime     uint64  // clock ticks
	STime     uint64  // clock ticks
	StartTime uint64  // clock ticks since boot
	RSSBytes  uint64
	Cgroup    string
}

// IsKernelThread reports whether the process is a kernel thread.
func (p *Proc) IsKernelThread() bool {
	return p.Flags&PFKthread != 0 || p.PPID == 2 || p.PID == 2
}

// ListPIDs returns the numeric entries of /proc.
func ListPIDs(fs hostfs.FS) ([]int, error) {
	names, err := fs.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(names))
	for _, n := range names {
		if n == "" || n[0] < '0' || n[0] > '9' {
			continue
		}
		if pid, err := strconv.Atoi(n); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// ParseStat parses /proc/<pid>/stat content into p.
func ParseStat(data []byte, pageSize int, p *Proc) error {
	open := bytes.IndexByte(data, '(')
	closeIdx := bytes.LastIndexByte(data, ')')
	if open < 0 || closeIdx < open {
		return fmt.Errorf("malformed stat")
	}
	p.Comm = string(data[open+1 : closeIdx])
	// Fields after the comm start at field 3 (state).
	f := strings.Fields(string(data[closeIdx+1:]))
	if len(f) < 22 {
		return fmt.Errorf("malformed stat: %d fields", len(f))
	}
	num := func(i int) uint64 {
		v, _ := strconv.ParseUint(f[i], 10, 64)
		return v
	}
	p.PPID = int(num(1))  // field 4
	p.Flags = num(6)      // field 9
	p.UTime = num(11)     // field 14
	p.STime = num(12)     // field 15
	p.StartTime = num(19) // field 22
	rss, _ := strconv.ParseInt(f[21], 10, 64)
	if rss > 0 {
		p.RSSBytes = uint64(rss) * uint64(pageSize)
	}
	return nil
}

// ParseStatusUID returns the real UID from /proc/<pid>/status content.
func ParseStatusUID(data []byte) (uint32, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			f := strings.Fields(line[4:])
			if len(f) > 0 {
				v, err := strconv.ParseUint(f[0], 10, 32)
				return uint32(v), err
			}
		}
	}
	return 0, fmt.Errorf("no Uid line in status")
}

// ReadProc reads one process. Kernel threads are returned with only the
// stat fields filled in.
func ReadProc(fs hostfs.FS, pid, pageSize int) (*Proc, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	st, err := fs.ReadFile(dir + "/stat")
	if err != nil {
		return nil, err
	}
	p := &Proc{PID: pid}
	if err := ParseStat(st, pageSize, p); err != nil {
		return nil, fmt.Errorf("pid %d: %v", pid, err)
	}
	p.Exe = p.Comm
	if p.IsKernelThread() {
		return p, nil
	}
	status, err := fs.ReadFile(dir + "/status")
	if err != nil {
		return nil, err
	}
	if p.UID, err = ParseStatusUID(status); err != nil {
		return nil, fmt.Errorf("pid %d: %v", pid, err)
	}
	if b, err := fs.ReadFile(dir + "/loginuid"); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32); err == nil {
			u := uint32(v)
			p.LoginUID = &u
		}
	}
	if b, err := fs.ReadFile(dir + "/cgroup"); err == nil {
		p.Cgroup = string(b)
	}
	if target, err := fs.Readlink(dir + "/exe"); err == nil {
		target = strings.TrimSuffix(target, " (deleted)")
		p.ExePath = target
		if b := path.Base(target); b != "" && b != "." && b != "/" {
			p.Exe = b
		}
	}
	return p, nil
}

// ReadCmdline returns the argv of a process. It is only used for QEMU
// detection and by the diagnose command; it is never sent to central.
func ReadCmdline(fs hostfs.FS, pid int) ([]string, error) {
	b, err := fs.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return nil, nil
	}
	return strings.Split(string(b), "\x00"), nil
}

// CPUTimes is the aggregate line of /proc/stat.
type CPUTimes struct {
	Total uint64
	Idle  uint64
	Cores int
}

// ReadCPUTimes parses /proc/stat.
func ReadCPUTimes(fs hostfs.FS) (CPUTimes, error) {
	b, err := fs.ReadFile("/proc/stat")
	if err != nil {
		return CPUTimes{}, err
	}
	var ct CPUTimes
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "cpu" {
			found = true
			for i := 1; i < len(f) && i <= 8; i++ { // user..steal; guest is already in user
				v, _ := strconv.ParseUint(f[i], 10, 64)
				ct.Total += v
				if i == 4 || i == 5 { // idle, iowait
					ct.Idle += v
				}
			}
		} else if strings.HasPrefix(f[0], "cpu") {
			ct.Cores++
		}
	}
	if !found {
		return ct, fmt.Errorf("no cpu line in /proc/stat")
	}
	return ct, nil
}

// CPUPercent returns the busy percentage between two readings.
func CPUPercent(prev, cur CPUTimes) (float64, bool) {
	if cur.Total <= prev.Total {
		return 0, false
	}
	dt := float64(cur.Total - prev.Total)
	di := float64(0)
	if cur.Idle > prev.Idle {
		di = float64(cur.Idle - prev.Idle)
	}
	pct := 100 * (dt - di) / dt
	if pct < 0 {
		pct = 0
	}
	return pct, true
}

// MemInfo holds /proc/meminfo values in bytes.
type MemInfo struct {
	Total, Available, SwapTotal, SwapFree uint64
}

// ReadMemInfo parses /proc/meminfo.
func ReadMemInfo(fs hostfs.FS) (MemInfo, error) {
	b, err := fs.ReadFile("/proc/meminfo")
	if err != nil {
		return MemInfo{}, err
	}
	var m MemInfo
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		v *= 1024 // values are in kB
		switch f[0] {
		case "MemTotal:":
			m.Total = v
		case "MemAvailable:":
			m.Available = v
		case "SwapTotal:":
			m.SwapTotal = v
		case "SwapFree:":
			m.SwapFree = v
		}
	}
	if m.Total == 0 {
		return m, fmt.Errorf("no MemTotal in /proc/meminfo")
	}
	return m, nil
}

// ReadLoadAvg parses /proc/loadavg.
func ReadLoadAvg(fs hostfs.FS) ([3]float64, error) {
	var out [3]float64
	b, err := fs.ReadFile("/proc/loadavg")
	if err != nil {
		return out, err
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return out, fmt.Errorf("malformed /proc/loadavg")
	}
	for i := 0; i < 3; i++ {
		out[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return out, nil
}

// ReadUptime returns the system uptime in seconds.
func ReadUptime(fs hostfs.FS) (float64, error) {
	b, err := fs.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return 0, fmt.Errorf("malformed /proc/uptime")
	}
	return strconv.ParseFloat(f[0], 64)
}

// ReadCPUModel returns the first "model name" in /proc/cpuinfo.
func ReadCPUModel(fs hostfs.FS) string {
	b, err := fs.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "model name") {
			if i := strings.IndexByte(line, ':'); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

// ReadOSRelease returns PRETTY_NAME from /etc/os-release.
func ReadOSRelease(fs hostfs.FS) string {
	b, err := fs.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return ""
}

// ReadTrimmed returns a file's content without surrounding whitespace.
func ReadTrimmed(fs hostfs.FS, name string) string {
	b, err := fs.ReadFile(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
