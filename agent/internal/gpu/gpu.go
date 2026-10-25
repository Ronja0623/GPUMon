// Package gpu abstracts NVML so that the collector can run against a fake.
package gpu

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotSupported is returned when the device does not support a query.
var ErrNotSupported = errors.New("not supported")

// ErrLibraryNotFound is returned by Init when libnvidia-ml.so is absent.
var ErrLibraryNotFound = errors.New("NVML library not found")

// ProcInfo is one process on a GPU.
type ProcInfo struct {
	PID     uint32
	UsedMiB *uint64 // nil when not available
}

// ProcUtil is one per-process utilization sample reported by NVML.
type ProcUtil struct {
	PID       uint32
	SMUtil    uint32
	TimeStamp uint64
}

// Device is one GPU.
type Device interface {
	UUID() (string, error)
	Name() (string, error)
	Memory() (totalMiB, usedMiB uint64, err error)
	Utilization() (uint32, error)
	Temperature() (uint32, error)
	PowerUsageMilliW() (uint32, error)
	PowerLimitMilliW() (uint32, error)
	FanSpeed() (uint32, error)
	ComputeProcesses() ([]ProcInfo, error)
	GraphicsProcesses() ([]ProcInfo, error)
	// ProcessUtilization returns samples newer than sinceMicros.
	ProcessUtilization(sinceMicros uint64) ([]ProcUtil, error)
}

// Library is the NVML entry point.
type Library interface {
	Init() error
	Shutdown()
	DriverVersion() (string, error)
	CUDADriverVersion() (int, error)
	Devices() ([]Device, error)
}

// FormatCUDAVersion turns NVML's integer CUDA version (12040) into "12.4".
func FormatCUDAVersion(v int) string {
	return fmt.Sprintf("%d.%d", v/1000, (v%1000)/10)
}

// FakeDevice is a configurable Device for tests and simulation. Errs maps a
// method name (for example "FanSpeed") to the error it returns.
type FakeDevice struct {
	UUIDVal     string
	NameVal     string
	TotalMiB    uint64
	UsedMiB     uint64
	Util        uint32
	Temp        uint32
	PowerMilliW uint32
	LimitMilliW uint32
	Fan         uint32
	Compute     []ProcInfo
	Graphics    []ProcInfo
	ProcUtils   []ProcUtil
	Errs        map[string]error
}

func (d *FakeDevice) err(name string) error {
	if d.Errs == nil {
		return nil
	}
	return d.Errs[name]
}

// UUID implements Device.
func (d *FakeDevice) UUID() (string, error) { return d.UUIDVal, d.err("UUID") }

// Name implements Device.
func (d *FakeDevice) Name() (string, error) { return d.NameVal, d.err("Name") }

// Memory implements Device.
func (d *FakeDevice) Memory() (uint64, uint64, error) { return d.TotalMiB, d.UsedMiB, d.err("Memory") }

// Utilization implements Device.
func (d *FakeDevice) Utilization() (uint32, error) { return d.Util, d.err("Utilization") }

// Temperature implements Device.
func (d *FakeDevice) Temperature() (uint32, error) { return d.Temp, d.err("Temperature") }

// PowerUsageMilliW implements Device.
func (d *FakeDevice) PowerUsageMilliW() (uint32, error) { return d.PowerMilliW, d.err("PowerUsage") }

// PowerLimitMilliW implements Device.
func (d *FakeDevice) PowerLimitMilliW() (uint32, error) { return d.LimitMilliW, d.err("PowerLimit") }

// FanSpeed implements Device.
func (d *FakeDevice) FanSpeed() (uint32, error) { return d.Fan, d.err("FanSpeed") }

// ComputeProcesses implements Device.
func (d *FakeDevice) ComputeProcesses() ([]ProcInfo, error) {
	return d.Compute, d.err("ComputeProcesses")
}

// GraphicsProcesses implements Device.
func (d *FakeDevice) GraphicsProcesses() ([]ProcInfo, error) {
	return d.Graphics, d.err("GraphicsProcesses")
}

// ProcessUtilization implements Device.
func (d *FakeDevice) ProcessUtilization(uint64) ([]ProcUtil, error) {
	if err := d.err("ProcessUtilization"); err != nil {
		return nil, err
	}
	return d.ProcUtils, nil
}

// FakeLibrary is a configurable Library.
type FakeLibrary struct {
	InitErr error
	Driver  string
	CUDA    int
	Devs    []*FakeDevice
	DevsErr error
}

// Init implements Library.
func (l *FakeLibrary) Init() error { return l.InitErr }

// Shutdown implements Library.
func (l *FakeLibrary) Shutdown() {}

// DriverVersion implements Library.
func (l *FakeLibrary) DriverVersion() (string, error) { return l.Driver, nil }

// CUDADriverVersion implements Library.
func (l *FakeLibrary) CUDADriverVersion() (int, error) { return l.CUDA, nil }

// Devices implements Library.
func (l *FakeLibrary) Devices() ([]Device, error) {
	if l.DevsErr != nil {
		return nil, l.DevsErr
	}
	out := make([]Device, len(l.Devs))
	for i, d := range l.Devs {
		out[i] = d
	}
	return out, nil
}

// Absent is a Library for hosts without NVML.
type Absent struct{}

// Init always fails with ErrLibraryNotFound.
func (Absent) Init() error { return ErrLibraryNotFound }

// Shutdown does nothing.
func (Absent) Shutdown() {}

// DriverVersion fails.
func (Absent) DriverVersion() (string, error) { return "", ErrLibraryNotFound }

// CUDADriverVersion fails.
func (Absent) CUDADriverVersion() (int, error) { return 0, ErrLibraryNotFound }

// Devices fails.
func (Absent) Devices() ([]Device, error) { return nil, ErrLibraryNotFound }

// IsNotSupported reports whether err is a NOT_SUPPORTED error.
func IsNotSupported(err error) bool {
	return errors.Is(err, ErrNotSupported) || (err != nil && strings.Contains(err.Error(), "NOT_SUPPORTED"))
}
