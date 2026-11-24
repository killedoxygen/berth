package sched

import (
	"strings"
	"testing"
	"time"

	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

const G = units.GiB

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func testConfig() *config.Config {
	c := config.Default(probe.Host{MemTotal: 64 * G, CPUs: 24, HomeDiskTotal: 1000 * G})
	c.Pool.Memory = units.Bytes(48 * G)
	c.Pool.Overhead = units.Bytes(2 * G)
	c.Pool.CPUs = 22
	c.Pool.Resources = map[string]int{"gpu": 1}
	c.Safety.MinAvailableMemory = units.Bytes(6 * G)
	c.Safety.MinFreeDisk = units.Bytes(16 * G)
	return &c
}

func healthy() probe.Health {
	return probe.Health{MemTotal: 64 * G, MemAvailable: 60 * G, ParentRoom: -1, JobWorking: map[string]int64{},
		Disks: []probe.Disk{{Path: "/home", Free: 100 * G, Total: 1000 * G}}}
}

type world struct {
	t      *testing.T
	cfg    *config.Config
	health probe.Health
	jobs   map[string]*job.Job
	peaks  map[string]int64
	seq    int
}

func newWorld(t *testing.T) *world {
	return &world{t: t, cfg: testConfig(), health: healthy(), jobs: map[string]*job.Job{}, peaks: map[string]int64{}}
}

func (w *world) add(state job.State, mem int64, cpus float64, edit ...func(*job.Job)) *job.Job {
	w.seq++
	j := &job.Job{ID: strings.Repeat(string(rune('a'+w.seq)), 32), State: state, Created: now.Add(time.Duration(w.seq) * time.Second),
		Spec: job.Spec{Memory: mem, CPUs: cpus, Disk: G, Kind: job.Batch, Argv: []string{"x"}}}
	if state.Active() {
		j.Started = j.Created
	}
	for _, e := range edit {
		e(j)
	}
	w.jobs[j.ID] = j
	return j
}

func (w *world) plan() Plan {
	in := Input{Cfg: w.cfg, Health: w.health, Now: now.Add(time.Minute), Peaks: w.peaks,
		Lookup: func(id string) *job.Job { return w.jobs[id] }}
	for _, j := range w.jobs {
		switch {
		case j.State == job.Queued:
			in.Queued = append(in.Queued, j)
		case j.State.Active():
			in.Active = append(in.Active, j)
		}
	}
	return Schedule(in)
}

func started(p Plan, j *job.Job) bool {
	for _, s := range p.Start {
		if s == j {
			return true
		}
	}
	return false
}

func codes(p Plan, j *job.Job) string {
	var out []string
	for _, r := range p.Waiting[j.ID].Reasons {
		out = append(out, r.Code)
	}
	return strings.Join(out, ",")
}

func TestBackfillPastBlockedJob(t *testing.T) {
	w := newWorld(t)
	running := w.add(job.Running, 30*G, 4)
	w.health.JobWorking[running.ID] = 25 * G
	big := w.add(job.Queued, 28*G, 4)
	small := w.add(job.Queued, 4*G, 1)
	p := w.plan()
	if started(p, big) || !started(p, small) {
		t.Fatalf("want small to start past big; start=%v waiting=%v", p.Start, p.Waiting)
	}
	if codes(p, big) != job.RCapMemory {
		t.Fatalf("big waits for %q, want memory", codes(p, big))
	}
	if !strings.Contains(p.Waiting[big.ID].Hint, "--memory 16G") {
		t.Fatalf("hint %q should offer the size that fits now (48-30-2=16G)", p.Waiting[big.ID].Hint)
	}
}

func TestAgingStopsLongJobsOvertaking(t *testing.T) {
	w := newWorld(t)
	w.add(job.Running, 30*G, 4)
	big := w.add(job.Queued, 28*G, 4, func(j *job.Job) { j.Created = now.Add(-time.Hour) })
	long := w.add(job.Queued, 2*G, 1)
	short := w.add(job.Queued, 2*G, 1, func(j *job.Job) { j.Spec.Timeout = 5 * time.Minute })
	urgent := w.add(job.Queued, 2*G, 1, func(j *job.Job) { j.Spec.Priority = 5 })
	p := w.plan()
	if started(p, long) || codes(p, long) != job.RAging {
		t.Fatalf("long job should be held for the aged one, got %q", codes(p, long))
	}
	if !started(p, short) || !started(p, urgent) {
		t.Fatal("short and higher-priority jobs may still overtake")
	}
	if started(p, big) {
		t.Fatal("big still does not fit")
	}
}

func TestPriorityOrder(t *testing.T) {
	w := newWorld(t)
	w.add(job.Running, 40*G, 4)
	low := w.add(job.Queued, 6*G, 1)
	high := w.add(job.Queued, 6*G, 1, func(j *job.Job) { j.Spec.Priority = 1 })
	p := w.plan()
	if !started(p, high) || started(p, low) {
		t.Fatal("only one fits; the higher priority one must get it")
	}
}

func TestServiceChargedByUseAfterSettling(t *testing.T) {
	w := newWorld(t)
	svc := w.add(job.Running, 32*G, 6, func(j *job.Job) { j.Spec.Kind = job.Service; j.Started = now })
	w.health.JobWorking[svc.ID] = 2 * G
	w.health.PoolWorking = 2 * G
	docker := w.add(job.Queued, 28*G, 4)
	if started(w.plan(), docker) {
		t.Fatal("a settling service holds its full reservation")
	}
	svc.Started = now.Add(-time.Hour)
	if !started(w.plan(), docker) {
		t.Fatal("a settled service is charged max(25% floor, use): 8G, so 28G fits")
	}
	w.peaks[svc.ID] = 20 * G
	if started(w.plan(), docker) {
		t.Fatal("a recent peak of 20G must still be charged")
	}
}

func TestUnsatisfiableWhileServicesRun(t *testing.T) {
	w := newWorld(t)
	w.add(job.Running, 32*G, 6, func(j *job.Job) { j.Spec.Kind = job.Service })
	huge := w.add(job.Queued, 40*G, 4)
	p := w.plan()
	if codes(p, huge) != job.RUnsatisfied {
		t.Fatalf("got %q, want unsatisfiable", codes(p, huge))
	}
	if !strings.Contains(p.Waiting[huge.ID].Hint, "--memory 14G") {
		t.Fatalf("hint %q should name the largest possible size (48-2-32)", p.Waiting[huge.ID].Hint)
	}
}

func TestDependencies(t *testing.T) {
	w := newWorld(t)
	first := w.add(job.Running, G, 1)
	next := w.add(job.Queued, G, 1, func(j *job.Job) { j.Spec.After = []string{first.ID} })
	p := w.plan()
	if started(p, next) || codes(p, next) != job.RDependency {
		t.Fatalf("must wait for its dependency, got %q", codes(p, next))
	}
	first.State = job.Succeeded
	if !started(w.plan(), next) {
		t.Fatal("dependency succeeded; should start")
	}
	first.State = job.Failed
	p = w.plan()
	if len(p.Cancel) != 1 || p.Cancel[0].Job != next {
		t.Fatal("a failed dependency cancels the job")
	}
	next.Spec.After, next.Spec.AfterAny = nil, []string{first.ID}
	if !started(w.plan(), next) {
		t.Fatal("after-any starts whatever the outcome")
	}
}

func TestGroupsAndResources(t *testing.T) {
	w := newWorld(t)
	w.add(job.Running, G, 1, func(j *job.Job) { j.Spec.Group = "repo"; j.Spec.Resources = map[string]int{"gpu": 1} })
	sameGroup := w.add(job.Queued, G, 1, func(j *job.Job) { j.Spec.Group = "repo" })
	needsGPU := w.add(job.Queued, G, 1, func(j *job.Job) { j.Spec.Resources = map[string]int{"gpu": 1} })
	other := w.add(job.Queued, G, 1)
	p := w.plan()
	if codes(p, sameGroup) != job.RGroup || codes(p, needsGPU) != job.RCapResource || !started(p, other) {
		t.Fatalf("group=%q gpu=%q other started=%v", codes(p, sameGroup), codes(p, needsGPU), started(p, other))
	}
}

func TestSafetyChecksAllReported(t *testing.T) {
	w := newWorld(t)
	w.health.MemAvailable = 8 * G
	w.health.Disks[0].Free = 16*G + 512*units.MiB
	w.health.Btrfs = []probe.Btrfs{{Path: "/", Unallocated: G}}
	j := w.add(job.Queued, 4*G, 1)
	got := codes(w.plan(), j)
	for _, want := range []string{job.RCapDisk, job.RStorage, job.RHostMemory} {
		if !strings.Contains(got, want) {
			t.Fatalf("reasons %q should include %q: report every failing check, not the first", got, want)
		}
	}
}

func TestParentCgroupRoom(t *testing.T) {
	w := newWorld(t)
	w.health.ParentRoom, w.health.ParentLimit = 3*G, 53*G
	j := w.add(job.Queued, 4*G, 1)
	if codes(w.plan(), j) != job.RParentMemory {
		t.Fatal("the enclosing cgroup's limit must be respected")
	}
}

func TestHoldStopsEverything(t *testing.T) {
	w := newWorld(t)
	j := w.add(job.Queued, G, 1)
	in := Input{Cfg: w.cfg, Health: w.health, Now: now, Queued: []*job.Job{j}, Hold: &job.Reason{Code: job.RPressure, Text: "pressure"}}
	p := Schedule(in)
	if len(p.Start) != 0 || codes(p, j) != job.RPressure {
		t.Fatal("a hold admits nothing")
	}
}
