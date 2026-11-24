// Package probe reads host facts (sizes, capabilities) and live health (free memory, pressure, disk).
package probe

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Host is what onboarding needs to size a sane default policy.
type Host struct {
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	MemTotal       int64  `json:"mem_total"`
	CPUs           int    `json:"cpus"`
	Systemd        bool   `json:"systemd"`                // PID 1 is systemd
	SystemdUser    bool   `json:"systemd_user"`           // a user manager is reachable
	CgroupV2       bool   `json:"cgroup_v2"`              // unified hierarchy mounted
	UserDelegation string `json:"user_delegation"`        // controllers delegated to the user manager
	ParentLimit    int64  `json:"parent_limit,omitempty"` // tightest memory.max above the user's processes
	SELinux        string `json:"selinux,omitempty"`      // enforcing, permissive, or empty
	HomeFS         string `json:"home_fs,omitempty"`      // filesystem type of $HOME
	HomeDiskTotal  int64  `json:"home_disk_total"`        // size of the filesystem holding $HOME
	GPUs           int    `json:"gpus,omitempty"`         // NVIDIA/AMD render devices found
	Docker         string `json:"docker,omitempty"`       // docker/podman client path
	Root           bool   `json:"root"`                   // running as root
	PSI            bool   `json:"psi"`                    // pressure stall information available
	InContainer    bool   `json:"in_container,omitempty"` // running inside a container
}

// Health is a live snapshot used for admission.
type Health struct {
	MemTotal     int64            `json:"mem_total"`
	MemAvailable int64            `json:"mem_available"`
	PressureFull float64          `json:"pressure_full"` // memory PSI "full" avg10, percent
	Disks        []Disk           `json:"disks"`
	Btrfs        []Btrfs          `json:"btrfs,omitempty"`
	ParentRoom   int64            `json:"parent_room"` // room under the tightest ancestor memory.max; -1 if none
	ParentLimit  int64            `json:"parent_limit,omitempty"`
	PoolWorking  int64            `json:"pool_working"` // non-reclaimable memory of all jobs
	JobWorking   map[string]int64 `json:"-"`            // per job id
	Errors       []string         `json:"errors,omitempty"`
}

type Disk struct {
	Path  string `json:"path"`
	Free  int64  `json:"free"`
	Total int64  `json:"total"`
}

type Btrfs struct {
	Path             string `json:"path"`
	UUID             string `json:"uuid"`
	Unallocated      int64  `json:"unallocated"`
	MetadataHeadroom int64  `json:"metadata_headroom"`
}

func lookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// ExpandPath resolves ~ and environment variables.
func ExpandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return os.ExpandEnv(p)
}

func baseHost() Host {
	h := Host{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Root: os.Geteuid() == 0}
	if d := lookPath("docker"); d != "" {
		h.Docker = d
	} else if p := lookPath("podman"); p != "" {
		h.Docker = p
	}
	home, _ := os.UserHomeDir()
	if total, _, err := diskUsage(home); err == nil {
		h.HomeDiskTotal = total
	}
	return h
}
