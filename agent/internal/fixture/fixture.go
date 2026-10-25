// Package fixture loads the files under agent/testdata into in-memory
// filesystems. It is imported by tests only.
package fixture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"gpumon/agent/internal/hostfs"
)

// Dir returns the absolute path of agent/testdata.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata")
}

// RepoRoot returns the repository root.
func RepoRoot() string { return filepath.Join(Dir(), "..", "..") }

// Read returns a testdata file.
func Read(rel string) []byte {
	b, err := os.ReadFile(filepath.Join(Dir(), rel))
	if err != nil {
		panic(err)
	}
	return b
}

// Proc describes one fixture process.
type Proc struct {
	PID        int      `yaml:"pid"`
	PPID       int      `yaml:"ppid"`
	Comm       string   `yaml:"comm"`
	Exe        string   `yaml:"exe"`
	UID        uint32   `yaml:"uid"`
	LoginUID   string   `yaml:"loginuid"`
	Cgroup     string   `yaml:"cgroup"`
	CgroupFile string   `yaml:"cgroup_file"`
	Argv       []string `yaml:"argv"`
	ArgvFile   string   `yaml:"argv_file"`
	Kthread    bool     `yaml:"kthread"`
	UTime      uint64   `yaml:"utime"`
	STime      uint64   `yaml:"stime"`
	Start      uint64   `yaml:"start"`
	RSSPages   uint64   `yaml:"rss_pages"`
}

// Set is a fixture file such as testdata/procs.yaml.
type Set struct {
	Passwd    string `yaml:"passwd"`
	Subuid    string `yaml:"subuid"`
	LoginDefs string `yaml:"login_defs"`
	Procs     []Proc `yaml:"procs"`
}

// LoadSet parses a fixture set.
func LoadSet(rel string) Set {
	var s Set
	if err := yaml.Unmarshal(Read(rel), &s); err != nil {
		panic(err)
	}
	return s
}

// Build returns an in-memory host filesystem with the set's files and
// processes plus minimal /proc host files.
func (s Set) Build() *hostfs.MapFS {
	fs := hostfs.NewMapFS()
	if s.Passwd != "" {
		fs.WriteFile("/etc/passwd", Read(s.Passwd))
	}
	if s.Subuid != "" {
		fs.WriteFile("/etc/subuid", Read(s.Subuid))
	}
	if s.LoginDefs != "" {
		fs.WriteFile("/etc/login.defs", Read(s.LoginDefs))
	}
	WriteHost(fs, 1000, 500, 8)
	for _, p := range s.Procs {
		WriteProc(fs, p)
	}
	return fs
}

// WriteHost writes /proc/stat, meminfo, loadavg, and uptime.
func WriteHost(fs *hostfs.MapFS, total, idle uint64, cores int) {
	var b strings.Builder
	fmt.Fprintf(&b, "cpu  %d 0 0 %d 0 0 0 0 0 0\n", total-idle, idle)
	for i := 0; i < cores; i++ {
		fmt.Fprintf(&b, "cpu%d 0 0 0 0 0 0 0 0 0 0\n", i)
	}
	b.WriteString("intr 0\nctxt 0\n")
	fs.WriteString("/proc/stat", b.String())
	fs.WriteString("/proc/meminfo", "MemTotal:       16384000 kB\nMemFree:         1000000 kB\nMemAvailable:    8192000 kB\nSwapTotal:       2048000 kB\nSwapFree:        1024000 kB\n")
	fs.WriteString("/proc/loadavg", "1.50 1.25 1.00 3/400 12345\n")
	fs.WriteString("/proc/uptime", "1000.00 7000.00\n")
	fs.WriteString("/proc/sys/kernel/hostname", "fixture-host\n")
	fs.WriteString("/proc/sys/kernel/osrelease", "6.8.0-45-generic\n")
	fs.WriteString("/proc/cpuinfo", "processor\t: 0\nmodel name\t: Fixture CPU @ 3.00GHz\n")
	fs.WriteString("/etc/os-release", "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n")
}

// WriteProc writes the files of one process.
func WriteProc(fs *hostfs.MapFS, p Proc) {
	dir := fmt.Sprintf("/proc/%d", p.PID)
	flags := uint64(0x400100)
	if p.Kthread {
		flags |= 0x00200000
	}
	fs.WriteString(dir+"/stat", fmt.Sprintf("%d (%s) S %d %d %d 0 -1 %d 0 0 0 0 %d %d 0 0 20 0 1 0 %d 1000 %d 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0\n",
		p.PID, p.Comm, p.PPID, p.PID, p.PID, flags, p.UTime, p.STime, p.Start, p.RSSPages))
	if p.Kthread {
		return
	}
	fs.WriteString(dir+"/status", fmt.Sprintf("Name:\t%s\nUmask:\t0022\nState:\tS (sleeping)\nUid:\t%d\t%d\t%d\t%d\nGid:\t0\t0\t0\t0\n", p.Comm, p.UID, p.UID, p.UID, p.UID))
	switch p.LoginUID {
	case "":
	case "unset":
		fs.WriteString(dir+"/loginuid", "4294967295")
	default:
		fs.WriteString(dir+"/loginuid", p.LoginUID)
	}
	cg := p.Cgroup
	if p.CgroupFile != "" {
		cg = string(Read(p.CgroupFile))
	}
	if cg == "" {
		cg = fmt.Sprintf("0::/user.slice/user-%d.slice/session-1.scope", p.UID)
	}
	fs.WriteString(dir+"/cgroup", strings.TrimRight(cg, "\n")+"\n")
	argv := p.Argv
	if p.ArgvFile != "" {
		argv = strings.Split(strings.TrimRight(string(Read(p.ArgvFile)), "\n"), "\n")
	}
	if len(argv) == 0 {
		argv = []string{p.Exe}
	}
	fs.WriteString(dir+"/cmdline", strings.Join(argv, "\x00")+"\x00")
	if p.Exe != "" {
		fs.Symlink(dir+"/exe", p.Exe)
	}
}
