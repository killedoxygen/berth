# Changelog

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
