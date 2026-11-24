// Package config holds the policy. Every value has a default derived from the
// host (see Detect), so berth runs with no config file at all; a TOML file
// overrides only what it names.
package config

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

type Config struct {
	Pool      Pool      `toml:"pool"`
	Defaults  Defaults  `toml:"defaults"`
	Safety    Safety    `toml:"safety"`
	Scheduler Scheduler `toml:"scheduler"`
	Services  Services  `toml:"services"`
	Backend   Backend   `toml:"backend"`
	Logs      Logs      `toml:"logs"`
	Shims     Shims     `toml:"shims"`
}

// Logs bounds what job output may cost: each job's output is gzip-compressed
// in segments and capped (head and tail kept), finished jobs expire, and all
// logs together stay within a budget (oldest finished jobs' logs go first).
type Logs struct {
	SegmentSize   units.Bytes    `toml:"segment_size"`    // compressed once full
	MaxPerJob     units.Bytes    `toml:"max_per_job"`     // compressed bytes kept per job
	MaxTotal      units.Bytes    `toml:"max_total"`       // all job logs together
	Retention     units.Duration `toml:"retention"`       // finished jobs and their logs are deleted after this
	DaemonLogSize units.Bytes    `toml:"daemon_log_size"` // daemon log rotation, when not under journald
	DaemonLogKeep int            `toml:"daemon_log_keep"`
}

type Pool struct {
	Memory    units.Bytes    `toml:"memory"`    // all jobs together; also the slice's hard cap
	CPUs      float64        `toml:"cpus"`      // all jobs together
	Overhead  units.Bytes    `toml:"overhead"`  // headroom never reserved (page cache, the daemon, container runtimes)
	Resources map[string]int `toml:"resources"` // counted resources, e.g. gpu = 1
}

type Defaults struct {
	Memory units.Bytes `toml:"memory"`
	CPUs   float64     `toml:"cpus"`
	Disk   units.Bytes `toml:"disk"`
}

type Safety struct {
	MinAvailableMemory     units.Bytes    `toml:"min_available_memory"`     // host MemAvailable never planned below this
	MinFreeDisk            units.Bytes    `toml:"min_free_disk"`            // admission floor on every watched path
	EmergencyFreeDisk      units.Bytes    `toml:"emergency_free_disk"`      // below this, the newest batch job is stopped
	WatchPaths             []string       `toml:"watch_paths"`              // filesystems jobs write to
	PressurePause          float64        `toml:"pressure_pause"`           // memory PSI full avg10 % that pauses admission
	PressureResume         float64        `toml:"pressure_resume"`          // and the level that resumes it ...
	PressureHold           units.Duration `toml:"pressure_hold"`            // ... after staying there this long
	BtrfsMinUnallocated    units.Bytes    `toml:"btrfs_min_unallocated"`    // admission floor for unallocated chunks
	BtrfsTargetUnallocated units.Bytes    `toml:"btrfs_target_unallocated"` // maintenance reclaims up to this
	BtrfsMaintenance       bool           `toml:"btrfs_maintenance"`        // run bounded balances (root daemon only)
}

type Scheduler struct {
	Tick         units.Duration `toml:"tick"`
	Aging        units.Duration `toml:"aging"`         // a job blocked this long stops being overtaken by long jobs
	ShortJob     units.Duration `toml:"short_job"`     // jobs with a timeout at most this may still overtake it
	OwnerGrace   units.Duration `toml:"owner_grace"`   // time a bound job survives its submitter (reattach window)
	KeepFinished int            `toml:"keep_finished"` // finished job records kept at most (see also logs.retention)
}

type Services struct {
	FloorFraction float64        `toml:"floor_fraction"` // settled services are charged at least this share of their reservation
	Settle        units.Duration `toml:"settle"`         // charged in full until running this long
	PeakWindow    units.Duration `toml:"peak_window"`    // and afterwards at their peak over this window
	CPUCharge     float64        `toml:"cpu_charge"`     // CPU cores a settled service is charged
}

type Backend struct {
	Kind            string   `toml:"kind"`             // auto | systemd | process
	Slice           string   `toml:"slice"`            // systemd slice holding all jobs
	SELinuxContext  string   `toml:"selinux_context"`  // system mode: context for jobs (default: the user's login context)
	ExtraProperties []string `toml:"extra_properties"` // additional systemd unit properties for every job
}

type Shims struct {
	Dir       string         `toml:"dir"`
	Tools     []string       `toml:"tools"` // tools to shim; built-in routing knows cargo, make, gmake, ninja, cmake, go, docker, podman, git
	Rules     []ShimRule     `toml:"rules"` // extra routing for other tools
	Git       GitRoute       `toml:"git"`
	Container ContainerRoute `toml:"container"`
}

// ShimRule queues `tool ...` when the first non-option argument is one of
// Subcommands (any, if empty) and any argument is one of FlagsAny (if set).
type ShimRule struct {
	Tool        string         `toml:"tool"`
	Subcommands []string       `toml:"subcommands"`
	FlagsAny    []string       `toml:"flags_any"`
	Memory      units.Bytes    `toml:"memory"`
	CPUs        float64        `toml:"cpus"`
	Timeout     units.Duration `toml:"timeout"`
	Group       string         `toml:"group"`
}

type GitRoute struct {
	LargeIndex   units.Bytes    `toml:"large_index"` // a repo whose index is at least this is "large"
	Memory       units.Bytes    `toml:"memory"`
	CPUs         float64        `toml:"cpus"`
	Timeout      units.Duration `toml:"timeout"`
	LongTimeout  units.Duration `toml:"long_timeout"` // fetch, clone, gc, repack ...
	QueueTimeout units.Duration `toml:"queue_timeout"`
}

type ContainerRoute struct {
	RunMemory    units.Bytes `toml:"run_memory"` // applied to `run` when the caller sets no --memory
	RunCPUs      float64     `toml:"run_cpus"`
	BuildMemory  units.Bytes `toml:"build_memory"`
	BuildCPUs    float64     `toml:"build_cpus"`
	ClientMemory units.Bytes `toml:"client_memory"` // cap for the CLI process itself
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func roundGiB(v int64) int64 {
	if v >= 4*units.GiB {
		return v / units.GiB * units.GiB
	}
	return v / (256 * units.MiB) * (256 * units.MiB)
}

// Default sizes a policy for this host. It is conservative where mistakes hurt
// (host memory, disk) and generous where they do not (CPU overlap only slows).
func Default(h probe.Host) Config {
	mem := h.MemTotal
	if mem <= 0 {
		mem = 8 * units.GiB
	}
	limit := mem
	if h.ParentLimit > 0 && h.ParentLimit < limit {
		limit = h.ParentLimit
	}
	// Keep room for the desktop/agents themselves: a quarter, between 2 and 16 GiB.
	pool := roundGiB(limit - clamp(mem/4, 2*units.GiB, 16*units.GiB))
	if pool < units.GiB {
		pool = clamp(limit/2, 512*units.MiB, units.GiB)
	}
	cpus := math.Max(1, float64(h.CPUs))
	if h.CPUs > 4 {
		cpus = float64(h.CPUs - 1)
	}
	resources := map[string]int{}
	if h.GPUs > 0 {
		resources["gpu"] = h.GPUs
	}
	homeTotal := h.HomeDiskTotal
	minFree := clamp(homeTotal/25, 2*units.GiB, 20*units.GiB)
	kind := "process"
	if h.OS == "linux" && h.Systemd && h.CgroupV2 && (h.Root || h.SystemdUser) {
		kind = "systemd"
	}
	overhead := clamp(pool/24, 512*units.MiB, 2*units.GiB)
	if overhead > pool/4 {
		overhead = pool / 4
	}
	c := Config{
		Pool: Pool{Memory: units.Bytes(pool), CPUs: cpus, Overhead: units.Bytes(overhead), Resources: resources},
		Defaults: Defaults{
			Memory: units.Bytes(roundGiB(clamp(pool/8, 512*units.MiB, 8*units.GiB))),
			CPUs:   math.Max(1, math.Min(4, math.Floor(cpus/4))),
			Disk:   units.Bytes(2 * units.GiB),
		},
		Safety: Safety{
			MinAvailableMemory: units.Bytes(clamp(mem/12, 512*units.MiB, 6*units.GiB)),
			MinFreeDisk:        units.Bytes(minFree),
			EmergencyFreeDisk:  units.Bytes(minFree / 2),
			WatchPaths:         []string{"~"},
			PressurePause:      10, PressureResume: 2, PressureHold: units.Duration(30 * time.Second),
			BtrfsMinUnallocated:    units.Bytes(4 * units.GiB),
			BtrfsTargetUnallocated: units.Bytes(clamp(homeTotal/40, 4*units.GiB, 16*units.GiB)),
		},
		Scheduler: Scheduler{
			Tick: units.Duration(time.Second), Aging: units.Duration(10 * time.Minute),
			ShortJob: units.Duration(30 * time.Minute), OwnerGrace: units.Duration(10 * time.Second), KeepFinished: 2000,
		},
		Services: Services{FloorFraction: 0.25, Settle: units.Duration(10 * time.Minute), PeakWindow: units.Duration(time.Hour), CPUCharge: 1},
		Backend:  Backend{Kind: kind, Slice: "berth.slice"},
		Logs: Logs{SegmentSize: units.Bytes(4 * units.MiB), MaxPerJob: units.Bytes(64 * units.MiB),
			MaxTotal:  units.Bytes(clamp(homeTotal/200, 256*units.MiB, 4*units.GiB)),
			Retention: units.Duration(30 * 24 * time.Hour), DaemonLogSize: units.Bytes(10 * units.MiB), DaemonLogKeep: 5},
		Shims: Shims{
			Tools: []string{"cargo", "make", "gmake", "ninja", "cmake", "go", "docker", "podman", "git"},
			Git: GitRoute{LargeIndex: units.Bytes(32 * units.MiB), Memory: units.Bytes(4 * units.GiB), CPUs: 2,
				Timeout: units.Duration(10 * time.Minute), LongTimeout: units.Duration(time.Hour), QueueTimeout: units.Duration(15 * time.Minute)},
			Container: ContainerRoute{RunMemory: units.Bytes(4 * units.GiB), RunCPUs: 2, BuildMemory: units.Bytes(6 * units.GiB),
				BuildCPUs: 4, ClientMemory: units.Bytes(512 * units.MiB)},
		},
	}
	if c.Defaults.Memory > c.Pool.Memory-c.Pool.Overhead {
		c.Defaults.Memory = c.Pool.Memory - c.Pool.Overhead
	}
	if c.Shims.Container.RunMemory > c.Defaults.Memory {
		c.Shims.Container.RunMemory = c.Defaults.Memory
	}
	return c
}

// Validate rejects contradictory settings with a message that says what to change.
func (c *Config) Validate() error {
	switch {
	case c.Pool.Memory <= 0:
		return fmt.Errorf("pool.memory must be positive")
	case c.Pool.CPUs <= 0:
		return fmt.Errorf("pool.cpus must be positive")
	case c.Pool.Overhead >= c.Pool.Memory:
		return fmt.Errorf("pool.overhead (%s) must be below pool.memory (%s)", units.FormatBytes(int64(c.Pool.Overhead)), units.FormatBytes(int64(c.Pool.Memory)))
	case c.Defaults.Memory <= 0 || c.Defaults.Memory > c.Pool.Memory-c.Pool.Overhead:
		return fmt.Errorf("defaults.memory must be within pool.memory minus pool.overhead")
	case c.Defaults.CPUs <= 0 || c.Defaults.CPUs > c.Pool.CPUs:
		return fmt.Errorf("defaults.cpus must be within pool.cpus")
	case c.Safety.EmergencyFreeDisk > c.Safety.MinFreeDisk:
		return fmt.Errorf("safety.emergency_free_disk must not exceed safety.min_free_disk")
	case c.Backend.Kind != "auto" && c.Backend.Kind != "systemd" && c.Backend.Kind != "process":
		return fmt.Errorf("backend.kind must be auto, systemd or process")
	case c.Services.FloorFraction < 0 || c.Services.FloorFraction > 1:
		return fmt.Errorf("services.floor_fraction must be between 0 and 1")
	case c.Logs.SegmentSize < units.Bytes(4*units.KiB) || c.Logs.MaxPerJob < c.Logs.SegmentSize:
		return fmt.Errorf("logs.segment_size must be at least 4K and logs.max_per_job at least one segment")
	case c.Logs.MaxTotal < c.Logs.MaxPerJob:
		return fmt.Errorf("logs.max_total must be at least logs.max_per_job")
	case c.Logs.Retention < units.Duration(time.Minute):
		return fmt.Errorf("logs.retention must be at least 1m")
	}
	return nil
}

// Load returns host defaults overlaid with the file at path, if it exists.
func Load(path string, h probe.Host) (Config, error) {
	c := Default(h)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, c.Validate()
	}
	if err != nil {
		return c, err
	}
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return c, fmt.Errorf("%s: unknown setting %q", path, undecoded[0].String())
	}
	if c.Backend.Kind == "auto" {
		c.Backend.Kind = Default(h).Backend.Kind
	}
	return c, c.Validate()
}

// Encode renders a config as TOML, for `berth config` and setup.
func Encode(c Config) string {
	var buf bytes.Buffer
	_ = toml.NewEncoder(&buf).Encode(c)
	return buf.String()
}

// Paths locate the config, state and socket for a mode.
type Paths struct {
	System bool
	Config string
	State  string
	Socket string
}

func UserPaths() Paths {
	home, _ := os.UserHomeDir()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	if runtime.GOOS == "darwin" {
		state = filepath.Join(home, "Library", "Application Support")
	}
	p := Paths{Config: filepath.Join(cfg, "berth", "config.toml"), State: filepath.Join(state, "berth")}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		p.Socket = filepath.Join(rt, "berth.sock")
	} else {
		p.Socket = filepath.Join(p.State, "berth.sock")
	}
	return p
}

func SystemPaths() Paths {
	return Paths{System: true, Config: "/etc/berth/config.toml", State: "/var/lib/berth", Socket: "/run/berth/berth.sock"}
}

// ClientSocket picks the daemon a client talks to: $BERTH_SOCKET, the user's
// daemon, then a system daemon.
func ClientSocket() string {
	if s := os.Getenv("BERTH_SOCKET"); s != "" {
		return s
	}
	user := UserPaths().Socket
	if _, err := os.Stat(user); err == nil {
		return user
	}
	if _, err := os.Stat(SystemPaths().Socket); err == nil {
		return SystemPaths().Socket
	}
	return user
}
