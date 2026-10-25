package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/collect"
	"gpumon/agent/internal/config"
	"gpumon/agent/internal/disk"
	"gpumon/agent/internal/fixture"
	"gpumon/agent/internal/logx"
	"gpumon/agent/internal/payload"
	"gpumon/agent/internal/sim"
)

var update = flag.Bool("update", false, "regenerate schema/examples from the simulator")

var t0 = time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC)

func schemaPath() string    { return filepath.Join(fixture.RepoRoot(), "schema", "payload.schema.json") }
func examplesDir() string   { return filepath.Join(fixture.RepoRoot(), "schema", "examples") }
func inventoryPath() string { return filepath.Join(fixture.Dir(), "sim-inventory.yaml") }

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft7
	s, err := c.Compile(schemaPath())
	if err != nil {
		t.Fatalf("schema does not compile: %v", err)
	}
	return s
}

func validate(t *testing.T, s *jsonschema.Schema, b []byte) error {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return s.Validate(v)
}

// generate produces deterministic messages from the simulator.
func generate(t *testing.T) map[string]any {
	t.Helper()
	inv, err := sim.LoadInventory(inventoryPath())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, id := range []string{"srv-40", "srv-41", "srv-42", "srv-43"} {
		srv, err := inv.Find(id)
		if err != nil {
			t.Fatal(err)
		}
		clk := clock.NewFake(t0)
		w := sim.NewWorld(srv, 3, 42, t0)
		cfg := config.Default()
		cfg.Docker.DefaultOwner = srv.DockerDefaultOwner
		col := collect.New(cfg, collect.Deps{FS: w.FS, GPU: w.GPU, Docker: w.Docker, Statfs: w.Statfs, Clock: clk,
			Log: logx.New(&bytes.Buffer{}, logx.Error, clk)}, id, "example0000000001", "0.1.0-sim")
		col.Prime()
		clk.Advance(30 * time.Second)
		w.Advance(clk.Now())
		out["sample-"+id] = col.Sample()
		if id == "srv-41" {
			out["inventory-"+id] = col.Inventory()
		}
		if id == "srv-43" {
			sc := disk.Scanner{FS: w.DiskFS, Owner: col.Accounts().FileOwner, Clock: clk, Timeout: time.Hour}
			res, errs := sc.Scan(context.Background(), []string{"/home"}, func(p string) string { return disk.MountOf(p, cfg.Disk.StatfsMounts) })
			out["disk_scan-"+id] = col.DiskScanMessage(res, errs)
		}
	}
	return out
}

func marshal(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

// Verifies: REQ-ATT-006, REQ-AGT-011
func TestContractGeneratedPayloadsMatchSchemaAndExamples(t *testing.T) {
	s := compileSchema(t)
	msgs := generate(t)
	if *update {
		_ = os.MkdirAll(examplesDir(), 0o755)
	}
	for name, m := range msgs {
		b := marshal(m)
		if err := validate(t, s, b); err != nil {
			t.Errorf("%s does not validate: %v", name, err)
		}
		path := filepath.Join(examplesDir(), name+".json")
		if *update {
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing example %s (run go test ./internal/contract -update): %v", path, err)
		}
		if !bytes.Equal(want, b) {
			t.Errorf("%s differs from the generated payload; the agent payload changed (regenerate with -update and update central)", path)
		}
	}
	// Attribution is computed on the agent and carried in the payload.
	sample := string(marshal(msgs["sample-srv-41"]))
	for _, want := range []string{`"owner": "carol"`, `"attribution": "docker_label"`, `"container_name": "gpu0-job0"`, `"attribution": "loginuid"`} {
		if !strings.Contains(sample, want) {
			t.Errorf("sample-srv-41 lacks %s", want)
		}
	}
}

// Verifies: REQ-AGT-011
func TestContractExamplesDecodeStrictlyIntoGoTypes(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(examplesDir(), "*.json"))
	if len(files) < 6 {
		t.Fatalf("expected at least 6 examples, found %d", len(files))
	}
	s := compileSchema(t)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if err := validate(t, s, b); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		var target any
		switch {
		case strings.Contains(f, "sample-"):
			target = &payload.Sample{}
		case strings.Contains(f, "inventory-"):
			target = &payload.InventoryMsg{}
		case strings.Contains(f, "disk_scan-"):
			target = &payload.DiskScanMsg{}
		default:
			t.Fatalf("unknown example kind %s", f)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(target); err != nil {
			t.Errorf("%s does not match the Go payload types: %v", f, err)
		}
	}
}

// Verifies: REQ-AGT-011
func TestContractSchemaRejectsDrift(t *testing.T) {
	s := compileSchema(t)
	base := marshal(generate(t)["sample-srv-41"])
	mutations := map[string]func(m map[string]any){
		"missing schema_version": func(m map[string]any) { delete(m, "schema_version") },
		"wrong schema_version":   func(m map[string]any) { m["schema_version"] = 2 },
		"unknown top-level key":  func(m map[string]any) { m["cmdline"] = "python train.py" },
		"bad attribution": func(m map[string]any) {
			m["gpus"].([]any)[0].(map[string]any)["processes"].([]any)[0].(map[string]any)["attribution"] = "guess"
		},
		"process argv field": func(m map[string]any) {
			m["users"].([]any)[0].(map[string]any)["top_cpu"].([]any)[0].(map[string]any)["cmdline"] = "x"
		},
		"timestamp without milliseconds": func(m map[string]any) { m["timestamp"] = "2026-10-25T06:00:30Z" },
	}
	for name, mutate := range mutations {
		var m map[string]any
		_ = json.Unmarshal(base, &m)
		mutate(m)
		b, _ := json.Marshal(m)
		if err := validate(t, s, b); err == nil {
			t.Errorf("%s: schema accepted an invalid payload", name)
		}
	}
}
