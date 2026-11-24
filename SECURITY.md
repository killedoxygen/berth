# Security policy

## Reporting

Report vulnerabilities privately through GitHub's security advisories:
https://github.com/killedoxygen/berth/security/advisories/new. Please do not open
public issues for them. Expect an acknowledgement within a week.

## Model

**User mode** (default): the daemon runs as you and its socket is mode 0600;
peer credentials reject any other user. It can do nothing you could not do
yourself.

**System mode** (`berth setup --system`): one root daemon for a shared machine.
The threat considered is a local user trying to gain privileges, see or affect
other users' work, or deny service.

- Identity comes from the socket's peer credentials, never from the request.
  Every job runs as its submitter (systemd `--uid/--gid` with the user's groups).
- Isolation: a user can see, follow, read logs of, cancel, change and depend
  on (`--after`) only their own jobs; other users' jobs are not found by id.
  The status listing shows other users' jobs as state and size only (no
  command, directory, name, notes or reasons).
- Locks: `--group` names are per user (`u<uid>:name`), so no one can hold
  another user's lock; a `global:` prefix opts into a deliberately shared one.
- Services (`--service`, charged by use and so overcommitting) are reserved for
  root unless `users.allow_services`.
- Limits per user: `users.max_jobs` unfinished jobs, `users.max_connections`
  open connections, `users.max_share` of the pool's memory; waiting users are
  interleaved so no one jumps the queue by submitting many jobs. When logs
  exceed their budget, the heaviest user's oldest logs go first.
- Files: job records and environments are root-only (0600). Each user's logs
  are in a directory they own. The daemon never follows symlinks there and
  only reads regular files owned by that user; files it writes there are
  created exclusively under random names and handed to the user, so nothing a
  user plants can redirect root's reads or writes.
- SELinux: each job runs in its user's own default login context from the host
  policy (`selinuxdefcon`), never a broader one; if it cannot be determined, no
  context is forced. `backend.selinux_context` overrides it for everyone.
- Passthrough descriptors go only to the supervisor of that job, which must
  present the job's random token and run as the job's user.
- The daemon's service runs with `NoNewPrivileges`, read-only `/usr`, `/boot`
  and `/etc`, private `/tmp` and devices, no kernel module or clock access, and
  restricted namespaces. Labels are stripped of control characters before they
  reach logs or unit descriptions.

Everywhere:

- Job environments are never returned by the API. Job output is retained per
  `[logs]` (30 days by default); anything a job prints may be stored.
- Releases are built by GitHub Actions from tagged commits; `install.sh`
  verifies the SHA-256 checksum of the downloaded archive.

Out of scope: a user who can already run arbitrary code as root, and resource use
outside berth (berth bounds what it runs; it does not police the rest of the host).
