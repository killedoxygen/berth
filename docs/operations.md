# Operations runbook

Standard procedures for running berth on a host: install, change, upgrade,
monitor, retain data, and respond to incidents. Commands assume a per-user
installation; for a system installation use `sudo`, `systemctl` without
`--user`, and the paths in the table below.

| | User mode | System mode |
|---|---|---|
| Config | `~/.config/berth/config.toml` | `/etc/berth/config.toml` |
| State (jobs, logs, results) | `~/.local/state/berth` (macOS: `~/Library/Application Support/berth`) | `/var/lib/berth` |
| Socket | `$XDG_RUNTIME_DIR/berth.sock` | `/run/berth/berth.sock` |
| Service | `systemctl --user ... berth` / launchd `io.github.killedoxygen.berth` | `systemctl ... berth` |
| Daemon log | journald (`journalctl --user -u berth`), else `<state>/daemon.log` (rotated, compressed) | `journalctl -u berth` |

## 1. Install

1. `curl -fsSL https://raw.githubusercontent.com/killedoxygen/berth/main/install.sh | sh`
   (or `--system` for a shared server). The script verifies the release checksum.
2. Review `berth setup --dry-run` first on hosts you care about: it lists every file
   and service it will touch.
3. Verify: `berth doctor` reports no `FAIL`; `berth run -- true` exits 0.
4. Optional: `sudo berth setup --maint` on Btrfs hosts (bounded balance timer);
   `berth setup --linger` so the user daemon survives logout.

Rollback: `berth uninstall` (add `--purge` to delete config, jobs and logs).

## 2. Change configuration

1. Edit the config file; keep changes minimal (unset keys follow host defaults).
   `berth config --defaults` shows what the host would choose.
2. Validate and apply: `berth setup` (regenerates the pool slice from `[pool]` and
   restarts the daemon). Running jobs are not affected by a daemon restart.
3. Verify: `berth doctor` (the `slice` check confirms the pool cap) and `berth config`.

A misspelled key or invalid value is an error, never silently ignored: `berth setup`
refuses to apply it, and a daemon restarted with a broken file fails to start
(`berth doctor` shows why). Validate with `berth config` before restarting by hand.

## 3. Upgrade

1. Read `CHANGELOG.md` for the target version.
2. Rerun the installer (or replace the binary), then `berth setup` to refresh the
   service definition and restart the daemon.
3. Jobs survive: supervisors run independently of the daemon and their results are
   collected by the new daemon. Clients waiting on jobs reconnect automatically.
4. Verify: `berth doctor` shows the new version for both CLI and daemon.

Rollback: install the previous release the same way (`BERTH_VERSION=v0.1.1`).
Job records stay readable both ways. v0.2 converts v0.1 logs on start; after a
rollback to v0.1, logs written by v0.2 are not shown by `berth logs` (the
`.log.gz` files remain readable with `zcat`).

## 4. Monitoring

Poll these from any monitoring system (all are cheap, JSON, stable fields):

| Signal | Command | Alert when |
|---|---|---|
| Health | `berth doctor --json` | exit status 1, or any check `fail` |
| Admission | `berth status --json` → `.admission` | not `"ready"` while jobs wait, for over 30 minutes |
| Stuck jobs | `berth status --json` → `.jobs[] \| select(.state=="queued")` | `created` older than 1 hour (doctor also warns) |
| Disk | `.health.disks[].free` | below `safety.min_free_disk` (new work is held) |
| Btrfs | `.health.btrfs[].unallocated` | below `safety.btrfs_min_unallocated` |
| Logs | `.logs.bytes` vs `.logs.budget` | above 90% (doctor warns) |
| Failures | daemon log lines `failed`, `lost`, `emergency stop` | rate change |

Daemon log lines are one per event: `submit`, `hold` (with every reason, logged
when the reasons change), `start` (with time queued), `succeeded`/`failed`/
`cancelled`/`lost` (with runtime and reason), `retry`, `emergency stop`,
`memory pressure`, `retention`, `prune`, `btrfs`.

## 5. Data retention and logs

Policy (defaults; all in `[logs]`):

- Job output is stored in 4M segments, each gzip-compressed when full; a finished
  job's log is fully compressed.
- Each job keeps at most 64M compressed. Beyond that the earliest output after the
  first segment is dropped: the start and the end of the output are kept, with a
  marker where output was removed.
- All job logs together stay within `max_total` (default: 0.5% of the disk, 256M–4G).
  When exceeded, the oldest finished jobs' logs are removed first; their records
  remain, marked with why.
- Finished jobs (records, logs, results) are deleted after `retention` (30 days),
  and beyond `scheduler.keep_finished` records (2000).
- Retention runs every 10 minutes. Active jobs are never touched.
- `berth run` (passthrough) output goes to the caller's terminal and is not stored.
- The daemon's own log: journald when available (journald's own limits apply);
  otherwise `daemon.log`, rotated at 10M, 5 compressed generations.

Procedures:

- Preview: `berth prune --dry-run` (or `--older-than 7d --dry-run`).
- Free space now: `berth prune --older-than 7d`.
- Keep a log before it expires: `berth logs ID > file` (it is decompressed on read).
- Sensitive output: logs may contain whatever jobs print. Shorten `retention` on
  hosts that handle secrets, and prefer credential files to printing tokens.

What to back up: only the config file. Job state is operational and expires.

## 6. Incident procedures

**A job does not start.** `berth status ID` lists every reason and a hint.
- `pool memory` / `pool CPUs` / `gpu`: capacity held by other jobs; it starts when
  they finish. Smaller requests start sooner (the hint gives the size that fits now).
- `host memory` / `enclosing cgroup limit`: the machine itself is short; find the
  consumer outside berth before changing berth.
- `disk` / `btrfs`: free space (section 5) or run `sudo berth maint`.
- `unsatisfiable`: the request cannot fit while services run; lower it or stop a service.
- `group` / `dependency`: by design; check the job it names.
Do not raise limits merely to force a job through.

**The daemon is down.** `berth doctor` says so. Restart the service; check its log
for the cause. Running jobs continue meanwhile and their results are collected
after the restart; `berth run` callers wait up to 10 minutes for the daemon.

**Disk emergency** (free space below `emergency_free_disk`). Admission stops and the
newest batch job is stopped every 15 seconds until space recovers. Free space
(`berth prune`, regenerable caches), then confirm `admission: ready`.

**Memory pressure pause.** Admission resumes by itself after pressure stays low for
`pressure_hold`. If it persists, find the process outside berth that holds memory.

**A job was killed for memory** (`out of memory` note, exit 137). Rerun with a larger
`--memory`, or reduce its parallelism. On macOS and the process backend, limits are
checked every second, so very fast growth can briefly exceed the cap.

**A job is `lost`.** Its supervisor vanished without a result (host restart or an
external kill). Jobs submitted with `--retries` are requeued automatically.

**Btrfs unallocated space low.** `sudo berth maint --path /` runs bounded balances
(safe while jobs run). Never run a full balance on a busy host.

**Runaway or abandoned work.** `berth cancel ID`. Jobs from `berth run`, shims and
MCP are cancelled automatically when their submitter exits.

**Maintenance window.** `berth pause` (running jobs continue, nothing new starts),
do the work, `berth resume`.

## 7. Decommission

`berth uninstall --purge` (system: `sudo berth uninstall --system --purge`), and
`sudo systemctl disable --now berth-maint.timer` if installed.
