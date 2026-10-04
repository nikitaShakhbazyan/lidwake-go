package darwin

/*
#include <stdlib.h>
#include "darwin.h"
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// procPathMax is PROC_PIDPATHINFO_MAXSIZE (4 * MAXPATHLEN).
const procPathMax = 4 * 1024

// checkPID rejects values the kernel's pid_t can't hold, before they are truncated into one.
func checkPID(pid int) error {
	if pid < 0 || pid > math.MaxInt32 {
		return fmt.Errorf("darwin: invalid pid %d", pid)
	}
	return nil
}

func processPath(pid int) (string, error) {
	if err := checkPID(pid); err != nil {
		return "", err
	}
	buf := make([]byte, procPathMax)
	n := C.lw_proc_path(C.int32_t(pid), (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	if n < 0 {
		return "", fmt.Errorf("darwin: proc_pidpath(%d): %w", pid, syscall.Errno(-n))
	}
	path := buf[:n]
	if i := bytes.IndexByte(path, 0); i >= 0 {
		path = path[:i]
	}
	if len(path) == 0 {
		return "", fmt.Errorf("darwin: proc_pidpath(%d): empty path", pid)
	}
	return string(path), nil
}

type kinfo struct {
	ppid  int
	start time.Time
	comm  string
}

func kinfoOf(pid int) (kinfo, error) {
	if err := checkPID(pid); err != nil {
		return kinfo{}, err
	}
	var k C.lw_kinfo
	if rc := C.lw_kinfo_of(C.int32_t(pid), &k); rc != 0 {
		return kinfo{}, fmt.Errorf("darwin: sysctl KERN_PROC_PID %d: %w", pid, syscall.Errno(rc))
	}
	return kinfo{
		ppid:  int(k.ppid),
		start: time.Unix(int64(k.start_sec), int64(k.start_usec)*int64(time.Microsecond)),
		comm:  C.GoString(&k.comm[0]),
	}, nil
}

func processName(pid int) string {
	k, err := kinfoOf(pid)
	if err != nil {
		return ""
	}
	return k.comm
}

func parentPID(pid int) (int, error) {
	k, err := kinfoOf(pid)
	if err != nil {
		return 0, err
	}
	return k.ppid, nil
}

func processStartTime(pid int) (time.Time, error) {
	k, err := kinfoOf(pid)
	if err != nil {
		return time.Time{}, err
	}
	return k.start, nil
}

func processArgs(pid int) ([]string, error) {
	if err := checkPID(pid); err != nil {
		return nil, err
	}
	argmax := C.lw_argmax()
	if argmax <= 0 {
		return nil, fmt.Errorf("darwin: sysctl kern.argmax: %w", syscall.Errno(-argmax))
	}
	buf := make([]byte, int(argmax))
	size := C.size_t(len(buf))
	if rc := C.lw_procargs2(C.int32_t(pid), unsafe.Pointer(&buf[0]), &size); rc != 0 {
		return nil, fmt.Errorf("darwin: sysctl KERN_PROCARGS2 %d: %w", pid, syscall.Errno(rc))
	}
	args, err := parseProcArgs2(buf[:int(size)])
	if err != nil {
		return nil, fmt.Errorf("darwin: argv of %d: %w", pid, err)
	}
	return args, nil
}

// parseProcArgs2 decodes a KERN_PROCARGS2 blob: an int argc, the NUL-terminated executable path,
// alignment NULs, argc NUL-terminated arguments, then the environment. Exactly argc arguments are
// taken, so an environment variable containing a marker can never be mistaken for an argument.
func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) <= 4 {
		return nil, errors.New("short KERN_PROCARGS2 buffer")
	}
	argc := int(int32(binary.NativeEndian.Uint32(buf)))
	if argc <= 0 {
		return nil, errors.New("no arguments")
	}
	i := 4
	for i < len(buf) && buf[i] != 0 { // executable path
		i++
	}
	for i < len(buf) && buf[i] == 0 { // alignment padding
		i++
	}
	args := make([]string, 0, min(argc, 256))
	start := i
	for ; i < len(buf) && len(args) < argc; i++ {
		if buf[i] == 0 {
			args = append(args, string(buf[start:i]))
			start = i + 1
		}
	}
	if len(args) == 0 {
		return nil, errors.New("no arguments")
	}
	return args, nil
}

func processAlive(pid int) bool {
	if pid <= 0 || pid > math.MaxInt32 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

type procEntry struct{ pid, ppid int }

// procSnapshot is one KERN_PROC_ALL read of the process table.
func procSnapshot() ([]procEntry, error) {
	var pids, ppids *C.int32_t
	n := C.lw_proc_all(&pids, &ppids)
	if n < 0 {
		return nil, fmt.Errorf("darwin: sysctl KERN_PROC_ALL: %w", syscall.Errno(-n))
	}
	defer C.free(unsafe.Pointer(pids))
	ps := unsafe.Slice(pids, int(n))
	pps := unsafe.Slice(ppids, int(n))
	out := make([]procEntry, int(n))
	for i := range out {
		out[i] = procEntry{pid: int(ps[i]), ppid: int(pps[i])}
	}
	return out, nil
}

func childMap() (map[int][]int, error) {
	procs, err := procSnapshot()
	if err != nil {
		return nil, err
	}
	m := map[int][]int{}
	for _, p := range procs {
		if p.pid > 0 {
			m[p.ppid] = append(m[p.ppid], p.pid)
		}
	}
	return m, nil
}

func childPIDs(pid int) ([]int, error) {
	if err := checkPID(pid); err != nil {
		return nil, err
	}
	procs, err := procSnapshot()
	if err != nil {
		return nil, err
	}
	var out []int
	for _, p := range procs {
		if p.pid > 0 && p.pid != pid && p.ppid == pid {
			out = append(out, p.pid)
		}
	}
	return out, nil
}

func allPIDs() ([]int, error) {
	procs, err := procSnapshot()
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(procs))
	for _, p := range procs {
		if p.pid > 0 {
			out = append(out, p.pid)
		}
	}
	return out, nil
}

// timebase is mach_timebase_info, fixed for the life of the process. proc_taskinfo reports CPU
// time in Mach absolute-time ticks, not nanoseconds: on Apple Silicon a tick is ~41.7 ns (a
// 24 MHz timebase, 125/3), so reading ticks as nanoseconds under-reports CPU ~41.7× and makes a
// pinned core look idle. On Intel the ratio is 1/1. If the call fails, 1/1 is assumed.
var timebase = sync.OnceValues(func() (numer, denom uint32) {
	var n, d C.uint32_t
	if C.lw_timebase(&n, &d) != 0 {
		return 1, 1
	}
	return uint32(n), uint32(d)
})

func cpuTime(pid int) (time.Duration, error) {
	if err := checkPID(pid); err != nil {
		return 0, err
	}
	var ticks C.uint64_t
	if rc := C.lw_task_ticks(C.int32_t(pid), &ticks); rc != 0 {
		return 0, fmt.Errorf("darwin: proc_pidinfo(%d, PROC_PIDTASKINFO): %w", pid, syscall.Errno(rc))
	}
	numer, denom := timebase()
	return ticksToDuration(uint64(ticks), numer, denom), nil
}

// ticksToDuration converts Mach absolute-time ticks to a Duration (ticks * numer / denom ns)
// without overflowing the intermediate product; saturates at the largest Duration.
func ticksToDuration(ticks uint64, numer, denom uint32) time.Duration {
	if denom == 0 {
		numer, denom = 1, 1
	}
	hi, lo := bits.Mul64(ticks, uint64(numer))
	if hi >= uint64(denom) {
		return time.Duration(math.MaxInt64)
	}
	ns, _ := bits.Div64(hi, lo, uint64(denom))
	if ns > math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}
