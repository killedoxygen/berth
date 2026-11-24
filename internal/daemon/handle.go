package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/peer"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

type client struct {
	pid, uid int
}

func fail(code, format string, args ...any) *api.Response {
	return &api.Response{Error: fmt.Sprintf(format, args...), Code: code}
}

func (d *Daemon) handle(raw *net.UnixConn) {
	c := api.Wrap(raw)
	defer c.Close()
	pid, uid, err := peer.Cred(raw)
	if err != nil {
		_ = c.Send(fail(api.EDenied, "cannot identify the caller: %v", err))
		return
	}
	if !d.opt.Paths.System && uid != d.uid && uid != 0 {
		_ = c.Send(fail(api.EDenied, "this daemon belongs to uid %d", d.uid))
		return
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	var req api.Request
	if err := c.Recv(&req); err != nil {
		for _, fd := range c.TakeFDs() {
			syscall.Close(fd)
		}
		return
	}
	_ = c.SetDeadline(time.Time{})
	fds := c.TakeFDs()
	cl := client{pid: pid, uid: uid}
	switch req.Op {
	case "submit":
		d.submit(c, cl, req, fds)
	case "attach":
		d.attach(c, cl, req, fds)
	case "claim":
		d.claim(c, req)
	default:
		for _, fd := range fds {
			syscall.Close(fd)
		}
		resp := d.simple(cl, req)
		if resp.Error == "" {
			resp.OK = true
		}
		_ = c.Send(resp)
	}
}

// lookup resolves a job reference and checks the caller may act on it.
func (d *Daemon) lookup(cl client, ref string, write bool) (*job.Job, *api.Response) {
	j, err := d.st.Resolve(ref)
	if err != nil {
		return nil, fail(api.ENotFound, "%v", err)
	}
	if write && d.opt.Paths.System && cl.uid != 0 && j.UID != cl.uid {
		return nil, fail(api.EDenied, "job %s belongs to another user", job.Short(j.ID))
	}
	return j, nil
}

func (d *Daemon) simple(cl client, req api.Request) *api.Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch req.Op {
	case "ping":
		return &api.Response{Data: d.opt.Version}
	case "pause", "resume":
		if d.opt.Paths.System && cl.uid != 0 {
			return fail(api.EDenied, "only root can %s a system daemon", req.Op)
		}
		path := filepath.Join(d.opt.Paths.State, "paused")
		var err error
		if req.Op == "pause" {
			err = os.WriteFile(path, nil, 0o600)
		} else if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			return fail(api.EInternal, "%v", err)
		}
		d.log.Printf("admission %sd by uid %d", req.Op, cl.uid)
		d.poke()
		return &api.Response{}
	case "status":
		id := ""
		if req.ID != "" {
			j, resp := d.lookup(cl, req.ID, false)
			if resp != nil {
				return resp
			}
			id = j.ID
		}
		st := d.status(cl.uid, id)
		return &api.Response{Status: &st}
	case "get":
		j, resp := d.lookup(cl, req.ID, false)
		if resp != nil {
			return resp
		}
		p := j.Public()
		return &api.Response{Job: &p}
	case "cancel":
		j, resp := d.lookup(cl, req.ID, true)
		if resp != nil {
			return resp
		}
		reason := req.Reason
		if reason == "" {
			reason = "cancelled by request"
		}
		d.cancel(j, reason)
		d.poke()
		p := j.Public()
		return &api.Response{Job: &p}
	case "update":
		j, resp := d.lookup(cl, req.ID, true)
		if resp != nil {
			return resp
		}
		return d.update(j, req.Update)
	case "logs":
		j, resp := d.lookup(cl, req.ID, false)
		if resp != nil {
			return resp
		}
		if d.opt.Paths.System && cl.uid != 0 && j.UID != cl.uid {
			return fail(api.EDenied, "job %s belongs to another user", job.Short(j.ID))
		}
		return d.logs(j, req.Offset, req.Limit)
	}
	return fail(api.EInvalid, "unknown operation %q", req.Op)
}

func (d *Daemon) update(j *job.Job, u *api.Update) *api.Response {
	if u == nil {
		return fail(api.EInvalid, "nothing to update")
	}
	if j.State.Terminal() {
		return fail(api.EInvalid, "job %s has finished", job.Short(j.ID))
	}
	spec := j.Spec
	if u.Kind != nil {
		spec.Kind = *u.Kind
	}
	if u.Priority != nil {
		spec.Priority = *u.Priority
	}
	if u.Retries != nil {
		spec.Retries = *u.Retries
	}
	if u.Memory != nil || u.CPUs != nil {
		if j.State != job.Queued {
			return fail(api.EInvalid, "memory and cpus can only change while the job is queued")
		}
		if u.Memory != nil {
			spec.Memory = *u.Memory
		}
		if u.CPUs != nil {
			spec.CPUs = *u.CPUs
		}
	}
	spec.After, spec.AfterAny = nil, nil
	if err := d.normalize(&spec); err != nil {
		return fail(api.EInvalid, "%v", err)
	}
	spec.After, spec.AfterAny = j.Spec.After, j.Spec.AfterAny
	j.Spec = spec
	d.save(j)
	d.log.Printf("update %s [%s]: %s/%gcpu %s priority %d", job.Short(j.ID), j.Label(), units.FormatBytes(spec.Memory), spec.CPUs, spec.Kind, spec.Priority)
	d.poke()
	p := j.Public()
	return &api.Response{Job: &p}
}

const maxLogChunk = 256 << 10

func (d *Daemon) logs(j *job.Job, offset int64, limit int) *api.Response {
	if j.Spec.Passthrough {
		return &api.Response{Warning: "this job's output went to its submitter's terminal, not to a log"}
	}
	f, err := os.Open(d.st.LogPath(j.ID))
	if errors.Is(err, os.ErrNotExist) {
		return &api.Response{Offset: 0}
	}
	if err != nil {
		return fail(api.EInternal, "%v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fail(api.EInternal, "%v", err)
	}
	size := info.Size()
	if offset < 0 { // tail: the last -offset bytes
		offset = max(0, size+offset)
	}
	if limit <= 0 || limit > maxLogChunk {
		limit = maxLogChunk
	}
	if offset > size {
		offset = size
	}
	buf := make([]byte, min(int64(limit), size-offset))
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return fail(api.EInternal, "%v", err)
	}
	return &api.Response{Data: string(buf[:n]), Offset: offset + int64(n)}
}

func (d *Daemon) submit(c *api.Conn, cl client, req api.Request, fds []int) {
	closeFDs := func() {
		for _, fd := range fds {
			syscall.Close(fd)
		}
	}
	if req.Spec == nil {
		closeFDs()
		_ = c.Send(fail(api.EInvalid, "submit needs a spec"))
		return
	}
	spec := *req.Spec
	if spec.Passthrough && len(fds) != 3 {
		closeFDs()
		_ = c.Send(fail(api.EInvalid, "passthrough needs stdin, stdout and stderr descriptors"))
		return
	}
	if !spec.Passthrough {
		closeFDs()
		fds = nil
	}
	d.mu.Lock()
	if spec.Key != "" {
		for _, j := range d.st.All() {
			if j.Spec.Key == spec.Key && j.UID == cl.uid && !j.State.Terminal() {
				d.mu.Unlock()
				closeFDs()
				p := j.Public()
				_ = c.Send(&api.Response{OK: true, Job: &p, Reused: true})
				if req.Watch {
					d.watch(c, j.ID)
				}
				return
			}
		}
	}
	if err := d.normalize(&spec); err != nil {
		d.mu.Unlock()
		closeFDs()
		_ = c.Send(fail(api.EInvalid, "%v", err))
		return
	}
	j := &job.Job{ID: job.NewID(), UID: cl.uid, Spec: spec, State: job.Queued, Created: time.Now(), Token: job.NewToken()}
	if req.Bind {
		pid := cl.pid
		if req.BindPID > 0 {
			pid = req.BindPID
		}
		if start := probe.ProcStart(pid); start != 0 {
			j.Owner = &job.Owner{PID: pid, Start: start}
		}
	}
	if len(fds) == 3 {
		d.held[j.ID] = fds
	}
	d.save(j)
	d.log.Printf("submit %s [%s] %s/%gcpu %s%s", job.Short(j.ID), j.Label(), units.FormatBytes(spec.Memory), spec.CPUs, spec.Kind,
		map[bool]string{true: " bound", false: ""}[j.Owner != nil])
	p := j.Public()
	d.mu.Unlock()
	d.poke()
	if err := c.Send(&api.Response{OK: true, Job: &p}); err != nil {
		return
	}
	if req.Watch {
		d.watch(c, j.ID)
	}
}

// attach lets a submitter re-supply its terminal (after a daemon restart) and follow the job.
func (d *Daemon) attach(c *api.Conn, cl client, req api.Request, fds []int) {
	d.mu.Lock()
	j, resp := d.lookup(cl, req.ID, true)
	if resp != nil {
		d.mu.Unlock()
		for _, fd := range fds {
			syscall.Close(fd)
		}
		_ = c.Send(resp)
		return
	}
	if j.Spec.Passthrough && j.State == job.Queued && d.held[j.ID] == nil && len(fds) == 3 {
		d.held[j.ID] = fds
		fds = nil
	}
	for _, fd := range fds {
		syscall.Close(fd)
	}
	p := j.Public()
	d.mu.Unlock()
	d.poke()
	if err := c.Send(&api.Response{OK: true, Job: &p}); err == nil {
		d.watch(c, j.ID)
	}
}

// claim hands a passthrough job's descriptors to its supervisor.
func (d *Daemon) claim(c *api.Conn, req api.Request) {
	d.mu.Lock()
	j := d.st.Get(req.ID)
	if j == nil || req.Token == "" || req.Token != j.Token {
		d.mu.Unlock()
		_ = c.Send(fail(api.EDenied, "invalid claim"))
		return
	}
	fds := d.held[j.ID]
	delete(d.held, j.ID)
	d.mu.Unlock()
	if len(fds) != 3 {
		_ = c.Send(fail(api.ENotFound, "the submitter's terminal is gone"))
		return
	}
	_ = c.Send(&api.Response{OK: true}, fds...)
	for _, fd := range fds {
		syscall.Close(fd)
	}
}

// watch streams job events until the job ends or the client goes away.
func (d *Daemon) watch(c *api.Conn, id string) {
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	if d.watchers[id] == nil {
		d.watchers[id] = map[chan struct{}]struct{}{}
	}
	d.watchers[id][ch] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.watchers[id], ch)
		if len(d.watchers[id]) == 0 {
			delete(d.watchers, id)
		}
		d.mu.Unlock()
	}()
	// Detect the client hanging up while we wait.
	closed := make(chan struct{})
	go func() {
		var discard api.Request
		_ = c.Recv(&discard)
		close(closed)
	}()
	lastEvent := ""
	var lastReasons []job.Reason
	lastHint := ""
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		d.mu.Lock()
		j := d.st.Get(id)
		var p job.Job
		if j != nil {
			p = j.Public()
		}
		d.mu.Unlock()
		if j == nil {
			_ = c.Send(fail(api.ENotFound, "job %s disappeared", job.Short(id)))
			return
		}
		event := "queued"
		switch {
		case p.State.Terminal():
			event = "exit"
		case p.State.Active():
			event = "started"
		}
		if event != lastEvent || (event == "queued" && (!sameReasons(p.Reasons, lastReasons) || p.Hint != lastHint)) {
			if err := c.Send(&api.Response{OK: true, Event: event, Job: &p}); err != nil {
				return
			}
			lastEvent, lastReasons, lastHint = event, p.Reasons, p.Hint
		}
		if p.State.Terminal() {
			return
		}
		select {
		case <-ch:
		case <-heartbeat.C:
		case <-closed:
			return
		case <-d.quit:
			return
		}
	}
}
