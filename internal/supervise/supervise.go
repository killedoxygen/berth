// Package supervise runs one job: it is the first process of every job, under
// every backend. It wires stdio (log file, or the submitter's descriptors),
// enforces the deadline, enforces the memory cap where no cgroup does, detects
// OOM kills, cleans up the process group, and writes the result file the
// daemon reads. The result file survives a daemon restart.
package supervise

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

// Launch is everything the supervisor needs; the daemon writes it before starting it.
type Launch struct {
	ID          string        `json:"id"`
	Argv        []string      `json:"argv"`
	Cwd         string        `json:"cwd"`
	Env         []string      `json:"env"`
	Timeout     time.Duration `json:"timeout"`
	Passthrough bool          `json:"passthrough"`
	Token       string        `json:"token"`
	Socket      string        `json:"socket"`
	Log         string        `json:"log"`
	Result      string        `json:"result"`
	PgidFile    string        `json:"pgid_file"`
	MemoryCap   int64         `json:"memory_cap"`  // enforced here by polling only when EnforceRSS
	EnforceRSS  bool          `json:"enforce_rss"` // no cgroup limit exists (process backend)
	Grace       time.Duration `json:"grace"`       // SIGTERM to SIGKILL
}

// Outcome is what the result file holds.
type Outcome struct {
	Done bool `json:"done"`
	job.Result
}

const grace = 10 * time.Second

func Main(launchPath string) int {
	data, err := os.ReadFile(launchPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "berth supervise:", err)
		return 125
	}
	var l Launch
	if err := json.Unmarshal(data, &l); err != nil {
		fmt.Fprintln(os.Stderr, "berth supervise:", err)
		return 125
	}
	if l.Grace == 0 {
		l.Grace = grace
	}
	res := run(&l)
	if err := writeResult(l.Result, Outcome{Done: true, Result: res}); err != nil {
		fmt.Fprintln(os.Stderr, "berth supervise: writing result:", err)
		return 125
	}
	return 0
}

func writeResult(path string, o Outcome) error {
	data, _ := json.Marshal(o)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadResult returns the outcome once the supervisor has finished writing it.
func ReadResult(path string) (job.Result, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return job.Result{}, false
	}
	var o Outcome
	if json.Unmarshal(data, &o) != nil || !o.Done {
		return job.Result{}, false
	}
	return o.Result, true
}

func stdio(l *Launch) (in, out, errf *os.File, err error) {
	if !l.Passthrough {
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			return nil, nil, nil, err
		}
		log, err := os.OpenFile(l.Log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, nil, err
		}
		return devnull, log, log, nil
	}
	c, err := api.Dial(l.Socket)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("claiming the submitter's terminal: %w", err)
	}
	defer c.Close()
	if err := c.Send(api.Request{Op: "claim", ID: l.ID, Token: l.Token}); err != nil {
		return nil, nil, nil, err
	}
	var resp api.Response
	if err := c.Recv(&resp); err != nil {
		return nil, nil, nil, err
	}
	fds := c.TakeFDs()
	if !resp.OK || len(fds) != 3 {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		if resp.Error != "" {
			return nil, nil, nil, errors.New(resp.Error)
		}
		return nil, nil, nil, errors.New("the submitter's descriptors are no longer available")
	}
	return os.NewFile(uintptr(fds[0]), "stdin"), os.NewFile(uintptr(fds[1]), "stdout"), os.NewFile(uintptr(fds[2]), "stderr"), nil
}

func run(l *Launch) job.Result {
	signal.Ignore(syscall.SIGPIPE)
	in, out, errf, err := stdio(l)
	if err != nil {
		return job.Result{ExitCode: 125, Reason: "could not attach output: " + err.Error()}
	}
	if len(l.Argv) == 0 {
		return job.Result{ExitCode: 125, Reason: "empty command"}
	}
	path := l.Argv[0]
	if filepath.Base(path) == path {
		if p, err := lookPath(path, l.Env); err == nil {
			path = p
		} else {
			fmt.Fprintf(errf, "berth: %s: command not found\n", l.Argv[0])
			return job.Result{ExitCode: 127, Reason: "command not found: " + l.Argv[0]}
		}
	}
	cmd := exec.Command(path, l.Argv[1:]...)
	cmd.Args = l.Argv
	cmd.Dir = l.Cwd
	cmd.Env = append(append([]string{}, l.Env...), "BERTH_JOB_ID="+l.ID)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cgroup := probe.OwnCgroup()
	oomBefore := probe.CgroupOOMKills(cgroup)

	terms := make(chan os.Signal, 4)
	signal.Notify(terms, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		code := 126
		if errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		fmt.Fprintf(errf, "berth: cannot start %s: %v\n", l.Argv[0], err)
		return job.Result{ExitCode: code, Reason: "cannot start: " + err.Error()}
	}
	in.Close()
	out.Close()
	if errf != out {
		errf.Close()
	}
	pgid := cmd.Process.Pid
	if l.PgidFile != "" {
		_ = os.WriteFile(l.PgidFile, []byte(fmt.Sprint(pgid)), 0o600)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var deadline <-chan time.Time
	if l.Timeout > 0 {
		deadline = time.After(l.Timeout)
	}
	var poll <-chan time.Time
	if l.EnforceRSS && l.MemoryCap > 0 {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		poll = t.C
	}
	var result job.Result
	stopping := false
	var kill <-chan time.Time
	stop := func() {
		if stopping {
			return
		}
		stopping = true
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		kill = time.After(l.Grace)
	}
	var waitErr error
loop:
	for {
		select {
		case waitErr = <-done:
			break loop
		case <-deadline:
			result.TimedOut = true
			result.Reason = "timeout after " + units.FormatDuration(l.Timeout)
			stop()
		case <-poll:
			if rss := probe.GroupRSS(pgid); rss > l.MemoryCap {
				result.OOM = true
				result.Reason = fmt.Sprintf("out of memory: resident %s exceeded the %s limit", units.FormatBytes(rss), units.FormatBytes(l.MemoryCap))
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		case <-terms:
			if result.Reason == "" {
				result.Reason = "stopped"
			}
			stop()
		case <-kill:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	// Whatever the leader left behind in its group goes too.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		result.ExitCode = 0
	case errors.As(waitErr, &exitErr):
		ws := exitErr.Sys().(syscall.WaitStatus)
		if ws.Signaled() {
			result.Signal = ws.Signal().String()
			result.ExitCode = 128 + int(ws.Signal())
		} else {
			result.ExitCode = ws.ExitStatus()
		}
	default:
		result.ExitCode = 125
		result.Reason = waitErr.Error()
	}
	if result.TimedOut {
		result.ExitCode = 124
	}
	if !result.OOM && cgroup != "" && probe.CgroupOOMKills(cgroup) > oomBefore {
		result.OOM = true
		result.Reason = "out of memory: the kernel killed a process at the job's memory limit"
		if l.MemoryCap > 0 {
			result.Reason += " (" + units.FormatBytes(l.MemoryCap) + ")"
		}
	}
	return result
}

// lookPath resolves a bare command name against the job's PATH, not ours.
func lookPath(name string, env []string) (string, error) {
	path := ""
	for _, kv := range env {
		if v, ok := cutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	old := os.Getenv("PATH")
	os.Setenv("PATH", path)
	defer os.Setenv("PATH", old)
	return exec.LookPath(name)
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return s, false
}
