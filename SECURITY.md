# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](../../security/advisories/new) (the
**Security** tab of this repository), not in a public issue. Include the
version or commit, your platform, and steps to reproduce.

The project is pre-release; fixes go to `main`.

## What an agent trusts

`pool agent` runs commands that other devices send it. **Any device it allows
can run anything, as the user the agent runs as**: shell mode, plain programs,
shipped executables and environment variables are all equivalent for that
purpose. Everything below is about deciding who is allowed, and about what a
client trusts from an agent.

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

## Known limits

- **Other programs on the agent's machine count as that device.** A process
  running locally can connect to the agent's own Tailscale address and is
  identified as the agent's device, which is always allowed. On a shared
  machine, run the agent under a dedicated low-privilege account.
- **Job history is visible to allowed devices.** `/v1/jobs` and the dashboard
  show the first 200 characters of each job's command, and the agent logs
  commands. `/v1/log` serves that log (for `pool report`) to the same allowed
  devices, which could read it anyway by running a command. Pass secrets with
  `--env`, which is never shown or logged, not as arguments.
- **`--shell` with untrusted items is command injection by design.** In
  `pool map`, `{}` is substituted into the command as is.
- **Retries and backup copies can run a task twice.** Use
  `--speculate=false --retries 0` for tasks with side effects beyond their
  output.
- **Background processes.** On Linux and macOS, a job's leftover background
  processes are killed when it finishes, but a SIGKILLed agent leaves running
  jobs behind. On Windows, a job's background processes survive until the
  agent exits; the agent's job object then ends them.
- **The file cache is trusted after it is written.** Entries are verified
  against their SHA-256 when stored, not when reused; anything running as the
  agent's user could alter them.
- **Output is printed as received.** A compromised agent could send terminal
  escape sequences.

## Hardening checklist

- Keep `--allow` to the devices that need to submit jobs.
- Don't use `--listen`.
- Restrict port 7470 in your Tailscale ACLs to your own devices, for example:
  ```json
  {"action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self:7470"]}
  ```
- Run agents under an account with only the access the jobs need.
- Build binaries you share with `go build -trimpath`.
