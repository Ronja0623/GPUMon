// Package config loads and validates agent.yaml. Every key has a default;
// an invalid file produces an English error that names the key.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"gpumon/agent/internal/attrib"
	"gpumon/agent/internal/logx"
)

// DockerConfig controls rootful Docker attribution.
type DockerConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Socket       string `yaml:"socket"`
	OwnerLabel   string `yaml:"owner_label"`
	DefaultOwner string `yaml:"default_owner"`
}

// MultipassConfig controls multipass VM attribution.
type MultipassConfig struct {
	NamePatterns []string `yaml:"name_patterns"`
	DataDirs     []string `yaml:"data_dirs"`
}

// DiskConfig controls statfs reporting and the daily per-owner scan.
type DiskConfig struct {
	StatfsMounts       []string `yaml:"statfs_mounts"`
	ScanPaths          []string `yaml:"scan_paths"`
	ScanTimeLocal      string   `yaml:"scan_time_local"`
	ScanTimeoutMinutes float64  `yaml:"scan_timeout_minutes"`
	ScanOnStart        bool     `yaml:"scan_on_start"`
}

// Config is the agent configuration.
type Config struct {
	ServerID                string          `yaml:"server_id"`
	CentralURL              string          `yaml:"central_url"`
	TokenFile               string          `yaml:"token_file"`
	IntervalSeconds         float64         `yaml:"interval_seconds"`
	TopProcessesPerOwner    int             `yaml:"top_processes_per_owner"`
	UIDMin                  *uint32         `yaml:"uid_min"`
	InventoryIntervalHours  float64         `yaml:"inventory_interval_hours"`
	PushTimeoutSeconds      float64         `yaml:"push_timeout_seconds"`
	ErrorLogIntervalMinutes float64         `yaml:"error_log_interval_minutes"`
	Docker                  DockerConfig    `yaml:"docker"`
	Multipass               MultipassConfig `yaml:"multipass"`
	Disk                    DiskConfig      `yaml:"disk"`
	LogLevel                string          `yaml:"log_level"`
}

// Default returns the documented defaults.
func Default() Config {
	return Config{
		ServerID:                "",
		CentralURL:              "http://127.0.0.1:8080/api/v1/ingest",
		TokenFile:               "/etc/gpumon/token",
		IntervalSeconds:         30,
		TopProcessesPerOwner:    5,
		UIDMin:                  nil,
		InventoryIntervalHours:  6,
		PushTimeoutSeconds:      5,
		ErrorLogIntervalMinutes: 60,
		Docker: DockerConfig{
			Enabled:    true,
			Socket:     "/var/run/docker.sock",
			OwnerLabel: "owner",
		},
		Multipass: MultipassConfig{
			NamePatterns: append([]string(nil), attrib.DefaultNamePatterns...),
			DataDirs:     append([]string(nil), attrib.DefaultDataDirs...),
		},
		Disk: DiskConfig{
			StatfsMounts:       []string{"/", "/home"},
			ScanPaths:          []string{"/home"},
			ScanTimeLocal:      "03:30",
			ScanTimeoutMinutes: 120,
		},
		LogLevel: "info",
	}
}

// Load reads path. An empty path returns the defaults.
func Load(p string) (Config, error) {
	cfg := Default()
	if p == "" {
		return cfg, cfg.Validate()
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cfg, fmt.Errorf("config error: cannot read %s: %v", p, err)
	}
	return Parse(b)
}

// Parse decodes YAML content over the defaults and validates it.
func Parse(b []byte) (Config, error) {
	cfg := Default()
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return cfg, fmt.Errorf("config error: invalid YAML: %v", err)
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		if err := decodeInto(doc.Content[0], reflect.ValueOf(&cfg).Elem(), ""); err != nil {
			return cfg, err
		}
	}
	return cfg, cfg.Validate()
}

func typeDesc(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Ptr:
		return typeDesc(t.Elem())
	case reflect.Float64:
		return "a number"
	case reflect.Int:
		return "an integer"
	case reflect.Uint32:
		return "a non-negative integer"
	case reflect.Bool:
		return "true or false"
	case reflect.String:
		return "a string"
	case reflect.Slice:
		return "a list of strings"
	}
	return t.String()
}

func decodeInto(node *yaml.Node, v reflect.Value, prefix string) error {
	where := prefix
	if where == "" {
		where = "(top level)"
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("config error: key %q: expected a mapping", where)
	}
	t := v.Type()
	fields := map[string]int{}
	for i := 0; i < t.NumField(); i++ {
		fields[strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]] = i
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, val := node.Content[i].Value, node.Content[i+1]
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		idx, ok := fields[k]
		if !ok {
			return fmt.Errorf("config error: unknown key %q (line %d)", full, node.Content[i].Line)
		}
		fv := v.Field(idx)
		if fv.Kind() == reflect.Struct {
			if err := decodeInto(val, fv, full); err != nil {
				return err
			}
			continue
		}
		if val.Kind == yaml.ScalarNode && val.Tag == "!!null" {
			fv.Set(reflect.Zero(fv.Type()))
			continue
		}
		tmp := reflect.New(fv.Type())
		if err := val.Decode(tmp.Interface()); err != nil {
			return fmt.Errorf("config error: key %q: invalid value %q (expected %s)", full, val.Value, typeDesc(fv.Type()))
		}
		fv.Set(tmp.Elem())
	}
	return nil
}

var hhmm = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// Validate checks value ranges.
func (c *Config) Validate() error {
	bad := func(key, why string) error { return fmt.Errorf("config error: key %q: %s", key, why) }
	if c.IntervalSeconds <= 0 {
		return bad("interval_seconds", "must be greater than 0")
	}
	u, err := url.Parse(c.CentralURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return bad("central_url", "must be an http:// or https:// URL")
	}
	if c.TokenFile == "" {
		return bad("token_file", "must not be empty")
	}
	if c.TopProcessesPerOwner < 0 || c.TopProcessesPerOwner > 100 {
		return bad("top_processes_per_owner", "must be between 0 and 100")
	}
	if c.InventoryIntervalHours <= 0 {
		return bad("inventory_interval_hours", "must be greater than 0")
	}
	if c.PushTimeoutSeconds <= 0 {
		return bad("push_timeout_seconds", "must be greater than 0")
	}
	if c.ErrorLogIntervalMinutes < 0 {
		return bad("error_log_interval_minutes", "must not be negative")
	}
	if c.Docker.Enabled && c.Docker.Socket == "" {
		return bad("docker.socket", "must not be empty when docker.enabled is true")
	}
	if c.Docker.OwnerLabel == "" {
		return bad("docker.owner_label", "must not be empty")
	}
	for i, p := range c.Multipass.NamePatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return bad(fmt.Sprintf("multipass.name_patterns[%d]", i), "invalid regular expression: "+err.Error())
		}
		if re.NumSubexp() < 1 {
			return bad(fmt.Sprintf("multipass.name_patterns[%d]", i), "must contain a capture group for the instance name")
		}
	}
	for i, m := range c.Disk.StatfsMounts {
		if !path.IsAbs(m) {
			return bad(fmt.Sprintf("disk.statfs_mounts[%d]", i), "must be an absolute path")
		}
	}
	for i, m := range c.Disk.ScanPaths {
		if !path.IsAbs(m) {
			return bad(fmt.Sprintf("disk.scan_paths[%d]", i), "must be an absolute path")
		}
	}
	if !hhmm.MatchString(c.Disk.ScanTimeLocal) {
		return bad("disk.scan_time_local", `must be a 24-hour time "HH:MM"`)
	}
	if c.Disk.ScanTimeoutMinutes <= 0 {
		return bad("disk.scan_timeout_minutes", "must be greater than 0")
	}
	if _, err := logx.ParseLevel(c.LogLevel); err != nil {
		return bad("log_level", "must be one of debug, info, warn, error")
	}
	return nil
}

// AttribConfig converts the config to the attribution engine settings.
func (c *Config) AttribConfig() attrib.Config {
	ac := attrib.Config{
		OwnerLabel:   c.Docker.OwnerLabel,
		DefaultOwner: c.Docker.DefaultOwner,
		DataDirs:     c.Multipass.DataDirs,
	}
	for _, p := range c.Multipass.NamePatterns {
		ac.NamePatterns = append(ac.NamePatterns, regexp.MustCompile(p))
	}
	return ac
}
