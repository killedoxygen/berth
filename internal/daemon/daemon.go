// Package daemon owns the queue: it schedules, launches supervisors through a
// backend, watches them, and serves the socket API.
package daemon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/backend"
	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/logs"
	"github.com/killedoxygen/berth/internal/maint"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/sched"
	"github.com/killedoxygen/berth/internal/store"
	"github.com/killedoxygen/berth/internal/supervise"
	"github.com/killedoxygen/berth/internal/units"
)

type Options struct {
	Config  config.Config
	Paths   config.Paths
	Self    string // absolute path of the berth binary, used to launch supervisors
	Version string
	Log     io.Writer
}

type sample struct {
	at    time.Time
	bytes int64
}

type Daemon struct {
	opt  Options
	cfg  *config.Config
	st   *store.Store
	be   backend.Backend
	log  *log.Logger
	uid  int
	mu   sync.Mutex
	wake chan struct{}
	quit chan struct{}

	health   probe.Health
	peaks    map[string][]sample
	held     map[string][]int // passthrough descriptors awaiting their supervisor
	watchers map[string]map[chan struct{}]struct{}
	gone     map[string]time.Time // owner first seen gone
	connMu   sync.Mutex
	conns    map[int]int       // open connections per uid
	lastHold map[string]string // last logged reason codes per job

	started         time.Time
	pressurePaused  bool
	pressureClearAt time.Time
	lastEmergency   time.Time
	holdReason      *job.Reason
	summary         string
	maintenance     api.Maintenance
	lastMaint       time.Time
	lastRetention   time.Time
}

func New(opt Options) (*Daemon, error) {
	cfg := opt.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opt.Log == nil {
		opt.Log = os.Stderr
	}
	if err := os.MkdirAll(opt.Paths.State, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(opt.Paths.State)
	if err != nil {
		return nil, err
	}
	if opt.Paths.System {
		// Supervisors run as the submitting user and must reach their own files.
		for _, d := range []string{opt.Paths.State, st.LogDir(), st.RunDir()} {
			_ = os.Chmod(d, 0o711)
		}
	}
	be, err := backend.New(&cfg, opt.Paths.System, opt.Self, st.RunDir())
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		opt: opt, cfg: &cfg, st: st, be: be, uid: os.Getuid(),
		log:  log.New(opt.Log, "", 0),
		wake: make(chan struct{}, 1), quit: make(chan struct{}),
		peaks: map[string][]sample{}, held: map[string][]int{}, watchers: map[string]map[chan struct{}]struct{}{},
		gone: map[string]time.Time{}, lastHold: map[string]string{}, started: time.Now(), conns: map[int]int{},
	}
	return d, nil
}

func (d *Daemon) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Daemon) notify(id string) {
	for ch := range d.watchers[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (d *Daemon) save(j *job.Job) {
	if err := d.st.Save(j); err != nil {
		d.log.Printf("error: saving job %s: %v", job.Short(j.ID), err)
	}
	d.notify(j.ID)
}

// Serve listens on the socket and runs the scheduler until Stop.
func (d *Daemon) Serve() error {
	lock, err := os.OpenFile(filepath.Join(d.opt.Paths.State, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another berth daemon already uses %s", d.opt.Paths.State)
	}
	ln, err := d.listen()
	if err != nil {
		return err
	}
	defer ln.Close()
	d.log.Printf("berth %s: %s backend, pool %s / %g CPUs, state %s, socket %s", d.opt.Version, d.be.Name(),
		units.FormatBytes(int64(d.cfg.Pool.Memory)), d.cfg.Pool.CPUs, d.opt.Paths.State, d.opt.Paths.Socket)
	d.recover()
	go func() {
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				select {
				case <-d.quit:
					return
				default:
				}
				if errors.Is(err, net.ErrClosed) {
					return
				}
				d.log.Printf("accept: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			go d.handle(c)
		}
	}()
	notifySystemd()
	tick := time.Duration(d.cfg.Scheduler.Tick)
	for {
		d.tick()
		select {
		case <-d.quit:
			return nil
		case <-d.wake:
		case <-time.After(tick):
		}
	}
}

func (d *Daemon) Stop() { close(d.quit) }

func (d *Daemon) listen() (*net.UnixListener, error) {
	sock := d.opt.Paths.Socket
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return nil, err
	}
	if d.opt.Paths.System {
		// Explicit: the service's umask must not lock users out of the socket directory.
		if err := os.Chmod(filepath.Dir(sock), 0o755); err != nil {
			return nil, err
		}
	}
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		return nil, fmt.Errorf("a daemon is already listening on %s", sock)
	}
	_ = os.Remove(sock)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o600)
	if d.opt.Paths.System {
		mode = 0o666 // every user may submit; jobs run as the submitter (peer credentials)
	}
	return ln, os.Chmod(sock, mode)
}

func notifySystemd() {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = c.Write([]byte("READY=1"))
}

// recover reconciles state after a (re)start: jobs whose supervisor survived keep running.
func (d *Daemon) recover() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.st.Finished() {
		if err := d.logs(j).Seal(j.ID); err != nil {
			d.log.Printf("sealing the log of %s: %v", job.Short(j.ID), err)
		}
	}
	for _, j := range d.st.InStates(job.Starting) {
		if !d.be.Alive(j) {
			if res, ok := supervise.ReadResult(d.st.ResultPath(j.ID)); ok {
				d.finish(j, &res)
				continue
			}
			j.State = job.Queued
			j.Note = "requeued: the daemon stopped while starting it"
			d.save(j)
		} else {
			j.State = job.Running
			d.save(j)
		}
	}
}

// ---------------------------------------------------------------- the loop

func (d *Daemon) tick() {
	d.mu.Lock()
	now := time.Now()
	d.reap(now)
	d.owners(now)
	d.deadlines(now)
	d.measure(now)
	d.safety(now)
	in := d.input(now)
	plan := sched.Schedule(in)
	for _, c := range plan.Cancel {
		d.cancelQueued(c.Job, c.Note)
	}
	for _, j := range d.st.InStates(job.Queued) {
		w, ok := plan.Waiting[j.ID]
		if !ok {
			continue
		}
		if !sameReasons(j.Reasons, w.Reasons) || j.Hint != w.Hint {
			j.Reasons, j.Hint = w.Reasons, w.Hint
			d.save(j)
		}
		d.logHold(j)
	}
	d.summary = plan.Summary
	var starting []*job.Job
	for _, j := range plan.Start {
		j.State = job.Starting
		j.Reasons, j.Hint = nil, ""
		j.Started = now
		d.save(j)
		starting = append(starting, j)
	}
	d.mu.Unlock()

	for _, j := range starting {
		d.launch(j)
	}

	d.mu.Lock()
	if time.Since(d.lastRetention) > 10*time.Minute {
		d.lastRetention = time.Now()
		if r := d.retention(time.Now().Add(-time.Duration(d.cfg.Logs.Retention)), false); r.Jobs+r.Logs > 0 {
			d.log.Printf("retention: %s", r)
		}
	}
	d.mu.Unlock()
	d.maybeMaintain()
}

// input gathers the scheduler's view. Passthrough jobs whose submitter has not
// (re)attached its terminal yet are held back.
func (d *Daemon) input(now time.Time) sched.Input {
	var queued []*job.Job
	for _, j := range d.st.InStates(job.Queued) {
		if j.Spec.Passthrough && d.held[j.ID] == nil {
			r := []job.Reason{{Code: job.RAttach, Text: "waiting for the submitter to reattach its terminal"}}
			if !sameReasons(j.Reasons, r) {
				j.Reasons, j.Hint = r, ""
				d.save(j)
			}
			continue
		}
		queued = append(queued, j)
	}
	peaks := map[string]int64{}
	for id, ss := range d.peaks {
		for _, s := range ss {
			peaks[id] = max(peaks[id], s.bytes)
		}
	}
	return sched.Input{Cfg: d.cfg, Health: d.health, Now: now, Active: d.st.InStates(job.Starting, job.Running, job.Stopping),
		Queued: queued, Lookup: d.st.Get, Peaks: peaks, Hold: d.holdReason,
		Fair: d.opt.Paths.System, UserShare: map[bool]float64{true: d.cfg.Users.MaxShare}[d.opt.Paths.System]}
}

func (d *Daemon) launch(j *job.Job) {
	l := supervise.Launch{
		ID: j.ID, Argv: j.Spec.Argv, Cwd: j.Spec.Cwd, Env: j.Spec.Env, Timeout: j.Spec.Timeout,
		Passthrough: j.Spec.Passthrough, Token: j.Token, Socket: d.opt.Paths.Socket,
		LogDir: d.logDir(j), LogSegment: int64(d.cfg.Logs.SegmentSize), LogMax: int64(d.cfg.Logs.MaxPerJob),
		Result: d.st.ResultPath(j.ID), PgidFile: backend.PgidPath(d.st.RunDir(), j.ID),
		CPUs: j.Spec.CPUs, MemoryCap: j.Spec.Memory, EnforceRSS: !d.be.Enforces(),
	}
	if j.Spec.UnitMemory > 0 && j.Spec.UnitMemory < l.MemoryCap {
		l.MemoryCap = j.Spec.UnitMemory
	}
	launchPath := filepath.Join(d.st.RunDir(), j.ID+".launch")
	err := store.WriteJSON(launchPath, l, 0o600)
	if err == nil && d.opt.Paths.System {
		err = d.ownFiles(j, launchPath)
	}
	if err == nil {
		err = d.ensureLogDir(j)
	}
	var handle string
	if err == nil {
		_ = os.Remove(l.Result)
		if d.opt.Paths.System {
			err = d.ownFiles(j, l.Result)
		}
	}
	if err == nil {
		handle, err = d.be.Start(j, launchPath)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		j.State = job.Failed
		j.Finished = time.Now()
		j.Note = "could not start: " + err.Error()
		j.Result = &job.Result{ExitCode: 125, Reason: j.Note}
		d.closeHeld(j.ID)
		d.save(j)
		d.log.Printf("fail %s [%s]: %s", job.Short(j.ID), j.Label(), j.Note)
		return
	}
	j.Unit = handle
	waited := j.Started.Sub(j.Created)
	if j.State == job.Stopping { // cancelled while starting
		d.save(j)
		_ = d.be.Stop(j)
		return
	}
	j.State = job.Running
	d.save(j)
	d.log.Printf("start %s [%s] %s/%gcpu %s after %s queued", job.Short(j.ID), j.Label(),
		units.FormatBytes(j.Spec.Memory), j.Spec.CPUs, j.Spec.Kind, units.FormatDuration(waited))
}

// ownFiles creates files the supervisor (running as the job's user) must write.
func (d *Daemon) ownFiles(j *job.Job, paths ...string) error {
	for _, p := range paths {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		f.Close()
		if err := os.Chown(p, j.UID, -1); err != nil {
			return err
		}
	}
	return nil
}

// reap finalizes jobs whose supervisor has written a result or vanished.
func (d *Daemon) reap(now time.Time) {
	for _, j := range d.st.InStates(job.Running, job.Stopping) {
		if res, ok := supervise.ReadResult(d.st.ResultPath(j.ID)); ok {
			d.finish(j, &res)
			continue
		}
		if d.be.Alive(j) {
			continue
		}
		// The supervisor may have exited between the two checks.
		if res, ok := supervise.ReadResult(d.st.ResultPath(j.ID)); ok {
			d.finish(j, &res)
			continue
		}
		if now.Sub(j.Started) < 15*time.Second {
			continue // just launched; the unit may not be visible yet
		}
		d.finish(j, nil)
	}
}

func (d *Daemon) finish(j *job.Job, res *job.Result) {
	d.closeHeld(j.ID)
	delete(d.peaks, j.ID)
	delete(d.gone, j.ID)
	delete(d.lastHold, j.ID)
	j.Finished = time.Now()
	j.Result = res
	retry := false
	switch {
	case res == nil:
		j.State = job.Lost
		j.Note = "its supervisor vanished without a result (host restart or external kill)"
		retry = true
	case strings.HasPrefix(j.Note, "cancelled"):
		j.State = job.Cancelled
	case strings.HasPrefix(j.Note, "emergency"):
		j.State = job.Failed
		retry = true
	case res.ExitCode == 0 && !res.OOM && !res.TimedOut:
		j.State = job.Succeeded
		j.Note = ""
	default:
		j.State = job.Failed
		j.Note = res.Reason
		if j.Note == "" {
			j.Note = fmt.Sprintf("exit status %d", res.ExitCode)
			if res.Signal != "" {
				j.Note = "killed by " + res.Signal
			}
		}
	}
	d.save(j)
	for _, p := range []string{filepath.Join(d.st.RunDir(), j.ID+".launch"), backend.PgidPath(d.st.RunDir(), j.ID)} {
		_ = os.Remove(p)
	}
	dur := ""
	if !j.Started.IsZero() {
		dur = " after " + units.FormatDuration(j.Finished.Sub(j.Started))
	}
	d.log.Printf("%s %s [%s]%s %s", j.State, job.Short(j.ID), j.Label(), dur, j.Note)
	if retry && j.Spec.Retries > 0 && (j.Owner == nil || ownerAlive(j.Owner)) {
		n := *j
		n.ID, n.Token, n.Unit = job.NewID(), job.NewToken(), ""
		n.State, n.Note, n.Result, n.Reasons, n.Hint = job.Queued, "", nil, nil, ""
		n.Created, n.Started, n.Finished = time.Now(), time.Time{}, time.Time{}
		n.Spec.Retries--
		n.RetryOf = j.ID
		n.Spec.Passthrough = false // the original terminal cannot follow a retry
		d.save(&n)
		d.log.Printf("retry %s as %s (%d retries left)", job.Short(j.ID), job.Short(n.ID), n.Spec.Retries)
	}
}

func ownerAlive(o *job.Owner) bool {
	start := probe.ProcStart(o.PID)
	return start != 0 && start == o.Start
}

// owners cancels jobs whose submitter has been gone for longer than the grace period.
func (d *Daemon) owners(now time.Time) {
	grace := time.Duration(d.cfg.Scheduler.OwnerGrace)
	for _, j := range d.st.All() {
		if j.Owner == nil || j.State.Terminal() || j.State == job.Stopping {
			delete(d.gone, j.ID)
			continue
		}
		if ownerAlive(j.Owner) {
			delete(d.gone, j.ID)
			continue
		}
		since, ok := d.gone[j.ID]
		if !ok {
			d.gone[j.ID] = now
			continue
		}
		if now.Sub(since) < grace {
			continue
		}
		delete(d.gone, j.ID)
		d.cancel(j, "submitter exited")
	}
}

func (d *Daemon) deadlines(now time.Time) {
	for _, j := range d.st.InStates(job.Queued) {
		if j.Spec.QueueTimeout > 0 && now.Sub(j.Created) > j.Spec.QueueTimeout {
			last := "none"
			if len(j.Reasons) > 0 {
				last = j.Reasons[0].Text
			}
			d.cancelQueued(j, fmt.Sprintf("queue timeout: not started within %s (last reason: %s)", units.FormatDuration(j.Spec.QueueTimeout), last))
		}
	}
}

func (d *Daemon) cancelQueued(j *job.Job, note string) {
	if j.State != job.Queued {
		return
	}
	j.State = job.Cancelled
	j.Finished = time.Now()
	j.Note = "cancelled before start: " + note
	j.Reasons, j.Hint = nil, ""
	d.closeHeld(j.ID)
	delete(d.lastHold, j.ID)
	d.save(j)
	d.log.Printf("cancelled %s [%s]: %s", job.Short(j.ID), j.Label(), note)
}

// cancel stops a job in any live state.
func (d *Daemon) cancel(j *job.Job, reason string) {
	switch j.State {
	case job.Queued:
		d.cancelQueued(j, reason)
	case job.Starting:
		j.State, j.Note = job.Stopping, "cancelled: "+reason
		d.save(j)
	case job.Running:
		j.State, j.Note = job.Stopping, "cancelled: "+reason
		d.save(j)
		if err := d.be.Stop(j); err != nil {
			d.log.Printf("stop %s: %v", job.Short(j.ID), err)
		}
		d.log.Printf("stopping %s [%s]: %s", job.Short(j.ID), j.Label(), reason)
	}
}

func (d *Daemon) closeHeld(id string) {
	for _, fd := range d.held[id] {
		syscall.Close(fd)
	}
	delete(d.held, id)
}

// measure takes the health snapshot and tracks service peaks.
func (d *Daemon) measure(now time.Time) {
	active := d.st.InStates(job.Starting, job.Running, job.Stopping)
	cgroups := map[string]string{}
	for _, j := range active {
		if cg := d.be.JobCgroup(j); cg != "" {
			cgroups[j.ID] = cg
		}
	}
	h := probe.Snapshot(d.cfg.Safety.WatchPaths, d.be.PoolCgroup(), cgroups)
	if d.be.PoolCgroup() == "" {
		for _, j := range active {
			if w, ok := d.be.Working(j); ok {
				h.JobWorking[j.ID] = w
				h.PoolWorking += w
			}
		}
	}
	window := time.Duration(d.cfg.Services.PeakWindow)
	for _, j := range active {
		w := h.JobWorking[j.ID]
		if w > j.PeakBytes {
			j.PeakBytes = w
		}
		if j.Spec.Kind != job.Service {
			continue
		}
		ss := append(d.peaks[j.ID], sample{now, w})
		for len(ss) > 0 && now.Sub(ss[0].at) > window {
			ss = ss[1:]
		}
		d.peaks[j.ID] = ss
	}
	d.health = h
}

// safety applies the pause file, memory pressure and the disk emergency floor.
func (d *Daemon) safety(now time.Time) {
	d.holdReason = nil
	h, cfg := d.health, d.cfg
	for _, disk := range h.Disks {
		if disk.Free < int64(cfg.Safety.EmergencyFreeDisk) {
			text := fmt.Sprintf("emergency: %s has %s free, below the %s emergency floor", disk.Path,
				units.FormatBytes(disk.Free), units.FormatBytes(int64(cfg.Safety.EmergencyFreeDisk)))
			d.holdReason = &job.Reason{Code: job.REmergency, Text: text}
			d.emergencyStop(now, text)
			return
		}
	}
	if _, err := os.Stat(filepath.Join(d.opt.Paths.State, "paused")); err == nil {
		d.holdReason = &job.Reason{Code: job.RHold, Text: "admission paused (`berth resume` to continue)"}
		return
	}
	if h.MemTotal == 0 {
		return
	}
	min := int64(cfg.Safety.MinAvailableMemory)
	stressed := h.MemAvailable < min || (h.PressureFull > cfg.Safety.PressurePause && h.MemAvailable < 3*min)
	if stressed {
		if !d.pressurePaused {
			d.log.Printf("memory pressure: admission paused (available %s, PSI full %.1f%%)", units.FormatBytes(h.MemAvailable), h.PressureFull)
		}
		d.pressurePaused, d.pressureClearAt = true, time.Time{}
	}
	if d.pressurePaused {
		clear := h.MemAvailable >= min*3/2 && h.PressureFull < cfg.Safety.PressureResume
		switch {
		case !clear:
			d.pressureClearAt = time.Time{}
		case d.pressureClearAt.IsZero():
			d.pressureClearAt = now
		case now.Sub(d.pressureClearAt) >= time.Duration(cfg.Safety.PressureHold):
			d.pressurePaused = false
			d.log.Printf("memory pressure cleared: admission resumed")
			return
		}
		d.holdReason = &job.Reason{Code: job.RPressure, Text: fmt.Sprintf("memory pressure: %s available, PSI full %.1f%%; resumes after %s calm",
			units.FormatBytes(h.MemAvailable), h.PressureFull, units.FormatDuration(time.Duration(cfg.Safety.PressureHold)))}
	}
}

// emergencyStop stops the newest batch job (services last), one every 15 seconds.
func (d *Daemon) emergencyStop(now time.Time, why string) {
	if now.Sub(d.lastEmergency) < 15*time.Second {
		return
	}
	var pick *job.Job
	for _, j := range d.st.InStates(job.Running) {
		if pick == nil || (pick.Spec.Kind == job.Service && j.Spec.Kind != job.Service) ||
			(pick.Spec.Kind == j.Spec.Kind && j.Started.After(pick.Started)) {
			pick = j
		}
	}
	if pick == nil {
		return
	}
	d.lastEmergency = now
	pick.State, pick.Note = job.Stopping, why
	d.save(pick)
	_ = d.be.Stop(pick)
	d.log.Printf("emergency stop %s [%s]: %s", job.Short(pick.ID), pick.Label(), why)
}

func (d *Daemon) logHold(j *job.Job) {
	codes := make([]string, len(j.Reasons))
	for i, r := range j.Reasons {
		codes[i] = r.Code
	}
	key := strings.Join(codes, ",")
	if d.lastHold[j.ID] == key {
		return
	}
	d.lastHold[j.ID] = key
	texts := make([]string, len(j.Reasons))
	for i, r := range j.Reasons {
		texts[i] = r.Text
	}
	d.log.Printf("hold %s [%s] %s/%gcpu: %s", job.Short(j.ID), j.Label(), units.FormatBytes(j.Spec.Memory), j.Spec.CPUs, strings.Join(texts, "; "))
}

func sameReasons(a, b []job.Reason) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// maybeMaintain runs bounded btrfs balances in a root daemon when enabled.
func (d *Daemon) maybeMaintain() {
	if !d.cfg.Safety.BtrfsMaintenance || os.Geteuid() != 0 {
		return
	}
	d.mu.Lock()
	if d.maintenance.Running || time.Since(d.lastMaint) < 5*time.Minute {
		d.mu.Unlock()
		return
	}
	var target *probe.Btrfs
	for _, b := range d.health.Btrfs {
		if b.Unallocated < int64(d.cfg.Safety.BtrfsTargetUnallocated) {
			b := b
			target = &b
			break
		}
	}
	d.lastMaint = time.Now()
	if target == nil {
		d.mu.Unlock()
		return
	}
	d.maintenance.Running = true
	d.mu.Unlock()
	go func() {
		summary, err := maint.Balance(target.Path, int64(d.cfg.Safety.BtrfsTargetUnallocated), 5*time.Minute, func(s string) { d.log.Print(s) })
		if err != nil {
			summary = "btrfs maintenance: " + err.Error()
		}
		d.log.Print(summary)
		d.mu.Lock()
		d.maintenance = api.Maintenance{Finished: time.Now(), Summary: summary}
		d.mu.Unlock()
		d.poke()
	}()
}

// ---------------------------------------------------------------- requests

var groupRE = regexp.MustCompile(`^[A-Za-z0-9_.:@/-]{1,128}$`)

// normalize fills defaults and rejects requests that can never run, with the fix in the message.
// normalize fills defaults and rejects requests that can never run, or that
// uid may not make, with the fix in the message.
func (d *Daemon) normalize(s *job.Spec, uid int) error {
	cfg := d.cfg
	shared := d.opt.Paths.System && uid != 0
	if len(s.Argv) == 0 {
		return errors.New("no command given")
	}
	if !filepath.IsAbs(s.Cwd) {
		return errors.New("cwd must be an absolute path")
	}
	if s.Kind == "" {
		s.Kind = job.Batch
	}
	if s.Kind != job.Batch && s.Kind != job.Service {
		return errors.New("kind must be batch or service")
	}
	if s.Memory == 0 {
		s.Memory = int64(cfg.Defaults.Memory)
	}
	if s.CPUs == 0 {
		s.CPUs = cfg.Defaults.CPUs
	}
	if s.Disk < 0 {
		s.Disk = int64(cfg.Defaults.Disk)
		if s.Kind == job.Service {
			s.Disk = 0
		}
	}
	room := int64(cfg.Pool.Memory - cfg.Pool.Overhead)
	switch {
	case s.Memory < 64*units.MiB:
		return errors.New("memory must be at least 64M")
	case s.Memory > room:
		return fmt.Errorf("memory %s exceeds what this host's pool can ever give one job (%s); use --memory %s or less",
			units.FormatBytes(s.Memory), units.FormatBytes(room), units.FormatBytesDown(room))
	case s.CPUs <= 0 || s.CPUs > cfg.Pool.CPUs:
		return fmt.Errorf("cpus must be between 0.1 and %g (the pool)", cfg.Pool.CPUs)
	case s.UnitMemory < 0 || s.UnitMemory > s.Memory:
		return errors.New("unit memory must be within the reservation")
	case s.Timeout < 0 || s.QueueTimeout < 0 || s.Retries < 0 || s.Retries > 10:
		return errors.New("timeouts must be positive and retries 0-10")
	case s.Group != "" && !groupRE.MatchString(s.Group):
		return errors.New("group may contain letters, digits and _.:@/- (at most 128)")
	}
	for name, n := range s.Resources {
		total, ok := cfg.Pool.Resources[name]
		if !ok {
			return fmt.Errorf("resource %q is not defined on this host; defined: %v (pool.resources in the config)", name, keys(cfg.Pool.Resources))
		}
		if n <= 0 || n > total {
			return fmt.Errorf("resource %s: request 1..%d", name, total)
		}
	}
	for _, list := range [][]string{s.After, s.AfterAny} {
		for i, ref := range list {
			dep, err := d.resolve(ref, uid)
			if err != nil {
				return fmt.Errorf("dependency: %v", err)
			}
			list[i] = dep.ID
		}
	}
	if shared {
		if s.Kind == job.Service && !cfg.Users.AllowServices {
			return errors.New("services (--service) are reserved for root on this shared daemon (users.allow_services); submit it as a batch job")
		}
		// Groups are per user, so one user cannot hold another's lock. A
		// "global:" prefix opts into a group shared by everyone (a device, say).
		if s.Group != "" && !strings.HasPrefix(s.Group, "global:") && !strings.HasPrefix(s.Group, fmt.Sprintf("u%d:", uid)) {
			s.Group = fmt.Sprintf("u%d:%s", uid, s.Group)
		}
	}
	return nil
}

// resolve finds a job by id or prefix. On a shared daemon, users only see
// their own jobs this way (others are "not found", not "denied").
func (d *Daemon) resolve(ref string, uid int) (*job.Job, error) {
	if !d.opt.Paths.System || uid == 0 {
		return d.st.Resolve(ref)
	}
	return d.st.ResolveFor(ref, func(j *job.Job) bool { return j.UID == uid })
}

// view is a job as uid may see it: other users' jobs show state and size only.
func (d *Daemon) view(uid int, j *job.Job) job.Job {
	p := j.Public()
	if d.opt.Paths.System && uid != 0 && j.UID != uid {
		p.Spec = job.Spec{Argv: []string{"(another user's job)"}, Kind: j.Spec.Kind, Memory: j.Spec.Memory, CPUs: j.Spec.CPUs,
			Resources: j.Spec.Resources, Priority: j.Spec.Priority}
		p.Note, p.Hint, p.Reasons, p.Owner, p.Result, p.Unit, p.LogsGone = "", "", nil, nil, nil, "", ""

	}
	return p
}

// logs is a job's log directory; under a system daemon, its owner is checked on every read.
func (d *Daemon) logs(j *job.Job) logs.Dir {
	if d.opt.Paths.System {
		return logs.Dir{Path: d.logDir(j), Owner: j.UID}
	}
	return logs.Plain(d.logDir(j))
}

func keys(m map[string]int) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (d *Daemon) status(uid int, id string) api.Status {
	now := time.Now()
	in := d.input(now)
	u := sched.Measure(in)
	admission := "ready"
	if d.holdReason != nil {
		admission = d.holdReason.Text
	} else if d.summary != "" {
		admission = d.summary
	}
	st := api.Status{
		Version: d.opt.Version, Mode: map[bool]string{true: "system", false: "user"}[d.opt.Paths.System],
		Backend: d.be.Name(), Admission: admission, Health: d.health,
		Pool: api.PoolUsage{
			Memory: int64(d.cfg.Pool.Memory), MemoryCharged: u.MemoryCharged, Overhead: int64(d.cfg.Pool.Overhead),
			CPUs: d.cfg.Pool.CPUs, CPUsCharged: u.CPUsCharged, Resources: d.cfg.Pool.Resources, ResourcesInUse: u.ResourcesInUse,
			DefaultMemory: int64(d.cfg.Defaults.Memory), DefaultCPUs: d.cfg.Defaults.CPUs,
			FitsNowMemory: u.FitsNowMemory, FitsNowCPUs: u.FitsNowCPUs,
		},
	}
	st.Logs = api.LogUsage{Bytes: d.logBytes(), Budget: int64(d.cfg.Logs.MaxTotal), PerJob: int64(d.cfg.Logs.MaxPerJob),
		Retention: time.Duration(d.cfg.Logs.Retention)}
	if d.maintenance.Summary != "" || d.maintenance.Running {
		m := d.maintenance
		st.Maintenance = &m
	}
	var done []job.Job
	for _, j := range d.st.All() {
		if id != "" && j.ID != id {
			continue
		}
		p := d.view(uid, j)
		if j.State.Terminal() {
			done = append(done, p)
		} else {
			st.Jobs = append(st.Jobs, p)
		}
	}
	if len(done) > 50 && id == "" {
		done = done[len(done)-50:]
	}
	st.Jobs = append(st.Jobs, done...)
	return st
}

// logDir is where a job's output goes. A system daemon gives each user their
// own directory, because the job (running as that user) writes it.
func (d *Daemon) logDir(j *job.Job) string {
	if d.opt.Paths.System {
		return filepath.Join(d.st.LogDir(), strconv.Itoa(j.UID))
	}
	return d.st.LogDir()
}

func (d *Daemon) ensureLogDir(j *job.Job) error {
	dir := d.logDir(j)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if d.opt.Paths.System {
		return os.Chown(dir, j.UID, -1)
	}
	return nil
}

// Retained summarizes one retention pass.
type Retained struct {
	Jobs  int   // finished job records deleted (with their logs)
	Logs  int   // logs deleted to stay within the total budget (records kept)
	Freed int64 // bytes of logs removed
}

func (r Retained) String() string {
	return fmt.Sprintf("%d finished job(s) expired, %d log(s) dropped for the budget, %s freed", r.Jobs, r.Logs, units.FormatBytes(r.Freed))
}

// retention applies the log policy: finished jobs older than cutoff go
// entirely, records beyond keep_finished go, then the oldest finished jobs'
// logs go until all logs fit logs.max_total. Active jobs are never touched.
func (d *Daemon) retention(cutoff time.Time, dry bool) Retained {
	var r Retained
	remove := func(j *job.Job) {
		r.Freed += d.logs(j).JobSize(j.ID)
		if !dry {
			d.logs(j).Remove(j.ID)
		}
	}
	finished := d.st.Finished()
	excess := len(finished) - d.cfg.Scheduler.KeepFinished
	var kept []*job.Job
	for i, j := range finished {
		if j.Finished.Before(cutoff) || i < excess {
			remove(j)
			r.Jobs++
			if !dry {
				d.st.Delete(j.ID)
			}
			continue
		}
		kept = append(kept, j)
	}
	// Over budget: drop the oldest finished logs of whoever uses the most, so
	// one user's chatty jobs cannot evict everyone else's history.
	over := d.logBytes() - r.Freed - int64(d.cfg.Logs.MaxTotal)
	if over > 0 {
		sizes := map[*job.Job]int64{}
		for _, j := range kept {
			if j.LogsGone == "" {
				sizes[j] = d.logs(j).JobSize(j.ID)
			}
		}
		for _, victim := range evictions(kept, sizes, over) {
			remove(victim)
			r.Logs++
			if !dry {
				victim.LogsGone = "dropped to keep all logs within logs.max_total"
				d.save(victim)
			}
		}
	}
	return r
}

// logBytes is the disk used by all job logs.
func (d *Daemon) logBytes() int64 {
	total := logs.DirSize(d.st.LogDir())
	if d.opt.Paths.System {
		entries, _ := os.ReadDir(d.st.LogDir())
		for _, e := range entries {
			if e.IsDir() {
				total += logs.DirSize(filepath.Join(d.st.LogDir(), e.Name()))
			}
		}
	}
	return total
}

// evictions chooses which finished jobs' logs to drop to free at least over
// bytes: repeatedly the oldest job of the user whose logs are largest.
// finished is oldest first.
func evictions(finished []*job.Job, sizes map[*job.Job]int64, over int64) []*job.Job {
	usage := map[int]int64{}
	for j, n := range sizes {
		usage[j.UID] += n
	}
	taken := map[*job.Job]bool{}
	var out []*job.Job
	for over > 0 {
		heaviest, most := 0, int64(0)
		for uid, n := range usage {
			if n > most || (n == most && uid < heaviest) {
				heaviest, most = uid, n
			}
		}
		if most == 0 {
			break
		}
		var victim *job.Job
		for _, j := range finished {
			if !taken[j] && sizes[j] > 0 && j.UID == heaviest {
				victim = j
				break
			}
		}
		if victim == nil {
			delete(usage, heaviest)
			continue
		}
		taken[victim] = true
		out = append(out, victim)
		usage[heaviest] -= sizes[victim]
		over -= sizes[victim]
	}
	return out
}
