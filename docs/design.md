# Design

berth is one binary with four roles, chosen by how it is invoked:

| Invocation | Role |
|---|---|
| `berth daemon` | owns the queue: schedules, launches, watches, serves the socket |
| `berth __supervise LAUNCH` | the first process of every job |
| `berth <command>` | the CLI, and `berth mcp`, the MCP server |
| `cargo`, `make`, ... (symlinks) | shims that route heavy invocations through the queue |

```
 client (run / submit / mcp / shim) ──unix socket, JSON lines, SCM_RIGHTS──▶ daemon
                                                                              │ sched.Schedule (pure)
                                                                              │ backend.Start
                                                          ┌───────────────────┴──────────────────┐
                                                systemd transient unit                  detached session
                                                (cgroup limits, berth.slice)            (process backend)
                                                          └──────────── berth __supervise ───────┘
                                                                              │ exec, wait, timeout,
                                                                              │ OOM detection, cleanup
                                                                              ▼
                                                                         result file ──▶ daemon reaps it
```

## Daemon

`internal/daemon`. One lock guards all state. Each tick (1s, or at once after
a submit or cancel):

1. **reap**: a job whose supervisor wrote its result file is finished; one
   whose supervisor vanished without a result is `lost` (and retried if it has
   retries).
2. **owners**: bound jobs whose submitting process is gone (pid plus start
   time, so pid reuse is detected) are cancelled after a grace period.
3. **deadlines**: queued jobs past `--queue-timeout` are cancelled with their
   last reason.
4. **measure**: one health snapshot: host memory and PSI, disks, Btrfs
   allocation, enclosing cgroup room, per-job working memory, service peaks.
5. **safety**: pause file, memory-pressure hysteresis, disk emergency floor.
6. **schedule**: `sched.Schedule` returns starts, cancellations (failed
   dependencies) and, for every waiting job, all its reasons and a hint.
7. **launch**: outside the lock, the backend starts each supervisor.

Jobs are files (`state/jobs/<id>.json`, written atomically). Supervisors and
their result files do not depend on the daemon, so a daemon restart (or
upgrade) never touches running jobs.

## Scheduler

`internal/sched` is pure (no I/O), so every rule has a direct test. See the
README's rule table. Two choices worth explaining:

- **Backfill with aging instead of FIFO or pure backfill.** Strict FIFO lets
  one large waiting job block everything (head-of-line blocking). Pure
  backfill starves large jobs. Aging protects the oldest job held for
  capacity once it has waited long enough, while still letting short jobs
  (those with a bounded timeout) through, because they cannot delay it much.
- **Services are charged by use.** A model server reserving 32G but using 2G
  would otherwise lock most of the pool forever. Services pay full price while
  they settle, then their recent peak (never less than a floor), so a server
  that loads a model on demand is still covered. The kernel cap stays at the
  full reservation, and the pool slice caps everything together.

Requests that can never fit are found two ways: larger than the pool (rejected
at submit, with the largest size that could work), or larger than what
services leave (reported as `unsatisfiable`, which clients give up on).

## Supervisor

`internal/supervise`. It reads its launch file, wires stdio (the job's log
file, or the submitter's own descriptors, claimed from the daemon with a
per-job token), starts the command in its own process group, and then:

- enforces `--timeout` (SIGTERM to the group, SIGKILL after a grace period),
- without cgroups, enforces the memory cap by polling the group's resident memory,
- with cgroups, detects kernel OOM kills from `memory.events`,
- on stop, signals the group and waits; afterwards, kills anything left in it,
- writes the result file (exit code, signal, timeout/OOM flags, reason) and fsyncs it.

The systemd unit uses `KillMode=mixed` so a stop reaches the supervisor
first, and `OOMPolicy=continue` so an OOM kill hits the job's largest process,
not the supervisor that has to report it.

## Passthrough

`berth run` sends its stdin/stdout/stderr descriptors with the submit
request. The daemon holds them until the job starts; the supervisor claims
them and hands them to the command. Output never passes through the daemon,
so pipes, binary data, terminals and exit codes behave exactly as if the
command ran in place. If the daemon restarts while a passthrough job is
queued, the client reattaches and supplies its descriptors again.

## Backends

`internal/backend`: `systemd` (transient units in the pool slice; the user
manager when rootless, the system manager with `--uid` for a system daemon)
and `process` (detached sessions; works anywhere). Both run the same
supervisor, so results, timeouts and OOM reporting are identical.

## Shims

`internal/shim`. Routing is per tool and per subcommand; read-only and
interactive invocations, and everything inside a job, run the real tool
directly. A shim never fails a command because of berth: if the daemon is
unreachable, it runs the tool directly and says so on stderr.

## Protocol

`internal/api`: newline-delimited JSON over a Unix socket; errors carry
stable codes (`not_found`, `invalid`, `denied`, `unavailable`, `internal`).
Peer credentials identify the caller; a user daemon admits only its own
user.
