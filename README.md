# berth

**One machine, many jobs and agents, no OOM, no full disk, no silent waits.**

berth is a workload queue for a single host. Builds, test suites, containers,
model servers and large git operations ask for a berth (memory, CPUs, disk,
GPUs) and run when it is free, each under a hard limit. Everyone sharing the
machine (you, your scripts, a fleet of coding agents) gets the same guarantees:

- **The host stays up.** Every job is capped (cgroup v2 where available), the
  pool as a whole is capped, and admission keeps free memory, disk and Btrfs
  chunk space above floors. Under real memory pressure, admission pauses.
- **Nothing stalls without saying why.** A waiting job always carries its
  reasons ("pool memory: needs 8G, 3G of 48G free") and, where possible, the
  fix ("would start now with --memory 3G or less").
- **Nothing waits forever.** Small jobs backfill past big ones; big jobs age
  into priority so they are not starved; a request that can never fit is
  rejected or abandoned at once; queue timeouts and dependency failures cancel
  cleanly; a job whose submitter dies is cancelled.
- **It behaves like running the command yourself.** `berth run` hands the job
  your real stdin, stdout and stderr, and exits with the job's exit status.

It is one static binary for Linux and macOS (amd64, arm64), needs no root by
default, and sets itself up with one command.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/killedoxygen/berth/main/install.sh | sh
```

or, with Go: `go install github.com/killedoxygen/berth/cmd/berth@latest && berth setup`.

`berth setup` sizes a policy from the machine (memory, CPUs, GPUs, disk,
enclosing cgroup limits), installs a per-user service (systemd user unit or
launchd agent), creates tool shims, and puts them on `PATH` for your shells
and user services. `berth setup --dry-run` shows every change first;
`berth uninstall` reverts exactly what setup did. `berth doctor` checks the
result and prints the fix for anything wrong.

For a shared multi-user server, `sudo berth setup --system` installs one
root daemon instead: every user submits to it and jobs run as the submitter.

## Use

```sh
berth run -- cargo build --release              # waits its turn, then runs with your terminal
berth run -m 4G -c 2 -t 30m -- make test        # 4G memory, 2 CPUs, 30 minute deadline
berth submit --name e2e -- ./e2e.sh             # background; prints the job id
berth wait 3f2a                                  # stream its output, exit with its status
berth submit --after 3f2a -- ./deploy.sh        # run only if 3f2a succeeds
berth run --gpus 1 -- python train.py           # counted resources (GPUs are detected)
berth submit --service -m 32G -- ./model-server # long-running: charged by real use once settled
berth status                                     # capacity, what fits now, why each job waits
```

With shims installed, `cargo build`, `make`, `ninja`, `cmake --build`,
`go test`, `docker run` / `docker build` and heavy git commands in large
repositories go through the queue automatically. Light and interactive
invocations (`cargo metadata`, `make -n`, `git log`, `docker ps`,
`docker run -it`) run directly. `berth shims explain docker run img` shows the
decision for any command line.

```
$ berth status
berth v0.1.0 · systemd-user · pool 18G of 48G charged · 10 of 22 CPUs · gpu 1/1
fits now: --memory 11.6G --cpus 12 (default 4G / 4)
host: 43.2G of 62.7G memory available · /home/me 45.8G free · btrfs unallocated 17G
admission: 1 waiting; next 17264918: gpu: needs 1, 0 of 1 free

ID        STATE    FOR  MEM  CPU  KIND     NAME
02c71a9e  running  2s   6G   2    service  model-server
2b4eced5  running  2s   4G   4    batch    cargo build --release
6fc2e8c5  running  2s   8G   4    batch    python train.py
17264918  queued   2s   2G   4    batch    eval
  17264918 waits: gpu: needs 1, 0 of 1 free
```

## For AI agents

Coding agents are berth's main users: many of them, each convinced it owns the
machine. berth gives them:

- **An MCP server**: `berth mcp`. Tools: `berth_run` (run and wait, returns
  exit code, output tail, and why it waited), `berth_submit`, `berth_wait`,
  `berth_status`, `berth_logs`, `berth_cancel`. Jobs are cancelled when the
  agent's session ends.

  ```sh
  claude mcp add berth -- berth mcp          # Claude Code
  codex mcp add berth -- berth mcp           # Codex
  ```
  Any other MCP host: command `berth`, arguments `["mcp"]`.

- **Instructions** to paste into `AGENTS.md` / `CLAUDE.md`: `berth agents`.
- **Transparent shims**, so agents that never heard of berth still go through it.
- **Machine-readable everything**: `--json` on `status`, `submit`, `wait`,
  `doctor`; stable reason codes; documented exit codes.
- **Idempotent submission** with `--key`: a retried tool call reuses the
  unfinished job instead of starting a duplicate.
- **Fail fast**: impossible requests are rejected at submit with the largest
  size that could work; `berth run` gives up after 20s (`--give-up`) if the job
  cannot start while long-running services hold the memory.

## How it decides

| Rule | What it means |
|---|---|
| Reservations | A batch job holds its `--memory`/`--cpus`/resources while active. Memory is also its hard cap. |
| Services | `--service` jobs hold their full reservation for 10 minutes, then are charged by their peak use over the last hour (at least 25% of the reservation). |
| Order | Higher `--priority` first, then oldest. |
| Backfill | A job that does not fit never blocks later jobs that do. |
| Aging | A job held for capacity for 10 minutes stops being overtaken, except by short jobs (`--timeout` of 30 minutes or less) and higher priority ones. |
| Groups | `--group NAME`: at most one active job per group (one build per repository, one job per device). |
| Dependencies | `--after ID` starts after ID succeeds; if it fails, the job is cancelled. `--after-any ID` starts after ID finishes. |
| Safety | Free disk on watched paths, Btrfs unallocated space, host available memory and enclosing cgroup limits are all checked; every failing check is reported, not just the first. |
| Pressure | Sustained memory pressure (PSI) with little memory left pauses admission until it is calm again. |
| Emergency | Below the emergency disk floor, the newest batch job is stopped, one at a time. |
| Submitter | `berth run` and shim jobs are bound to their caller: if it exits, the job is cancelled (queued or running). |
| Recovery | Jobs outlive daemon restarts; results are written by each job's supervisor and collected later. `--retries N` requeues jobs lost to a host restart or emergency stop. |

## Exit status

`berth run` and `berth wait` exit with the job's own status, and otherwise:

| Code | Meaning |
|---|---|
| 75 | the job never started: queue timeout, could not fit, dependency failed, cancelled first |
| 124 | the job hit its `--timeout` |
| 125 | berth itself failed (daemon unreachable, invalid request) |
| 130 | you cancelled it (Ctrl-C) |
| 128+N | the job was killed by signal N (137: out of memory or SIGKILL) |

## Configuration

Every setting has a default derived from the machine; `berth config` prints
the effective values and `berth config --defaults` what was detected. Override
only what you need in `~/.config/berth/config.toml` (system mode:
`/etc/berth/config.toml`):

```toml
[pool]
memory = "48G"          # all jobs together (also the slice's hard cap)
cpus = 22
resources = { gpu = 1, license = 4 }

[defaults]
memory = "4G"
cpus = 2

[safety]
min_free_disk = "16G"
watch_paths = ["~", "/var/lib/docker"]

[[shims.rules]]          # route more tools
tool = "npm"
subcommands = ["ci", "test", "run"]
memory = "3G"
```

Rerun `berth setup` after changing the pool so the service's cgroup slice
matches.

## Platforms

| Platform | Backend | Limits |
|---|---|---|
| Linux with systemd (user manager) | `systemd` | kernel-enforced per job and for the pool (cgroup v2) |
| Linux, system daemon | `systemd` (system manager) | same; jobs run as the submitting user |
| macOS | `process` | memory enforced by the supervisor polling resident memory |
| Linux without systemd, containers | `process` | same as macOS |

Btrfs is understood natively: berth reads chunk allocation from sysfs, refuses
to start jobs when unallocated space runs out, and a root daemon (or
`sudo berth maint`) reclaims it with small, bounded balances that are safe
while jobs run.

## Security model

A user daemon accepts only its own user (peer credentials on the socket). A
system daemon accepts everyone, runs each job as its submitter, lets users
act only on their own jobs, and hides other users' command lines. Job
environments are stored with mode 0600 and never returned by the API.

## Development

```sh
go test ./...                 # unit tests and an end-to-end suite against a real daemon
go build -o berth ./cmd/berth
```

See [docs/design.md](docs/design.md) for the architecture.

## License

MIT
