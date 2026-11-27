# Signing Windows releases with SignPath Foundation

[SignPath Foundation](https://signpath.org) signs open-source releases for
free with a publicly trusted certificate. Windows, including Smart App
Control, accepts binaries signed this way, so a signed `pool.exe` runs on a PC
with Smart App Control on without an allow list of your own. The certificate
is issued to SignPath Foundation, so Windows shows "SignPath Foundation" as
the publisher.

The release workflow (`.github/workflows/release.yml`) is ready for it. Until
the steps below are done it skips signing and publishes unsigned binaries as
before.

Only binaries built by the release workflow get signed. A `pool.exe` you build
yourself stays unsigned; install from a signed release to get one Smart App
Control accepts.

## What SignPath Foundation asks of the project

From its [terms](https://signpath.org/terms):

- An OSI-approved license with no commercial dual licensing (MIT), and no
  proprietary code.
- Actively maintained, already released in the form to be signed, and its
  functionality described where it's downloaded (the README).
- Builds run on GitHub-hosted runners. The release workflow already does.
- Multi-factor authentication for everyone on the team, on both GitHub and
  SignPath.
- A "Code signing policy" on the home page: the attribution line, who may
  change the code, review contributions and approve releases, and a privacy
  statement. It's in the README.
- Every release signing request approved by hand by an approver.

## One-time setup

1. Turn on two-factor authentication for your GitHub account if it isn't
   already.
2. Apply at [signpath.org/apply](https://signpath.org/apply) with the
   repository URL. Wait for acceptance; SignPath sets up an organization and a
   project for you.
3. In SignPath, turn on multi-factor authentication for your account.
4. Trusted build system: under **Trusted Build Systems**, choose **Add
   predefined** > GitHub.com, then link it to the compute-pool project (the
   project's **Trusted build systems** section > **Link**). Optionally install
   the [SignPath GitHub App](https://github.com/apps/signpath) on the
   repository.
5. Artifact configuration: in the project, set the artifact configuration to
   the contents of [`.signpath/artifact-configuration.xml`](../.signpath/artifact-configuration.xml)
   and make it the default.
6. API token: create a token for a user (or CI user) allowed to submit signing
   requests to the project. Create the environment `signpath` (GitHub >
   Settings > Environments), limit its deployment branches and tags to the
   tag pattern `v*`, and store the token there as the environment secret
   `SIGNPATH_API_TOKEN`. Only the workflow's `sign` job uses that environment,
   so other jobs and branch pushes can't read the token. A `v*` tag on any
   commit can, though, including one whose workflow was edited, so also add
   yourself as a required reviewer of the environment (each release then
   waits for your approval before the token is used) or limit who can create
   `v*` tags with a tag ruleset. SignPath's own approval still guards every
   signing request.
7. Repository variables (same page, **Variables** tab):

   | Variable | Value |
   |---|---|
   | `SIGNPATH_ORGANIZATION_ID` | your SignPath organization ID. Setting it turns signing on. |
   | `SIGNPATH_PROJECT_SLUG` | the project's slug, if it isn't `compute-pool` |
   | `SIGNPATH_SIGNING_POLICY_SLUG` | the release signing policy's slug, if it isn't `release-signing` |

8. Trial run, if the project has a test signing policy: set
   `SIGNPATH_SIGNING_POLICY_SLUG` to its slug and `RELEASE_AS_DRAFT` to
   `true`, then push a tag with a plain version (such as `v0.1.6`). The
   release is published as a draft, with binaries signed by SignPath's test
   certificate, which Windows doesn't trust. Check that signing went through,
   then delete the draft release and the tag (`git push origin :v0.1.6`,
   `git tag -d v0.1.6`), and remove both variables.

## Each release

Push the signed tag as usual (`git tag -s v0.2.0`, `git push origin v0.2.0`).
The workflow's `build` job builds and uploads the two Windows binaries, and
its `sign` job submits them and waits for the signing request. Approve it in
SignPath within an hour; after that the `sign` job gives up and nothing is
published. The `verify` job then checks, on Windows, that each signed binary's
signature is `Valid` (trusted by Windows, as SignPath's test certificate is
not), unless `RELEASE_AS_DRAFT` is `true`. The `publish` job then puts the
signed binaries in place, computes
`SHA256SUMS` and the provenance attestations from them, and publishes the
release. Only `sign` sees the SignPath token, and it can't write to the
repository.

Check a downloaded binary on Windows:

```powershell
Get-AuthenticodeSignature .\pool-0.2.0-windows-amd64.exe | Format-List Status, SignerCertificate
```

`Status` should be `Valid`, with SignPath Foundation in the certificate's
subject.

## If signing fails

- **Metadata mismatch.** The artifact configuration checks each binary's
  product name, product version, original file name and copyright against
  what the workflow's go-winres step sets. If you change one there, change it
  in `.signpath/artifact-configuration.xml` and paste the file into SignPath
  again. The copyright year is in both.
- **"Windows doesn't trust its signature".** The `verify` job found a binary
  whose signature isn't `Valid`, usually because the files were signed with a
  test signing policy (`SIGNPATH_SIGNING_POLICY_SLUG` left over from a trial)
  while `RELEASE_AS_DRAFT` isn't `true`. Nothing was published. Fix the
  variables, delete the tag and push it again.
- **"Expected 2 matches".** The configuration expects exactly the amd64 and
  arm64 binaries. Adding a Windows target means raising `min-matches` and
  `max-matches`.
- **Timed out.** The request wasn't approved within the hour, so nothing was
  published. Re-run the failed `sign` job (which also runs `verify` and
  `publish`) within 7 days, while the build's files are kept, and approve the
  new request; after that, push a new tag. SignPath's
  checks allow only a few re-runs of one workflow run (three, in its docs), so
  past that push a new tag.
