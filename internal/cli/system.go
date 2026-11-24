package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/daemon"
	"github.com/killedoxygen/berth/internal/logs"
	"github.com/killedoxygen/berth/internal/maint"
	"github.com/killedoxygen/berth/internal/mcp"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/shim"
	"github.com/killedoxygen/berth/internal/units"
)

// self is the absolute path of the running binary.
func self() string {
	p, err := os.Executable()
	if err != nil {
		return "berth"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// clientConfig is the policy a client-side tool (shims, doctor) should follow:
// the user's config, else the system config, else host defaults.
func clientConfig() (config.Config, config.Paths, error) {
	host := probe.Detect()
	user, system := config.UserPaths(), config.SystemPaths()
	paths := user
	if _, err := os.Stat(user.Config); err != nil {
		if _, err := os.Stat(system.Config); err == nil {
			paths = system
		} else if config.ClientSocket() == system.Socket {
			paths = system
		}
	}
	cfg, err := config.Load(paths.Config, host)
	return cfg, paths, err
}

func cmdDaemon(args []string) int {
	fs := newFlags("daemon", "[--system]")
	system := fs.Bool("system", false, "system-wide daemon (root): any user may submit; jobs run as the submitter")
	logFile := fs.String("log-file", "", "write the daemon log here, size-rotated and compressed (default: stderr, e.g. the journal)")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	paths := config.UserPaths()
	if *system {
		if os.Geteuid() != 0 {
			return fail(fmt.Errorf("--system needs root"))
		}
		paths = config.SystemPaths()
	}
	host := probe.Detect()
	cfg, err := config.Load(paths.Config, host)
	if err != nil {
		return fail(err)
	}
	if *system && cfg.Backend.SELinuxContext == "" && host.SELinux == "enforcing" {
		if _, err := os.Stat("/etc/selinux/targeted"); err == nil {
			// Jobs get the context of a normal login, not the service manager's (which may not use docker, etc).
			cfg.Backend.SELinuxContext = "unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023"
		}
	}
	if *system && cfg.Backend.Kind == "process" {
		return fail(fmt.Errorf("a system daemon needs the systemd backend"))
	}
	opt := daemon.Options{Config: cfg, Paths: paths, Self: self(), Version: Version}
	if *logFile != "" {
		_ = os.MkdirAll(filepath.Dir(*logFile), 0o700)
		w, err := logs.OpenRotating(*logFile, int64(cfg.Logs.DaemonLogSize), cfg.Logs.DaemonLogKeep)
		if err != nil {
			return fail(err)
		}
		opt.Log = w
	}
	d, err := daemon.New(opt)
	if err != nil {
		return fail(err)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigs; d.Stop() }()
	if err := d.Serve(); err != nil {
		return fail(err)
	}
	return 0
}

func cmdMCP(args []string) int {
	fs := newFlags("mcp", "")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if err := mcp.Serve(os.Stdin, os.Stdout, Version); err != nil {
		return fail(err)
	}
	return 0
}

func cmdAgents(args []string) int {
	fs := newFlags("agents", "")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	cfg, _, _ := clientConfig()
	fmt.Printf(`## Heavy commands on this machine: berth

%s

From a shell:
- berth run -m 2G -- cargo test         # waits its turn, then runs with your terminal
- berth submit --name build -- make -j8 # background; prints an id
- berth wait ID / berth logs -f ID      # follow it
- berth status                          # capacity, "fits now", why each job waits
- Exit 75 means the job never started (it says why); 124 timed out; 125 berth error.
- Default size here: --memory %s --cpus %g. Ask for less when you can.
`, mcp.Instructions, units.FormatBytes(int64(cfg.Defaults.Memory)), cfg.Defaults.CPUs)
	if tools := installedShims(cfg); len(tools) > 0 {
		fmt.Printf("- These tools are already routed through berth automatically: %s.\n", strings.Join(tools, ", "))
	}
	return 0
}

func installedShims(cfg config.Config) []string {
	dir := shim.Dir(&cfg)
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func cmdConfig(args []string) int {
	fs := newFlags("config", "[--defaults] [--path]")
	defaults := fs.Bool("defaults", false, "print the defaults detected for this host, ignoring the config file")
	path := fs.Bool("path", false, "print the config file path")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	cfg, paths, err := clientConfig()
	if *path {
		fmt.Println(paths.Config)
		return 0
	}
	if *defaults {
		cfg = config.Default(probe.Detect())
	} else if err != nil {
		return fail(err)
	}
	fmt.Printf("# effective configuration (%s)\n", paths.Config)
	fmt.Print(config.Encode(cfg))
	return 0
}

func cmdShims(args []string) int {
	fs := newFlags("shims", "[explain TOOL ARGS...]")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	cfg, _, err := clientConfig()
	if err != nil {
		return fail(err)
	}
	if fs.NArg() > 0 && fs.Arg(0) == "explain" {
		rest := fs.Args()[1:]
		if len(rest) == 0 {
			return fail(fmt.Errorf("berth shims explain TOOL [ARGS...]"))
		}
		cwd, _ := os.Getwd()
		d := shim.Route(&cfg, rest[0], rest[1:], cwd, probe.IsTerminal(0) && probe.IsTerminal(1))
		verdict := "runs directly"
		if d.Queue {
			verdict = "queued"
			size := []string{}
			if d.Memory > 0 {
				size = append(size, "memory "+units.FormatBytes(d.Memory))
			}
			if d.CPUs > 0 {
				size = append(size, fmt.Sprintf("%g CPUs", d.CPUs))
			}
			if d.Timeout > 0 {
				size = append(size, "timeout "+units.FormatDuration(d.Timeout))
			}
			if d.Group != "" {
				size = append(size, "group "+d.Group)
			}
			if len(size) > 0 {
				verdict += " (" + strings.Join(size, ", ") + ")"
			}
		}
		fmt.Printf("%s: %s: %s\n", strings.Join(rest, " "), verdict, d.Why)
		if strings.Join(d.Argv, " ") != strings.Join(rest[1:], " ") {
			fmt.Printf("  runs as: %s %s\n", rest[0], strings.Join(d.Argv, " "))
		}
		return 0
	}
	dir := shim.Dir(&cfg)
	fmt.Printf("shim directory: %s (on PATH: %v)\n", dir, onPath(dir))
	for _, t := range cfg.Shims.Tools {
		state := "not installed"
		if target, err := os.Readlink(filepath.Join(dir, t)); err == nil {
			state = "-> " + target
		}
		real := shim.Real(t, self())
		if real == "" {
			real = "(tool not found)"
		}
		fmt.Printf("  %-8s %-14s real: %s\n", t, state, real)
	}
	return 0
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

func cmdMaint(args []string) int {
	fs := newFlags("maint", "[--path DIR] [--budget 5m]")
	path := fs.String("path", "/", "a path on the btrfs filesystem")
	budget := &durFlag{5 * time.Minute}
	fs.Var(budget, "budget", "total time allowed")
	target := &sizeFlag{}
	fs.Var(target, "target", "stop once this much is unallocated (default: safety.btrfs_target_unallocated)")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	cfg, _, err := clientConfig()
	if err != nil {
		return fail(err)
	}
	want := int64(cfg.Safety.BtrfsTargetUnallocated)
	if target.v > 0 {
		want = target.v
	}
	summary, err := maint.Balance(*path, want, budget.v, func(s string) { fmt.Println(s) })
	if err != nil {
		return fail(err)
	}
	fmt.Println(summary)
	return 0
}
