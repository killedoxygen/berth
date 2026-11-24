package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/client"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/units"
)

func cmdRun(args []string) int {
	fs := newFlags("run", "[options] [--] COMMAND [ARGS...]")
	f := addJobFlags(fs)
	quiet := fs.Bool("quiet", os.Getenv("BERTH_QUIET") != "", "no progress messages on stderr")
	giveUp := &durFlag{20 * time.Second}
	fs.Var(giveUp, "give-up", "exit 75 if the job cannot start while current services run for this long (0: wait)")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	spec, err := f.spec(fs.Args())
	if err != nil {
		return fail(err)
	}
	return client.Run(spec, client.RunOptions{Quiet: *quiet, GiveUp: giveUp.v, Label: label(spec)})
}

func cmdSubmit(args []string) int {
	fs := newFlags("submit", "[options] [--] COMMAND [ARGS...]")
	f := addJobFlags(fs)
	asJSON := fs.Bool("json", false, "print the job as JSON")
	wait := fs.Bool("wait", false, "then wait, streaming output; exit with the job's status")
	bind := fs.Bool("bind", false, "cancel the job when the calling shell (the parent process) exits")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	spec, err := f.spec(fs.Args())
	if err != nil {
		return fail(err)
	}
	pid := 0
	if *bind {
		pid = os.Getppid()
	}
	resp, err := client.Submit(spec, *bind, pid)
	if err != nil {
		return fail(err)
	}
	j := resp.Job
	if resp.Reused {
		fmt.Fprintf(os.Stderr, "berth: an unfinished job with key %q exists; reusing %s\n", spec.Key, job.Short(j.ID))
	}
	if *asJSON {
		printJSON(j)
	} else {
		fmt.Println(j.ID)
	}
	if !*wait {
		return 0
	}
	return follow(j.ID, 0, false)
}

// follow streams a job's output (or, for passthrough jobs, just waits) and returns its exit status.
func follow(id string, timeout time.Duration, quiet bool) int {
	prog := &client.Progress{Out: os.Stderr, Quiet: quiet, Label: "job " + job.Short(id)}
	var stop time.Time
	if timeout > 0 {
		stop = time.Now().Add(timeout)
	}
	// Report waiting until it starts, then stream.
	for {
		j, err := client.Get(id)
		if err != nil {
			return fail(err)
		}
		if j.State != job.Queued {
			prog.Started(j)
			break
		}
		prog.Update(j)
		if !stop.IsZero() && time.Now().After(stop) {
			fmt.Fprintf(os.Stderr, "berth: %s still queued after %s: %s\n", job.Short(id), units.FormatDuration(timeout), client.Explain(j))
			return client.ExitNotStarted
		}
		time.Sleep(500 * time.Millisecond)
	}
	j, err := client.Follow(id, os.Stdout, 0, stop)
	if err == client.ErrTimeout {
		fmt.Fprintf(os.Stderr, "berth: %s still %s after %s\n", job.Short(id), j.State, units.FormatDuration(timeout))
		return client.ExitNotStarted
	}
	if err != nil {
		return fail(err)
	}
	if j.State != job.Succeeded && !quiet && j.Note != "" {
		fmt.Fprintf(os.Stderr, "berth: %s %s: %s\n", job.Short(j.ID), j.State, j.Note)
	}
	return client.ExitCode(j)
}

func cmdWait(args []string) int {
	fs := newFlags("wait", "[options] ID...")
	timeout := &durFlag{}
	fs.Var(timeout, "timeout", "stop waiting after this long (exit 75); the job continues")
	asJSON := fs.Bool("json", false, "print final jobs as JSON instead of streaming output")
	quiet := fs.Bool("quiet", false, "no progress messages")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() == 0 {
		return fail(fmt.Errorf("give at least one job id"))
	}
	code := 0
	var final []*job.Job
	for _, id := range fs.Args() {
		if *asJSON {
			j, err := client.Watch(id, timeout.v, nil)
			if err != nil && err != client.ErrTimeout {
				return fail(err)
			}
			final = append(final, j)
			if j != nil && j.State.Terminal() {
				code = client.ExitCode(j)
			} else {
				code = client.ExitNotStarted
			}
			continue
		}
		code = follow(id, timeout.v, *quiet)
	}
	if *asJSON {
		printJSON(final)
	}
	return code
}

func cmdLogs(args []string) int {
	fs := newFlags("logs", "[options] ID")
	followFlag := fs.Bool("f", false, "follow until the job ends")
	tail := &sizeFlag{}
	fs.Var(tail, "tail", "only the last SIZE of output, e.g. 64K")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("give one job id"))
	}
	id := fs.Arg(0)
	if *followFlag {
		j, err := client.Follow(id, os.Stdout, tail.v, time.Time{})
		if err != nil {
			return fail(err)
		}
		return client.ExitCode(j)
	}
	offset := int64(0)
	if tail.v > 0 {
		offset = -tail.v
	}
	for {
		resp, err := client.Call(api.Request{Op: "logs", ID: id, Offset: offset})
		if err != nil {
			return fail(err)
		}
		if resp.Warning != "" {
			fmt.Fprintln(os.Stderr, "berth:", resp.Warning)
			return 0
		}
		os.Stdout.WriteString(resp.Data)
		if resp.Data == "" || resp.Offset == offset {
			return 0
		}
		offset = resp.Offset
	}
}

func cmdCancel(args []string) int {
	fs := newFlags("cancel", "[--reason TEXT] ID...")
	reason := fs.String("reason", "", "recorded on the job")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() == 0 {
		return fail(fmt.Errorf("give at least one job id"))
	}
	code := 0
	for _, id := range fs.Args() {
		j, err := client.Cancel(id, *reason)
		if err != nil {
			code = fail(err)
			continue
		}
		fmt.Printf("%s %s\n", job.Short(j.ID), j.State)
	}
	return code
}

func cmdUpdate(args []string) int {
	fs := newFlags("update", "[options] ID")
	var mem sizeFlag
	fs.Var(&mem, "memory", "new memory reservation (queued jobs only)")
	cpus := fs.Float64("cpus", 0, "new CPU reservation (queued jobs only)")
	prio := fs.Int("priority", 0, "new priority")
	retries := fs.Int("retries", -1, "new retry budget")
	service := fs.Bool("service", false, "make it a service (charged by use once settled)")
	batch := fs.Bool("batch", false, "make it a batch job (charged its full reservation)")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("give one job id"))
	}
	u := &api.Update{}
	set := map[string]bool{}
	fs.Visit(func(f *flagT) { set[f.Name] = true })
	if set["memory"] {
		u.Memory = &mem.v
	}
	if set["cpus"] {
		u.CPUs = cpus
	}
	if set["priority"] {
		u.Priority = prio
	}
	if set["retries"] {
		u.Retries = retries
	}
	if *service || *batch {
		k := job.Service
		if *batch {
			k = job.Batch
		}
		u.Kind = &k
	}
	resp, err := client.Call(api.Request{Op: "update", ID: fs.Arg(0), Update: u})
	if err != nil {
		return fail(err)
	}
	j := resp.Job
	fmt.Printf("%s %s %s/%gcpu %s priority %d\n", job.Short(j.ID), j.State, units.FormatBytes(j.Spec.Memory), j.Spec.CPUs, j.Spec.Kind, j.Spec.Priority)
	return 0
}

func cmdPause(args []string) int  { return toggle("pause", args) }
func cmdResume(args []string) int { return toggle("resume", args) }

func toggle(op string, args []string) int {
	fs := newFlags(op, "")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	if _, err := client.Call(api.Request{Op: op}); err != nil {
		return fail(err)
	}
	fmt.Println("admission", map[string]string{"pause": "paused", "resume": "resumed"}[op])
	return 0
}
