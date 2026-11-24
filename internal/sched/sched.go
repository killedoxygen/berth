// Package sched decides which queued jobs start. It is pure: inputs in, decisions out.
//
// Order is priority (higher first), then age. A job that does not fit never
// blocks later jobs that do (backfill), except that once a job has been held
// for capacity longer than Aging, later jobs may only overtake it if they are
// short (bounded timeout) or of higher priority, so large jobs cannot starve.
//
// Batch jobs are charged their full reservation. Services are charged in full
// while they settle, then by their recent peak use (at least a floor).
package sched

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

type Input struct {
	Cfg    *config.Config
	Health probe.Health
	Now    time.Time
	Active []*job.Job            // starting, running, stopping
	Queued []*job.Job            // any order
	Lookup func(string) *job.Job // any job by id, for dependencies
	Peaks  map[string]int64      // service id -> peak working bytes over the peak window
	Hold   *job.Reason           // when set, nothing starts (pause, pressure, emergency)
	// Shared (system-mode) daemon: interleave users and cap each one's share.
	Fair      bool
	UserShare float64 // fraction of pool memory one non-root user may hold; 0: no cap
}

type Wait struct {
	Reasons  []job.Reason
	Hint     string
	Position int
}

type Cancel struct {
	Job  *job.Job
	Note string
}

type Plan struct {
	Start   []*job.Job
	Cancel  []Cancel
	Waiting map[string]Wait
	Summary string // one line: why the oldest waiting job waits
}

// Charge is what an active job holds against the pool.
type Charge struct {
	Memory    int64
	CPUs      float64
	Disk      int64
	Resources map[string]int
}

func ChargeOf(cfg *config.Config, j *job.Job, working, peak int64, now time.Time) Charge {
	c := Charge{Memory: j.Spec.Memory, CPUs: j.Spec.CPUs, Disk: j.Spec.Disk, Resources: j.Spec.Resources}
	if j.Spec.Kind != job.Service {
		return c
	}
	c.Disk = 0
	settled := j.State == job.Running && !j.Started.IsZero() && now.Sub(j.Started) >= time.Duration(cfg.Services.Settle)
	if !settled {
		return c
	}
	floor := int64(float64(j.Spec.Memory) * cfg.Services.FloorFraction)
	observed := int64(float64(max64(working, peak)) * 1.1)
	c.Memory = min64(j.Spec.Memory, max64(floor, observed))
	c.CPUs = math.Min(j.Spec.CPUs, cfg.Services.CPUCharge)
	return c
}

type pool struct {
	cfg       *config.Config
	h         probe.Health
	charges   []Charge
	working   []int64
	groups    map[string]*job.Job
	now       time.Time
	byUser    map[int]int64 // memory charged per uid
	userShare float64
}

func (p *pool) add(j *job.Job, c Charge, working int64) {
	p.charges = append(p.charges, c)
	p.working = append(p.working, working)
	if p.byUser == nil {
		p.byUser = map[int]int64{}
	}
	p.byUser[j.UID] += c.Memory
	if j.Spec.Group != "" {
		p.groups[j.Spec.Group] = j
	}
}

func (p *pool) sums() (mem int64, cpu float64, disk int64, res map[string]int, unmet int64) {
	res = map[string]int{}
	for i, c := range p.charges {
		mem += c.Memory
		cpu += c.CPUs
		disk += c.Disk
		for k, v := range c.Resources {
			res[k] += v
		}
		if d := c.Memory - p.working[i]; d > 0 {
			unmet += d
		}
	}
	return
}

func fmtB(b int64) string { return units.FormatBytes(b) }

// fit returns every reason the job cannot start now, and the largest memory request that would.
func (p *pool) fit(j *job.Job) (reasons []job.Reason, maxMem int64) {
	cfg, h := p.cfg, p.h
	add := func(code, format string, args ...any) {
		reasons = append(reasons, job.Reason{Code: code, Text: fmt.Sprintf(format, args...)})
	}
	if p.userShare > 0 && j.UID != 0 {
		limit := int64(float64(int64(cfg.Pool.Memory)-int64(cfg.Pool.Overhead)) * p.userShare)
		if held := p.byUser[j.UID]; held+j.Spec.Memory > limit {
			add(job.RUserShare, "your active jobs hold %s of your %s share of the pool (users.max_share)", fmtB(held), fmtB(limit))
		}
	}
	if holder, ok := p.groups[j.Spec.Group]; ok && j.Spec.Group != "" {
		add(job.RGroup, "group %q is held by job %s (%s)", j.Spec.Group, job.Short(holder.ID), holder.Label())
	}
	mem, cpu, disk, res, unmet := p.sums()
	poolMem := int64(cfg.Pool.Memory)
	used := max64(mem, h.PoolWorking) + int64(cfg.Pool.Overhead)
	maxMem = poolMem - used
	if used+j.Spec.Memory > poolMem {
		add(job.RCapMemory, "pool memory: needs %s, %s of %s free", fmtB(j.Spec.Memory), fmtB(max64(0, poolMem-used)), fmtB(poolMem))
	}
	if cpu+j.Spec.CPUs > cfg.Pool.CPUs+1e-9 {
		add(job.RCapCPU, "pool CPUs: needs %g, %g of %g free", j.Spec.CPUs, math.Max(0, cfg.Pool.CPUs-cpu), cfg.Pool.CPUs)
	}
	for _, name := range sortedKeys(j.Spec.Resources) {
		want, total := j.Spec.Resources[name], cfg.Pool.Resources[name]
		if res[name]+want > total {
			add(job.RCapResource, "%s: needs %d, %d of %d free", name, want, max(0, total-res[name]), total)
		}
	}
	for _, d := range h.Disks {
		room := d.Free - int64(cfg.Safety.MinFreeDisk) - disk
		if room < j.Spec.Disk {
			add(job.RCapDisk, "disk %s: %s free, floor %s + %s budgeted to running jobs, needs %s more",
				d.Path, fmtB(d.Free), fmtB(int64(cfg.Safety.MinFreeDisk)), fmtB(disk), fmtB(j.Spec.Disk))
			break
		}
	}
	for _, b := range h.Btrfs {
		if b.Unallocated < int64(cfg.Safety.BtrfsMinUnallocated) {
			add(job.RStorage, "btrfs %s: %s unallocated < %s floor (balance reclaims it; see `berth doctor`)",
				b.Path, fmtB(b.Unallocated), fmtB(int64(cfg.Safety.BtrfsMinUnallocated)))
		}
	}
	need := j.Spec.Memory + unmet
	if h.MemTotal > 0 {
		avail := h.MemAvailable - int64(cfg.Safety.MinAvailableMemory)
		if avail < need {
			add(job.RHostMemory, "host memory: %s available, reserve %s, active jobs may still grow by %s",
				fmtB(h.MemAvailable), fmtB(int64(cfg.Safety.MinAvailableMemory)), fmtB(unmet))
		}
		maxMem = min64(maxMem, avail-unmet)
	}
	if h.ParentRoom >= 0 {
		if h.ParentRoom < need {
			add(job.RParentMemory, "enclosing cgroup limit %s: %s room, active jobs may still grow by %s",
				fmtB(h.ParentLimit), fmtB(h.ParentRoom), fmtB(unmet))
		}
		maxMem = min64(maxMem, h.ParentRoom-unmet)
	}
	return reasons, maxMem
}

// impossible explains why a job can never start while current services run.
func (p *pool) impossible(j *job.Job, services []Charge) string {
	cfg := p.cfg
	var smem int64
	var scpu float64
	sres := map[string]int{}
	for _, c := range services {
		smem += c.Memory
		scpu += c.CPUs
		for k, v := range c.Resources {
			sres[k] += v
		}
	}
	room := int64(cfg.Pool.Memory) - int64(cfg.Pool.Overhead) - smem
	if j.Spec.Memory > room {
		if smem > 0 {
			return fmt.Sprintf("needs %s memory but at most %s can be free while services hold %s (pool %s)",
				fmtB(j.Spec.Memory), fmtB(max64(0, room)), fmtB(smem), fmtB(int64(cfg.Pool.Memory)))
		}
		return fmt.Sprintf("needs %s memory but the pool is %s (%s overhead)", fmtB(j.Spec.Memory), fmtB(int64(cfg.Pool.Memory)), fmtB(int64(cfg.Pool.Overhead)))
	}
	if j.Spec.CPUs > cfg.Pool.CPUs-scpu+1e-9 {
		return fmt.Sprintf("needs %g CPUs but at most %g can be free", j.Spec.CPUs, cfg.Pool.CPUs-scpu)
	}
	for name, want := range j.Spec.Resources {
		total, ok := cfg.Pool.Resources[name]
		if !ok {
			return fmt.Sprintf("resource %q is not defined on this host (pool.resources)", name)
		}
		if want > total-sres[name] {
			return fmt.Sprintf("needs %d %s but at most %d can be free", want, name, total-sres[name])
		}
	}
	return ""
}

func short(cfg *config.Config, j *job.Job) bool {
	return j.Spec.Timeout > 0 && j.Spec.Timeout <= time.Duration(cfg.Scheduler.ShortJob)
}

// Order sorts queued jobs: priority, then age. With fair, users are
// interleaved: each user's first waiting job before anyone's second.
func Order(q []*job.Job, fair bool) {
	rank := map[*job.Job]int{}
	if fair {
		byAge := append([]*job.Job(nil), q...)
		sort.SliceStable(byAge, func(a, b int) bool { return byAge[a].Created.Before(byAge[b].Created) })
		seen := map[int]int{}
		for _, j := range byAge {
			rank[j] = seen[j.UID]
			seen[j.UID]++
		}
	}
	sort.SliceStable(q, func(a, b int) bool {
		if q[a].Spec.Priority != q[b].Spec.Priority {
			return q[a].Spec.Priority > q[b].Spec.Priority
		}
		if rank[q[a]] != rank[q[b]] {
			return rank[q[a]] < rank[q[b]]
		}
		return q[a].Created.Before(q[b].Created)
	})
}

func Schedule(in Input) Plan {
	plan := Plan{Waiting: map[string]Wait{}}
	p := &pool{cfg: in.Cfg, h: in.Health, groups: map[string]*job.Job{}, now: in.Now, userShare: in.UserShare}
	var services []Charge
	for _, j := range in.Active {
		w := in.Health.JobWorking[j.ID]
		c := ChargeOf(in.Cfg, j, w, in.Peaks[j.ID], in.Now)
		p.add(j, c, w)
		if j.Spec.Kind == job.Service {
			services = append(services, c)
		}
	}
	queue := append([]*job.Job(nil), in.Queued...)
	Order(queue, in.Fair)

	var protected *job.Job
	position := 0
	for _, j := range queue {
		position++
		wait := func(reasons []job.Reason, hint string) {
			plan.Waiting[j.ID] = Wait{Reasons: reasons, Hint: hint, Position: position}
		}
		// Dependencies first: they can cancel the job outright.
		depReasons, cancel := dependencies(in, j)
		if cancel != "" {
			plan.Cancel = append(plan.Cancel, Cancel{Job: j, Note: cancel})
			position--
			continue
		}
		if why := p.impossible(j, services); why != "" {
			wait([]job.Reason{{Code: job.RUnsatisfied, Text: why}}, unsatisfiableHint(in.Cfg, j, services))
			continue
		}
		if in.Hold != nil {
			wait(append([]job.Reason{*in.Hold}, depReasons...), "")
			continue
		}
		reasons, maxMem := p.fit(j)
		reasons = append(depReasons, reasons...)
		if len(reasons) == 0 && protected != nil && !short(in.Cfg, j) && j.Spec.Priority <= protected.Spec.Priority {
			reasons = []job.Reason{{Code: job.RAging, Text: fmt.Sprintf("job %s has waited %s for capacity and goes first",
				job.Short(protected.ID), units.FormatDuration(in.Now.Sub(protected.Created)))}}
		}
		if len(reasons) == 0 {
			plan.Start = append(plan.Start, j)
			p.add(j, Charge{Memory: j.Spec.Memory, CPUs: j.Spec.CPUs, Disk: j.Spec.Disk, Resources: j.Spec.Resources}, 0)
			position--
			continue
		}
		wait(reasons, hint(in, j, reasons, maxMem))
		allCapacity := true
		for _, r := range reasons {
			if !job.Capacity(r.Code) {
				allCapacity = false
			}
		}
		if protected == nil && allCapacity && in.Now.Sub(j.Created) >= time.Duration(in.Cfg.Scheduler.Aging) {
			protected = j
		}
	}
	for _, j := range queue {
		if w, ok := plan.Waiting[j.ID]; ok {
			plan.Summary = fmt.Sprintf("%d waiting; next %s: %s", len(plan.Waiting), job.Short(j.ID), joinReasons(w.Reasons))
			break
		}
	}
	return plan
}

func dependencies(in Input, j *job.Job) (reasons []job.Reason, cancel string) {
	check := func(ids []string, needSuccess bool) {
		for _, id := range ids {
			var d *job.Job
			if in.Lookup != nil {
				d = in.Lookup(id)
			}
			switch {
			case d == nil:
				cancel = fmt.Sprintf("dependency %s does not exist", job.Short(id))
			case d.State.Terminal() && needSuccess && d.State != job.Succeeded:
				cancel = fmt.Sprintf("dependency %s %s", job.Short(id), d.State)
			case !d.State.Terminal():
				reasons = append(reasons, job.Reason{Code: job.RDependency,
					Text: fmt.Sprintf("waiting for job %s (%s, %s)", job.Short(d.ID), d.Label(), d.State)})
			}
		}
	}
	check(j.Spec.After, true)
	check(j.Spec.AfterAny, false)
	return
}

func hint(in Input, j *job.Job, reasons []job.Reason, maxMem int64) string {
	onlyMemory := true
	for _, r := range reasons {
		switch r.Code {
		case job.RCapMemory, job.RHostMemory, job.RParentMemory:
		default:
			onlyMemory = false
		}
	}
	if onlyMemory && maxMem >= 256*units.MiB && maxMem < j.Spec.Memory {
		return fmt.Sprintf("would start now with --memory %s or less", units.FormatBytesDown(maxMem))
	}
	if len(reasons) == 1 && reasons[0].Code == job.RAging {
		return fmt.Sprintf("jobs with --timeout %s or less may still go ahead", units.FormatDuration(time.Duration(in.Cfg.Scheduler.ShortJob)))
	}
	return ""
}

func unsatisfiableHint(cfg *config.Config, j *job.Job, services []Charge) string {
	var smem int64
	for _, c := range services {
		smem += c.Memory
	}
	room := int64(cfg.Pool.Memory) - int64(cfg.Pool.Overhead) - smem
	if j.Spec.Memory > room && room >= 256*units.MiB {
		return fmt.Sprintf("request --memory %s or less, or stop a service (`berth status`)", units.FormatBytesDown(room))
	}
	return "lower the request or raise the pool in the config (`berth config`)"
}

func joinReasons(rs []job.Reason) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.Text
	}
	return strings.Join(parts, "; ")
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Usage summarizes what active jobs hold and what a default-sized job could still get right now.
type Usage struct {
	MemoryCharged  int64
	CPUsCharged    float64
	ResourcesInUse map[string]int
	FitsNowMemory  int64
	FitsNowCPUs    float64
}

func Measure(in Input) Usage {
	p := &pool{cfg: in.Cfg, h: in.Health, groups: map[string]*job.Job{}, now: in.Now}
	for _, j := range in.Active {
		w := in.Health.JobWorking[j.ID]
		p.add(j, ChargeOf(in.Cfg, j, w, in.Peaks[j.ID], in.Now), w)
	}
	mem, cpu, _, res, _ := p.sums()
	probeJob := &job.Job{Spec: job.Spec{Memory: 0, CPUs: 0}}
	_, maxMem := p.fit(probeJob)
	if in.Hold != nil {
		maxMem = 0
	}
	return Usage{MemoryCharged: mem, CPUsCharged: cpu, ResourcesInUse: res,
		FitsNowMemory: max64(0, maxMem), FitsNowCPUs: math.Max(0, in.Cfg.Pool.CPUs-cpu)}
}
