// Package cli implements the berth command line.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/client"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/units"
)

var Version = "dev"

type command struct {
	name    string
	summary string
	run     func(args []string) int
}

var commands []command

func init() {
	commands = []command{
		{"run", "run a command as a job with your terminal; exit status is the command's", cmdRun},
		{"submit", "queue a command in the background; prints the job id", cmdSubmit},
		{"wait", "wait for jobs to finish, streaming their output", cmdWait},
		{"status", "show capacity and jobs, with why each waiting job waits", cmdStatus},
		{"logs", "print a job's output (-f to follow)", cmdLogs},
		{"cancel", "cancel jobs", cmdCancel},
		{"update", "change a job's size, priority, kind or retries", cmdUpdate},
		{"pause", "stop admitting new jobs (running jobs continue)", cmdPause},
		{"resume", "admit jobs again", cmdResume},
		{"setup", "install berth for this user (or --system): config, service, shims, PATH", cmdSetup},
		{"uninstall", "remove what setup installed", cmdUninstall},
		{"doctor", "check the installation and explain how to fix problems", cmdDoctor},
		{"config", "print the effective configuration", cmdConfig},
		{"shims", "list shimmed tools, or explain how one invocation would be routed", cmdShims},
		{"agents", "print instructions for AI agents (for AGENTS.md / CLAUDE.md)", cmdAgents},
		{"mcp", "serve the Model Context Protocol on stdio (for agent hosts)", cmdMCP},
		{"daemon", "run the daemon in the foreground", cmdDaemon},
		{"maint", "reclaim btrfs unallocated space with bounded balances (root)", cmdMaint},
		{"version", "print the version", func([]string) int { fmt.Println("berth", Version); return 0 }},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `berth: share one machine's memory, CPUs, disk and GPUs among jobs and agents.

Usage: berth <command> [options]

`)
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(os.Stderr, `
Examples:
  berth run -m 4G -- cargo build --release
  berth submit --name e2e --timeout 30m -- make test
  berth status
  berth run --after 3f2a -- ./deploy.sh

Exit status of run/wait is the job's own; 75 = never started (queue timeout,
cannot fit, cancelled first), 124 = job timed out, 125 = berth error,
130 = cancelled by you. "berth <command> -h" shows options.
`)
}

// Main dispatches berth subcommands.
func Main(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] == "--version" || args[0] == "-V" {
		fmt.Println("berth", Version)
		return 0
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(args[1:])
		}
	}
	fmt.Fprintf(os.Stderr, "berth: unknown command %q (berth help)\n", args[0])
	return 2
}

func newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet("berth "+name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: berth %s %s\n\n", name, synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse handles -h and errors uniformly; it returns false when the caller should exit with code.
func parse(fs *flag.FlagSet, args []string) (bool, int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, 0
		}
		return false, 2
	}
	return true, 0
}

// fail prints an error the same way for every command and returns 125.
func fail(err error) int {
	var ae *api.Error
	if errors.As(err, &ae) && ae.Code == api.EUnavailable {
		fmt.Fprintln(os.Stderr, "berth:", ae.Message)
		return client.ExitBerth
	}
	fmt.Fprintln(os.Stderr, "berth:", err)
	return client.ExitBerth
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ---------------------------------------------------------------- job flags

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

type sizeFlag struct{ v int64 }

func (s *sizeFlag) String() string {
	if s.v == 0 {
		return ""
	}
	return units.FormatBytes(s.v)
}
func (s *sizeFlag) Set(v string) (err error) { s.v, err = units.ParseBytes(v); return }

type durFlag struct{ v time.Duration }

func (d *durFlag) String() string {
	if d.v == 0 {
		return ""
	}
	return d.v.String()
}
func (d *durFlag) Set(v string) (err error) { d.v, err = units.ParseDuration(v); return }

type jobFlags struct {
	memory, disk, unit    sizeFlag
	cpus                  float64
	timeout, queueTimeout durFlag
	gpus                  int
	resources, after, any listFlag
	labels, env           listFlag
	group, name, key, cwd string
	priority, retries     int
	service, noEnv        bool
}

func addJobFlags(fs *flag.FlagSet) *jobFlags {
	f := &jobFlags{disk: sizeFlag{-1}}
	for _, n := range []string{"m", "memory"} {
		fs.Var(&f.memory, n, "memory to reserve and hard-cap, e.g. 4G (default: the host default, see `berth status`)")
	}
	for _, n := range []string{"c", "cpus"} {
		fs.Float64Var(&f.cpus, n, 0, "CPU cores to reserve and cap (default: the host default)")
	}
	fs.Var(&f.disk, "disk", "disk space the job may write; an admission budget, not a quota (default 2G)")
	fs.IntVar(&f.gpus, "gpus", 0, "GPUs to reserve (shorthand for --resource gpu=N)")
	fs.Var(&f.resources, "resource", "reserve a counted pool resource, NAME=N (repeatable)")
	for _, n := range []string{"t", "timeout"} {
		fs.Var(&f.timeout, n, "execution deadline, e.g. 30m (default: none)")
	}
	fs.Var(&f.queueTimeout, "queue-timeout", "give up if not started within this long (default: wait)")
	for _, n := range []string{"g", "group"} {
		fs.StringVar(&f.group, n, "", "exclusive group: at most one active job per group")
	}
	fs.Var(&f.after, "after", "start only after job ID succeeds (repeatable)")
	fs.Var(&f.any, "after-any", "start only after job ID finishes, whatever the outcome (repeatable)")
	for _, n := range []string{"p", "priority"} {
		fs.IntVar(&f.priority, n, 0, "higher starts first among waiting jobs")
	}
	fs.BoolVar(&f.service, "service", false, "long-running service: charged by observed use once settled")
	fs.IntVar(&f.retries, "retries", 0, "requeue up to N times after a host restart or emergency stop")
	for _, n := range []string{"n", "name"} {
		fs.StringVar(&f.name, n, "", "label shown in status and logs")
	}
	fs.StringVar(&f.key, "key", "", "idempotency key: reuse an unfinished job with the same key")
	fs.Var(&f.labels, "label", "attach KEY=VALUE metadata (repeatable)")
	fs.StringVar(&f.cwd, "cwd", "", "working directory (default: current)")
	fs.Var(&f.env, "env", "set KEY=VALUE in the job's environment (repeatable)")
	fs.BoolVar(&f.noEnv, "clean-env", false, "do not pass the current environment (only PATH, HOME, USER, LANG and --env)")
	fs.Var(&f.unit, "unit-memory", "cap the command's own processes below the reservation (for clients whose work runs elsewhere)")
	return f
}

func (f *jobFlags) spec(argv []string) (job.Spec, error) {
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return job.Spec{}, errors.New("no command given (berth run -- COMMAND [ARGS])")
	}
	cwd := f.cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	env := os.Environ()
	if f.noEnv {
		env = nil
		for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TERM", "TMPDIR"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	for _, kv := range f.env {
		if !strings.Contains(kv, "=") {
			return job.Spec{}, fmt.Errorf("--env %q: want KEY=VALUE", kv)
		}
		env = append(env, kv)
	}
	s := job.Spec{Argv: argv, Cwd: cwd, Env: env, Name: f.name, Key: f.key, Memory: f.memory.v, CPUs: f.cpus, Disk: f.disk.v,
		UnitMemory: f.unit.v, Group: f.group, After: f.after, AfterAny: f.any, Priority: f.priority, Timeout: f.timeout.v,
		QueueTimeout: f.queueTimeout.v, Retries: f.retries, Kind: job.Batch}
	if f.service {
		s.Kind = job.Service
	}
	if f.gpus > 0 || len(f.resources) > 0 {
		s.Resources = map[string]int{}
		if f.gpus > 0 {
			s.Resources["gpu"] = f.gpus
		}
		for _, r := range f.resources {
			name, n, ok := strings.Cut(r, "=")
			count, err := strconv.Atoi(n)
			if !ok || err != nil || count <= 0 {
				return s, fmt.Errorf("--resource %q: want NAME=N", r)
			}
			s.Resources[name] = count
		}
	}
	if len(f.labels) > 0 {
		s.Labels = map[string]string{}
		for _, kv := range f.labels {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return s, fmt.Errorf("--label %q: want KEY=VALUE", kv)
			}
			s.Labels[k] = v
		}
	}
	return s, nil
}

func label(s job.Spec) string {
	j := job.Job{Spec: s}
	return j.Label()
}

func sortedJobs(js []job.Job) []job.Job {
	sort.SliceStable(js, func(a, b int) bool { return js[a].Created.Before(js[b].Created) })
	return js
}

type flagT = flag.Flag
