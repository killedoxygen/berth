// Package backend starts and stops job supervisors.
//
// systemd: each job is a transient unit in the pool slice, with hard memory,
// CPU and task limits from the kernel (cgroup v2). Works rootless through the
// user's systemd manager, or system-wide as root.
//
// process: each job is a detached process session. Memory caps are enforced
// by the supervisor polling resident memory. Works anywhere (macOS, BSD-like,
// containers, Linux without systemd).
package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/probe"
)

type Backend interface {
	Name() string
	// Start launches the supervisor for a job; launch is the supervisor's launch file.
	Start(j *job.Job, launch string) (handle string, err error)
	// Alive reports whether the job's supervisor still runs.
	Alive(j *job.Job) bool
	// Stop asks the supervisor to stop the job (it escalates to SIGKILL itself).
	Stop(j *job.Job) error
	// PoolCgroup is the cgroup holding all jobs, "" when there is none.
	PoolCgroup() string
	// JobCgroup is a job's cgroup, "" when there is none.
	JobCgroup(j *job.Job) string
	// Working returns a job's resident memory when no cgroup measures it.
	Working(j *job.Job) (int64, bool)
	// Enforces reports whether the kernel enforces per-job memory caps.
	Enforces() bool
}

func New(cfg *config.Config, system bool, self, runDir string) (Backend, error) {
	switch cfg.Backend.Kind {
	case "systemd":
		return newSystemd(cfg, system, self)
	case "process":
		return &Process{self: self, runDir: runDir}, nil
	}
	return nil, fmt.Errorf("unknown backend %q", cfg.Backend.Kind)
}

func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ---------------------------------------------------------------- systemd

type Systemd struct {
	cfg    *config.Config
	system bool
	self   string
	pool   string
	mu     sync.Mutex
	ctxs   map[int]string // SELinux login context per uid
}

// contextFor is the SELinux context a system-mode job of uid runs in: the
// configured one, else the user's own default login context from the host
// policy (what an ssh login would get: unconfined_t, staff_t, user_t ...).
// Jobs must not inherit the service manager's context (which, for example,
// may not use the docker socket), and must never get more than the user's own.
// If it cannot be determined, none is set and systemd's default applies.
func (s *Systemd) contextFor(uid int) string {
	if s.cfg.Backend.SELinuxContext != "" || !s.system {
		return s.cfg.Backend.SELinuxContext
	}
	if data, err := os.ReadFile("/sys/fs/selinux/enforce"); err != nil || strings.TrimSpace(string(data)) != "1" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx, ok := s.ctxs[uid]; ok {
		return ctx
	}
	ctx := ""
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		for _, from := range []string{"system_u:system_r:sshd_t:s0-s0:c0.c1023", "system_u:system_r:init_t:s0"} {
			// selinuxdefcon can print the right context and still exit 1; trust a well-formed answer.
			out, _ := run(5*time.Second, "selinuxdefcon", u.Username, from)
			if c := strings.TrimSpace(out); contextRE.MatchString(c) {
				ctx = c
				break
			}
		}
	}
	if s.ctxs == nil {
		s.ctxs = map[int]string{}
	}
	s.ctxs[uid] = ctx
	return ctx
}

// contextRE accepts an SELinux context user:role:type:level (level may contain ':' and ',').
var contextRE = regexp.MustCompile(`^[a-z0-9_]+_u:[a-z0-9_]+_r:[a-z0-9_]+_t:s[0-9]+[a-z0-9.,:\-]*$`)

func UnitName(id string) string { return "berth-" + id + ".service" }

func newSystemd(cfg *config.Config, system bool, self string) (*Systemd, error) {
	s := &Systemd{cfg: cfg, system: system, self: self}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return nil, fmt.Errorf("backend systemd needs systemd-run: %w", err)
	}
	s.pool = s.slicePath()
	return s, nil
}

func (s *Systemd) Name() string {
	if s.system {
		return "systemd-system"
	}
	return "systemd-user"
}

func (s *Systemd) ctl(args ...string) []string {
	if s.system {
		return args
	}
	return append([]string{"--user"}, args...)
}

// slicePath maps a slice name to its cgroup path ("a-b.slice" -> "a.slice/a-b.slice").
func (s *Systemd) slicePath() string {
	name := strings.TrimSuffix(s.cfg.Backend.Slice, ".slice")
	parts := strings.Split(name, "-")
	var segs []string
	for i := range parts {
		segs = append(segs, strings.Join(parts[:i+1], "-")+".slice")
	}
	rel := strings.Join(segs, "/")
	if s.system {
		return "/" + rel
	}
	uid := os.Getuid()
	return fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/%s", uid, uid, rel)
}

func (s *Systemd) PoolCgroup() string             { return s.pool }
func (s *Systemd) JobCgroup(j *job.Job) string    { return s.pool + "/" + UnitName(j.ID) }
func (s *Systemd) Working(*job.Job) (int64, bool) { return 0, false }
func (s *Systemd) Enforces() bool                 { return true }

func (s *Systemd) Start(j *job.Job, launch string) (string, error) {
	cap := j.Spec.Memory
	if j.Spec.UnitMemory > 0 && j.Spec.UnitMemory < cap {
		cap = j.Spec.UnitMemory
	}
	// No MemoryHigh: throttling just below the cap turns an overrun into a minutes-long
	// crawl before the kill. A job over its cap fails at once with a clear reason.
	swap := cap / 8
	if swap > 512<<20 {
		swap = 512 << 20
	}
	props := []string{
		"MemoryAccounting=yes",
		"MemoryMax=" + strconv.FormatInt(cap, 10),
		"MemorySwapMax=" + strconv.FormatInt(swap, 10),
		fmt.Sprintf("CPUQuota=%d%%", int(j.Spec.CPUs*100+0.5)),
		"TasksMax=8192",
		// The supervisor reports OOM kills; the kernel kills the largest process, not the supervisor.
		"OOMPolicy=continue",
		// Stop signals the supervisor, which stops the job's process group and records the result.
		"KillMode=mixed",
		"TimeoutStopSec=30",
		"CollectMode=inactive-or-failed",
		// Job output goes to berth's compressed logs; only the supervisor's own
		// diagnostics reach the journal.
		"StandardOutput=journal",
		"StandardError=journal",
	}
	if j.Spec.Timeout > 0 {
		props = append(props, "RuntimeMaxSec="+strconv.Itoa(int((j.Spec.Timeout+time.Minute).Seconds())))
	}
	if ctx := s.contextFor(j.UID); ctx != "" {
		props = append(props, "SELinuxContext="+ctx)
	}
	props = append(props, s.cfg.Backend.ExtraProperties...)
	args := s.ctl("--quiet", "--unit="+UnitName(j.ID), "--slice="+s.cfg.Backend.Slice,
		"--description=berth "+job.Short(j.ID)+": "+j.Label(), "--service-type=exec")
	if s.system {
		args = append(args, "--uid="+strconv.Itoa(j.UID), "--gid="+strconv.Itoa(primaryGID(j.UID)))
	}
	for _, p := range props {
		args = append(args, "--property="+p)
	}
	args = append(args, "--", s.self, "__supervise", launch)
	if _, err := run(30*time.Second, "systemd-run", args...); err != nil {
		return "", err
	}
	return UnitName(j.ID), nil
}

func (s *Systemd) Alive(j *job.Job) bool {
	if _, err := os.Stat(filepath.Join("/sys/fs/cgroup", s.JobCgroup(j), "cgroup.procs")); err == nil {
		return true
	}
	// The cgroup path may differ from the computed one on unusual layouts; ask systemd.
	out, err := run(10*time.Second, "systemctl", s.ctl("show", "--property=ActiveState", "--value", UnitName(j.ID))...)
	if err != nil {
		return true // unknown: assume alive, check again next tick
	}
	state := strings.TrimSpace(out)
	return state == "active" || state == "activating" || state == "deactivating" || state == "reloading"
}

func (s *Systemd) Stop(j *job.Job) error {
	_, err := run(10*time.Second, "systemctl", s.ctl("stop", "--no-block", UnitName(j.ID))...)
	return err
}

func primaryGID(uid int) int {
	data, err := os.ReadFile("/etc/passwd")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Split(line, ":")
			if len(f) > 3 && f[2] == strconv.Itoa(uid) {
				if g, err := strconv.Atoi(f[3]); err == nil {
					return g
				}
			}
		}
	}
	return uid
}

// ---------------------------------------------------------------- process

type Process struct{ self, runDir string }

func (p *Process) Name() string              { return "process" }
func (p *Process) PoolCgroup() string        { return "" }
func (p *Process) JobCgroup(*job.Job) string { return "" }
func (p *Process) Enforces() bool            { return false }

func (p *Process) Start(j *job.Job, launch string) (string, error) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return "", err
	}
	defer devnull.Close()
	cmd := exec.Command(p.self, "__supervise", launch)
	// The supervisor's own diagnostics go to the daemon's log.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap; after a daemon restart the supervisor is reparented instead
	start := probe.ProcStart(pid)
	return fmt.Sprintf("pid:%d:%d", pid, start), nil
}

func parseHandle(h string) (pid int, start uint64, ok bool) {
	f := strings.Split(h, ":")
	if len(f) != 3 || f[0] != "pid" {
		return 0, 0, false
	}
	pid, err1 := strconv.Atoi(f[1])
	start, err2 := strconv.ParseUint(f[2], 10, 64)
	return pid, start, err1 == nil && err2 == nil
}

func (p *Process) Alive(j *job.Job) bool {
	pid, start, ok := parseHandle(j.Unit)
	if !ok {
		return false
	}
	now := probe.ProcStart(pid)
	return now != 0 && now == start && !zombie(pid)
}

func (p *Process) Stop(j *job.Job) error {
	pid, _, ok := parseHandle(j.Unit)
	if !ok || !p.Alive(j) {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}

// Working measures the job's process group (the supervisor writes its pgid next to the result).
func (p *Process) Working(j *job.Job) (int64, bool) {
	data, err := os.ReadFile(PgidPath(p.runDir, j.ID))
	if err != nil {
		return 0, false
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return probe.GroupRSS(pgid), true
}

// PgidPath is where the supervisor records the job's process group.
func PgidPath(dir, id string) string { return filepath.Join(dir, id+".pgid") }

func zombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}
