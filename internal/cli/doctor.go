package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/client"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/shim"
	"github.com/killedoxygen/berth/internal/units"
)

func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail | info
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func cmdDoctor(args []string) int {
	fs := newFlags("doctor", "[--json]")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	var checks []check
	add := func(name, status, detail, fix string) {
		checks = append(checks, check{name, status, detail, fix})
	}
	host := probe.Detect()
	cfg, paths, err := clientConfig()
	if err != nil {
		add("config", "fail", err.Error(), "fix the file, or move it aside to use host defaults: "+paths.Config)
	} else {
		src := "host defaults (no config file)"
		if _, err := os.Stat(paths.Config); err == nil {
			src = paths.Config
		}
		add("config", "ok", fmt.Sprintf("%s: pool %s / %g CPUs, default job %s / %g CPUs", src,
			units.FormatBytes(int64(cfg.Pool.Memory)), cfg.Pool.CPUs, units.FormatBytes(int64(cfg.Defaults.Memory)), cfg.Defaults.CPUs), "")
	}

	sock := client.Socket()
	resp, err := api.Call(sock, api.Request{Op: "status"})
	var st *api.Status
	if err != nil {
		add("daemon", "fail", err.Error(), "berth setup   (or run `berth daemon` in a terminal to see why it stops)")
	} else {
		st = resp.Status
		detail := fmt.Sprintf("%s daemon %s at %s, %s backend", st.Mode, st.Version, sock, st.Backend)
		if st.Version != Version {
			add("daemon", "warn", detail+"; this CLI is "+Version, restartHint(st.Mode))
		} else {
			add("daemon", "ok", detail, "")
		}
	}

	switch {
	case host.OS == "linux" && cfg.Backend.Kind == "process" && host.Systemd:
		add("limits", "warn", "process backend: memory caps are enforced by polling, not by the kernel",
			"enable the systemd user manager (systemctl --user) or run a system daemon: sudo berth setup --system")
	case cfg.Backend.Kind == "process":
		add("limits", "info", "process backend: memory caps enforced by polling resident memory (no cgroups here)", "")
	case host.OS == "linux" && !host.Root:
		need := []string{"memory", "cpu", "pids"}
		var missing []string
		for _, c := range need {
			if !strings.Contains(" "+host.UserDelegation+" ", " "+c+" ") {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 {
			add("limits", "warn", "the user manager lacks cgroup controllers: "+strings.Join(missing, ", "),
				"sudo mkdir -p /etc/systemd/system/user@.service.d && printf '[Service]\\nDelegate=cpu cpuset io memory pids\\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf && sudo systemctl daemon-reload")
		} else {
			add("limits", "ok", "kernel-enforced memory, CPU and task limits (cgroup v2: "+host.UserDelegation+")", "")
		}
	}
	if host.OS == "linux" && cfg.Backend.Kind == "systemd" {
		ctl := []string{"--user"}
		if st != nil && st.Mode == "system" {
			ctl = nil
		}
		out, err := exec.Command("systemctl", append(ctl, "show", cfg.Backend.Slice, "--property=MemoryMax", "--value")...).Output()
		got := strings.TrimSpace(string(out))
		want := strconv.FormatInt(int64(cfg.Pool.Memory), 10)
		switch {
		case err != nil:
		case got == want:
			add("slice", "ok", cfg.Backend.Slice+" caps all jobs at "+units.FormatBytes(int64(cfg.Pool.Memory)), "")
		default:
			add("slice", "warn", fmt.Sprintf("%s MemoryMax is %s, config pool is %s", cfg.Backend.Slice, got, units.FormatBytes(int64(cfg.Pool.Memory))),
				"berth setup   (regenerates the slice from the config)")
		}
	}
	if host.OS == "linux" && (st == nil || st.Mode == "user") && lingerOff() {
		add("linger", "warn", "the user daemon and its jobs stop when you log out", "berth setup --linger")
	}

	dir := shim.Dir(&cfg)
	if tools := installedShims(cfg); len(tools) == 0 {
		add("shims", "info", "no tool shims installed; heavy tools run unqueued unless called via berth run", "berth setup")
	} else {
		// What matters is what this PATH resolves each tool to.
		me, _ := filepath.EvalSymlinks(self())
		var routed, bypassed []string
		for _, t := range tools {
			p, err := exec.LookPath(t)
			if err != nil {
				continue
			}
			if r, _ := filepath.EvalSymlinks(p); r == me {
				routed = append(routed, t)
			} else {
				bypassed = append(bypassed, t)
			}
		}
		if len(bypassed) > 0 {
			add("shims", "warn", "on this PATH these run unrouted: "+strings.Join(bypassed, " "),
				`export PATH="`+dir+`:$PATH"   (setup adds this to your shell rc files and user services)`)
		} else {
			add("shims", "ok", "routing "+strings.Join(routed, " "), "")
		}
	}

	if st != nil {
		h := st.Health
		for _, d := range h.Disks {
			status, fix := "ok", ""
			if d.Free < int64(cfg.Safety.MinFreeDisk) {
				status, fix = "warn", "free space below the admission floor: nothing new starts until it is freed"
			}
			add("disk", status, fmt.Sprintf("%s: %s free of %s (floor %s)", d.Path, units.FormatBytes(d.Free), units.FormatBytes(d.Total), units.FormatBytes(int64(cfg.Safety.MinFreeDisk))), fix)
		}
		if l := st.Logs; l.Budget > 0 {
			status, fix := "ok", ""
			if l.Bytes > l.Budget*9/10 {
				status, fix = "warn", "berth prune --older-than 7d   (or raise logs.max_total)"
			}
			add("logs", status, fmt.Sprintf("%s of %s budget, compressed, finished jobs kept %s", units.FormatBytes(l.Bytes),
				units.FormatBytes(l.Budget), units.FormatDuration(l.Retention)), fix)
		}
		for _, b := range h.Btrfs {
			status, fix := "ok", ""
			if b.Unallocated < int64(cfg.Safety.BtrfsMinUnallocated) {
				status, fix = "warn", "sudo berth maint --path "+b.Path+"   (bounded balance; safe while jobs run)"
			}
			add("btrfs", status, fmt.Sprintf("%s: %s unallocated (floor %s)", b.Path, units.FormatBytes(b.Unallocated), units.FormatBytes(int64(cfg.Safety.BtrfsMinUnallocated))), fix)
		}
		for _, j := range st.Jobs {
			if j.State == job.Queued && time.Since(j.Created) > time.Hour {
				add("waiting", "warn", fmt.Sprintf("job %s (%s) has waited %s: %s", job.Short(j.ID), j.Label(),
					units.FormatDuration(time.Since(j.Created)), client.Explain(&j)), "berth cancel "+job.Short(j.ID)+"  or  berth update "+job.Short(j.ID)+" --memory SMALLER")
			}
		}
	}
	if host.SELinux == "enforcing" {
		add("selinux", "info", "enforcing; user-mode jobs run in your login context, system-mode jobs in "+orDefault(cfg.Backend.SELinuxContext, "the login context (auto)"), "")
	}

	if *asJSON {
		printJSON(checks)
	} else {
		for _, c := range checks {
			mark := map[string]string{"ok": "ok  ", "warn": "WARN", "fail": "FAIL", "info": "info"}[c.Status]
			fmt.Printf("%s %-8s %s\n", mark, c.Name, c.Detail)
			if c.Fix != "" {
				fmt.Printf("              fix: %s\n", c.Fix)
			}
		}
	}
	for _, c := range checks {
		if c.Status == "fail" {
			return 1
		}
	}
	return 0
}

func restartHint(mode string) string {
	if mode == "system" {
		return "sudo systemctl restart berth"
	}
	return "systemctl --user restart berth   (or rerun berth setup)"
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
