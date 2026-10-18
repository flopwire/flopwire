# Releases

Release-please runs after pushes to `main` and on manual dispatch. It keeps
a release PR that updates `CHANGELOG.md` and `.release-please-manifest.json`.
Merging the release PR creates a `vX.Y.Z` tag and GitHub release. The
Release workflow then attaches binaries to that release and publishes the
server image.

`fix:` and `perf:` bump the patch, and `feat:` bumps the minor. Breaking
changes bump the minor before `1.0.0` and the major after it. Release-please owns subsequent manifest and changelog updates.

## Release assets

The Release workflow (`.github/workflows/release.yml`) runs GoReleaser on
the tag and uploads to the release that release-please created:

| Asset | Content |
|---|---|
| `flopwire_<os>_<arch>.tar.gz` | The CLI for darwin and linux, amd64 and arm64, with `LICENSE` and `NOTICE`. `flopwire version` prints the tag. |
| `SHA256SUMS` | Checksums of the archives. |
| `ghcr.io/flopwire/flopwire:<X.Y.Z>` and `:latest` | The server image for linux/amd64 and linux/arm64, from `Dockerfile.release`. |

Asset names carry no version, so `releases/latest/download/<name>` always
fetches the newest release. Each archive has a GitHub artifact
attestation:

```sh
gh attestation verify flopwire_darwin_arm64.tar.gz --repo flopwire/flopwire
```

A `go install github.com/flopwire/flopwire/cmd/flopwire@vX.Y.Z` build
reports the module version from Go's build info.

## Upgrade a device

Every server response carries `X-Flopwire-Version`, and the device agent
asks github.com for the newest release once a day (one HEAD request, no
token; `FLOPWIRE_NO_RELEASE_CHECK=1` turns it off), so a device with no
server, or one whose server lags, hears too. When either source names a
newer release than the device runs, the agent says so once in its log, in
`flopwire agent status`, in `flopwire setup --check`, and as one line in
each new session's context at its SessionStart hook. Nothing is refused.

```sh
flopwire upgrade            # install the latest release over the hooks' binary
flopwire upgrade --check    # print this version and the latest release
flopwire upgrade --version v0.2.0
```

`flopwire upgrade` downloads the archive for this machine, verifies it
against the release's `SHA256SUMS`, checks that the binary runs, and
replaces the recorded binary (the one the plugins' hooks run) with one
rename, so running hooks keep their old binary. On macOS it restarts a
launchd `com.flopwire.agent` that runs that binary. Re-running
`curl -fsSL https://flopwire.com/install.sh | sh` does the same. A
development build (`dev` or a commit hash) never gets a notice.

## Bot identity

The workflow selects credentials in this order:

1. The GitHub App configured by `RELEASE_APP_CLIENT_ID` and
   `RELEASE_APP_PRIVATE_KEY`.
2. The `RELEASE_TOKEN` secret.
3. The built-in `GITHUB_TOKEN`.

Use the App or PAT to run normal CI on release PRs. GitHub suppresses
workflow events caused by `GITHUB_TOKEN`, including release PR updates
and tag pushes. The fallback still creates release PRs and releases;
it directly calls the Release workflow after creating a release.
It does not trigger release-PR CI. Configure the App or PAT before
merging a release PR.

### GitHub App

1. Install the existing release App on the repository that will host Flopwire.
2. Grant repository Contents and Pull requests write permissions.
3. Set the repository variable `RELEASE_APP_CLIENT_ID` to the App client ID.
4. Set the repository secret `RELEASE_APP_PRIVATE_KEY` to its private key.
5. Run the Release Please workflow on `main`.

### PAT

1. Create a token that can read and write repository contents and pull requests.
2. Store it as the repository secret `RELEASE_TOKEN`.
3. Run the Release Please workflow on `main`.

For the built-in-token fallback, enable "Allow GitHub Actions to create
and approve pull requests" in Settings > Actions > General.

## Cut a release

1. Wait for CI and E2E sync checks on the release PR.
2. Run the [release checklist](release-checklist.md) on the candidate.
3. Record the measured results in the release PR.
4. Review the proposed version and changelog.
5. Merge the release PR.
6. Check the Release workflow for the new tag, then the assets on the release.
7. After the first release only: GitHub creates the `flopwire` package
   private. In the organization's Packages settings, make
   `ghcr.io/flopwire/flopwire` public, or every `docker compose pull`
   without a login fails.

The workflow's `verify` job checks out the tag, validates its semantic
version and ancestry on `main`, runs Go tests, and builds a CLI that
prints the tag as its version. The `publish` job then runs GoReleaser.
The workflow runs on tag pushes from the App, PAT, or a human. The
release-please workflow calls it directly for tags created with
`GITHUB_TOKEN`. Manual dispatch accepts a tag for retries. GoReleaser
re-uploads nothing that already exists: delete a partial asset from the
release before retrying. Retry only the newest tag: a retry also moves the
image's `latest` tag to the tag it builds.

A failed workflow does not remove an already created release. It does
not deploy a server.

To retry automation without a new commit, run:

```sh
gh workflow run release-please.yml --ref main
gh workflow run release.yml --ref main -f tag=v0.1.0
```

The first command refreshes release-please. The second re-runs the
Release workflow for an existing tag; replace the example tag with the
release being retried.
