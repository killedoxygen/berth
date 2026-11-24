// Package maint reclaims Btrfs unallocated space with small, bounded balances.
//
// Btrfs can report plenty of free space while having no unallocated chunks
// left, and then fail metadata allocations. Each pass relocates only a few
// low-usage data chunks at a time under a deadline, stops at the target, and
// cancels any balance that overruns. It never runs a full balance and is safe
// to run while jobs are active.
package maint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

type step struct {
	usage int
	limit int
}

// Steps go from free (empty chunks) to progressively fuller chunks.
var steps = []step{{0, 0}, {10, 4}, {25, 4}, {40, 4}, {55, 2}}

// Balance reclaims unallocated space on the filesystem holding path until target or budget.
func Balance(path string, target int64, budget time.Duration, log func(string)) (string, error) {
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("btrfs balance needs root (run `sudo berth maint`, or use a system daemon)")
	}
	btrfs, err := exec.LookPath("btrfs")
	if err != nil {
		return "", fmt.Errorf("btrfs-progs not installed")
	}
	before, ok := unallocated(path)
	if !ok {
		return "", fmt.Errorf("%s is not on btrfs", path)
	}
	if status, err := output(30*time.Second, btrfs, "balance", "status", path); err != nil || !strings.HasPrefix(status, "No balance found") {
		return "another balance is running; not starting one: " + strings.TrimSpace(status), nil
	}
	deadline := time.Now().Add(budget)
	current := before
	var actions []string
	for _, s := range steps {
		if s.usage > 0 && current >= target {
			break
		}
		remaining := time.Until(deadline)
		if remaining < 20*time.Second {
			actions = append(actions, "time budget reached")
			break
		}
		filter := fmt.Sprintf("-dusage=%d", s.usage)
		if s.limit > 0 {
			filter += fmt.Sprintf(",limit=%d", s.limit)
		}
		stepBudget := min(remaining, 2*time.Minute)
		started := time.Now()
		_, err := output(stepBudget, btrfs, "balance", "start", filter, path)
		if err != nil {
			if strings.Contains(err.Error(), "timed out") {
				// The relocation continues in the kernel after its client exits; stop it.
				_, _ = output(90*time.Second, btrfs, "balance", "cancel", path)
				actions = append(actions, filter+": exceeded "+units.FormatDuration(stepBudget)+", cancelled")
			} else {
				actions = append(actions, filter+": "+err.Error())
			}
			break
		}
		current, _ = unallocated(path)
		actions = append(actions, fmt.Sprintf("%s in %s", filter, units.FormatDuration(time.Since(started))))
		if log != nil {
			log(fmt.Sprintf("btrfs %s: %s unallocated after %s", path, units.FormatBytes(current), filter))
		}
	}
	return fmt.Sprintf("btrfs %s: unallocated %s -> %s (target %s; %s)", path, units.FormatBytes(before),
		units.FormatBytes(current), units.FormatBytes(target), strings.Join(actions, ", ")), nil
}

func unallocated(path string) (int64, bool) {
	h := probe.Snapshot([]string{path}, "", nil)
	if len(h.Btrfs) == 0 {
		return 0, false
	}
	return h.Btrfs[0].Unallocated, true
}

func output(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		return string(out), fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
