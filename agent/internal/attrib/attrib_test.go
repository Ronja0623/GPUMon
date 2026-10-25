package attrib_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"gpumon/agent/internal/accounts"
	"gpumon/agent/internal/attrib"
	"gpumon/agent/internal/config"
	"gpumon/agent/internal/docker"
	"gpumon/agent/internal/fixture"
	"gpumon/agent/internal/hostfs"
	"gpumon/agent/internal/logx"
	"gpumon/agent/internal/procfs"
)

const (
	labeledID   = "4f2a9c3e5b7d1f0a8c6e4b2d9f7a5c3e1b0d8f6a4c2e0b9d7f5a3c1e8b6d4f2a"
	unlabeledID = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3c2b1a0f9e8d"
)

// fakeTransport serves the Docker inspect fixtures and records every request.
type fakeTransport struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	down    bool
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.methods = append(f.methods, r.Method)
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	if f.down {
		return nil, errors.New("dial unix /var/run/docker.sock: connect: no such file or directory")
	}
	body, code := `{"message":"No such container"}`, http.StatusNotFound
	switch r.URL.Path {
	case "/containers/" + labeledID + "/json":
		body, code = string(fixture.Read("docker/inspect_labeled.json")), http.StatusOK
	case "/containers/" + unlabeledID + "/json":
		body, code = string(fixture.Read("docker/inspect_unlabeled.json")), http.StatusOK
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

type env struct {
	fs    *hostfs.MapFS
	procs map[int]*procfs.Proc
	eng   *attrib.Engine
	tr    *fakeTransport
	errs  *logx.ErrorSet
}

func newEnv(t *testing.T, defaultOwner string) *env {
	t.Helper()
	fs := fixture.LoadSet("procs.yaml").Build()
	acc, _, _ := accounts.Load(fs, nil)
	pids, err := procfs.ListPIDs(fs)
	if err != nil {
		t.Fatal(err)
	}
	procs := map[int]*procfs.Proc{}
	for _, pid := range pids {
		p, err := procfs.ReadProc(fs, pid, 4096)
		if err != nil {
			t.Fatal(err)
		}
		procs[pid] = p
	}
	cfg := config.Default()
	cfg.Docker.DefaultOwner = defaultOwner
	tr := &fakeTransport{}
	errs := logx.NewErrorSet()
	eng := &attrib.Engine{
		Cfg: cfg.AttribConfig(), Acc: acc, Docker: docker.NewClientWithTransport(tr), Procs: procs,
		Cmdline: func(pid int) ([]string, error) { return procfs.ReadCmdline(fs, pid) },
		Errs:    errs, Ctx: context.Background(),
	}
	return &env{fs: fs, procs: procs, eng: eng, tr: tr, errs: errs}
}

func (e *env) attr(t *testing.T, pid int) attrib.Result {
	t.Helper()
	p, ok := e.procs[pid]
	if !ok {
		t.Fatalf("no fixture process %d", pid)
	}
	return e.eng.Attribute(p)
}

func expect(t *testing.T, r attrib.Result, owner string, m attrib.Method) {
	t.Helper()
	if r.Owner != owner || r.Method != m {
		t.Fatalf("got owner=%q method=%q, want owner=%q method=%q", r.Owner, r.Method, owner, m)
	}
}

func TestREQ_ATT_002_Rule1_RootfulDockerLabel_CgroupV1(t *testing.T) {
	e := newEnv(t, "dave")
	r := e.attr(t, 200)
	expect(t, r, "carol", attrib.MethodDockerLabel)
	if r.Ctx.ContainerName != "carol-train" || r.Ctx.ContainerImage != "pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime" || r.Ctx.ContainerID != labeledID[:12] {
		t.Fatalf("unexpected container context %+v", r.Ctx)
	}
}

func TestREQ_ATT_002_Rule1_RootfulDockerDefaultOwner_CgroupV2(t *testing.T) {
	e := newEnv(t, "dave")
	r := e.attr(t, 201)
	expect(t, r, "dave", attrib.MethodDockerDefaultOwner)
	if r.Ctx.ContainerName != "nightly-eval" {
		t.Fatalf("container name = %q", r.Ctx.ContainerName)
	}
}

func TestREQ_ATT_002_Rule1_RootfulDockerNoLabelNoDefault(t *testing.T) {
	e := newEnv(t, "")
	expect(t, e.attr(t, 201), attrib.OwnerUnattributed, attrib.MethodUnattributed)
}

func TestREQ_ATT_002_Rule1_CgroupFormats(t *testing.T) {
	for _, f := range []string{"cgroup/v1_docker.txt", "cgroup/v2_docker_systemd.txt", "cgroup/v2_docker_cgroupfs.txt"} {
		id, _, rootless := attrib.FindContainerID(string(fixture.Read(f)))
		if len(id) != 64 || rootless {
			t.Errorf("%s: id=%q rootless=%v", f, id, rootless)
		}
	}
	if id, _, rootless := attrib.FindContainerID(string(fixture.Read("cgroup/v2_rootless.txt"))); id == "" || !rootless {
		t.Errorf("rootless cgroup: id=%q rootless=%v", id, rootless)
	}
	if id, _, _ := attrib.FindContainerID(string(fixture.Read("cgroup/v2_session.txt"))); id != "" {
		t.Errorf("session cgroup should not contain a container id, got %q", id)
	}
}

func TestREQ_ATT_002_Rule2_MultipassName(t *testing.T) {
	e := newEnv(t, "")
	r := e.attr(t, 501)
	expect(t, r, "frank", attrib.MethodMultipassName)
	if r.Ctx.VMName != "frank-dev" {
		t.Fatalf("vm_name = %q", r.Ctx.VMName)
	}
}

func TestREQ_ATT_002_Rule2_MultipassInvalidPrefixKeepsVMName(t *testing.T) {
	e := newEnv(t, "")
	r := e.attr(t, 502)
	expect(t, r, attrib.OwnerUnattributed, attrib.MethodUnattributed)
	if r.Ctx.VMName != "testvm-1" {
		t.Fatalf("vm_name = %q", r.Ctx.VMName)
	}
}

func TestREQ_ATT_002_Rule2_MultipassDataDirAndNameFlag(t *testing.T) {
	e := newEnv(t, "")
	r := e.attr(t, 503)
	expect(t, r, "alice", attrib.MethodMultipassName)
	if r.Ctx.VMName != "alice-gpu-test" {
		t.Fatalf("vm_name = %q", r.Ctx.VMName)
	}
	// A QEMU process that is not multipass falls through to the later rules.
	expect(t, e.attr(t, 504), attrib.OwnerSystem, attrib.MethodSystem)
}

func TestREQ_ATT_002_Rule3_Subuid(t *testing.T) {
	e := newEnv(t, "")
	expect(t, e.attr(t, 301), "erin", attrib.MethodSubuid)
}

func TestREQ_ATT_002_Rule4_SudoLoginuid(t *testing.T) {
	e := newEnv(t, "")
	expect(t, e.attr(t, 400), "alice", attrib.MethodLoginUID)
}

func TestREQ_ATT_002_Rule5_DirectRootLogin(t *testing.T) {
	e := newEnv(t, "")
	expect(t, e.attr(t, 401), attrib.OwnerRootShared, attrib.MethodRootShared)
}

func TestREQ_ATT_002_Rule6_System(t *testing.T) {
	e := newEnv(t, "")
	r := e.attr(t, 402)
	expect(t, r, attrib.OwnerSystem, attrib.MethodSystem)
	if r.Ctx.Account != "root" {
		t.Fatalf("account = %q, want root", r.Ctx.Account)
	}
	r = e.attr(t, 403) // UID below uid_min
	expect(t, r, attrib.OwnerSystem, attrib.MethodSystem)
	if r.Ctx.Account != "postgres" {
		t.Fatalf("account = %q, want postgres", r.Ctx.Account)
	}
}

func TestREQ_ATT_002_Rule7_RegularUser(t *testing.T) {
	e := newEnv(t, "")
	expect(t, e.attr(t, 701), "alice", attrib.MethodUID)
	// Rootless Docker container root maps to the user's own UID.
	r := e.attr(t, 300)
	expect(t, r, "erin", attrib.MethodUID)
	if r.Ctx.ContainerID == "" {
		t.Fatal("rootless container id should be kept as context")
	}
}

// Verifies: REQ-ATT-002
func TestRuleOrderConflicts(t *testing.T) {
	e := newEnv(t, "dave")
	login := func(v uint32) *uint32 { return &v }
	v1 := string(fixture.Read("cgroup/v1_docker.txt"))
	rootless := string(fixture.Read("cgroup/v2_rootless.txt"))
	argv := map[int][]string{
		9001: strings.Split(strings.TrimSpace(string(fixture.Read("qemu/argv_named.txt"))), "\n"),
	}
	e.eng.Cmdline = func(pid int) ([]string, error) { return argv[pid], nil }
	cases := []struct {
		name   string
		p      procfs.Proc
		owner  string
		method attrib.Method
	}{
		{"docker beats sudo loginuid", procfs.Proc{PID: 9000, PPID: 100, UID: 0, LoginUID: login(1001), Cgroup: v1}, "carol", attrib.MethodDockerLabel},
		{"docker beats direct root login", procfs.Proc{PID: 9000, UID: 0, LoginUID: login(0), Cgroup: v1}, "carol", attrib.MethodDockerLabel},
		{"docker beats subuid", procfs.Proc{PID: 9000, UID: 232071, LoginUID: login(procfs.LoginUIDUnset), Cgroup: v1}, "carol", attrib.MethodDockerLabel},
		{"multipass beats sudo loginuid", procfs.Proc{PID: 9001, PPID: 500, Exe: "qemu-system-x86_64", UID: 0, LoginUID: login(1002)}, "frank", attrib.MethodMultipassName},
		{"rootless docker cgroup does not use the rootful rule", procfs.Proc{PID: 9002, UID: 232071, Cgroup: rootless}, "erin", attrib.MethodSubuid},
		{"subuid beats regular uid", procfs.Proc{PID: 9003, UID: 100005}, "alice", attrib.MethodSubuid},
		{"sudo beats system for uid 0", procfs.Proc{PID: 9004, UID: 0, LoginUID: login(1005)}, "carol", attrib.MethodLoginUID},
		{"system account with a loginuid stays system", procfs.Proc{PID: 9005, UID: 115, LoginUID: login(1001)}, attrib.OwnerSystem, attrib.MethodSystem},
		{"uid 0 with unreadable loginuid is system", procfs.Proc{PID: 9006, UID: 0}, attrib.OwnerSystem, attrib.MethodSystem},
		{"sudo with unknown loginuid", procfs.Proc{PID: 9007, UID: 0, LoginUID: login(4243)}, "uid:4243", attrib.MethodLoginUID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.p
			if p.Exe == "" {
				p.Exe = "python3"
			}
			expect(t, e.eng.Attribute(&p), c.owner, c.method)
		})
	}
}

var validMethods = map[attrib.Method]bool{
	"uid": true, "loginuid": true, "subuid": true, "docker_label": true, "docker_default_owner": true,
	"multipass_name": true, "system": true, "root_shared": true, "unattributed": true,
}

func TestREQ_ATT_001_OutputFields(t *testing.T) {
	e := newEnv(t, "")
	specials := map[string]bool{}
	for pid, p := range e.procs {
		if p.IsKernelThread() {
			continue
		}
		r := e.eng.Attribute(p)
		if r.Owner == "" || !validMethods[r.Method] {
			t.Errorf("pid %d: invalid result %+v", pid, r)
		}
		specials[r.Owner] = true
	}
	for _, s := range []string{"root-shared", "system", "unattributed", "uid:4242"} {
		if !specials[s] {
			t.Errorf("special owner %q not produced by the fixtures", s)
		}
	}
	expect(t, e.attr(t, 600), "uid:4242", attrib.MethodUID)
}

func TestREQ_ATT_003_SameUsernameDifferentUIDs(t *testing.T) {
	a := accounts.ParsePasswd(fixture.Read("etc/passwd_srv_a"))
	b := accounts.ParsePasswd(fixture.Read("etc/passwd_srv_b"))
	attribute := func(pw *accounts.Passwd, uid uint32) string {
		e := &attrib.Engine{Acc: &accounts.Accounts{Passwd: pw, UIDMin: 1000}, Procs: map[int]*procfs.Proc{}}
		return e.Attribute(&procfs.Proc{PID: 10, UID: uid, Exe: "python3"}).Owner
	}
	if got := attribute(a, 1001); got != "alice" {
		t.Fatalf("server A uid 1001 = %q", got)
	}
	if got := attribute(b, 1010); got != "alice" {
		t.Fatalf("server B uid 1010 = %q", got)
	}
	// The same UID means different people on the two servers.
	if got := attribute(b, 1001); got != "bob" {
		t.Fatalf("server B uid 1001 = %q", got)
	}
}

func TestREQ_ATT_005_SubuidFormats(t *testing.T) {
	ranges, warnings := accounts.ParseSubuid(fixture.Read("etc/subuid"))
	if len(ranges) != 4 {
		t.Fatalf("got %d ranges, want 4: %+v", len(ranges), ranges)
	}
	if len(warnings) != 2 {
		t.Fatalf("got %d warnings, want 2: %v", len(warnings), warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "skipping malformed line") {
			t.Errorf("unexpected warning %q", w)
		}
	}
	acc := &accounts.Accounts{Passwd: accounts.ParsePasswd(fixture.Read("etc/passwd_srv_a")), Subuid: ranges, UIDMin: 1000}
	cases := map[uint32]string{100000: "alice", 165535: "alice", 165536: "bob", 232071: "erin", 296608: "frank"}
	for uid, want := range cases {
		got, _, ok := acc.SubuidOwner(uid)
		if !ok || got != want {
			t.Errorf("uid %d: got %q (%v), want %q", uid, got, ok, want)
		}
	}
	if _, _, ok := acc.SubuidOwner(99999); ok {
		t.Error("uid 99999 should not match")
	}
}

func TestREQ_ATT_007_DockerUnreachableFallsBack(t *testing.T) {
	e := newEnv(t, "dave")
	e.tr.down = true
	expect(t, e.attr(t, 200), "dave", attrib.MethodDockerDefaultOwner)
	e.eng.Cfg.DefaultOwner = ""
	expect(t, e.attr(t, 200), attrib.OwnerUnattributed, attrib.MethodUnattributed)
	found := false
	for _, m := range e.errs.List() {
		if strings.Contains(m, "docker: cannot inspect container 4f2a9c3e5b7d") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing English docker error, got %v", e.errs.List())
	}
	// No Docker client at all behaves the same way.
	e.eng.Docker = nil
	e.eng.Cfg.DefaultOwner = "dave"
	expect(t, e.attr(t, 200), "dave", attrib.MethodDockerDefaultOwner)
}

func TestREQ_SEC_005_DockerClientOnlyIssuesGET(t *testing.T) {
	e := newEnv(t, "dave")
	for pid := range e.procs {
		if !e.procs[pid].IsKernelThread() {
			e.attr(t, pid)
		}
	}
	if len(e.tr.methods) == 0 {
		t.Fatal("expected Docker API calls")
	}
	for i, m := range e.tr.methods {
		if m != http.MethodGet {
			t.Errorf("request %d to %s used method %s", i, e.tr.paths[i], m)
		}
		if !strings.HasPrefix(e.tr.paths[i], "/containers/") {
			t.Errorf("unexpected path %s", e.tr.paths[i])
		}
	}
	// Labels and the image are parsed from the inspect fixture; the
	// command line and environment in it are never copied.
	c, err := docker.NewClientWithTransport(e.tr).Inspect(context.Background(), labeledID)
	if err != nil || c.Labels["owner"] != "carol" {
		t.Fatalf("inspect: %+v %v", c, err)
	}
	if strings.Contains(strings.Join([]string{c.Name, c.Image}, " "), "SECRET123") {
		t.Fatal("secret leaked from the inspect JSON")
	}
}

func TestExtractVMNamePatterns(t *testing.T) {
	cfg := config.Default()
	pats := cfg.AttribConfig().NamePatterns
	cases := map[string]string{
		"qemu -drive file=/x/instances/bob-1/disk.img": "bob-1",
		"qemu -name guest=carol-vm,debug-threads=on":   "carol-vm",
		"qemu -name dave-box":                          "dave-box",
		"qemu -m 4096":                                 "",
	}
	for argv, want := range cases {
		if got := attrib.ExtractVMName(strings.Fields(argv), pats); got != want {
			t.Errorf("%q: got %q want %q", argv, got, want)
		}
	}
}
