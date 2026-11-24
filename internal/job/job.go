// Package job defines the job record shared by the daemon, scheduler, supervisor and clients.
package job

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type State string

const (
	Queued    State = "queued"
	Starting  State = "starting"
	Running   State = "running"
	Stopping  State = "stopping"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Cancelled State = "cancelled"
	Lost      State = "lost" // its supervisor vanished without a result (host reboot, external kill)
)

func (s State) Terminal() bool {
	return s == Succeeded || s == Failed || s == Cancelled || s == Lost
}

func (s State) Active() bool { return s == Starting || s == Running || s == Stopping }

type Kind string

const (
	Batch   Kind = "batch"   // charged at its reservation while active
	Service Kind = "service" // long-running; charged by observed use once settled
)

// Reason explains why a queued job is not running. Code is stable for programs; Text is for people.
type Reason struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// Reason codes. Capacity reasons clear as other jobs finish; the rest need something else to change.
const (
	RCapMemory    = "memory"
	RCapCPU       = "cpu"
	RCapResource  = "resource"
	RCapDisk      = "disk"
	RHostMemory   = "host_memory"
	RParentMemory = "parent_memory"
	RPressure     = "pressure"
	RStorage      = "storage"
	RGroup        = "group"
	RDependency   = "dependency"
	RAging        = "aging"
	RHold         = "hold"
	RUnsatisfied  = "unsatisfiable"
	REmergency    = "emergency"
	RAttach       = "attach"
)

// Capacity reports whether a reason code is about pool capacity held by other jobs.
func Capacity(code string) bool {
	switch code {
	case RCapMemory, RCapCPU, RCapResource, RCapDisk, RHostMemory, RParentMemory:
		return true
	}
	return false
}

// Spec is what a submitter asks for.
type Spec struct {
	Argv         []string          `json:"argv"`
	Cwd          string            `json:"cwd"`
	Env          []string          `json:"env,omitempty"`
	Name         string            `json:"name,omitempty"`
	Key          string            `json:"key,omitempty"` // idempotency key: an active job with the same key is reused
	Kind         Kind              `json:"kind"`
	Memory       int64             `json:"memory"`                // bytes reserved and hard-capped
	CPUs         float64           `json:"cpus"`                  // cores reserved and capped
	Disk         int64             `json:"disk"`                  // expected disk growth, admission budget only
	UnitMemory   int64             `json:"unit_memory,omitempty"` // cap for the job's own processes if lower than Memory (container clients)
	Resources    map[string]int    `json:"resources,omitempty"`   // named counted resources, e.g. gpu=1
	Group        string            `json:"group,omitempty"`       // at most one active job per group
	After        []string          `json:"after,omitempty"`       // start only after these jobs succeed
	AfterAny     []string          `json:"after_any,omitempty"`   // start after these jobs finish, whatever the outcome
	Priority     int               `json:"priority,omitempty"`
	Timeout      time.Duration     `json:"timeout,omitempty"`       // execution deadline; 0 none
	QueueTimeout time.Duration     `json:"queue_timeout,omitempty"` // cancel if not started in time; 0 none
	Retries      int               `json:"retries,omitempty"`       // requeue after loss or emergency stop
	Passthrough  bool              `json:"passthrough,omitempty"`   // the submitter's stdin/stdout/stderr are used
	Labels       map[string]string `json:"labels,omitempty"`
}

// Owner binds a job to a submitting process: if it exits, the job is cancelled.
type Owner struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"` // process start time, to detect PID reuse
}

type Result struct {
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
	Reason   string `json:"reason,omitempty"` // e.g. "timeout after 10m", "out of memory (limit 4G)"
	OOM      bool   `json:"oom,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// Job is the durable record.
type Job struct {
	ID        string    `json:"id"`
	UID       int       `json:"uid"`
	Spec      Spec      `json:"spec"`
	State     State     `json:"state"`
	Reasons   []Reason  `json:"reasons,omitempty"`
	Hint      string    `json:"hint,omitempty"`
	Note      string    `json:"note,omitempty"` // final explanation for terminal states
	Owner     *Owner    `json:"owner,omitempty"`
	Created   time.Time `json:"created"`
	Started   time.Time `json:"started,omitempty"`
	Finished  time.Time `json:"finished,omitempty"`
	Result    *Result   `json:"result,omitempty"`
	RetryOf   string    `json:"retry_of,omitempty"`
	Token     string    `json:"token,omitempty"` // secret the supervisor presents to claim passthrough descriptors
	Unit      string    `json:"unit,omitempty"`  // backend handle: systemd unit name or "pid:start"
	PeakBytes int64     `json:"peak_bytes,omitempty"`
}

func NewID() string { return randHex(16) }

func NewToken() string { return randHex(24) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Short is the 8-character form shown to people; any unique prefix is accepted by the daemon.
func Short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Label is a short description for logs and status.
func (j *Job) Label() string {
	if j.Spec.Name != "" {
		return j.Spec.Name
	}
	out := ""
	for i, a := range j.Spec.Argv {
		if i == 3 {
			out += " …"
			break
		}
		if i > 0 {
			out += " "
		}
		if i == 0 {
			a = base(a)
		}
		out += a
	}
	if len(out) > 72 {
		out = out[:71] + "…"
	}
	return out
}

func base(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// Public returns a copy safe to show any client: no environment, no token.
func (j *Job) Public() Job {
	c := *j
	c.Spec.Env = nil
	c.Token = ""
	return c
}
