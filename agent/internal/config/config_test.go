package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gpumon/agent/internal/config"
	"gpumon/agent/internal/fixture"
)

func TestREQ_OPS_005_InvalidConfigNamesTheKey(t *testing.T) {
	cases := map[string]string{
		"interval_seconds: 0\n":                        `key "interval_seconds"`,
		"interval_seconds: soon\n":                     `key "interval_seconds": invalid value "soon"`,
		"intervall_seconds: 30\n":                      `unknown key "intervall_seconds"`,
		"docker:\n  sockett: /x\n":                     `unknown key "docker.sockett"`,
		"disk:\n  scan_time_local: '25:99'\n":          `key "disk.scan_time_local"`,
		"disk:\n  scan_timeout_minutes: -1\n":          `key "disk.scan_timeout_minutes"`,
		"multipass:\n  name_patterns: ['(unclosed']\n": `key "multipass.name_patterns[0]"`,
		"multipass:\n  name_patterns: ['no-group']\n":  `key "multipass.name_patterns[0]": must contain a capture group`,
		"central_url: ftp://x\n":                       `key "central_url"`,
		"log_level: verbose\n":                         `key "log_level"`,
		"top_processes_per_owner: [1]\n":               `key "top_processes_per_owner"`,
		"uid_min: -5\n":                                `key "uid_min"`,
		"disk:\n  statfs_mounts: [relative/path]\n":    `key "disk.statfs_mounts[0]"`,
	}
	for in, want := range cases {
		_, err := config.Parse([]byte(in))
		if err == nil {
			t.Errorf("%q: expected an error", in)
			continue
		}
		if !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), "config error: ") {
			t.Errorf("%q: error %q does not contain %q", in, err, want)
		}
	}
}

func TestREQ_OPS_005_DefaultsMatchSpecification(t *testing.T) {
	c, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerID != "" || c.TokenFile != "/etc/gpumon/token" || c.IntervalSeconds != 30 || c.TopProcessesPerOwner != 5 ||
		c.UIDMin != nil || c.Docker.Socket != "/var/run/docker.sock" || c.Docker.OwnerLabel != "owner" || c.Docker.DefaultOwner != "" ||
		strings.Join(c.Disk.StatfsMounts, ",") != "/,/home" || c.Disk.ScanTimeLocal != "03:30" || c.Disk.ScanTimeoutMinutes != 120 ||
		c.LogLevel != "info" || len(c.Multipass.NamePatterns) != 2 {
		t.Fatalf("defaults = %+v", c)
	}
	c, err = config.Parse([]byte("uid_min: 500\n"))
	if err != nil || c.UIDMin == nil || *c.UIDMin != 500 {
		t.Fatalf("uid_min override: %v %v", c.UIDMin, err)
	}
}

func yamlKeys(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		k := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if t.Field(i).Type.Kind() == reflect.Struct {
			out = append(out, yamlKeys(t.Field(i).Type, prefix+k+".")...)
			continue
		}
		out = append(out, prefix+k)
	}
	return out
}

// Verifies: REQ-OPS-005
func TestExampleConfigDocumentsEveryKey(t *testing.T) {
	path := filepath.Join(fixture.RepoRoot(), "deploy", "agent.yaml.example")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, k := range yamlKeys(reflect.TypeOf(config.Config{}), "") {
		leaf := k[strings.LastIndex(k, ".")+1:]
		if !strings.Contains(text, leaf+":") {
			t.Errorf("key %s is not documented in deploy/agent.yaml.example", k)
		}
	}
	doc := filepath.Join(fixture.RepoRoot(), "docs", "configuration.md")
	if d, err := os.ReadFile(doc); err == nil {
		for _, k := range yamlKeys(reflect.TypeOf(config.Config{}), "") {
			if !strings.Contains(string(d), "`"+k+"`") {
				t.Errorf("key %s is not in docs/configuration.md", k)
			}
		}
	} else {
		t.Errorf("docs/configuration.md missing: %v", err)
	}
	// The example parses once the placeholder URL is filled in.
	filled := strings.Replace(text, "<CENTRAL_HOST>", "192.0.2.10", 1)
	if _, err := config.Parse([]byte(filled)); err != nil {
		t.Fatalf("example config does not parse: %v", err)
	}
}
