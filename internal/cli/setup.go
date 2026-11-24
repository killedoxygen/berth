package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/shim"
	"github.com/killedoxygen/berth/internal/units"
)

// installed records what setup created, so uninstall removes exactly that.
type installed struct {
	Mode    string   `json:"mode"`
	Binary  string   `json:"binary"`
	Files   []string `json:"files"`    // created by setup; removed by uninstall
	RCFiles []string `json:"rc_files"` // got a marked PATH block
	Service string   `json:"service"`  // systemd unit, launchd label, or ""
}

const (
	launchdLabel = "io.github.killedoxygen.berth"
	rcBegin      = "# >>> berth >>>"
	rcEnd        = "# <<< berth <<<"
)

type setupCtx struct {
	system, dry bool
	host        probe.Host
	paths       config.Paths
	cfg         config.Config
	rec         installed
	steps       []string
}

func (s *setupCtx) did(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.steps = append(s.steps, line)
	prefix := "  "
	if s.dry {
		prefix = "  (dry run) "
	}
	fmt.Println(prefix + line)
}

func (s *setupCtx) write(path, content string, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return nil
	}
	s.did("write %s", path)
	if s.dry {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		return err
	}
	s.record(path)
	return nil
}

func (s *setupCtx) record(path string) {
	for _, f := range s.rec.Files {
		if f == path {
			return
		}
	}
	s.rec.Files = append(s.rec.Files, path)
}

func (s *setupCtx) run(name string, args ...string) error {
	s.did("%s %s", name, strings.Join(args, " "))
	if s.dry {
		return nil
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func recordPath(paths config.Paths) string { return filepath.Join(paths.State, "installed.json") }

func cmdSetup(args []string) int {
	fs := newFlags("setup", "[--system] [--no-shims] [--no-path] [--no-service] [--dry-run]")
	system := fs.Bool("system", false, "install a system-wide daemon (root) instead of a per-user one")
	noShims := fs.Bool("no-shims", false, "do not route cargo/make/docker/git... through the queue")
	noPath := fs.Bool("no-path", false, "do not put the shims on PATH (shell rc files and user services)")
	noService := fs.Bool("no-service", false, "do not install or start a background service")
	linger := fs.Bool("linger", false, "keep the user daemon and jobs running after logout (loginctl enable-linger)")
	maintTimer := fs.Bool("maint", false, "root only: install a timer that keeps btrfs unallocated space above the target with bounded balances")
	dry := fs.Bool("dry-run", false, "show what would change without changing anything")
	binDir := fs.String("bin-dir", "", "copy the berth binary here first (default: keep it where it is unless it is temporary)")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	s := &setupCtx{system: *system, dry: *dry, host: probe.Detect()}
	if *maintTimer {
		return setupMaint(*dry)
	}
	if s.system && os.Geteuid() != 0 {
		return fail(fmt.Errorf("--system needs root: sudo %s setup --system", self()))
	}
	s.paths = config.UserPaths()
	if s.system {
		s.paths = config.SystemPaths()
	}
	if data, err := os.ReadFile(recordPath(s.paths)); err == nil {
		_ = json.Unmarshal(data, &s.rec)
	}
	s.rec.Mode = map[bool]string{true: "system", false: "user"}[s.system]
	var err error
	s.cfg, err = config.Load(s.paths.Config, s.host)
	if err != nil {
		return fail(err)
	}
	h := s.host
	fmt.Printf("berth %s setup (%s mode) on %s/%s: %s memory, %d CPUs", Version, s.rec.Mode, h.OS, h.Arch, units.FormatBytes(h.MemTotal), h.CPUs)
	if h.GPUs > 0 {
		fmt.Printf(", %d GPU(s)", h.GPUs)
	}
	fmt.Printf(", backend %s\n", s.cfg.Backend.Kind)
	steps := []func() error{s.binary(*binDir), s.config, s.service(*noService), s.shims(*noShims), s.path(*noPath || *noShims)}
	for _, step := range steps {
		if err := step(); err != nil {
			return fail(err)
		}
	}
	if !s.dry {
		_ = os.MkdirAll(s.paths.State, 0o700)
		if data, err := json.MarshalIndent(s.rec, "", " "); err == nil {
			_ = os.WriteFile(recordPath(s.paths), data, 0o600)
		}
	}
	if *linger && h.OS == "linux" && !s.system {
		if u, err := user.Current(); err == nil {
			if err := s.run("loginctl", "enable-linger", u.Username); err != nil {
				fmt.Println("  warning:", err)
			}
		}
	}
	if s.dry {
		return 0
	}
	if !*noService {
		if err := waitDaemon(s.paths.Socket, 10*time.Second); err != nil {
			fmt.Println("\nThe daemon did not come up:", err)
			fmt.Println("Run `berth doctor` for details.")
			return 1
		}
	}
	fmt.Printf(`
Ready. Pool: %s memory, %g CPUs; default job %s / %g CPUs.
  berth status                  capacity and jobs
  berth run -- cargo build      run anything heavy through the queue
  berth doctor                  check everything
  berth agents >> AGENTS.md     teach coding agents to use it
  claude mcp add berth -- %s mcp    (or any MCP host: command "%s mcp")
`, units.FormatBytes(int64(s.cfg.Pool.Memory)), s.cfg.Pool.CPUs, units.FormatBytes(int64(s.cfg.Defaults.Memory)), s.cfg.Defaults.CPUs, s.rec.Binary, s.rec.Binary)
	if !*noPath && !*noShims && !onPath(shim.Dir(&s.cfg)) {
		fmt.Println("Open a new shell (or `source` your shell rc) to pick up the shims.")
	}
	if h.OS == "linux" && !s.system && !*linger && lingerOff() {
		fmt.Println("Note: the daemon stops when you log out. `berth setup --linger` keeps it running.")
	}
	return 0
}

func waitDaemon(socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := api.Call(socket, api.Request{Op: "ping"})
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func lingerOff() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	out, err := exec.Command("loginctl", "show-user", u.Username, "--property=Linger", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "no"
}

// binary settles where the service will run berth from.
func (s *setupCtx) binary(dir string) func() error {
	return func() error {
		current := self()
		temporary := strings.HasPrefix(current, os.TempDir()) || strings.Contains(current, "/go-build")
		if dir == "" && !temporary && !(s.system && !strings.HasPrefix(current, "/usr")) {
			s.rec.Binary = current
			return nil
		}
		if dir == "" {
			dir = "/usr/local/bin"
			if !s.system {
				home, _ := os.UserHomeDir()
				dir = filepath.Join(home, ".local", "bin")
			}
		}
		target := filepath.Join(dir, "berth")
		s.rec.Binary = target
		if target == current {
			return nil
		}
		s.did("install %s -> %s", current, target)
		if s.dry {
			return nil
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		tmp := target + ".new"
		if err := os.WriteFile(tmp, data, 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			return err
		}
		s.record(target)
		return nil
	}
}

// config writes a commented config so the defaults stay host-derived until changed.
func (s *setupCtx) config() error {
	if _, err := os.Stat(s.paths.Config); err == nil {
		return nil
	}
	var b bytes.Buffer
	b.WriteString("# berth configuration. Every setting below is a default sized from this host;\n")
	b.WriteString("# uncomment a line to override it. `berth config` prints the effective values.\n\n")
	for _, line := range strings.Split(strings.TrimSpace(config.Encode(s.cfg)), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "[") {
			b.WriteString(line + "\n")
			continue
		}
		b.WriteString("# " + strings.TrimSpace(line) + "\n")
	}
	return s.write(s.paths.Config, b.String(), 0o644)
}

func (s *setupCtx) service(skip bool) func() error {
	return func() error {
		if skip {
			return nil
		}
		switch {
		case s.host.OS == "darwin":
			err := s.launchd()
			if err == nil {
				return nil
			}
			// No GUI login session (ssh, CI): launchd refuses the agent. Run it directly instead.
			fmt.Println("  launchd unavailable here:", err)
		case s.host.Systemd && (s.system || s.host.SystemdUser):
			return s.systemd()
		}
		// No service manager we can use: start it now, detached, and say how to make it permanent.
		s.did("start `%s daemon` in the background (no usable systemd/launchd)", s.rec.Binary)
		if s.dry {
			return nil
		}
		logPath := filepath.Join(s.paths.State, "daemon.log")
		_ = os.MkdirAll(s.paths.State, 0o700)
		cmd := exec.Command(s.rec.Binary, "daemon", "--log-file", logPath)
		cmd.SysProcAttr = detached()
		if err := cmd.Start(); err != nil {
			return err
		}
		s.rec.Service = fmt.Sprintf("pid:%d:%d", cmd.Process.Pid, probe.ProcStart(cmd.Process.Pid))
		fmt.Printf("  add `%s daemon --log-file %s &` to your login startup to keep it across reboots\n", s.rec.Binary, logPath)
		return nil
	}
}

func (s *setupCtx) systemd() error {
	dir := "/etc/systemd/system"
	ctl := []string{}
	execArgs := s.rec.Binary + " daemon"
	if !s.system {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "systemd", "user")
		ctl = []string{"--user"}
	} else {
		execArgs += " --system"
	}
	unit := fmt.Sprintf(`[Unit]
Description=berth workload daemon
Documentation=https://github.com/killedoxygen/berth

[Service]
Type=notify
ExecStart=%s
Restart=always
RestartSec=2
# Job supervisors run in their own units (or sessions) and outlive daemon restarts.
KillMode=process
OOMScoreAdjust=-500

[Install]
WantedBy=%s
`, execArgs, map[bool]string{true: "multi-user.target", false: "default.target"}[s.system])
	cfg := s.cfg
	slice := fmt.Sprintf(`[Unit]
Description=berth jobs (generated by berth setup from the pool settings)

[Slice]
MemoryAccounting=yes
MemoryMax=%d
MemoryHigh=%d
MemorySwapMax=%d
CPUQuota=%d%%
TasksMax=32768
`, int64(cfg.Pool.Memory), int64(cfg.Pool.Memory)/100*97, min(int64(cfg.Pool.Memory)/8, 8*units.GiB), int(cfg.Pool.CPUs*100))
	if err := s.write(filepath.Join(dir, "berth.service"), unit, 0o644); err != nil {
		return err
	}
	if err := s.write(filepath.Join(dir, cfg.Backend.Slice), slice, 0o644); err != nil {
		return err
	}
	s.rec.Service = "berth.service"
	if err := s.run("systemctl", append(ctl, "daemon-reload")...); err != nil {
		return err
	}
	if err := s.run("systemctl", append(ctl, "enable", "berth.service")...); err != nil {
		return err
	}
	// Restart so a new binary or config takes effect; running jobs are unaffected.
	return s.run("systemctl", append(ctl, "restart", "berth.service")...)
}

func (s *setupCtx) launchd() error {
	home, _ := os.UserHomeDir()
	plist := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	logPath := filepath.Join(s.paths.State, "daemon.log")
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string><string>daemon</string><string>--log-file</string><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>AbandonProcessGroup</key><true/>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, s.rec.Binary, logPath, filepath.Join(s.paths.State, "daemon.crash.log"))
	if err := s.write(plist, content, 0o644); err != nil {
		return err
	}
	s.rec.Service = launchdLabel
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run()
	return s.run("launchctl", "bootstrap", domain, plist)
}

func (s *setupCtx) shims(skip bool) func() error {
	return func() error {
		if skip {
			return nil
		}
		dir := shim.Dir(&s.cfg)
		if s.system {
			dir = "/usr/local/lib/berth/shims"
		}
		if !s.dry {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		var made []string
		for _, t := range s.cfg.Shims.Tools {
			link := filepath.Join(dir, t)
			if shim.Real(t, s.rec.Binary) == "" {
				continue // tool not installed here
			}
			if target, err := os.Readlink(link); err == nil && target == s.rec.Binary {
				made = append(made, t)
				continue
			}
			made = append(made, t)
			if s.dry {
				continue
			}
			_ = os.Remove(link)
			if err := os.Symlink(s.rec.Binary, link); err != nil {
				return err
			}
			s.record(link)
		}
		if len(made) > 0 {
			s.did("shims in %s: %s", dir, strings.Join(made, " "))
		}
		return nil
	}
}

func (s *setupCtx) path(skip bool) func() error {
	return func() error {
		if skip {
			return nil
		}
		dir := shim.Dir(&s.cfg)
		if s.system {
			dir = "/usr/local/lib/berth/shims"
			return s.write("/etc/profile.d/berth.sh", fmt.Sprintf("%s\ncase \":$PATH:\" in *\":%s:\"*) ;; *) PATH=\"%s:$PATH\" ;; esac\n%s\n", rcBegin, dir, dir, rcEnd), 0o644)
		}
		home, _ := os.UserHomeDir()
		posix := fmt.Sprintf("%s\ncase \":$PATH:\" in *\":%s:\"*) ;; *) export PATH=\"%s:$PATH\" ;; esac\n%s\n", rcBegin, dir, dir, rcEnd)
		fish := fmt.Sprintf("%s\nfish_add_path --prepend %s\n%s\n", rcBegin, dir, rcEnd)
		for _, rc := range []struct{ path, block string }{
			{filepath.Join(home, ".bashrc"), posix}, {filepath.Join(home, ".zshrc"), posix},
			{filepath.Join(home, ".profile"), posix}, {filepath.Join(home, ".config", "fish", "config.fish"), fish},
		} {
			if _, err := os.Stat(rc.path); err != nil {
				continue
			}
			if err := s.rcBlock(rc.path, rc.block); err != nil {
				return err
			}
		}
		if runtime.GOOS == "linux" && s.host.SystemdUser {
			// User services (agents, editors) read environment.d; set it for the running manager too.
			envd := filepath.Join(home, ".config", "environment.d", "60-berth.conf")
			if err := s.write(envd, fmt.Sprintf("PATH=%s:${PATH}\n", dir), 0o644); err != nil {
				return err
			}
			if out, err := exec.Command("systemctl", "--user", "show-environment").Output(); err == nil {
				for _, line := range strings.Split(string(out), "\n") {
					if p, ok := strings.CutPrefix(line, "PATH="); ok && !strings.Contains(":"+p+":", ":"+dir+":") {
						_ = s.run("systemctl", "--user", "set-environment", "PATH="+dir+":"+p)
					}
				}
			}
		}
		return nil
	}
}

func (s *setupCtx) rcBlock(path, block string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	if i := strings.Index(text, rcBegin); i >= 0 {
		if j := strings.Index(text[i:], rcEnd); j >= 0 {
			end := i + j + len(rcEnd)
			if end < len(text) && text[end] == '\n' {
				end++
			}
			if text[i:end] == block {
				return nil
			}
			text = text[:i] + text[i+j+len(rcEnd):]
			text = strings.TrimSuffix(text, "\n")
		}
	}
	if !strings.HasSuffix(text, "\n") && text != "" {
		text += "\n"
	}
	text += block
	s.did("PATH block in %s", path)
	for _, f := range s.rec.RCFiles {
		if f == path {
			return s.writeRC(path, text)
		}
	}
	s.rec.RCFiles = append(s.rec.RCFiles, path)
	return s.writeRC(path, text)
}

func (s *setupCtx) writeRC(path, text string) error {
	if s.dry {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), info.Mode().Perm())
}

func cmdUninstall(args []string) int {
	fs := newFlags("uninstall", "[--system] [--purge]")
	system := fs.Bool("system", false, "remove the system-wide installation")
	purge := fs.Bool("purge", false, "also delete the config and all job records and logs")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	paths := config.UserPaths()
	ctl := []string{"--user"}
	if *system {
		paths, ctl = config.SystemPaths(), nil
	}
	var rec installed
	data, err := os.ReadFile(recordPath(paths))
	if err != nil {
		return fail(fmt.Errorf("no installation record at %s (nothing installed by setup)", recordPath(paths)))
	}
	_ = json.Unmarshal(data, &rec)
	switch {
	case strings.HasPrefix(rec.Service, "pid:"):
		var pid int
		var start uint64
		if _, err := fmt.Sscanf(rec.Service, "pid:%d:%d", &pid, &start); err == nil && start != 0 && probe.ProcStart(pid) == start {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 50 && probe.ProcStart(pid) == start; i++ {
				time.Sleep(100 * time.Millisecond)
			}
			fmt.Println("  stopped the daemon (pid", pid, ")")
		}
	case rec.Service == launchdLabel:
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel).Run()
	case rec.Service != "":
		_ = exec.Command("systemctl", append(ctl, "disable", "--now", rec.Service)...).Run()
	}
	for _, f := range rec.Files {
		if f == paths.Config && !*purge {
			continue
		}
		if err := os.Remove(f); err == nil {
			fmt.Println("  removed", f)
		}
	}
	for _, rc := range rec.RCFiles {
		data, err := os.ReadFile(rc)
		if err != nil {
			continue
		}
		text := string(data)
		if i := strings.Index(text, rcBegin); i >= 0 {
			if j := strings.Index(text[i:], rcEnd); j >= 0 {
				text = text[:i] + strings.TrimPrefix(text[i+j+len(rcEnd):], "\n")
				if err := os.WriteFile(rc, []byte(text), 0o644); err == nil {
					fmt.Println("  removed PATH block from", rc)
				}
			}
		}
	}
	if len(ctl) > 0 {
		_ = exec.Command("systemctl", append(ctl, "daemon-reload")...).Run()
	}
	_ = os.Remove(recordPath(paths))
	if *purge {
		_ = os.RemoveAll(paths.State)
		_ = os.Remove(paths.Config)
		_ = os.Remove(filepath.Dir(paths.Config)) // only if now empty
		if *system {
			_ = os.Remove(filepath.Dir(paths.Socket))
		}
		fmt.Println("  removed", paths.State, "and", paths.Config)
	}
	fmt.Println("berth uninstalled.")
	return 0
}

// setupMaint installs a root systemd timer running `berth maint` for every
// btrfs filesystem among the watched paths. It is independent of the daemon,
// so a rootless installation still gets storage maintenance.
func setupMaint(dry bool) int {
	if os.Geteuid() != 0 {
		return fail(fmt.Errorf("--maint needs root: sudo %s setup --maint", self()))
	}
	s := &setupCtx{system: true, dry: dry, paths: config.SystemPaths()}
	// Under sudo, size maintenance from the invoking user's configuration and home.
	if name := os.Getenv("SUDO_USER"); name != "" {
		if u, err := user.Lookup(name); err == nil {
			os.Setenv("HOME", u.HomeDir)
			os.Unsetenv("XDG_CONFIG_HOME")
		}
	}
	cfg, _, err := clientConfig()
	if err != nil {
		return fail(err)
	}
	bin := self()
	if strings.HasPrefix(bin, "/home/") || strings.HasPrefix(bin, os.TempDir()) {
		target := "/usr/local/bin/berth"
		s.did("install %s -> %s (root timers run root-owned binaries)", bin, target)
		if !dry {
			data, err := os.ReadFile(bin)
			if err != nil {
				return fail(err)
			}
			if err := os.WriteFile(target+".new", data, 0o755); err != nil {
				return fail(err)
			}
			if err := os.Rename(target+".new", target); err != nil {
				return fail(err)
			}
		}
		bin = target
	}
	var paths []string
	for _, b := range probe.Snapshot(cfg.Safety.WatchPaths, "", nil).Btrfs {
		paths = append(paths, b.Path)
	}
	if len(paths) == 0 {
		return fail(fmt.Errorf("no btrfs filesystem among the watched paths %v; nothing to maintain", cfg.Safety.WatchPaths))
	}
	target := units.FormatBytes(int64(cfg.Safety.BtrfsTargetUnallocated))
	var execs strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&execs, "ExecStart=%s maint --path %s --target %s --budget 5m\n", bin, p, target)
	}
	unit := fmt.Sprintf(`[Unit]
Description=berth: reclaim btrfs unallocated space (bounded balances)
Documentation=https://github.com/killedoxygen/berth

[Service]
Type=oneshot
%sNice=15
IOSchedulingClass=idle
TimeoutStartSec=12min
`, execs.String())
	timer := `[Unit]
Description=berth: btrfs maintenance every 10 minutes

[Timer]
OnBootSec=3min
OnUnitActiveSec=10min
RandomizedDelaySec=1min

[Install]
WantedBy=timers.target
`
	for _, f := range []struct{ path, content string }{
		{"/etc/systemd/system/berth-maint.service", unit}, {"/etc/systemd/system/berth-maint.timer", timer},
	} {
		if err := s.write(f.path, f.content, 0o644); err != nil {
			return fail(err)
		}
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "berth-maint.timer"}} {
		if err := s.run("systemctl", args...); err != nil {
			return fail(err)
		}
	}
	fmt.Printf("btrfs maintenance timer installed for %s (target %s unallocated)\n", strings.Join(paths, ", "), target)
	return 0
}
