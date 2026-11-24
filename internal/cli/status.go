package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/client"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/units"
)

func cmdStatus(args []string) int {
	fs := newFlags("status", "[--json] [--all] [ID]")
	asJSON := fs.Bool("json", false, "machine-readable output (stable fields)")
	all := fs.Bool("all", false, "include recently finished jobs")
	if ok, code := parse(fs, args); !ok {
		return code
	}
	req := api.Request{Op: "status"}
	if fs.NArg() > 0 {
		req.ID = fs.Arg(0)
	}
	resp, err := client.Call(req)
	if err != nil {
		return fail(err)
	}
	st := resp.Status
	if *asJSON {
		printJSON(st)
		return 0
	}
	if req.ID != "" && len(st.Jobs) == 1 {
		printJob(&st.Jobs[0])
		return 0
	}
	printOverview(st, *all)
	return 0
}

func b(v int64) string { return units.FormatBytes(v) }

func printOverview(st *api.Status, all bool) {
	p, h := st.Pool, st.Health
	fmt.Printf("berth %s · %s · pool %s of %s charged · %g of %g CPUs", st.Version, st.Backend, b(p.MemoryCharged), b(p.Memory), p.CPUsCharged, p.CPUs)
	for name, total := range p.Resources {
		fmt.Printf(" · %s %d/%d", name, p.ResourcesInUse[name], total)
	}
	fmt.Println()
	fmt.Printf("fits now: --memory %s --cpus %g (default %s / %g)\n", b(p.FitsNowMemory), p.FitsNowCPUs, b(p.DefaultMemory), p.DefaultCPUs)
	var host []string
	if h.MemTotal > 0 {
		host = append(host, fmt.Sprintf("%s of %s memory available", b(h.MemAvailable), b(h.MemTotal)))
	}
	if h.PressureFull > 0.5 {
		host = append(host, fmt.Sprintf("PSI full %.1f%%", h.PressureFull))
	}
	for _, d := range h.Disks {
		host = append(host, fmt.Sprintf("%s %s free", d.Path, b(d.Free)))
	}
	for _, bt := range h.Btrfs {
		host = append(host, fmt.Sprintf("btrfs unallocated %s", b(bt.Unallocated)))
	}
	if len(host) > 0 {
		fmt.Println("host:", strings.Join(host, " · "))
	}
	fmt.Println("admission:", st.Admission)
	if l := st.Logs; l.Budget > 0 {
		fmt.Printf("logs: %s of %s budget (compressed; up to %s per job; finished jobs kept %s)\n",
			b(l.Bytes), b(l.Budget), b(l.PerJob), units.FormatDuration(l.Retention))
	}
	if m := st.Maintenance; m != nil {
		if m.Running {
			fmt.Println("maintenance: running")
		} else {
			fmt.Printf("maintenance: %s (%s ago)\n", m.Summary, units.FormatDuration(time.Since(m.Finished)))
		}
	}
	var shown []job.Job
	for _, j := range st.Jobs {
		if all || !j.State.Terminal() {
			shown = append(shown, j)
		}
	}
	if len(shown) == 0 {
		if !all {
			fmt.Println("\nno active jobs (--all shows finished ones)")
		}
		return
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tFOR\tMEM\tCPU\tKIND\tNAME")
	now := time.Now()
	for _, j := range shown {
		age := now.Sub(j.Created)
		switch {
		case j.State.Active():
			age = now.Sub(j.Started)
		case j.State.Terminal():
			age = j.Finished.Sub(j.Started)
			if j.Started.IsZero() {
				age = 0
			}
		}
		state := string(j.State)
		if j.State.Terminal() && j.Result != nil && j.Result.ExitCode != 0 {
			state += fmt.Sprintf("(%d)", j.Result.ExitCode)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%g\t%s\t%s\n", job.Short(j.ID), state, units.FormatDuration(age), b(j.Spec.Memory), j.Spec.CPUs, j.Spec.Kind, j.Label())
	}
	w.Flush()
	for _, j := range shown {
		if j.State == job.Queued {
			fmt.Printf("  %s waits: %s\n", job.Short(j.ID), client.Explain(&j))
		} else if j.State.Terminal() && j.Note != "" && j.State != job.Succeeded {
			fmt.Printf("  %s %s: %s\n", job.Short(j.ID), j.State, strings.TrimPrefix(j.Note, string(j.State)+": "))
		}
	}
}

func printJob(j *job.Job) {
	fmt.Printf("job %s  %s\n", j.ID, j.State)
	fmt.Printf("  command   %s\n", strings.Join(j.Spec.Argv, " "))
	fmt.Printf("  cwd       %s\n", j.Spec.Cwd)
	fmt.Printf("  size      %s memory, %g CPUs, %s disk budget, %s", b(j.Spec.Memory), j.Spec.CPUs, b(j.Spec.Disk), j.Spec.Kind)
	for k, v := range j.Spec.Resources {
		fmt.Printf(", %s=%d", k, v)
	}
	fmt.Println()
	if j.Spec.Group != "" {
		fmt.Printf("  group     %s\n", j.Spec.Group)
	}
	if len(j.Spec.After)+len(j.Spec.AfterAny) > 0 {
		fmt.Printf("  after     %s\n", strings.Join(append(j.Spec.After, j.Spec.AfterAny...), " "))
	}
	if j.Spec.Timeout > 0 || j.Spec.QueueTimeout > 0 {
		fmt.Printf("  limits    timeout %s, queue timeout %s\n", orNone(j.Spec.Timeout), orNone(j.Spec.QueueTimeout))
	}
	fmt.Printf("  created   %s\n", j.Created.Format(time.RFC3339))
	if !j.Started.IsZero() {
		fmt.Printf("  started   %s (waited %s)\n", j.Started.Format(time.RFC3339), units.FormatDuration(j.Started.Sub(j.Created)))
	}
	if !j.Finished.IsZero() {
		fmt.Printf("  finished  %s\n", j.Finished.Format(time.RFC3339))
	}
	if j.PeakBytes > 0 {
		fmt.Printf("  peak      %s working memory\n", b(j.PeakBytes))
	}
	if j.Owner != nil {
		fmt.Printf("  bound to  pid %d (cancelled if it exits)\n", j.Owner.PID)
	}
	if j.Result != nil {
		fmt.Printf("  exit      %d", j.Result.ExitCode)
		if j.Result.Signal != "" {
			fmt.Printf(" (%s)", j.Result.Signal)
		}
		fmt.Println()
	}
	if j.Note != "" {
		fmt.Printf("  note      %s\n", j.Note)
	}
	if j.LogsGone != "" {
		fmt.Printf("  logs      %s\n", j.LogsGone)
	}
	for _, r := range j.Reasons {
		fmt.Printf("  waits     [%s] %s\n", r.Code, r.Text)
	}
	if j.Hint != "" {
		fmt.Printf("  hint      %s\n", j.Hint)
	}
}

func orNone(d time.Duration) string {
	if d == 0 {
		return "none"
	}
	return units.FormatDuration(d)
}
