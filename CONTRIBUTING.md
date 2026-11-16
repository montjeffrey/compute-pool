# Contributing to compute-pool

Thanks for looking at compute-pool. This guide covers how to run the checks
locally and what a pull request should look like. Please read the
[Code of Conduct](CODE_OF_CONDUCT.md) before participating.

## Supported versions

`go.mod` declares `go 1.24` and pins `toolchain go1.27.3`. CI
(`.github/workflows/ci.yml`) takes its Go version from `go.mod`, so it builds
with go1.27.3, on Ubuntu, Windows and macOS. Windows is the most tested
platform; Linux and macOS are covered by CI.

## Build and test

These are the commands the CI `test` job runs:

```bash
go mod verify
gofmt -l .
go vet ./...
go test -race ./...          # Linux
go test ./...                # Windows and macOS
go build -trimpath -o pool-bin ./cmd/pool
```

CI fails if `gofmt -l .` prints any file, so format before you push.

CI also runs `govulncheck`, a `gitleaks` scan over the full history, and
`zizmor` over `.github`. If you edit a workflow, keep third-party actions pinned
to a commit SHA and keep `permissions` minimal.

## Pull requests

- Branch from `main` and open a pull request against `main`.
- Keep each pull request small and focused on one change. Unrelated cleanups
  belong in their own pull request.
- Add or update tests for any behavior change or bug fix.
- Update the README or `docs/` when user-visible behavior changes. Changes to
  what an allowed device can do need a matching update to the threat model in
  SECURITY.md.
- CI must pass before a pull request is merged.
- Use clear commit messages. Conventional prefixes such as `feat:`, `fix:`,
  `docs:` and `ci:` are welcome.
- Explain what changed and why in the pull request description.

## Reporting bugs and requesting features

Use the issue forms under the Issues tab. Include the output of `pool version`
and the operating system of each device involved. Do not paste tailnet
addresses or secrets you do not want public.

## Security

Do not report security problems in a public issue or pull request. Follow
[SECURITY.md](SECURITY.md), which uses GitHub's private vulnerability
reporting.
