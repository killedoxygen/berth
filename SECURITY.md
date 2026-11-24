# Security policy

## Reporting

Report vulnerabilities privately through GitHub's security advisories:
https://github.com/killedoxygen/berth/security/advisories/new. Please do not open
public issues for them. Expect an acknowledgement within a week.

## Model

- **User mode** (default): the daemon runs as you and its socket is mode 0600; peer
  credentials reject any other user. It can do nothing you could not do yourself.
- **System mode**: a root daemon. Any local user may submit; each job runs as its
  submitter (systemd `--uid/--gid`), in the login SELinux context where SELinux is
  enforcing. Users can read, cancel and change only their own jobs; status hides
  other users' commands. Each user's job logs live in a directory they own.
- Job environments are stored with mode 0600 and never returned by the API.
  Passthrough descriptors are handed only to the supervisor holding the job's
  random token.
- Job output is retained according to `[logs]` (30 days by default); anything a job
  prints may be stored. Avoid printing secrets.
- Releases are built by GitHub Actions from tagged commits; `install.sh` verifies
  the SHA-256 checksum of the downloaded archive.

Out of scope: a user who can already run arbitrary code as root, and resource use
outside berth (berth bounds what it runs; it does not police the rest of the host).
