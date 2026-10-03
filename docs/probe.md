# Check message delivery after a harness release

`flopwire probe` runs the message-bus delivery tests against the Claude Code,
Codex and Devin CLI versions installed on this machine. Each case passes
only when the hook log shows the right hook delivered the message and the
model quotes the message's marker back. The command exits non-zero when a
case fails.

## When to run it

- After you install a new release of `claude`, `codex` or `devin`.
- After a change to `flopwire hook`, the plugins' hooks or the standing
  instruction.
- Before a Flopwire release.

Nothing runs it on a schedule. Run it by hand.

## Run it

1. Log in to each harness that you want to test.
2. From the repository root, run the probe with its own local agent:

   ```sh
   flopwire probe --local --notes
   ```

3. Read the table. A `FAIL` row names what is missing in its evidence.
4. Commit the new section of `notes/message-bus/probe-runs.md`.

A full run takes about 3 minutes; the harnesses run at the same time.
Each harness runs about eight short turns on its cheapest model: `haiku`,
`gpt-5.6-luna` and `swe-2-medium`.

| Flag | Effect |
|---|---|
| `--harness claude,codex,devin` | Test only these harnesses. The default is every one installed. |
| `--case idle,mid-turn,...` | Run only these cases. |
| `--model NAME` or `--model codex=NAME,...` | Use another model. |
| `--local` | Start a local-only agent in the scratch directory. Without it the probe uses your running agent, and only Claude Code can run. |
| `--json` | Print the report as JSON. |
| `--notes` | Append the run to `notes/message-bus/probe-runs.md` (`--notes-file` changes the path). |
| `--idle-wait 60s` | How long the idle case waits. |
| `--dir PATH` | Keep the scratch files in PATH. The default is a new directory in the OS temp directory. The probe prints it. |

## What each case proves

Each harness gets two headless sessions in a scratch project: a sender and
a recipient. Every message goes through the device agent's bus, as a real
`flopwire send` does.

| Case | What it proves |
|---|---|
| `idle` | A message sent to an idle session does not start a turn within the wait, and stays `queued`. Flopwire never wakes a session. |
| `prompt-submit` | A message queued before the user's prompt is printed by the `UserPromptSubmit` hook, once, and reaches the model. |
| `framing` | The `<flopwire-message>` wrapper arrives intact: the model quotes the message id, the sender session (`from`), the intent and the marker. |
| `mid-turn` | A message queued while a tool runs is printed by the next `PostToolUse` hook, before the turn ends, and reaches the model. |
| `subagent` | A message queued while a subagent runs is never printed by a hook inside the subagent and is not in the subagent's transcript. The session's own next hook prints it once, after the subagent returns. |
| `guardian` (Codex) | A message queued while Codex's auto-review subagent reviews an escalated command is not taken by a hook during the review. The session's next hook prints it. |

The hook log (`<scratch>/<harness>/tap.jsonl`) records every hook: its
event, tool, `agent_id` and the message ids it printed. The verdicts read
it, so a harness change that moves a message to another hook or into a
subagent fails the case even when the model still sees the marker.

## What it does not touch

- Your harness configuration. Claude Code runs with
  `--setting-sources project`. Codex and Devin run in scratch homes. The
  probe copies only their login files there (`~/.codex/auth.json`,
  `~/.local/share/devin/credentials.toml`) and deletes the copies at the
  end. The probe never writes your harness files, the login files
  included.
- Claude Code still writes its transcripts to `~/.claude/projects`, as
  any session does. The probe's sessions appear there under the scratch
  project's name, with an empty directory each in `~/.claude/session-env`.

## Codex login refresh

Codex refresh tokens are single use. A refresh in the probe's copy would
use up the token that `~/.codex/auth.json` still holds, and your next
Codex refresh would then fail. Codex refreshes when its access token is
within 5 minutes of expiry, or 8 days after `last_refresh`.

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
- Without `--local`, Codex and Devin cannot run: their scratch homes are
  outside what your running agent watches, and the probe stops with an
  error. Use `--local`, or `--harness claude` with your running agent.

## What it does not cover

- The Claude desktop app, the Codex desktop app and the IDE extensions.
  Check them by hand after their releases: send a message to a session
  in each surface, and check that it arrives at the next tool call or
  prompt.
- Interactive TUI sessions. The probe drives each harness headless:
  `claude -p` with stream-json input, `codex app-server` and `devin acp`.
  Each keeps one process open, so the session stays live and idle between
  turns. `codex exec` and `devin -p` end their session when they exit,
  and the bus then marks its queued messages undelivered.
- opencode and the vendor cloud sessions.
