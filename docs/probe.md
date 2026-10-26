# Check message delivery after a harness release

`flopwire probe` runs the message-bus delivery tests against the Claude Code,
Codex, Devin CLI and opencode versions installed on this machine. Each case passes
only when the hook log shows the right hook delivered the message and the
harness's own transcript shows the message's wrapper as it reached the
model. The command exits non-zero when a case fails.

## When to run it

- After you install a new release of `claude`, `codex`, `devin` or
  `opencode`.
- After a change to `flopwire hook`, the plugins' hooks, the opencode
  plugin or the standing instruction.
- Before a Flopwire release.

The [nightly validation workflow](dev/nightly-validation.md) runs the Codex
installed-plugin probe on macOS when its API key is configured. Run the other
CLI and GUI checks by hand.

## Run it

1. Log in to each harness that you want to test.
2. From the repository root, run the probe with its own local agent:

   ```sh
   flopwire probe --local --notes
   ```

3. Read the table. A `FAIL` row names what is missing in its evidence.
4. Commit the new section of `notes/log/probe-runs.md`.

A full run takes about 3 minutes; the harnesses run at the same time.
Each harness runs about eight short turns on its cheapest model: `haiku`,
`gpt-5.6-luna`, `swe-2-medium` and `opencode/big-pickle`.

| Flag | Effect |
|---|---|
| `--harness claude,codex,devin,opencode` | Test only these harnesses. The default is every one installed. |
| `--case idle,mid-turn,...` | Run only these cases. |
| `--model NAME` or `--model codex=NAME,...` | Use another model. |
| `--local` | Start a local-only agent in the scratch directory. Without it the probe uses your running agent, and only Claude Code can run. |
| `--as-installed` | Run the plugins' own hook commands instead of the tap by absolute path, and add the `hook-binary` case. See [Plugins as installed](#plugins-as-installed). |
| `--json` | Print the report as JSON. |
| `--notes` | Append the run to `notes/log/probe-runs.md` (`--notes-file` changes the path). |
| `--idle-wait 60s` | How long the idle case waits. |
| `--dir PATH` | Keep the scratch files in PATH. The default is a new directory in the OS temp directory. The probe prints it. |

## What each case proves

Each harness gets two headless sessions in a scratch project: a sender and
a recipient. Every message goes through the device agent's bus, as a real
`flopwire send` does.

| Case | What it proves |
|---|---|
| `idle` | A message sent to an idle session does not start a turn within the wait, and stays `queued`. Ordinary sends never wake a session; this probe does not request explicit wake. |
| `prompt-submit` | A message queued before the user's prompt is printed by the `UserPromptSubmit` hook, once, and reaches the model. |
| `prompt-submit-observed` (opt-in, `--local`) | One recipient inbox read establishes that the exact message is visible locally as `queued` before the generic prompt. The session's `UserPromptSubmit` prints it once and its native transcript records the hook context. |
| `framing` | The `<flopwire-message>` wrapper arrives intact. The transcript holds it closed, with the sent message's id, sender session (`from`), harness (`agent`), `sender="own"`, intent and marker. |
| `mid-turn` | A message queued while a tool runs is printed by the next `PostToolUse` hook, before the turn ends, and reaches the model. |
| `subagent` | A message queued while a subagent runs is never printed by a hook inside the subagent and is not in the subagent's transcript. The session's own next hook prints it once, after the subagent returns. |
| `guardian` (Codex) | A message queued while Codex's auto-review subagent reviews an escalated command is not taken by a hook during the review. The session's next hook prints it. |
| `native-wake` (opt-in, Claude/Codex, `--local`) | An ordinary message stays queued while idle. Explicit wake starts a real recipient turn without a submitted prompt. The recipient sends a marker reply over Flopwire. Both messages reach native hook context with read receipts. The reply does not wake the sender. |

### Test real native wakes

This case spends model usage. It is excluded from default probes and CI.
It uses four short model turns per harness on a successful run.

```sh
flopwire probe --local --harness claude,codex --case native-wake --idle-wait 10s --json
```

The probe creates fresh sender and recipient sessions. It does not submit a
recipient prompt after requesting wake. Claude uses its native inbox socket.
Codex uses `codex queue` against a disposable daemon in a scratch home.
The daemon connects through the same WebSocket control socket as native
clients. The probe stops that daemon and its verified scratch updater before
deleting its copied login.
It does not restart the user's active daemon.

The Codex recipient uses Flopwire MCP for its reply, so its shell sandbox
stays enabled. The scratch configuration exposes only inbox/send and
approves the synthetic send tool for this test. Claude uses the scratch binary's CLI. Native transcripts,
hook output, message identity, and read receipts determine the result.
A model echoing a marker alone is insufficient. The sender gets a manual
probe prompt only after the reply stays queued without waking it.

This case qualifies local routing and the installed native transports.
Cross-device server routing has separate integration tests; this probe
does not claim a real two-machine wake. Run with `--as-installed` to also
qualify the installed plugin command path.

To compare the immediate prompt boundary with an observed local row, run:

```sh
flopwire probe --local --as-installed --harness codex --case prompt-submit,prompt-submit-observed --json
```

`prompt-submit` sends and submits the prompt immediately. Its timing is
unchanged. `prompt-submit-observed` adds one inbox read, with a one-second
request budget. It does not poll, wait for import, or retry a missing row.
The JSON result includes the read's UTC start, finish, and state under
`local_inbox`. A missing or unavailable observation fails the case even
if the later hook delivers the message. This keeps local visibility and
context entry as separate checks.

The observation requires `--local`: an inbox request through a connected
agent reads server state. Local inbox state proves row visibility for the
exact recipient. It does not establish the import time, and `queued` can
include an active lease. The read itself adds latency. Keep both cases when
investigating a timing failure. Neither case qualifies the VS Code extension;
Codex runs through `app-server`, and its native rollout is the context oracle.

opencode has no command hooks. Its plugin runs `flopwire hook` itself; in
the probe it runs the tap instead, so its log has the same entries. A
subagent there is a child session: the plugin names it as `agent_id`, and
the `task` tool call marks when it ran. A message delivered during an
opencode turn earns one more reply before the session goes idle, so the
probe reads every reply of the turn.

The hook log (`<scratch>/<harness>/tap.jsonl`) records every hook: its
event, tool, `agent_id` and the message ids it printed. The verdicts read
it, so a harness change that moves a message to another hook or into a
subagent fails the case even when the model still sees the marker.

"Reaches the model" is read from the recipient's transcript, as the
harness stored it. The probe parses it with Flopwire's own transcript
parser and looks for the message's wrapper in the rows that the harness
records as hook context:

| Harness | Hook context row |
|---|---|
| Claude Code | A `hook_additional_context` attachment in the session's JSONL transcript. |
| Codex | A developer message of kind `hooks.additional_context` in the rollout. |
| Devin | A system node in `sessions.db` that starts with the hook's output. |
| opencode | The text part that the plugin delivered, with `flopwire.id` metadata, in `opencode.db`. |

A prompt, a model reply or a tool result never counts, whatever it
quotes. A transcript that the probe cannot read fails the case.

The wrapper must also have reached the model whole. The case fails when
the wrapper is not closed, when the harness cut the hook context (Claude
Code's `<persisted-output>` stub past 10,000 characters, Codex's
truncation past 10,000 bytes), or when no model reply follows the row in
the transcript. In `framing`, the wrapper's text must equal the sent text,
and the hook log must show that the session's `UserPromptSubmit` printed
the message once. In `subagent`, the session's transcript must hold the
wrapper exactly once. The probe reads the transcript again for up to 20 s,
because a harness may write it after the turn ends; the hook log and the
reply that must follow the row bound that wait to the probed turn.

`framing` expects `sender="own"`: the probe's sessions belong to one
person, in `--local` mode and against the running agent alike. A probe
between two people would expect `teammate`.

The model's reply is corroboration only. Each case asks the model to
quote the message, and the evidence says whether it did. A cheap model
that answers the message instead of quoting it does not fail a case.

## Plugins as installed

By default the scratch project's hooks call this binary by absolute path
(`<flopwire> probe tap`). That hides how a real install finds the binary:
every harness runs the plugin's hook command through a shell, whose
`PATH` can differ from yours ([agent.md](harnesses/README.md#how-the-hooks-find-the-binary)).

With `--as-installed`, the probe installs this binary's own plugins the
way `flopwire setup` does, and the delivery hooks are the plugins' own
commands:

| Harness | Install |
|---|---|
| Claude Code | `claude --plugin-dir` with the Claude Code plugin, for the probe's sessions only. |
| Codex | `codex plugin marketplace add` and `codex plugin add` in the scratch `CODEX_HOME`. The probe trusts the plugin's hooks there. |
| Devin | `devin plugins install --local` in the scratch `HOME`. |
| opencode | The plugin file, without `FLOPWIRE_BIN`, so it searches for the binary. |

The harness runs the shim by its plugin root, through its own shell. The
shim finds this binary through the path recorded in the scratch config
directory (`FLOPWIRE_CONFIG`), as `flopwire setup` records it. With
`FLOPWIRE_PROBE_TAP` set, `flopwire hook` writes the hook log itself, with
the binary that ran and how the shim found it. The project keeps only
observing hooks (`PreToolUse`, `SubagentStart`, `SubagentStop`).

| Case | What it proves |
|---|---|
| `hook-binary` | Every delivery hook ran this binary through the plugin's own hook command, and the shim (or the opencode plugin) said how it found it. |

The plugins' Codex `Stop` hook is `async`, so Codex runs it after it
reports the turn done. Before the idle wait and after each turn, the
probe waits up to 15 s until every turn of the recipient has its `Stop`
in the hook log.

## What it does not touch

- Your harness configuration. Claude Code runs with
  `--setting-sources project`. Codex and Devin run in scratch homes. The
  probe copies only their login files there (`~/.codex/auth.json`,
  `~/.local/share/devin/credentials.toml`) and deletes the copies at the
  end. opencode runs with scratch XDG directories: its own config, the
  plugin and its store are in the scratch directory. Its free model needs
  no login; if `~/.local/share/opencode/auth.json` exists, the probe
  copies it like the others. The probe never writes your harness files, the login files
  included.
- Claude Code still writes its transcripts to `~/.claude/projects`, as
  any session does. The probe's sessions appear there under the scratch
  project's name, with an empty directory each in `~/.claude/session-env`.

## Codex login refresh

Codex refresh tokens are single use. A refresh in the probe's copy would
use up the token that `~/.codex/auth.json` still holds, and your next
Codex refresh would then fail. Codex refreshes when its access token is
within 5 minutes of expiry. In Codex 0.160.0 and 0.162.1, the
8-day `last_refresh` rule applies only when the token expiry cannot be read.
The probe follows that precedence and rejects unreadable expiry conservatively.

- Before it copies the login, the probe reads both times. If a refresh
  could happen within the next 30 minutes, it skips Codex and prints
  "Codex token refresh due; run `codex` once to refresh, then rerun the
  probe". Do that, then run the probe again.
- If Codex refreshes during the run anyway, the probe prints a warning.
  If Codex later asks you to log in, run `codex login`.

## Known limits

- A probe killed with `SIGKILL` (or ended by `SIGHUP`) leaves the login
  copies in its scratch directory (mode 0700). Delete the scratch
  directory that the probe printed.
- Without `--local`, Codex, Devin and opencode cannot run: their scratch homes are
  outside what your running agent watches, and the probe stops with an
  error. Use `--local`, or `--harness claude` with your running agent.

## What it does not cover

- The Claude desktop app, the Codex desktop app, Devin Desktop and the
  IDE extensions. Check them by hand after their releases. See
  [manual-checks.md](dev/manual-checks.md).
- Interactive TUI sessions. The Codex hook review screen and the Devin TUI
  with a teammate sender have their own checks in
  [manual-checks.md](dev/manual-checks.md). The probe drives each harness headless:
  `claude -p` with stream-json input, `codex app-server`, `devin acp` and
  `opencode serve`.
  Each keeps one process open, so the session stays live and idle between
  turns. `codex exec` and `devin -p` end their session when they exit,
  and the bus then marks its queued messages undelivered.
- The vendor cloud sessions.
