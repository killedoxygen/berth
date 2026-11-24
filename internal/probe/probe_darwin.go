//go:build darwin

package probe

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func diskUsage(path string) (total, free int64, err error) {
	var st unix.Statfs_t
	if err = unix.Statfs(path, &st); err != nil {
		return
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize), nil
}

func memTotal() int64 {
	v, _ := unix.SysctlUint64("hw.memsize")
	return int64(v)
}

// memAvailable is what the kernel itself considers available: the percentage
// behind memory_pressure and jetsam (it counts reclaimable file cache, which a
// sum of vm_stat's free/inactive pages misses). vm_stat is the fallback.
func memAvailable() int64 {
	if level, err := unix.SysctlUint32("kern.memorystatus_level"); err == nil && level > 0 && level <= 100 {
		return memTotal() / 100 * int64(level)
	}
	return vmStatAvailable()
}

// pressure maps the kernel's memory pressure level (1 normal, 2 warning, 4
// critical) onto the PSI-like percentage the admission rules use.
func pressure() float64 {
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return 0
	}
	switch {
	case level >= 4:
		return 60
	case level >= 2:
		return 20
	}
	return 0
}

func vmStatAvailable() int64 {
	out, err := exec.Command("/usr/bin/vm_stat").Output()
	if err != nil {
		return 0
	}
	page := int64(os.Getpagesize())
	var pages int64
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				f := strings.Fields(line[i+len("page size of "):])
				if len(f) > 0 {
					if v, err := strconv.ParseInt(f[0], 10, 64); err == nil {
						page = v
					}
				}
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			v, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
			pages += v
		}
	}
	return pages * page
}

// OwnCgroup has no meaning on macOS.
func OwnCgroup() string { return "" }

func CgroupWorking(string) (int64, bool) { return 0, false }

func CgroupOOMKills(string) int64 { return 0 }

// ProcStart returns the process start time in microseconds since the epoch (0 if gone).
func ProcStart(pid int) uint64 {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return 0
	}
	tv := kp.Proc.P_starttime
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec)
}

// GroupRSS sums resident memory of a process group via ps.
func GroupRSS(pgid int) int64 {
	out, err := exec.Command("/bin/ps", "-o", "rss=", "-g", strconv.Itoa(pgid)).Output()
	if err != nil {
		return 0
	}
	var total int64
	for _, f := range strings.Fields(string(out)) {
		v, _ := strconv.ParseInt(f, 10, 64)
		total += v * 1024
	}
	return total
}

func Snapshot(watch []string, _ string, _ map[string]string) Health {
	h := Health{ParentRoom: -1, JobWorking: map[string]int64{}, MemTotal: memTotal(), MemAvailable: memAvailable(), PressureFull: pressure()}
	for _, p := range watch {
		p = ExpandPath(p)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		total, free, err := diskUsage(p)
		if err != nil {
			h.Errors = append(h.Errors, "disk "+p+": "+err.Error())
			continue
		}
		h.Disks = append(h.Disks, Disk{Path: p, Free: free, Total: total})
	}
	return h
}

func Detect() Host {
	h := baseHost()
	h.MemTotal = memTotal()
	home, _ := os.UserHomeDir()
	var st unix.Statfs_t
	if unix.Statfs(home, &st) == nil {
		h.HomeFS = unix.ByteSliceToString(st.Fstypename[:])
	}
	return h
}
