// Package attrib decides which user owns a process (Section 4.2 of the spec).
// The first matching rule wins:
//
//  1. rootful Docker container (label, then per-host default owner)
//  2. multipass QEMU VM (instance name prefix)
//  3. subuid range (rootless containers)
//  4. UID 0 with a valid loginuid (sudo or su)
//  5. UID 0 with loginuid 0 (direct root login)
//  6. UID 0 with no loginuid, or UID below uid_min (system)
//  7. the username of the UID
package attrib

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gpumon/agent/internal/accounts"
	"gpumon/agent/internal/docker"
	"gpumon/agent/internal/logx"
	"gpumon/agent/internal/procfs"
)

// Method is the attribution method reported with every process.
type Method string

// Attribution methods.
const (
	MethodUID                Method = "uid"
	MethodLoginUID           Method = "loginuid"
	MethodSubuid             Method = "subuid"
	MethodDockerLabel        Method = "docker_label"
	MethodDockerDefaultOwner Method = "docker_default_owner"
	MethodMultipassName      Method = "multipass_name"
	MethodSystem             Method = "system"
	MethodRootShared         Method = "root_shared"
	MethodUnattributed       Method = "unattributed"
)

// Special owner values.
const (
	OwnerRootShared   = "root-shared"
	OwnerSystem       = "system"
	OwnerUnattributed = "unattributed"
)

// DefaultNamePatterns extract a multipass instance name from a QEMU argv.
var DefaultNamePatterns = []string{`/instances/([^/]+)/`, `-name\s+(\S+)`}

// DefaultDataDirs are the multipass data directories (snap and non-snap installs).
var DefaultDataDirs = []string{"/var/snap/multipass/common/data/multipassd", "/var/lib/multipassd"}

// Config controls the Docker and multipass rules.
type Config struct {
	OwnerLabel   string
	DefaultOwner string
	NamePatterns []*regexp.Regexp
	DataDirs     []string
}

// Context carries optional details about where a process runs.
type Context struct {
	ContainerID    string // short (12 hex digits)
	ContainerName  string
	ContainerImage string
	VMName         string
	Account        string // real account name for system processes
}

// Evidence explains a decision. It is only filled for the diagnose command
// and is never sent to central.
type Evidence struct {
	UID             uint32
	Username        string
	LoginUID        string
	CgroupLine      string
	ContainerID     string
	Rootless        bool
	ContainerLabels map[string]string
	DockerError     string
	SubuidMatch     string
	ParentChain     []string
	Argv            []string
	Steps           []string
}

// Result is the attribution of one process.
type Result struct {
	Owner    string
	Method   Method
	Ctx      Context
	Evidence *Evidence
}

// Engine attributes processes on one host for one sample.
type Engine struct {
	Cfg          Config
	Acc          *accounts.Accounts
	Docker       docker.Client // nil when Docker is not configured
	Procs        map[int]*procfs.Proc
	Cmdline      func(pid int) ([]string, error)
	Errs         *logx.ErrorSet
	Ctx          context.Context
	WithEvidence bool
}

var (
	reCgroupV1 = regexp.MustCompile(`/docker/([0-9a-f]{64})`)
	reCgroupV2 = regexp.MustCompile(`docker-([0-9a-f]{64})\.scope`)
	reUserSvc  = regexp.MustCompile(`/user@\d+\.service/`)
)

// FindContainerID returns the Docker container ID in a /proc/<pid>/cgroup
// file, the matching line, and whether the container belongs to a rootless
// (per-user) Docker daemon.
func FindContainerID(cgroup string) (id, line string, rootless bool) {
	for _, l := range strings.Split(cgroup, "\n") {
		m := reCgroupV2.FindStringSubmatch(l)
		if m == nil {
			m = reCgroupV1.FindStringSubmatch(l)
		}
		if m != nil {
			return m[1], l, reUserSvc.MatchString(l)
		}
	}
	return "", "", false
}

// ExtractVMName applies the patterns to the joined argv and returns the
// first captured instance name.
func ExtractVMName(argv []string, patterns []*regexp.Regexp) string {
	joined := strings.Join(argv, " ")
	for _, re := range patterns {
		m := re.FindStringSubmatch(joined)
		if len(m) < 2 || m[1] == "" {
			continue
		}
		name := m[1]
		// QEMU accepts "-name guest=<name>,debug-threads=on".
		if i := strings.Index(name, "guest="); i >= 0 {
			name = name[i+len("guest="):]
		}
		if i := strings.IndexByte(name, ','); i >= 0 {
			name = name[:i]
		}
		if name != "" {
			return name
		}
	}
	return ""
}

// IsQEMU reports whether the executable matches qemu-system-*.
func IsQEMU(p *procfs.Proc) bool {
	return strings.HasPrefix(p.Exe, "qemu-system-") || strings.HasPrefix(p.Comm, "qemu-system-")
}

func (e *Engine) note(ev *Evidence, format string, args ...any) {
	if ev != nil {
		ev.Steps = append(ev.Steps, fmt.Sprintf(format, args...))
	}
}

func (e *Engine) addErr(format string, args ...any) {
	if e.Errs != nil {
		e.Errs.Add(format, args...)
	}
}

func loginString(l *uint32) string {
	switch {
	case l == nil:
		return "unreadable"
	case *l == procfs.LoginUIDUnset:
		return "unset (4294967295)"
	}
	return fmt.Sprint(*l)
}

// Attribute applies the precedence rules to p.
func (e *Engine) Attribute(p *procfs.Proc) Result {
	var r Result
	var ev *Evidence
	if e.WithEvidence {
		ev = &Evidence{UID: p.UID, Username: e.Acc.Username(p.UID), LoginUID: loginString(p.LoginUID)}
		r.Evidence = ev
	}
	done := func(owner string, m Method, format string, args ...any) Result {
		r.Owner, r.Method = owner, m
		e.note(ev, format, args...)
		return r
	}

	// Rule 1: rootful Docker.
	cid, line, rootless := FindContainerID(p.Cgroup)
	if cid != "" {
		r.Ctx.ContainerID = cid[:12]
		if ev != nil {
			ev.CgroupLine, ev.ContainerID, ev.Rootless = line, cid, rootless
		}
	}
	switch {
	case cid != "" && !rootless:
		return e.dockerRule(cid, &r, ev)
	case cid != "":
		e.note(ev, "rule 1 (rootful Docker): container %s runs under a per-user service (rootless Docker); skipped", cid[:12])
	default:
		e.note(ev, "rule 1 (rootful Docker): no Docker container ID in the cgroup path; skipped")
	}

	// Rule 2: multipass VM.
	if IsQEMU(p) {
		chain := e.parentChain(p)
		viaParent := false
		for _, n := range chain {
			if n == "multipassd" {
				viaParent = true
			}
		}
		var argv []string
		if e.Cmdline != nil {
			var err error
			if argv, err = e.Cmdline(p.PID); err != nil {
				e.addErr("cannot read the argv of QEMU process %d: %v", p.PID, err)
			}
		}
		joined := strings.Join(argv, " ")
		viaDir := false
		for _, d := range e.Cfg.DataDirs {
			if d != "" && strings.Contains(joined, d) {
				viaDir = true
			}
		}
		if ev != nil {
			ev.ParentChain, ev.Argv = chain, argv
		}
		if viaParent || viaDir {
			name := ExtractVMName(argv, e.Cfg.NamePatterns)
			r.Ctx.VMName = name
			prefix := name
			if i := strings.IndexByte(name, '-'); i >= 0 {
				prefix = name[:i]
			}
			if name != "" && prefix != "" && e.Acc.Passwd.Exists(prefix) {
				return done(prefix, MethodMultipassName,
					"rule 2 (multipass VM): QEMU (multipassd parent: %v, data dir in argv: %v), instance %q, prefix %q is a local user", viaParent, viaDir, name, prefix)
			}
			return done(OwnerUnattributed, MethodUnattributed,
				"rule 2 (multipass VM): QEMU (multipassd parent: %v, data dir in argv: %v), instance %q, prefix %q is not a local user", viaParent, viaDir, name, prefix)
		}
		e.note(ev, "rule 2 (multipass VM): QEMU process but no multipassd parent and no multipass data dir in argv; skipped")
	} else {
		e.note(ev, "rule 2 (multipass VM): executable %q is not qemu-system-*; skipped", p.Exe)
	}

	// Rule 3: subuid.
	if owner, rng, ok := e.Acc.SubuidOwner(p.UID); ok {
		if ev != nil {
			ev.SubuidMatch = fmt.Sprintf("uid %d is in range %d-%d owned by %s", p.UID, rng.Start, rng.Start+rng.Count-1, owner)
		}
		return done(owner, MethodSubuid, "rule 3 (subuid): uid %d is in the subuid range of %s", p.UID, owner)
	}
	if ev != nil {
		ev.SubuidMatch = "no matching /etc/subuid range"
	}
	e.note(ev, "rule 3 (subuid): uid %d is not in any /etc/subuid range; skipped", p.UID)

	if p.UID == 0 {
		lu := p.LoginUID
		// Rule 4: sudo or su.
		if lu != nil && *lu != procfs.LoginUIDUnset && *lu != 0 {
			owner := e.Acc.Username(*lu)
			return done(owner, MethodLoginUID, "rule 4 (sudo/su): uid 0 with loginuid %d (%s)", *lu, owner)
		}
		// Rule 5: direct root login.
		if lu != nil && *lu == 0 {
			return done(OwnerRootShared, MethodRootShared, "rule 5 (direct root login): uid 0 with loginuid 0")
		}
		// Rule 6: system daemon.
		r.Ctx.Account = e.Acc.Username(0)
		return done(OwnerSystem, MethodSystem, "rule 6 (system): uid 0 with loginuid %s", loginString(lu))
	}
	if p.UID < e.Acc.UIDMin {
		r.Ctx.Account = e.Acc.Username(p.UID)
		return done(OwnerSystem, MethodSystem, "rule 6 (system): uid %d is below uid_min %d (account %s)", p.UID, e.Acc.UIDMin, r.Ctx.Account)
	}

	// Rule 7: regular user.
	owner := e.Acc.Username(p.UID)
	return done(owner, MethodUID, "rule 7 (regular user): uid %d resolves to %s", p.UID, owner)
}

func (e *Engine) dockerRule(cid string, r *Result, ev *Evidence) Result {
	short := cid[:12]
	var c *docker.Container
	var err error
	if e.Docker == nil {
		err = errors.New("Docker API is not configured")
	} else {
		ctx := e.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		c, err = e.Docker.Inspect(ctx, cid)
	}
	if err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			e.addErr("docker: container %s is not known to the rootful Docker daemon", short)
		} else {
			e.addErr("docker: cannot inspect container %s, falling back to the default owner: %v", short, err)
		}
		if ev != nil {
			ev.DockerError = err.Error()
		}
	} else {
		r.Ctx.ContainerName, r.Ctx.ContainerImage = c.Name, c.Image
		if ev != nil {
			ev.ContainerLabels = c.Labels
		}
		if v := strings.TrimSpace(c.Labels[e.Cfg.OwnerLabel]); v != "" {
			r.Owner, r.Method = logx.ASCII(v), MethodDockerLabel
			e.note(ev, "rule 1 (rootful Docker): container %s has label %s=%s", short, e.Cfg.OwnerLabel, v)
			return *r
		}
	}
	if e.Cfg.DefaultOwner != "" {
		r.Owner, r.Method = e.Cfg.DefaultOwner, MethodDockerDefaultOwner
		e.note(ev, "rule 1 (rootful Docker): container %s has no %q label; using docker.default_owner %s", short, e.Cfg.OwnerLabel, e.Cfg.DefaultOwner)
		return *r
	}
	r.Owner, r.Method = OwnerUnattributed, MethodUnattributed
	e.note(ev, "rule 1 (rootful Docker): container %s has no %q label and docker.default_owner is empty", short, e.Cfg.OwnerLabel)
	return *r
}

func (e *Engine) parentChain(p *procfs.Proc) []string {
	var chain []string
	seen := map[int]bool{p.PID: true}
	cur := p
	for i := 0; i < 64; i++ {
		parent, ok := e.Procs[cur.PPID]
		if !ok || seen[parent.PID] {
			break
		}
		seen[parent.PID] = true
		name := parent.Exe
		if parent.Comm == "multipassd" {
			name = parent.Comm
		}
		chain = append(chain, name)
		cur = parent
	}
	return chain
}

// SortedMethods returns the distinct methods in a stable order.
func SortedMethods(set map[Method]struct{}) []string {
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, string(m))
	}
	sort.Strings(out)
	return out
}
