package disk

import (
	"fmt"
	"runtime"
	"syscall"
)

const (
	ioprioClassIdle  = 3
	ioprioClassShift = 13
	ioprioWhoProcess = 1
	lowestNice       = 19
)

// LowerThreadPriority pins the calling goroutine to its OS thread and gives
// that thread the lowest CPU priority (nice 19) and the idle IO class. The
// thread is never unlocked, so the Go runtime discards it when the goroutine
// exits and the lowered priority does not leak to other work.
func LowerThreadPriority() error {
	runtime.LockOSThread()
	tid := syscall.Gettid()
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, lowestNice); err != nil {
		return fmt.Errorf("setpriority: %v", err)
	}
	_, _, e := syscall.Syscall(syscall.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), uintptr(ioprioClassIdle<<ioprioClassShift))
	if e != 0 {
		return fmt.Errorf("ioprio_set: %v", e)
	}
	return nil
}
