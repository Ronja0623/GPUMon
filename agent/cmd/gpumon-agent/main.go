// Command gpumon-agent collects GPU, CPU, memory, and disk usage and pushes
// it to gpumon-central. It opens no listening sockets.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gpumon/agent/internal/agent"
	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/collect"
	"gpumon/agent/internal/config"
	"gpumon/agent/internal/diagnose"
	"gpumon/agent/internal/disk"
	"gpumon/agent/internal/docker"
	"gpumon/agent/internal/gpu"
	"gpumon/agent/internal/hostfs"
	"gpumon/agent/internal/logx"
	"gpumon/agent/internal/push"
	"gpumon/agent/internal/sim"
	"gpumon/agent/internal/token"
)

var version = "0.1.0"

const defaultConfig = "/etc/gpumon/agent.yaml"

const usage = `Usage: gpumon-agent <command> [flags]

Commands:
  run        Sample and push to central every interval (default command).
  diagnose   Print everything the agent detects, with attribution evidence.
  simulate   Push synthetic samples for one server of an inventory file.
  version    Print the agent version.

Run "gpumon-agent <command> -h" for the flags of a command.
`

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		os.Exit(runCmd(args))
	case "diagnose":
		os.Exit(diagnoseCmd(args))
	case "simulate":
		os.Exit(simulateCmd(args))
	case "version":
		fmt.Println(version)
	case "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func newLogger(cfg config.Config) *logx.Logger {
	lv, _ := logx.ParseLevel(cfg.LogLevel)
	l := logx.New(os.Stderr, lv, clock.Real{})
	l.SetWindow(time.Duration(cfg.ErrorLogIntervalMinutes * float64(time.Minute)))
	return l
}

func fatal(msg string, err error) int {
	logx.New(os.Stderr, logx.Error, clock.Real{}).Error(msg, "error", err)
	return 1
}

func serverID(cfg config.Config) string {
	if cfg.ServerID != "" {
		return cfg.ServerID
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return h
}

func hostDeps(cfg config.Config, log *logx.Logger) collect.Deps {
	var dc docker.Client
	if cfg.Docker.Enabled {
		dc = docker.NewCached(docker.NewUnixClient(cfg.Docker.Socket), clock.Real{})
	}
	return collect.Deps{FS: hostfs.OS{}, GPU: gpu.NVML{}, Docker: dc, Statfs: disk.OSStatfs, Clock: clock.Real{}, Log: log}
}

func interval(cfg config.Config) time.Duration {
	return time.Duration(cfg.IntervalSeconds * float64(time.Second))
}

func newAgent(cfg config.Config, col *collect.Collector, tok string, log *logx.Logger, fs disk.FS) *agent.Agent {
	iv := interval(cfg)
	prime := time.Second
	if iv < prime {
		prime = iv
	}
	return &agent.Agent{
		Col:            col,
		Pusher:         push.NewHTTP(cfg.CentralURL, tok, time.Duration(cfg.PushTimeoutSeconds*float64(time.Second))),
		Log:            log,
		Clock:          clock.Real{},
		Interval:       iv,
		InventoryEvery: time.Duration(cfg.InventoryIntervalHours * float64(time.Hour)),
		PrimeDelay:     prime,
		PushTimeout:    time.Duration(cfg.PushTimeoutSeconds * float64(time.Second)),
		Scanner: &disk.Scanner{FS: fs, Clock: clock.Real{},
			Timeout: time.Duration(cfg.Disk.ScanTimeoutMinutes * float64(time.Minute)), Lower: disk.LowerThreadPriority},
		ScanPaths:    cfg.Disk.ScanPaths,
		ScanTime:     cfg.Disk.ScanTimeLocal,
		ScanOnStart:  cfg.Disk.ScanOnStart,
		StatfsMounts: cfg.Disk.StatfsMounts,
	}
}

func runCmd(args []string) int {
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fl.String("config", defaultConfig, "path to agent.yaml")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fatal("refusing to start: invalid configuration", err)
	}
	log := newLogger(cfg)
	tok, err := token.Read(cfg.TokenFile)
	if err != nil {
		return fatal("refusing to start: token problem", err)
	}
	col := collect.New(cfg, hostDeps(cfg, log), serverID(cfg), agent.NewRunID(), version)
	col.PageSize = os.Getpagesize()
	ag := newAgent(cfg, col, tok, log, disk.OSFS{})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := ag.Run(ctx); err != nil {
		return fatal("agent stopped with an error", err)
	}
	return 0
}

func diagnoseCmd(args []string) int {
	fl := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	cfgPath := fl.String("config", defaultConfig, "path to agent.yaml (defaults are used when the file does not exist)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	path := *cfgPath
	if _, err := os.Stat(path); err != nil && path == defaultConfig {
		fmt.Fprintf(os.Stderr, "note: %s not found, using the default configuration\n", path)
		path = ""
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fatal("invalid configuration", err)
	}
	cfg.LogLevel = "error"
	log := newLogger(cfg)
	col := collect.New(cfg, hostDeps(cfg, log), serverID(cfg), "diagnose", version)
	col.PageSize = os.Getpagesize()
	col.Prime()
	time.Sleep(time.Second)
	d := col.Collect(true)
	diagnose.Print(os.Stdout, d, col.Inventory())
	return 0
}

func simulateCmd(args []string) int {
	fl := flag.NewFlagSet("simulate", flag.ContinueOnError)
	invPath := fl.String("inventory", "", "inventory file describing the simulated servers (required)")
	id := fl.String("server-id", "", "server to simulate (required)")
	cfgPath := fl.String("config", "", "optional agent.yaml for defaults")
	url := fl.String("central-url", "", "override central_url")
	tokFile := fl.String("token-file", "", "override token_file")
	iv := fl.Float64("interval-seconds", 0, "override interval_seconds")
	users := fl.Int("users", 0, "number of background users (user01..userNN)")
	seed := fl.Int64("seed", 1, "random seed")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *invPath == "" || *id == "" {
		fmt.Fprint(os.Stderr, "simulate needs --inventory and --server-id\n")
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fatal("invalid configuration", err)
	}
	if *url != "" {
		cfg.CentralURL = *url
	}
	if *tokFile != "" {
		cfg.TokenFile = *tokFile
	}
	if *iv > 0 {
		cfg.IntervalSeconds = *iv
	}
	inv, err := sim.LoadInventory(*invPath)
	if err != nil {
		return fatal("cannot load the inventory", err)
	}
	srv, err := inv.Find(*id)
	if err != nil {
		return fatal("cannot simulate", err)
	}
	cfg.ServerID = srv.ID
	cfg.Docker.DefaultOwner = srv.DockerDefaultOwner
	cfg.Disk.ScanOnStart = true
	if err := cfg.Validate(); err != nil {
		return fatal("invalid configuration", err)
	}
	log := newLogger(cfg)
	tok, err := token.Read(cfg.TokenFile)
	if err != nil {
		return fatal("refusing to start: token problem", err)
	}
	world := sim.NewWorld(srv, *users, *seed+int64(len(srv.ID)), time.Now())
	deps := collect.Deps{FS: world.FS, GPU: world.GPU, Docker: world.Docker, Statfs: world.Statfs, Clock: clock.Real{}, Log: log}
	col := collect.New(cfg, deps, srv.ID, agent.NewRunID(), version+"-sim")
	ag := newAgent(cfg, col, tok, log, world.DiskFS)
	ag.Scanner.Lower = nil
	ag.BeforeSample = func() { world.Advance(time.Now()) }
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := ag.Run(ctx); err != nil {
		return fatal("simulation stopped with an error", err)
	}
	return 0
}
