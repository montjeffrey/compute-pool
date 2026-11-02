# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](../../security/advisories/new) (the
**Security** tab of this repository), not in a public issue. Include the
version or commit, your platform, and steps to reproduce.

The project is pre-release; fixes go to `main`.

## Threat model

`pool agent` runs commands that other devices send it. **Any device it allows
can run anything, as the user the agent runs as**: shell mode, plain programs,
shipped executables and environment variables are all equivalent for that
purpose. The design therefore rests on deciding who is allowed, and on limiting
what an allowed job can reach.

What is worth protecting:

1. The account, files and credentials of whoever runs the agent.
2. The agent itself: its program file, its startup task and its `--allow` list.
3. Job results, and the files cached for later jobs.
4. Secrets passed with `--env`.
5. Other devices on the tailnet.
6. The release binaries and the workflow that builds them.

Who it defends against, and where that defense stops today:

| Adversary | What stops them | Where it stops |
|---|---|---|
| A device on your tailnet that isn't yours (another user's, shared in, or tagged) | `tailscale whois` must report your user **and** a device named in `--allow` | A Tailscale ACL or grant on port 7470 adds a network-level layer (see the README's "Secure deployment") |
| A device you allowed, compromised | Nothing: it can run anything as the agent's user, **by design** | Run agents under an account with only the access jobs need |
| Hostile code inside a legitimate job (a poisoned dependency, say) | Jobs run in a fresh scratch folder at below-normal priority | Same account as the agent, so it can reach everything that account can; see "Known limits" for the cache, other jobs' folders, the agent's program and log, leftover processes and other agents |
| Another local user, on the agent's machine or on any device in `--allow` | Nothing yet | Identity is per device: any process on an allowed device is that device, and the agent's own device is always allowed |
| A web page open in a browser on any device | `Origin`, `Sec-Fetch-Site`, JSON content type and `Host` checks | — |
| A compromised dependency, GitHub Action or release workflow | Actions pinned to commit SHAs; in CI (not in the release workflow): `go mod verify`, `govulncheck`, secret scanning and workflow linting (zizmor); CodeQL and OpenSSF Scorecard reports in code scanning; Dependabot waits 7 days before proposing a new release; build provenance on releases | Branch and tag protection, signed binaries and an SBOM are planned. There is one maintainer, so no change is reviewed by a second person (Scorecard's Code-Review check stays low) |
| A compromised agent, seen from the client | Returned files are checked as hostile (below) | Output is printed as received |

Out of scope: an administrator on the agent's machine, physical access, and a
compromised Tailscale client.

### Root of trust

Identity is whatever the Tailscale control plane says `whois` is. Whoever
controls your Tailscale account (or its identity provider) can add a device
that passes every check here. To narrow that:

- turn on [Tailnet Lock](https://tailscale.com/kb/1226/tailnet-lock), so new
  devices need a signature from a device you already trust;
- turn on device approval, and leave key expiry on;
- use MFA on the identity provider you sign in to Tailscale with.

## What an agent trusts

A request is accepted only when all of these hold:

1. It arrives on the device's Tailscale address. The agent binds only there
   unless `--listen` is given, which logs a warning.
2. Its source address is in Tailscale's ranges (`100.64.0.0/10`,
   `fd7a:115c:a1e0::/48`); anything else is refused before any lookup.
3. `tailscale whois` reports a device owned by **the same Tailscale user** as
   the agent's device, and that device is **named in `--allow`** (the agent's
   own device is always allowed). Logins are compared exactly apart from ASCII
   case. The agent refuses to start on a tagged device, where "same owner"
   would mean every tagged device.
4. It is not from a web browser: requests with an `Origin` header or a
   cross-site `Sec-Fetch-Site` are refused, jobs must be sent as
   `application/json` (a page cannot send that without a CORS preflight, which
   the agent never answers), and the `Host` header must name the agent, which
   stops DNS rebinding.

Lookups are cached for one minute, so removing a device from your tailnet takes
up to a minute to take effect on agents that saw it recently.

## What a client trusts

`pool run`, `pool map` and `pool dashboard` only contact devices owned by the
same Tailscale user, never devices shared into the tailnet, owned by other
users, or tagged. Jobs, shipped files and `--env` values therefore never reach
a device someone else controls. A raw IP passed to `--node` must still be one of
your devices.

What an agent sends back is checked as if it were hostile: returned file paths
must stay inside the output folder (no `..`, absolute paths, drive letters,
Windows device names or control characters), are never written through an
existing symbolic link, may not repeat, and are capped at 256 MiB and 10,000
files per job. `pool map` keeps at most 16 MiB each of a task's stdout and
stderr.

## Release integrity

Release binaries are built by the release workflow from the tagged commit, with
`-trimpath`. Each release carries:

- `SHA256SUMS` for every binary;
- a build provenance attestation per binary (releases after v0.1.0), checked
  with `gh attestation verify <file> --repo montjeffrey/compute-pool`;
- a signed tag (v0.1.1 and later; the v0.1.0 tag and the first commits are
  unsigned, and signing isn't enforced).

Binaries are **not code-signed yet**. On Windows, Smart App Control may refuse
to run them, and SmartScreen may warn.

## Known limits

- **Identity is per device, not per user.** Any process on an allowed device,
  whichever local user started it, is that device. That includes the agent's
  own machine, which is always allowed: a process there, including one started
  by another logged-on user, runs jobs as the agent's user. Don't run an agent,
  or list a device in `--allow`, where people you don't trust are logged on.
  The local dashboard likewise shows every agent's job history to any local
  user.
- **A job can submit jobs to other agents, and to its own.** A job runs on its
  agent's device, so to every agent that allows that device, its own included,
  it *is* that device. If two agents allow each other, hostile code that
  reaches one reaches both. Keep allow lists pointing one way: workers allow
  only the machines that submit jobs, never each other.
- **Jobs run as the agent's user, with its environment.** They can read
  everything that user can, and they inherit the agent's environment
  variables. On Windows, `pool install` runs the agent as you.
- **Running jobs share one account.** A hostile job can read or change other
  jobs' scratch folders, including their inputs and their outputs before they
  are collected, and can read their environment.
- **A job can change the agent's program and log.** On Windows, `pool install`
  puts `pool.exe` and `agent.log` under `%LOCALAPPDATA%`, which the agent's
  user (and so every job) can write to; a replaced program runs the next time
  the agent starts. On Linux and macOS the binary and the service file you set
  up are normally writable by the same user. A job can also set up its own
  persistence as that user (scheduled tasks, startup entries, cron).
- **On Windows, `pool install` trusts `%LOCALAPPDATA%`.** The task runs as the
  account whose token runs `pool install`, but the program and log go wherever
  that shell's `LOCALAPPDATA` points. If the two belong to different accounts
  (an environment carried over by `runas /env`, a remote session, or a script
  that sets it), the agent runs as one account from a folder the other can
  write, and that other account can then run code as the agent's account.
  Install from a shell that is logged on as the account the agent should run
  as, and check the path `pool install` prints.
- **On Windows, `pool install` from a non-elevated shell runs the agent at
  logon, in your desktop session**, so jobs run there too. From an elevated
  shell it runs at startup, outside any desktop session.
- **Cached files are not all re-checked when reused.** Every cached file is
  verified against its SHA-256 when stored. When it is reused, only a shipped
  file that a non-shell job runs directly (its first argument) is re-hashed.
  Scripts run through an interpreter, and every file in a `--shell` job, are
  copied as they are. Anything running as the agent's user, including another
  job, can change a cached file, and the next job that uses it gets the
  changed copy. The program is re-hashed before the job starts, not at the
  instant it starts, so a job running at the same time can still swap it in
  between.
- **Job history is visible to allowed devices.** `/v1/jobs` and the dashboard
  show the first 200 characters of each job's command, and the agent logs
  commands. Pass secrets with `--env`, which is never shown or logged, not as
  arguments. Jobs running at the same time can still read each other's
  environment.
- **`--shell` with untrusted items is command injection by design.** In
  `pool map`, `{}` is substituted into the command as is.
- **Retries and backup copies can run a task twice.** Use
  `--speculate=false --retries 0` for tasks with side effects beyond their
  output.
- **Background processes.** On Linux and macOS, leftover processes in a job's
  process group are killed when it finishes, but one that starts its own
  session escapes, and a SIGKILLed agent leaves running jobs behind. On
  Windows, a job's background processes survive until the agent exits; the
  agent's job object then ends them, if it could be created (a failure is only
  logged).
- **Output is printed as received.** A compromised agent could send terminal
  escape sequences.

## Hardening checklist

The README's [Secure deployment](README.md#secure-deployment) section walks
through these.

- Keep `--allow` to the devices that need to submit jobs, and point it one way.
- Don't use `--listen`.
- Restrict port 7470 in your tailnet policy, at least to your own devices:
  ```json
  {"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self:7470"]}
  ```
  and better, to the workers only (example in the README).
- Turn on Tailnet Lock and keep key expiry on.
- Run agents under an account with only the access the jobs need.
- Build binaries you share with `go build -trimpath`.
