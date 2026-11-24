// Package client talks to the daemon on behalf of the CLI, the tool shims and the MCP server.
package client

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/supervise"
	"github.com/killedoxygen/berth/internal/units"
)

// Exit codes beyond the job's own.
const (
	ExitNotStarted = 75  // queue timeout, unsatisfiable, cancelled before start
	ExitBerth      = 125 // berth itself failed (daemon unreachable, invalid request)
	ExitCancelled  = 130 // cancelled by the submitter (Ctrl-C)
)

func Socket() string { return config.ClientSocket() }

func Call(req api.Request) (*api.Response, error) { return api.Call(Socket(), req) }

// Get fetches one job.
func Get(id string) (*job.Job, error) {
	resp, err := Call(api.Request{Op: "get", ID: id})
	if err != nil {
		return nil, err
	}
	return resp.Job, nil
}

func Cancel(id, reason string) (*job.Job, error) {
	resp, err := Call(api.Request{Op: "cancel", ID: id, Reason: reason})
	if err != nil {
		return nil, err
	}
	return resp.Job, nil
}

// Submit queues a job (log mode). bind ties it to pid (0: the calling process).
func Submit(spec job.Spec, bind bool, pid int) (*api.Response, error) {
	spec.Passthrough = false
	return Call(api.Request{Op: "submit", Spec: &spec, Bind: bind, BindPID: pid})
}

// ExitCode maps a finished job to a process exit status.
func ExitCode(j *job.Job) int {
	switch {
	case j.Result != nil:
		return j.Result.ExitCode
	case j.State == job.Succeeded:
		return 0
	case j.State == job.Cancelled && j.Started.IsZero():
		return ExitNotStarted
	case j.State == job.Cancelled:
		return ExitCancelled
	}
	return 1
}

// Progress reports waiting on stderr: nothing for quick starts (2s), then a
// line on every change of reason and every 30 seconds.
type Progress struct {
	Out     io.Writer
	Quiet   bool
	Label   string
	began   time.Time
	job     *job.Job
	last    string
	lastAt  time.Time
	printed bool
}

// Update records the latest queued state and reports it if due.
func (p *Progress) Update(j *job.Job) {
	if p.began.IsZero() {
		p.began = time.Now()
	}
	p.job = j
	p.Tick()
}

// Tick reports the current waiting state if due; call it periodically.
func (p *Progress) Tick() {
	if p.Quiet || p.job == nil || p.job.State != job.Queued {
		return
	}
	waited := time.Since(p.began)
	if waited < 2*time.Second && !unsatisfiable(p.job) {
		return
	}
	text := Explain(p.job)
	if text == p.last && time.Since(p.lastAt) < 30*time.Second {
		return
	}
	fmt.Fprintf(p.Out, "berth: %s queued %s [%s]: %s\n", p.Label, units.FormatDuration(waited), job.Short(p.job.ID), text)
	p.last, p.lastAt, p.printed = text, time.Now(), true
}

func (p *Progress) Started(j *job.Job) {
	p.job = nil
	if p.printed && !p.Quiet {
		fmt.Fprintf(p.Out, "berth: %s started after %s\n", p.Label, units.FormatDuration(time.Since(p.began)))
	}
}

// Explain is the one-line "why is it waiting" for people and agents.
func Explain(j *job.Job) string {
	if len(j.Reasons) == 0 {
		return "waiting for the scheduler"
	}
	parts := make([]string, len(j.Reasons))
	for i, r := range j.Reasons {
		parts[i] = r.Text
	}
	s := strings.Join(parts, "; ")
	if j.Hint != "" {
		s += " (" + j.Hint + ")"
	}
	return s
}

func unsatisfiable(j *job.Job) bool {
	return len(j.Reasons) == 1 && j.Reasons[0].Code == job.RUnsatisfied
}

type RunOptions struct {
	Quiet  bool
	GiveUp time.Duration // give up on a job that cannot start while services run (0: never)
	Label  string
	Stderr io.Writer
}

// Run executes spec with this process's stdin/stdout/stderr and returns the exit status.
// The job is bound to this process: if it dies, the daemon cancels the job.
func Run(spec job.Spec, opt RunOptions) int {
	if opt.Stderr == nil {
		opt.Stderr = os.Stderr
	}
	spec.Passthrough = true
	prog := &Progress{Out: opt.Stderr, Quiet: opt.Quiet, Label: opt.Label}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	c, err := api.Dial(Socket())
	if err != nil {
		fmt.Fprintln(opt.Stderr, "berth:", api.Unavailable(Socket(), err))
		return ExitBerth
	}
	if err := c.Send(api.Request{Op: "submit", Spec: &spec, Bind: true, Watch: true}, 0, 1, 2); err != nil {
		fmt.Fprintln(opt.Stderr, "berth: submit:", err)
		return ExitBerth
	}
	var id string
	var unsatSince time.Time
	interrupts := 0
	events := make(chan *api.Response)
	errs := make(chan error, 1)
	reader := func(c *api.Conn) {
		for {
			var r api.Response
			if err := c.Recv(&r); err != nil {
				errs <- err
				return
			}
			events <- &r
		}
	}
	go reader(c)
	for {
		select {
		case r := <-events:
			if !r.OK {
				fmt.Fprintln(opt.Stderr, "berth:", r.Error)
				return ExitBerth
			}
			if r.Job == nil {
				continue
			}
			j := r.Job
			id = j.ID
			switch r.Event {
			case "", "queued":
				if r.Reused {
					fmt.Fprintf(opt.Stderr, "berth: reusing active job %s with the same key\n", job.Short(j.ID))
				}
				prog.Update(j)
				if unsatisfiable(j) && opt.GiveUp > 0 {
					if unsatSince.IsZero() {
						unsatSince = time.Now()
					}
				} else {
					unsatSince = time.Time{}
				}
			case "started":
				prog.Started(j)
			case "exit":
				if j.State != job.Succeeded && !opt.Quiet {
					note := j.Note
					if note == "" {
						note = string(j.State)
					}
					if j.State == job.Failed && j.Result != nil && j.Result.Reason == "" {
						note = "" // an ordinary non-zero exit needs no commentary
					}
					if note != "" {
						fmt.Fprintf(opt.Stderr, "berth: %s %s: %s\n", job.Short(j.ID), j.State, note)
					}
				}
				return ExitCode(j)
			}
		case err := <-errs:
			// The daemon went away (restart?). Reattach, re-supplying the terminal if needed.
			if id == "" {
				fmt.Fprintln(opt.Stderr, "berth: lost the daemon before the job was accepted:", err)
				return ExitBerth
			}
			nc, res, rerr := reattach(id, opt.Stderr)
			if res != nil {
				return res.ExitCode
			}
			if rerr != nil {
				fmt.Fprintf(opt.Stderr, "berth: lost the daemon and could not reattach to %s: %v\n", job.Short(id), rerr)
				return ExitBerth
			}
			c = nc
			go reader(c)
		case sig := <-sigs:
			interrupts++
			if id == "" || interrupts > 1 {
				return ExitCancelled
			}
			fmt.Fprintf(opt.Stderr, "\nberth: %s, cancelling %s (again to leave immediately)\n", sig, job.Short(id))
			_, _ = Cancel(id, "submitter interrupted")
		case <-time.After(time.Second):
			prog.Tick()
			if !unsatSince.IsZero() && time.Since(unsatSince) >= opt.GiveUp {
				j, _ := Get(id)
				_, _ = Cancel(id, "cannot start while current services run")
				msg := "cannot start while current services run"
				if j != nil {
					msg = Explain(j)
				}
				fmt.Fprintf(opt.Stderr, "berth: giving up on %s: %s\n", job.Short(id), msg)
				return ExitNotStarted
			}
		}
	}
}

// reattach reconnects after the daemon went away. The job keeps running
// meanwhile; if it finishes before the daemon returns, its result file (user
// mode) gives the exit status directly.
func reattach(id string, stderr io.Writer) (*api.Conn, *job.Result, error) {
	began := time.Now()
	said := false
	resultFile := filepath.Join(config.UserPaths().State, "run", id+".result")
	for {
		c, err := api.Dial(Socket())
		if err == nil {
			if err = c.Send(api.Request{Op: "attach", ID: id}, 0, 1, 2); err == nil {
				if said {
					fmt.Fprintln(stderr, "berth: daemon is back")
				}
				return c, nil, nil
			}
			c.Close()
		}
		if res, ok := supervise.ReadResult(resultFile); ok {
			return nil, &res, nil
		}
		if !said && time.Since(began) > 5*time.Second {
			fmt.Fprintf(stderr, "berth: daemon unreachable; job %s keeps running, waiting up to 10m for the daemon\n", job.Short(id))
			said = true
		}
		if time.Since(began) > 10*time.Minute {
			return nil, nil, err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Watch waits for a job to finish, calling onEvent for each update, and returns the final job.
// It survives daemon restarts. timeout 0 waits indefinitely.
func Watch(id string, timeout time.Duration, onEvent func(*api.Response)) (*job.Job, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	var last *job.Job
	for {
		c, err := api.Dial(Socket())
		if err != nil {
			if !deadline.IsZero() && time.Now().After(deadline) {
				return last, api.Unavailable(Socket(), err)
			}
			time.Sleep(time.Second)
			continue
		}
		if err := c.Send(api.Request{Op: "attach", ID: id}); err != nil {
			c.Close()
			return last, err
		}
		done := make(chan struct{})
		if !deadline.IsZero() {
			go func() {
				select {
				case <-time.After(time.Until(deadline)):
					c.Close()
				case <-done:
				}
			}()
		}
		for {
			var r api.Response
			if err := c.Recv(&r); err != nil {
				break
			}
			if !r.OK {
				close(done)
				c.Close()
				return last, &api.Error{Code: r.Code, Message: r.Error}
			}
			if r.Job != nil {
				last = r.Job
				if onEvent != nil {
					onEvent(&r)
				}
				if r.Job.State.Terminal() {
					close(done)
					c.Close()
					return last, nil
				}
			}
		}
		close(done)
		c.Close()
		if !deadline.IsZero() && time.Now().After(deadline) {
			return last, ErrTimeout
		}
		time.Sleep(500 * time.Millisecond)
	}
}

var ErrTimeout = errors.New("timed out waiting")

// Follow copies a log-mode job's output to w until the job ends (or stopAt passes).
func Follow(id string, w io.Writer, fromTail int64, stopAt time.Time) (*job.Job, error) {
	offset := int64(0)
	if fromTail > 0 {
		offset = -fromTail
	}
	for {
		j, err := Get(id)
		if err != nil {
			return nil, err
		}
		for {
			resp, err := Call(api.Request{Op: "logs", ID: id, Offset: offset})
			if err != nil {
				return j, err
			}
			if resp.Warning != "" {
				return Watch(id, time.Until(stopAt), nil)
			}
			if resp.Data != "" {
				_, _ = io.WriteString(w, resp.Data)
			}
			moved := resp.Offset != offset
			offset = resp.Offset
			if !moved || len(resp.Data) < 256<<10 {
				break
			}
		}
		if j.State.Terminal() {
			// One last read: output written just before the job ended.
			if resp, err := Call(api.Request{Op: "logs", ID: id, Offset: offset}); err == nil && resp.Data != "" {
				_, _ = io.WriteString(w, resp.Data)
			}
			return j, nil
		}
		if !stopAt.IsZero() && time.Now().After(stopAt) {
			return j, ErrTimeout
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// Tail returns up to n bytes from the end of a job's log.
func Tail(id string, n int64) (string, error) {
	resp, err := Call(api.Request{Op: "logs", ID: id, Offset: -n})
	if err != nil {
		return "", err
	}
	return resp.Data, nil
}
