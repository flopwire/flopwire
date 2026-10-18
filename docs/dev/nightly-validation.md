# Nightly validation

The `Nightly validation` workflow runs daily at 07:41 UTC. It also supports
manual runs from the Actions tab. These checks do not block merges or releases.

## Checks

The macOS job runs agent, local retrieval, local index and CLI tests with the
race detector. It also runs the redaction latency and allocation guards without
the race detector. PostgreSQL and MinIO integration remain in Linux CI.

The Codex job installs the latest stable Codex CLI and runs
`flopwire probe --harness codex --local --as-installed` with `gpt-5.6-luna`.
It checks idle behavior, prompt-submit delivery, framing, delivery during a
turn, subagent isolation, guardian isolation and the installed hook binary.
It uses synthetic sessions and a local agent. It does not test the desktop
app or IDE extension.

The probe runs only on trusted `main` schedule or manual events. Pull requests
that change this automation run the macOS tests without API credentials.

## Add the API key

1. Create an OpenAI API key in the project that will pay for these checks.
2. Open the repository's **Settings > Secrets and variables > Actions**.
3. Add a repository secret named `OPENAI_API_KEY`.
4. Open **Actions > Nightly validation > Run workflow**.
5. Select `main`.
6. Read the Codex job summary and download the `nightly-codex` artifact.

One OpenAI key is sufficient for the current Codex-only scope. No Claude key
is required. The project must have billing and model access enabled. Guardian
availability under API-key authentication still needs verification with the
configured project. An unavailable guardian fails the probe; it is not skipped.

Without the secret, the Codex job reports **setup required** and makes no model
calls. The macOS job still runs. The workflow may be green in this state;
the summary explicitly says that no live Codex checks ran.

## Results and triage

A failed nightly check opens or updates one GitHub issue for that check.
The issue links to the run, job logs and artifacts. A passing run closes its
issue. A skipped or cancelled check leaves its existing issue open. Missing
credentials do not create a failure issue or resolve a previous failure.

The owner triages these issues. Authentication, installation and model-access
failures are reported alongside test failures. There are no automatic probe
retries, so an intermittent failure remains visible.

Artifacts expire after seven days. `nightly-macos` contains race-suite and
non-race guard test events. The guards still run after a race-suite failure.
`nightly-codex` contains the probe verdicts, runtime, CLI version, diagnostics
and token usage grouped by model. Raw transcripts and login files are excluded.
Temporary credentials are removed when the probe exits.

Use token usage to track API cost. Reviewer usage is reported separately from
Luna usage. The workflow has timeouts, but these are not dollar spending caps.
Standard GitHub-hosted runner compute is free for this public repository.
