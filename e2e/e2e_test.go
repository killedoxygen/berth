// Package e2e drives the real berth binary against a real daemon (process
// backend, so it runs on any CI machine) through the CLI, a shim and MCP.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	bin       string
	env       []string
	root      string
	daemonCmd *exec.Cmd
)

const testConfig = `
[pool]
memory = "2G"
overhead = "256M"
cpus = 4
[defaults]
memory = "256M"
cpus = 1
disk = "0"
[safety]
min_available_memory = "64M"
min_free_disk = "1M"
emergency_free_disk = "0"
watch_paths = ["%s"]
btrfs_min_unallocated = "0"
[scheduler]
tick = "100ms"
owner_grace = "1s"
[backend]
kind = "process"
`

func TestMain(m *testing.M) {
	var err error
	root, err = os.MkdirTemp("", "berth-e2e-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(root, "berth")
	build := exec.Command("go", "build", "-o", bin, "../cmd/berth")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	for _, d := range []string{"config/berth", "state", "run"} {
		os.MkdirAll(filepath.Join(root, d), 0o700)
	}
	os.WriteFile(filepath.Join(root, "config/berth/config.toml"), []byte(fmt.Sprintf(testConfig, root)), 0o600)
	env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_STATE_HOME="+filepath.Join(root, "state"),
		"XDG_RUNTIME_DIR="+filepath.Join(root, "run"), "BERTH_SOCKET=", "HOME="+root)
	startDaemon()
	code := m.Run()
	stopDaemon()
	if code == 0 {
		os.RemoveAll(root)
	} else {
		fmt.Println("daemon log and state kept in", root)
	}
	os.Exit(code)
}

func startDaemon() {
	logf, _ := os.OpenFile(filepath.Join(root, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	daemonCmd = exec.Command(bin, "daemon")
	daemonCmd.Env, daemonCmd.Stdout, daemonCmd.Stderr = env, logf, logf
	if err := daemonCmd.Start(); err != nil {
		panic(err)
	}
	for i := 0; i < 100; i++ {
		if exec.Command(bin, "status").Run() == nil {
			break
		}
		c := exec.Command(bin, "status")
		c.Env = env
		if c.Run() == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func stopDaemon() {
	daemonCmd.Process.Signal(syscall.SIGTERM)
	daemonCmd.Wait()
}

type result struct {
	stdout, stderr string
	code           int
}

func berth(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return runCmd(t, exec.Command(bin, args...), stdin)
}

func runCmd(t *testing.T, cmd *exec.Cmd, stdin string) result {
	t.Helper()
	var out, errb bytes.Buffer
	if cmd.Env == nil {
		cmd.Env = env
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%v: %v", cmd.Args, err)
	}
	return result{out.String(), errb.String(), code}
}

type jobView struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Note    string `json:"note"`
	Hint    string `json:"hint"`
	Reasons []struct{ Code, Text string }
	Result  *struct {
		ExitCode int  `json:"exit_code"`
		OOM      bool `json:"oom"`
	} `json:"result"`
}

func jobState(t *testing.T, id string) jobView {
	t.Helper()
	r := berth(t, "", "status", "--json", id)
	var st struct{ Jobs []jobView }
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil || len(st.Jobs) != 1 {
		t.Fatalf("status %s: %v %q %q", id, err, r.stdout, r.stderr)
	}
	return st.Jobs[0]
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func submit(t *testing.T, args ...string) string {
	t.Helper()
	r := berth(t, "", append([]string{"submit"}, args...)...)
	if r.code != 0 {
		t.Fatalf("submit %v: %d %s", args, r.code, r.stderr)
	}
	return strings.TrimSpace(r.stdout)
}

func TestRunKeepsStdioAndExitStatus(t *testing.T) {
	r := berth(t, "hello\n", "run", "--", "sh", "-c", "cat; echo to-stderr >&2; exit 7")
	if r.code != 7 || r.stdout != "hello\n" || !strings.Contains(r.stderr, "to-stderr") || strings.Contains(r.stdout, "to-stderr") {
		t.Fatalf("got %+v", r)
	}
}

func TestSubmitWaitAndLogs(t *testing.T) {
	id := submit(t, "--name", "two-lines", "--", "sh", "-c", "echo line1; sleep 0.3; echo line2")
	r := berth(t, "", "wait", id)
	if r.code != 0 || r.stdout != "line1\nline2\n" {
		t.Fatalf("wait: %+v", r)
	}
	if got := berth(t, "", "logs", id[:6]).stdout; got != "line1\nline2\n" {
		t.Fatalf("logs by prefix: %q", got)
	}
	if s := jobState(t, id); s.State != "succeeded" {
		t.Fatalf("state %s", s.State)
	}
}

func TestTimeout(t *testing.T) {
	start := time.Now()
	r := berth(t, "", "run", "--timeout", "500ms", "--", "sleep", "30")
	if r.code != 124 || time.Since(start) > 10*time.Second || !strings.Contains(r.stderr, "timeout") {
		t.Fatalf("got %+v after %s", r, time.Since(start))
	}
}

func TestMemoryCapWithoutCgroups(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	r := berth(t, "", "run", "-m", "128M", "--", "python3", "-c", "import time; a = bytearray(400 << 20); time.sleep(20)")
	if r.code != 137 || !strings.Contains(r.stderr, "out of memory") {
		t.Fatalf("got %+v", r)
	}
}

func TestBackfillExplainsAndHints(t *testing.T) {
	a := submit(t, "-m", "1G", "--", "sleep", "3")
	eventually(t, "a running", 5*time.Second, func() bool { return jobState(t, a).State == "running" })
	b := submit(t, "-m", "1500M", "--", "true")
	c := submit(t, "-m", "256M", "--", "true")
	eventually(t, "small job to backfill", 5*time.Second, func() bool { return jobState(t, c).State == "succeeded" })
	s := jobState(t, b)
	if s.State != "queued" || len(s.Reasons) == 0 || s.Reasons[0].Code != "memory" || !strings.Contains(s.Hint, "--memory") {
		t.Fatalf("b should wait for memory with a size hint: %+v", s)
	}
	if r := berth(t, "", "wait", b); r.code != 0 {
		t.Fatalf("b eventually runs: %+v", r)
	}
}

func TestFailedDependencyCancels(t *testing.T) {
	a := submit(t, "--", "false")
	b := submit(t, "--after", a, "--", "echo", "never")
	r := berth(t, "", "wait", b)
	if r.code != 75 || jobState(t, b).State != "cancelled" {
		t.Fatalf("got %+v %+v", r, jobState(t, b))
	}
}

func TestSubmitterExitCancelsJob(t *testing.T) {
	cmd := exec.Command(bin, "run", "--name", "orphan-test", "--", "sleep", "60")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var id string
	eventually(t, "job to run", 5*time.Second, func() bool {
		r := berth(t, "", "status", "--json")
		var st struct {
			Jobs []struct {
				ID, State string
				Spec      struct{ Name string }
			}
		}
		json.Unmarshal([]byte(r.stdout), &st)
		for _, j := range st.Jobs {
			if j.Spec.Name == "orphan-test" && j.State == "running" {
				id = j.ID
				return true
			}
		}
		return false
	})
	cmd.Process.Kill()
	cmd.Wait()
	eventually(t, "job cancelled after its submitter died", 10*time.Second, func() bool { return jobState(t, id).State == "cancelled" })
}

func TestUnsatisfiableFailsFast(t *testing.T) {
	r := berth(t, "", "run", "-m", "5G", "--", "true")
	if r.code != 125 || !strings.Contains(r.stderr, "--memory 1.7G or less") {
		t.Fatalf("an impossible size is rejected at once with the fix: %+v", r)
	}
	svc := submit(t, "--service", "-m", "1500M", "--", "sleep", "30")
	defer berth(t, "", "cancel", svc)
	eventually(t, "service running", 5*time.Second, func() bool { return jobState(t, svc).State == "running" })
	start := time.Now()
	r = berth(t, "", "run", "--give-up", "1s", "-m", "1G", "--", "true")
	if r.code != 75 || !strings.Contains(r.stderr, "giving up") || time.Since(start) > 8*time.Second {
		t.Fatalf("got %+v after %s", r, time.Since(start))
	}
}

func TestDaemonRestartKeepsRunningJob(t *testing.T) {
	id := submit(t, "--", "sh", "-c", "sleep 1.5; echo survived")
	eventually(t, "running", 5*time.Second, func() bool { return jobState(t, id).State == "running" })
	stopDaemon()
	startDaemon()
	r := berth(t, "", "wait", id)
	if r.code != 0 || r.stdout != "survived\n" {
		t.Fatalf("got %+v", r)
	}
}

func TestShimQueuesBuildTool(t *testing.T) {
	if _, err := os.Stat("/usr/bin/make"); err != nil {
		t.Skip("make not available")
	}
	shims := filepath.Join(root, "shims")
	os.MkdirAll(shims, 0o755)
	os.Symlink(bin, filepath.Join(shims, "make"))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("all:\n\t@echo job=$${BERTH_JOB_ID:-none}\n"), 0o644)
	cmd := exec.Command(filepath.Join(shims, "make"))
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, env...), "PATH="+shims+":/usr/bin:/bin")
	r := runCmd(t, cmd, "")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "job=") || strings.Contains(r.stdout, "none") {
		t.Fatalf("make should have run inside a berth job: %+v", r)
	}
	cmd = exec.Command(filepath.Join(shims, "make"), "-n")
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, env...), "PATH="+shims+":/usr/bin:/bin")
	if r := runCmd(t, cmd, ""); r.code != 0 || !strings.Contains(r.stdout, "echo job=") {
		t.Fatalf("a dry run goes straight to make: %+v", r)
	}
}

func TestMCP(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"berth_run","arguments":{"command":"echo from-mcp; exit 3","memory":"128M"}}}`,
	}, "\n") + "\n"
	r := berth(t, in, "mcp")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 responses (notification gets none), got %d: %s", len(lines), r.stdout)
	}
	// Some hosts show only structuredContent, so it must carry the output and exit code too.
	var resp struct {
		Result struct {
			StructuredContent struct {
				ExitCode int    `json:"exit_code"`
				Output   string `json:"output"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(lines[2]), &resp)
	if sc := resp.Result.StructuredContent; sc.ExitCode != 3 || sc.Output != "from-mcp\n" {
		t.Fatalf("structured result incomplete: %+v", sc)
	}
	if !strings.Contains(lines[1], `"berth_run"`) || !strings.Contains(lines[2], "exit code 3") || !strings.Contains(lines[2], "from-mcp") {
		t.Fatalf("unexpected: %s", r.stdout)
	}
}

func TestRunSurvivesDaemonOutage(t *testing.T) {
	cmd := exec.Command(bin, "run", "--", "sh", "-c", "sleep 2; echo finished; exit 5")
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	daemonCmd.Process.Kill() // no clean shutdown
	daemonCmd.Wait()
	err := cmd.Wait()
	startDaemon()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 5 || out.String() != "finished\n" {
		t.Fatalf("the job's own status must reach the caller while the daemon is down: code %d out %q err %q", code, out.String(), errb.String())
	}
}

func TestBuildParallelismFollowsReservation(t *testing.T) {
	r := berth(t, "", "run", "-c", "2", "--clean-env", "--env", "MAKEFLAGS=-j7", "--", "sh", "-c", `echo "$BERTH_CPUS $CARGO_BUILD_JOBS $MAKEFLAGS"`)
	if r.stdout != "2 2 -j7\n" {
		t.Fatalf("want sizes from the reservation, but the caller's explicit setting kept: %q", r.stdout)
	}
}
