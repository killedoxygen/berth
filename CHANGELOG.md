# Changelog

## v0.3.0

Multi-user (system mode) hardening, after a security review:

- Log handling as root never follows symlinks or reads files the user does not
  own; files root writes in user directories are created exclusively. (Before,
  a user could make the root daemon read any file through a planted symlink.)
- Users reach only their own jobs (get, logs, cancel, update, wait, `--after`);
  status shows others' jobs as state and size only.
- Groups are per user unless prefixed `global:`.
- `--service` is root-only unless `users.allow_services`.
- Per-user limits: `users.max_jobs`, `users.max_connections`, `users.max_share`;
  waiting users are interleaved; log eviction targets the heaviest user first.
- Jobs run in each user's own SELinux login context instead of a fixed one.
- The system service is sandboxed (NoNewPrivileges, ProtectSystem, private
  /tmp and devices, restricted namespaces); the socket directory stays
  reachable under its restrictive umask.
- The passthrough claim checks the caller's uid; labels lose control characters.

## v0.2.0

- Job logs are segmented and gzip-compressed (4M segments; finished logs fully
  compressed), capped per job (64M, keeping the start and the end with a marker),
  and capped in total (oldest finished jobs' logs go first).
- Retention: finished jobs and their logs expire after 30 days; `berth prune`
  (`--older-than`, `--dry-run`) applies it on demand. `berth status` and
  `berth doctor` report log usage.
- The daemon writes a size-rotated, compressed log (`--log-file`) when not under
  journald (launchd, hosts without systemd).
- Logs from v0.1 are converted on daemon start.
- `docs/operations.md`: install, change, upgrade, monitoring, retention and
  incident procedures. `SECURITY.md`.

## v0.1.1

- macOS: available memory and memory pressure come from the kernel's own
  memorystatus figures.

## v0.1.0

- First release.
