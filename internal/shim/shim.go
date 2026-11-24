// Package shim routes heavy tool invocations through the queue. berth is
// installed as a symlink named after each tool (busybox style); invoked under
// such a name, it decides whether this invocation is heavy, and either runs
// the real tool directly or as a queued passthrough job with the same
// terminal, output and exit status.
//
// Light, interactive and nested invocations always run directly, and so does
// everything when the daemon is unreachable: a shim must never be the reason
// a command fails.
package shim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/client"
	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

// Decision is the routing outcome for one invocation.
type Decision struct {
	Queue   bool
	Argv    []string // arguments after the tool name (may gain container limits)
	Memory  int64    // 0: daemon default
	CPUs    float64
	Unit    int64 // cap for the tool's own process when the work runs elsewhere (containers)
	Timeout time.Duration
	Queue2  time.Duration // queue timeout
	Group   string
	Why     string // for `berth shims explain`
}

// Known lists tools with built-in routing.
var Known = []string{"cargo", "make", "gmake", "ninja", "cmake", "go", "docker", "podman", "git"}

// Route decides for tool with args. cwd and env matter for git.
func Route(cfg *config.Config, tool string, args []string, cwd string, interactive bool) Decision {
	d := Decision{Argv: args}
	if len(args) > 0 && (contains(args, "--version") || contains(args, "--help") || args[0] == "help" || args[0] == "version") {
		d.Why = "help/version"
		return d
	}
	for _, r := range cfg.Shims.Rules {
		if r.Tool != tool {
			continue
		}
		sub := firstPositional(args)
		if (len(r.Subcommands) == 0 || containsStr(r.Subcommands, sub)) && (len(r.FlagsAny) == 0 || containsAny(args, r.FlagsAny...)) {
			return Decision{Queue: true, Argv: args, Memory: int64(r.Memory), CPUs: r.CPUs, Timeout: time.Duration(r.Timeout), Group: r.Group,
				Why: "config rule for " + tool}
		}
		d.Why = "no config rule matches"
		return d
	}
	switch tool {
	case "cargo":
		sub := firstPositional(args)
		if containsStr([]string{"build", "b", "test", "t", "check", "c", "bench", "clippy", "rustc", "doc", "install"}, sub) {
			d.Queue, d.Why = true, "cargo "+sub
		} else {
			d.Why = "light cargo subcommand"
		}
	case "make", "gmake":
		if containsAny(args, "-n", "--dry-run", "--just-print", "-q", "--question", "-p", "--print-data-base") {
			d.Why = "make dry run"
		} else {
			d.Queue, d.Why = true, "make"
		}
	case "ninja":
		if containsAny(args, "-t", "-n") {
			d.Why = "ninja tool or dry run"
		} else {
			d.Queue, d.Why = true, "ninja"
		}
	case "cmake":
		if containsAny(args, "--build", "--workflow") {
			d.Queue, d.Why = true, "cmake --build"
		} else {
			d.Why = "cmake configure"
		}
	case "go":
		sub := firstPositional(args)
		if containsStr([]string{"build", "test", "install", "vet"}, sub) {
			d.Queue, d.Why = true, "go "+sub
		} else {
			d.Why = "light go subcommand"
		}
	case "docker", "podman":
		return container(cfg, args, interactive)
	case "git":
		if interactive {
			d.Why = "interactive git"
			return d
		}
		return gitRoute(cfg, args, cwd)
	default:
		d.Why = "no routing for " + tool
	}
	return d
}

func contains(args []string, s string) bool { return containsStr(args, s) }

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsAny(args []string, flags ...string) bool {
	for _, f := range flags {
		if containsStr(args, f) {
			return true
		}
	}
	return false
}

func firstPositional(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// ---------------------------------------------------------------- containers

var runValueShort = "aceEhlmpuvw"

// containerRun reads `run` options: detached?, memory, cpus, and where options end.
func containerRun(args []string, start int) (detached bool, memory, cpus string) {
	for i := start; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			return
		}
		if strings.HasPrefix(a, "--") {
			name, inline, eq := strings.Cut(a[2:], "=")
			next := func() string {
				if eq {
					return inline
				}
				if i+1 < len(args) {
					return args[i+1]
				}
				return ""
			}
			switch name {
			case "detach", "d":
				if !eq || inline == "true" {
					detached = true
				}
			case "memory":
				memory = next()
			case "cpus":
				cpus = next()
			}
			boolean := containsStr([]string{"detach", "interactive", "tty", "rm", "privileged", "init", "read-only", "publish-all",
				"disable-content-trust", "no-healthcheck", "oom-kill-disable", "quiet", "help", "sig-proxy", "replace", "tls-verify"}, name)
			if !eq && !boolean {
				i++
			}
			continue
		}
		cluster := a[1:]
		for k, ch := range cluster {
			if ch == 'd' {
				detached = true
			}
			if strings.ContainsRune(runValueShort, ch) {
				val := cluster[k+1:]
				if val == "" && i+1 < len(args) {
					val = args[i+1]
					i++
				}
				if ch == 'm' {
					memory = val
				}
				break
			}
		}
	}
	return
}

func containerSub(args []string) int {
	withValue := map[string]bool{"-H": true, "--host": true, "--context": true, "-c": true, "--config": true, "-l": true,
		"--log-level": true, "--connection": true, "--url": true, "--root": true, "--runroot": true}
	for i := 0; i < len(args); i++ {
		if withValue[args[i]] {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		return i
	}
	return -1
}

func container(cfg *config.Config, args []string, interactive bool) Decision {
	d := Decision{Argv: args}
	pos := containerSub(args)
	if pos < 0 {
		d.Why = "no subcommand"
		return d
	}
	verb, rest := args[pos], args[pos+1:]
	c := cfg.Shims.Container
	build := verb == "build" || verb == "bake" ||
		((verb == "buildx" || verb == "builder" || verb == "image") && len(rest) > 0 && (rest[0] == "build" || rest[0] == "bake")) ||
		(verb == "compose" && containsStr(rest, "build"))
	if build {
		return Decision{Queue: true, Argv: args, Memory: int64(c.BuildMemory), CPUs: c.BuildCPUs, Unit: int64(c.ClientMemory),
			Why: "image build (runs in the container engine; charged at build size)"}
	}
	runAt := -1
	if verb == "run" {
		runAt = pos + 1
	} else if (verb == "container") && len(rest) > 0 && rest[0] == "run" {
		runAt = pos + 2
	}
	if runAt < 0 {
		d.Why = "light container subcommand"
		return d
	}
	detached, memory, cpus := containerRun(args, runAt)
	mem, _ := units.ParseBytes(memory)
	cores, _ := strconv.ParseFloat(cpus, 64)
	var injected []string
	if mem <= 0 {
		mem = int64(c.RunMemory)
		injected = append(injected, "--memory", units.FormatBytes(mem))
	}
	if cores <= 0 {
		cores = c.RunCPUs
		injected = append(injected, "--cpus", strconv.FormatFloat(cores, 'f', -1, 64))
	}
	argv := append(append(append([]string{}, args[:runAt]...), injected...), args[runAt:]...)
	if detached || interactive {
		// Not queued (the client returns at once, or a person is typing), but still bounded.
		d.Argv, d.Why = argv, "detached or interactive run: limits applied, not queued"
		return d
	}
	return Decision{Queue: true, Argv: argv, Memory: mem, CPUs: cores, Unit: min(int64(c.ClientMemory), mem),
		Why: "container run, charged at the container's limit (" + units.FormatBytes(mem) + ")"}
}

// ---------------------------------------------------------------- git

// gitHeavy commands may be queued in large repositories; everything else, notably
// every read-only command, runs directly.
var gitHeavy = map[string]bool{"clone": true, "fetch": true, "pull": true, "push": true, "gc": true, "repack": true, "prune": true,
	"fsck": true, "maintenance": true, "checkout": true, "switch": true, "merge": true, "rebase": true, "reset": true,
	"cherry-pick": true, "revert": true, "am": true, "stash": true, "submodule": true, "sparse-checkout": true, "restore": true,
	"filter-branch": true, "filter-repo": true, "lfs": true, "index-pack": true, "pack-objects": true, "multi-pack-index": true,
	"commit-graph": true, "read-tree": true}

var gitLong = map[string]bool{"clone": true, "fetch": true, "pull": true, "push": true, "gc": true, "repack": true, "fsck": true,
	"maintenance": true, "submodule": true, "filter-branch": true, "filter-repo": true, "lfs": true, "index-pack": true,
	"pack-objects": true, "multi-pack-index": true}

var partialRE = regexp.MustCompile(`(?mi)^\s*(promisor\s*=\s*true|partialclonefilter\s*=)`)

func gitRoute(cfg *config.Config, args []string, cwd string) Decision {
	d := Decision{Argv: args}
	dir, gitDir, sub := cwd, os.Getenv("GIT_DIR"), ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			dir = resolve(dir, args[i+1])
			i++
		case a == "--git-dir" && i+1 < len(args):
			gitDir = resolve(dir, args[i+1])
			i++
		case a == "-c" || a == "--work-tree" || a == "--namespace" || a == "--config-env":
			i++
		case strings.HasPrefix(a, "--git-dir="):
			gitDir = resolve(dir, strings.TrimPrefix(a, "--git-dir="))
		case strings.HasPrefix(a, "-"):
		default:
			sub = a
		}
		if sub != "" {
			break
		}
	}
	if !gitHeavy[sub] {
		d.Why = "git " + sub + " runs directly (only heavy commands in large repositories are queued)"
		return d
	}
	if gitDir == "" {
		gitDir = findGitDir(dir)
	}
	if gitDir == "" {
		d.Why = "not in a repository"
		return d
	}
	common := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common = resolve(gitDir, strings.TrimSpace(string(data)))
	}
	large := exists(filepath.Join(common, "berth-heavy"))
	if info, err := os.Stat(filepath.Join(gitDir, "index")); err == nil && info.Size() >= int64(cfg.Shims.Git.LargeIndex) {
		large = true
	}
	if !large && gitLong[sub] {
		if data, err := os.ReadFile(filepath.Join(common, "config")); err == nil && partialRE.Match(data) {
			large = true
		}
	}
	if !large {
		d.Why = "repository is not large"
		return d
	}
	sum := sha256.Sum256([]byte(common))
	g := cfg.Shims.Git
	timeout := time.Duration(g.Timeout)
	if gitLong[sub] {
		timeout = time.Duration(g.LongTimeout)
	}
	return Decision{Queue: true, Argv: args, Memory: int64(g.Memory), CPUs: g.CPUs, Timeout: timeout,
		Queue2: time.Duration(g.QueueTimeout), Group: "git:" + hex.EncodeToString(sum[:8]), Why: "git " + sub + " in a large repository"}
}

func resolve(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func findGitDir(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		marker := filepath.Join(d, ".git")
		if info, err := os.Stat(marker); err == nil {
			if info.IsDir() {
				return marker
			}
			data, _ := os.ReadFile(marker)
			if p, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: "); ok {
				return resolve(d, p)
			}
			return ""
		}
		if exists(filepath.Join(d, "HEAD")) && exists(filepath.Join(d, "objects")) {
			return d
		}
		if d == "/" || d == "." {
			return ""
		}
	}
}

// ---------------------------------------------------------------- entry

// Dir is where shim symlinks live.
func Dir(cfg *config.Config) string {
	if cfg.Shims.Dir != "" {
		return probe.ExpandPath(cfg.Shims.Dir)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "berth", "shims")
}

// seenVar lists the real tools shims have already executed in this process
// chain. Another wrapper system on PATH (one that calls "the next docker on
// PATH") would otherwise bounce back into berth forever; with this, each hop
// moves on to the next candidate.
const seenVar = "BERTH_SHIM_SEEN"

// Real finds the tool on PATH, skipping shims (anything that resolves to the
// berth binary) and tools this chain already went through.
func Real(tool string, self string) string {
	selfReal, _ := filepath.EvalSymlinks(self)
	seen := map[string]bool{}
	for _, p := range filepath.SplitList(os.Getenv(seenVar)) {
		seen[p] = true
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, tool)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if r, err := filepath.EvalSymlinks(p); err == nil && r == selfReal {
			continue
		}
		if seen[p] {
			continue
		}
		return p
	}
	return ""
}

func nested() bool {
	if os.Getenv("BERTH_JOB_ID") != "" {
		return true
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && strings.Contains(string(data), "/berth-")
}

// Main runs as tool. It does not return on the direct path (exec).
func Main(cfg *config.Config, tool string, args []string, self string) int {
	real := Real(tool, self)
	if real == "" {
		fmt.Fprintf(os.Stderr, "berth: %s: not found on PATH (outside the berth shims)\n", tool)
		return 127
	}
	seen := os.Getenv(seenVar)
	if seen != "" {
		seen += string(filepath.ListSeparator)
	}
	os.Setenv(seenVar, seen+real)
	direct := func(argv []string) int {
		err := syscall.Exec(real, append([]string{tool}, argv...), os.Environ())
		fmt.Fprintf(os.Stderr, "berth: exec %s: %v\n", real, err)
		return 126
	}
	if nested() || os.Getenv("BERTH_BYPASS") == "1" {
		return direct(args)
	}
	cwd, _ := os.Getwd()
	interactive := probe.IsTerminal(0) && probe.IsTerminal(1)
	d := Route(cfg, tool, args, cwd, interactive)
	if !d.Queue {
		return direct(d.Argv)
	}
	if _, err := api.Call(client.Socket(), api.Request{Op: "ping"}); err != nil {
		if os.Getenv("BERTH_QUIET") == "" {
			fmt.Fprintf(os.Stderr, "berth: daemon unreachable, running %s directly (`berth doctor` to fix)\n", tool)
		}
		return direct(d.Argv)
	}
	if v := os.Getenv("BERTH_MEMORY"); v != "" {
		if b, err := units.ParseBytes(v); err == nil {
			d.Memory = b
		}
	}
	if v := os.Getenv("BERTH_CPUS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			d.CPUs = f
		}
	}
	if d.Unit > 0 && d.Memory > 0 && d.Unit > d.Memory {
		d.Unit = d.Memory
	}
	spec := job.Spec{
		Argv: append([]string{real}, d.Argv...), Cwd: cwd, Env: os.Environ(), Name: strings.Join(append([]string{tool}, firstN(d.Argv, 2)...), " "),
		Memory: d.Memory, CPUs: d.CPUs, Disk: -1, UnitMemory: d.Unit, Timeout: d.Timeout, QueueTimeout: d.Queue2, Group: d.Group,
	}
	return client.Run(spec, client.RunOptions{Label: spec.Name, GiveUp: 20 * time.Second, Quiet: os.Getenv("BERTH_QUIET") != ""})
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
