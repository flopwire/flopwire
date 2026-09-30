# Releases

Release-please runs after pushes to `main` and on manual dispatch. It keeps
a release PR that updates `CHANGELOG.md` and `.release-please-manifest.json`.
Merging the release PR creates a `vX.Y.Z` tag and GitHub release.

The empty manifest means Flopwire has no prior release. The initial version
is configured as `0.1.0`; it is not a previously published version.
After that release, `fix:` and `perf:` bump the patch, and `feat:` bumps
the minor. Breaking changes bump the minor before `1.0.0` and the major
after it. Release-please owns subsequent manifest and changelog updates.

## Bot identity

The workflow selects credentials in this order:

1. The GitHub App configured by `RELEASE_APP_CLIENT_ID` and
   `RELEASE_APP_PRIVATE_KEY`, matching agentboard.
2. The `RELEASE_TOKEN` secret, matching pandora.
3. The built-in `GITHUB_TOKEN`.

Use the App or PAT to run normal CI on release PRs. GitHub suppresses
workflow events caused by `GITHUB_TOKEN`, including release PR updates
and tag pushes. The fallback still creates release PRs and releases;
it directly calls the source-release gate after creating a release.
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
6. Check the Source release gate workflow for the new tag.

The gate checks out the tag, validates its semantic version and ancestry
on `main`, runs Go tests, and builds a CLI that prints the tag as its
version. It runs on tag pushes from the App, PAT, or a human. The
release-please workflow calls the same gate directly for tags created
with `GITHUB_TOKEN`. Manual dispatch accepts a tag for retries.

The existing source-only release policy applies. GitHub provides its
source archives. This workflow does not publish binaries, container
images, or packages and does not deploy a server. The gate verifies
after tagging; a failed gate does not remove an already created release.

To retry automation without a new commit, run:

```sh
gh workflow run release-please.yml --ref main
gh workflow run source-release.yml --ref main -f tag=v0.1.0
```

The first command refreshes release-please. The second verifies an
existing tag; replace the example tag with the release being checked.
