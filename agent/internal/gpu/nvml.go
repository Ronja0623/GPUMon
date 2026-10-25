package gpu

import (
	"errors"
	"fmt"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// NVML is the real Library backed by libnvidia-ml.so, loaded at runtime.
type NVML struct{}

func nvErr(ret nvml.Return) error {
	switch ret {
	case nvml.SUCCESS:
		return nil
	case nvml.ERROR_NOT_SUPPORTED:
		return ErrNotSupported
	case nvml.ERROR_LIBRARY_NOT_FOUND:
		return ErrLibraryNotFound
	}
	return errors.New(nvml.ErrorString(ret))
}

// Init loads and initializes NVML.
func (NVML) Init() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("NVML initialization failed: %v", r)
		}
	}()
	return nvErr(nvml.Init())
}

// Shutdown releases NVML.
func (NVML) Shutdown() { _ = nvml.Shutdown() }

// DriverVersion returns the NVIDIA driver version.
func (NVML) DriverVersion() (string, error) {
	v, ret := nvml.SystemGetDriverVersion()
	return v, nvErr(ret)
}

// CUDADriverVersion returns the CUDA driver API version as an integer.
func (NVML) CUDADriverVersion() (int, error) {
	v, ret := nvml.SystemGetCudaDriverVersion()
	return v, nvErr(ret)
}

// Devices returns all GPUs in NVML index order.
func (NVML) Devices() ([]Device, error) {
	n, ret := nvml.DeviceGetCount()
	if err := nvErr(ret); err != nil {
		return nil, err
	}
	out := make([]Device, 0, n)
	for i := 0; i < n; i++ {
		d, ret := nvml.DeviceGetHandleByIndex(i)
		if err := nvErr(ret); err != nil {
			return out, fmt.Errorf("GPU %d: %v", i, err)
		}
		out = append(out, nvDevice{d})
	}
	return out, nil
}

type nvDevice struct{ d nvml.Device }

func (n nvDevice) UUID() (string, error) {
	v, ret := n.d.GetUUID()
	return v, nvErr(ret)
}

func (n nvDevice) Name() (string, error) {
	v, ret := n.d.GetName()
	return v, nvErr(ret)
}

func (n nvDevice) Memory() (uint64, uint64, error) {
	m, ret := n.d.GetMemoryInfo()
	return m.Total >> 20, m.Used >> 20, nvErr(ret)
}

func (n nvDevice) Utilization() (uint32, error) {
	u, ret := n.d.GetUtilizationRates()
	return u.Gpu, nvErr(ret)
}

func (n nvDevice) Temperature() (uint32, error) {
	v, ret := n.d.GetTemperature(nvml.TEMPERATURE_GPU)
	return v, nvErr(ret)
}

func (n nvDevice) PowerUsageMilliW() (uint32, error) {
	v, ret := n.d.GetPowerUsage()
	return v, nvErr(ret)
}

func (n nvDevice) PowerLimitMilliW() (uint32, error) {
	v, ret := n.d.GetEnforcedPowerLimit()
	return v, nvErr(ret)
}

func (n nvDevice) FanSpeed() (uint32, error) {
	v, ret := n.d.GetFanSpeed()
	return v, nvErr(ret)
}

// valueNotAvailable is NVML_VALUE_NOT_AVAILABLE for 64-bit counters.
const valueNotAvailable = ^uint64(0)

func convertProcs(ps []nvml.ProcessInfo) []ProcInfo {
	out := make([]ProcInfo, 0, len(ps))
	for _, p := range ps {
		pi := ProcInfo{PID: p.Pid}
		if p.UsedGpuMemory != valueNotAvailable {
			v := p.UsedGpuMemory >> 20
			pi.UsedMiB = &v
		}
		out = append(out, pi)
	}
	return out
}

func (n nvDevice) ComputeProcesses() ([]ProcInfo, error) {
	ps, ret := n.d.GetComputeRunningProcesses()
	return convertProcs(ps), nvErr(ret)
}

func (n nvDevice) GraphicsProcesses() ([]ProcInfo, error) {
	ps, ret := n.d.GetGraphicsRunningProcesses()
	return convertProcs(ps), nvErr(ret)
}

func (n nvDevice) ProcessUtilization(since uint64) ([]ProcUtil, error) {
	ss, ret := n.d.GetProcessUtilization(since)
	if ret == nvml.ERROR_NOT_FOUND {
		return nil, nil // no samples in the period
	}
	if err := nvErr(ret); err != nil {
		return nil, err
	}
	out := make([]ProcUtil, 0, len(ss))
	for _, s := range ss {
		out = append(out, ProcUtil{PID: s.Pid, SMUtil: s.SmUtil, TimeStamp: s.TimeStamp})
	}
	return out, nil
}
