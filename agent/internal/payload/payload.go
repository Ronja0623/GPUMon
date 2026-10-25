// Package payload defines the agent-to-central messages. The JSON Schema in
// schema/payload.schema.json is the authoritative contract; tests validate
// these types against it.
package payload

import "time"

// SchemaVersion is sent in every message.
const SchemaVersion = 1

// Message types.
const (
	TypeSample    = "sample"
	TypeInventory = "inventory"
	TypeDiskScan  = "disk_scan"
)

// FormatTime returns a UTC RFC 3339 timestamp with millisecond precision.
func FormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Header is common to every message.
type Header struct {
	SchemaVersion int    `json:"schema_version"`
	Type          string `json:"type"`
	ServerID      string `json:"server_id"`
	RunID         string `json:"run_id"`
	Timestamp     string `json:"timestamp"`
	AgentVersion  string `json:"agent_version"`
}

// Sample is one periodic measurement.
type Sample struct {
	Header
	Seq             uint64       `json:"seq"`
	IntervalSeconds float64      `json:"interval_seconds"`
	Host            Host         `json:"host"`
	GPUs            []GPU        `json:"gpus"`
	Users           []User       `json:"users"`
	Filesystems     []Filesystem `json:"filesystems"`
	Errors          []string     `json:"errors"`
}

// Host holds host-wide CPU and memory figures.
type Host struct {
	CPUPercent        *float64   `json:"cpu_percent"`
	LogicalCores      int        `json:"logical_cores"`
	LoadAvg           [3]float64 `json:"load_avg"`
	MemTotalBytes     uint64     `json:"mem_total_bytes"`
	MemUsedBytes      uint64     `json:"mem_used_bytes"`
	MemAvailableBytes uint64     `json:"mem_available_bytes"`
	SwapTotalBytes    uint64     `json:"swap_total_bytes"`
	SwapUsedBytes     uint64     `json:"swap_used_bytes"`
}

// GPU holds device metrics. Pointer fields are null when NVML reports
// NOT_SUPPORTED or an error.
type GPU struct {
	Index          int          `json:"index"`
	UUID           string       `json:"uuid"`
	Name           *string      `json:"name"`
	VRAMTotalMiB   *uint64      `json:"vram_total_mib"`
	VRAMUsedMiB    *uint64      `json:"vram_used_mib"`
	UtilizationPct *uint32      `json:"utilization_pct"`
	TemperatureC   *uint32      `json:"temperature_c"`
	PowerDrawW     *float64     `json:"power_draw_w"`
	PowerLimitW    *float64     `json:"power_limit_w"`
	FanPct         *uint32      `json:"fan_pct"`
	Processes      []GPUProcess `json:"processes"`
}

// ProcessContext is optional information about where a process runs.
type ProcessContext struct {
	ContainerID    string `json:"container_id,omitempty"`
	ContainerName  string `json:"container_name,omitempty"`
	ContainerImage string `json:"container_image,omitempty"`
	VMName         string `json:"vm_name,omitempty"`
	Account        string `json:"account,omitempty"`
}

// GPUProcess is one process using a GPU. UtilizationPct is present only
// when NVML provides per-process utilization.
type GPUProcess struct {
	PID            int     `json:"pid"`
	Type           string  `json:"type"`
	VRAMMiB        *uint64 `json:"vram_mib"`
	UtilizationPct *uint32 `json:"utilization_pct,omitempty"`
	Exe            string  `json:"exe"`
	Owner          string  `json:"owner"`
	Attribution    string  `json:"attribution"`
	ProcessContext
}

// User aggregates the non-kernel processes of one owner.
type User struct {
	Owner        string       `json:"owner"`
	Attributions []string     `json:"attributions"`
	CPUCores     float64      `json:"cpu_cores"`
	RSSBytes     uint64       `json:"rss_bytes"`
	ProcessCount int          `json:"process_count"`
	TopCPU       []TopProcess `json:"top_cpu"`
	TopRSS       []TopProcess `json:"top_rss"`
}

// TopProcess is a top process entry. It never contains a command line.
type TopProcess struct {
	PID           int     `json:"pid"`
	Exe           string  `json:"exe"`
	CPUCores      float64 `json:"cpu_cores"`
	RSSBytes      uint64  `json:"rss_bytes"`
	ContainerName string  `json:"container_name,omitempty"`
	VMName        string  `json:"vm_name,omitempty"`
}

// Filesystem holds statfs totals for one configured mount point.
type Filesystem struct {
	Mount     string `json:"mount"`
	SizeBytes uint64 `json:"size_bytes"`
	UsedBytes uint64 `json:"used_bytes"`
	FreeBytes uint64 `json:"free_bytes"`
}

// InventoryMsg carries the hardware inventory.
type InventoryMsg struct {
	Header
	Inventory Inventory `json:"inventory"`
	Errors    []string  `json:"errors"`
}

// Inventory describes the host.
type Inventory struct {
	Hostname          string         `json:"hostname"`
	OSRelease         string         `json:"os_release"`
	Kernel            string         `json:"kernel"`
	CPUModel          string         `json:"cpu_model"`
	LogicalCores      int            `json:"logical_cores"`
	MemTotalBytes     uint64         `json:"mem_total_bytes"`
	DriverVersion     *string        `json:"driver_version"`
	CUDADriverVersion *string        `json:"cuda_driver_version"`
	GPUs              []InventoryGPU `json:"gpus"`
	Filesystems       []InventoryFS  `json:"filesystems"`
}

// InventoryGPU is one GPU in the inventory.
type InventoryGPU struct {
	Index        int     `json:"index"`
	UUID         string  `json:"uuid"`
	Name         *string `json:"name"`
	VRAMTotalMiB *uint64 `json:"vram_total_mib"`
}

// InventoryFS is one monitored filesystem.
type InventoryFS struct {
	Mount     string `json:"mount"`
	SizeBytes uint64 `json:"size_bytes"`
}

// DiskScanMsg carries the result of the daily per-owner disk scan.
type DiskScanMsg struct {
	Header
	Scan   DiskScan `json:"scan"`
	Errors []string `json:"errors"`
}

// DiskScan is one completed (or timed-out) scan.
type DiskScan struct {
	StartedAt   string     `json:"started_at"`
	CompletedAt string     `json:"completed_at"`
	Partial     bool       `json:"partial"`
	Paths       []ScanPath `json:"paths"`
}

// ScanPath is the result for one scanned path.
type ScanPath struct {
	Path       string       `json:"path"`
	Mount      string       `json:"mount"`
	TotalBytes uint64       `json:"total_bytes"`
	Partial    bool         `json:"partial"`
	Owners     []OwnerUsage `json:"owners"`
}

// OwnerUsage is the allocated size owned by one owner.
type OwnerUsage struct {
	Owner string `json:"owner"`
	Bytes uint64 `json:"bytes"`
}
