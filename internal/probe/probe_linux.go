//go:build linux

package probe

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

func diskUsage(path string) (total, free int64, err error) {
	var st unix.Statfs_t
	if err = unix.Statfs(path, &st); err != nil {
		return
	}
	return int64(st.Blocks) * st.Bsize, int64(st.Bavail) * st.Bsize, nil
}

func meminfo() (total, available int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			available = v * 1024
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("cannot read /proc/meminfo")
	}
	return total, available, nil
}

// pressureFull returns the memory PSI "full avg10" of a pressure file.
func pressureFull(path string) (float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, part := range strings.Fields(line)[1:] {
			if v, ok := strings.CutPrefix(part, "avg10="); ok {
				f, err := strconv.ParseFloat(v, 64)
				return f, err == nil
			}
		}
	}
	return 0, false
}

// OwnCgroup is this process's cgroup v2 path relative to the cgroup root.
func OwnCgroup() string {
	return cgroupOf("self")
}

func cgroupOf(pid string) string {
	data, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return p
		}
	}
	return ""
}

// CgroupWorking returns memory in use by a cgroup minus reclaimable clean file cache.
func CgroupWorking(rel string) (int64, bool) {
	dir := filepath.Join(cgroupRoot, rel)
	cur, err := readInt(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, false
	}
	stat := readKV(filepath.Join(dir, "memory.stat"))
	clean := stat["inactive_file"] - stat["file_dirty"] - stat["file_writeback"]
	if clean < 0 {
		clean = 0
	}
	if clean > cur {
		clean = cur
	}
	return cur - clean, true
}

// CgroupOOMKills returns the oom_kill counter of a cgroup.
func CgroupOOMKills(rel string) int64 {
	return readKV(filepath.Join(cgroupRoot, rel, "memory.events"))["oom_kill"]
}

func readInt(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return -1, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

func readKV(path string) map[string]int64 {
	out := map[string]int64{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			out[f[0]] = v
		}
	}
	return out
}

// ancestorRoom walks from a cgroup to the root and returns the smallest
// (memory.max - working) among ancestors that have a limit, and that limit.
func ancestorRoom(rel string) (room, limit int64) {
	room, limit = -1, 0
	for p := rel; p != "/" && p != "." && p != ""; p = filepath.Dir(p) {
		max, err := readInt(filepath.Join(cgroupRoot, p, "memory.max"))
		if err != nil || max < 0 {
			continue
		}
		working, _ := CgroupWorking(p)
		r := max - working
		if room < 0 || r < room {
			room, limit = r, max
		}
	}
	return room, limit
}

// ProcStart returns a process's start time in clock ticks since boot (0 if gone).
func ProcStart(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0
	}
	v, _ := strconv.ParseUint(f[19], 10, 64)
	return v
}

// GroupRSS sums the resident memory of all processes in a process group.
func GroupRSS(pgid int) int64 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	page := int64(os.Getpagesize())
	var total int64
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		s := string(data)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 22 {
			continue
		}
		if g, _ := strconv.Atoi(f[2]); g != pgid {
			continue
		}
		rss, _ := strconv.ParseInt(f[21], 10, 64)
		total += rss * page
	}
	return total
}

type mount struct{ point, fstype, source string }

func mounts() []mount {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	var out []mount
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) < 5 || len(right) < 2 {
			continue
		}
		out = append(out, mount{point: unescape(left[4]), fstype: right[0], source: right[1]})
	}
	return out
}

func unescape(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// mountFor finds the mount containing path (longest matching mount point).
func mountFor(path string) (mount, bool) {
	path, _ = filepath.EvalSymlinks(path)
	var best mount
	found := false
	for _, m := range mounts() {
		if path == m.point || strings.HasPrefix(path, strings.TrimSuffix(m.point, "/")+"/") || m.point == "/" {
			if !found || len(m.point) > len(best.point) {
				best, found = m, true
			}
		}
	}
	return best, found
}

func fsType(path string) string {
	if m, ok := mountFor(path); ok {
		return m.fstype
	}
	return ""
}

// btrfsFor reads allocation state from sysfs (no root needed).
func btrfsFor(path string) (Btrfs, bool) {
	m, ok := mountFor(path)
	if !ok || m.fstype != "btrfs" {
		return Btrfs{}, false
	}
	dev := filepath.Base(m.source)
	if resolved, err := filepath.EvalSymlinks(m.source); err == nil {
		dev = filepath.Base(resolved)
	}
	fss, _ := filepath.Glob("/sys/fs/btrfs/*/devices/" + dev)
	if len(fss) == 0 {
		return Btrfs{}, false
	}
	base := filepath.Dir(filepath.Dir(fss[0]))
	num := func(rel string) int64 {
		v, _ := readInt(filepath.Join(base, rel))
		return v
	}
	var size int64
	devs, _ := filepath.Glob(filepath.Join(base, "devices", "*", "size"))
	for _, d := range devs {
		v, _ := readInt(d)
		size += v * 512
	}
	if size == 0 {
		return Btrfs{}, false
	}
	allocated := num("allocation/data/disk_total") + num("allocation/metadata/disk_total") + num("allocation/system/disk_total")
	committed := num("allocation/metadata/bytes_used") + num("allocation/metadata/bytes_reserved") +
		num("allocation/metadata/bytes_pinned") + num("allocation/metadata/bytes_may_use")
	b := Btrfs{Path: path, UUID: filepath.Base(base), Unallocated: size - allocated,
		MetadataHeadroom: num("allocation/metadata/total_bytes") - committed}
	if b.Unallocated < 0 {
		b.Unallocated = 0
	}
	if b.MetadataHeadroom < 0 {
		b.MetadataHeadroom = 0
	}
	return b, true
}

// Snapshot measures live health. poolCgroup may be empty (process backend).
func Snapshot(watch []string, poolCgroup string, jobCgroups map[string]string) Health {
	h := Health{ParentRoom: -1, JobWorking: map[string]int64{}}
	var err error
	if h.MemTotal, h.MemAvailable, err = meminfo(); err != nil {
		h.Errors = append(h.Errors, err.Error())
	}
	if f, ok := pressureFull("/proc/pressure/memory"); ok {
		h.PressureFull = f
	}
	seenFS := map[string]bool{}
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
		if b, ok := btrfsFor(p); ok && !seenFS[b.UUID] {
			seenFS[b.UUID] = true
			h.Btrfs = append(h.Btrfs, b)
		}
	}
	if poolCgroup != "" {
		if w, ok := CgroupWorking(poolCgroup); ok {
			h.PoolWorking = w
		}
		h.ParentRoom, h.ParentLimit = ancestorRoom(filepath.Dir(poolCgroup))
		if f, ok := pressureFull(filepath.Join(cgroupRoot, poolCgroup, "memory.pressure")); ok && f > h.PressureFull {
			h.PressureFull = f
		}
	}
	for id, cg := range jobCgroups {
		if w, ok := CgroupWorking(cg); ok {
			h.JobWorking[id] = w
		}
	}
	return h
}

// Detect gathers onboarding facts.
func Detect() Host {
	h := baseHost()
	h.MemTotal, _, _ = meminfo()
	if target, err := os.Readlink("/proc/1/exe"); err == nil && strings.Contains(target, "systemd") {
		h.Systemd = true
	} else if _, err := os.Stat("/run/systemd/system"); err == nil {
		h.Systemd = true
	}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err == nil {
		h.CgroupV2 = true
	}
	_, h.PSI = pressureFull("/proc/pressure/memory")
	if os.Getenv("XDG_RUNTIME_DIR") != "" {
		if _, err := os.Stat(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "systemd", "private")); err == nil {
			h.SystemdUser = true
		}
	}
	own := OwnCgroup()
	if i := strings.Index(own, "/user@"); i >= 0 {
		mgr := own[:i] + "/" + strings.SplitN(own[i+1:], "/", 2)[0]
		if data, err := os.ReadFile(filepath.Join(cgroupRoot, mgr, "cgroup.subtree_control")); err == nil {
			h.UserDelegation = strings.TrimSpace(string(data))
		}
	}
	if own != "" {
		_, h.ParentLimit = ancestorRoom(own)
	}
	if data, err := os.ReadFile("/sys/fs/selinux/enforce"); err == nil {
		h.SELinux = map[string]string{"1": "enforcing", "0": "permissive"}[strings.TrimSpace(string(data))]
	}
	home, _ := os.UserHomeDir()
	h.HomeFS = fsType(home)
	nv, _ := filepath.Glob("/dev/nvidia[0-9]*")
	h.GPUs = len(nv)
	if h.GPUs == 0 {
		for _, card := range globAll("/sys/class/drm/card[0-9]*/device/vendor") {
			if data, err := os.ReadFile(card); err == nil && strings.TrimSpace(string(data)) == "0x1002" {
				h.GPUs++
			}
		}
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		h.InContainer = true
	} else if _, err := os.Stat("/run/.containerenv"); err == nil {
		h.InContainer = true
	}
	return h
}

func globAll(p string) []string {
	m, _ := filepath.Glob(p)
	return m
}
