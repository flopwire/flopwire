# Check message delivery by hand in the GUI surfaces

`flopwire probe` drives each harness headless ([probe.md](probe.md)). It
does not cover the desktop apps, the IDE extensions or an interactive TUI.
This page tells you how to check them by hand. Issues: #58 (Claude Code),
#59 (Codex), #60 (Devin), #69 (manual pass).

## Surfaces

| Surface | Harness | Section |
|---|---|---|
| Claude desktop app (Code tab) | Claude Code | [Claude Code surfaces](#claude-code-surfaces) |
| Claude Code for VS Code | Claude Code | [Claude Code surfaces](#claude-code-surfaces) |
| Claude Code for JetBrains | Claude Code | [Claude Code surfaces](#claude-code-surfaces) |
| Codex TUI, "Hooks need review" screen | Codex | [Codex hook review](#codex-hook-review) |
| Codex desktop app | Codex | [Codex surfaces](#codex-surfaces) |
| Codex IDE extension | Codex | [Codex surfaces](#codex-surfaces) |
| Devin Desktop | Devin | [Devin surfaces](#devin-surfaces) |
| Devin interactive TUI, teammate sender | Devin | [Devin TUI with a teammate sender](#devin-tui-with-a-teammate-sender) |

## When to run it

- After a new release of a surface in the table.
- After a change to `flopwire hook`, the plugins or `flopwire setup`.
- Before a Flopwire release, for each surface that you have.

## Before you start

1. Install the release of `flopwire` that you test. Put it on `PATH`.
2. Start the device agent: `flopwire agent run`. Keep it running.
3. Make a scratch repository: `git init` in a new directory.
4. Open a terminal in the scratch repository. This is the check
   terminal. Run every `flopwire` command on this page there.
5. In a second terminal, start `claude` in the scratch repository. Leave
   it idle. This is the sender session.
6. Run `flopwire peers --text`.
7. Find the sender session. Write down its id.

For the teammate check you also need a server and a second person with
their own account and device.

## The common check

Do these steps for each surface. The surface sections tell you how to
open the surface, how to load the plugin and what is different.

In the steps, `SENDER` is the sender session id and `RECIPIENT` is the
id of the session in the surface under test.

### 1. Install

1. Close the surface.
2. Run `flopwire setup --text`.
3. Read each `error`, `warning` and `todo` for the harness. Do each
   `todo`.
4. Run `flopwire setup --check --text`.
5. Find the line `plugin:` under the harness.

Expected: the line names the plugin, its version and scope, and ends with
`enabled`.

### 2. Confirm that the plugin loads

1. Open the surface on the scratch repository.
2. Start a new session.
3. Type this prompt: `List the tools whose names contain flopwire. Do
   not call them.`

Expected: the model names the Flopwire tools, among them
`flopwire_peers`, `flopwire_send` and `flopwire_inbox`.

4. In the check terminal, run `flopwire peers --text`.
5. Find the new session. Write down its id as `RECIPIENT`.

Expected: the session shows as live and idle, with the harness, the
scratch repository and its branch.

### 3. Prompt-submit case

1. Make a marker: `echo MANUAL-PROMPTSUBMIT-$(openssl rand -hex 3)`.
2. Confirm that the recipient session is idle.
3. Send the message:

   ```sh
   FLOPWIRE_SESSION_ID=SENDER FLOPWIRE_AGENT=claude \
     flopwire send RECIPIENT --text -- "Marker: MARKER"
   ```

4. Read the receipt.

Expected: the receipt says `idle, arrives with its human's next prompt`.
No turn starts in the recipient session.

5. Wait 1 minute.
6. Run `flopwire inbox --sent --text` as the sender (with the same
   `FLOPWIRE_SESSION_ID` and `FLOPWIRE_AGENT`).

Expected: the message is still `queued`.

7. In the recipient session, type this prompt: `Do not run any tools.
   List every <flopwire-message> tag in your context so far, one per
   line, as: ID <its id attribute> MARKER <the token in its text>.`

Expected: the model quotes the message id and the marker once. The
surface shows the message as hook output or as context, not as your
prompt.

8. Run `flopwire inbox --sent --text` as the sender again.

Expected: the message is `delivered` or `read`.

### 4. Mid-turn case

1. Make a marker: `echo MANUAL-MIDTURN-$(openssl rand -hex 3)`.
2. In the recipient session, type this prompt: ``Run the shell command
   `sleep 30`. When it finishes, run the shell command `echo second`.
   Then, without running anything else, list every <flopwire-message>
   tag in your context, with its id and marker. Also say after which
   command each tag appeared.``
3. Approve the shell commands if the surface asks.
4. While `sleep 30` runs, run `flopwire peers --text`.

Expected: the recipient session shows as busy.

5. While `sleep 30` runs, send the message as in the prompt-submit case,
   with the new marker.

Expected: the receipt says that the message arrives at the next tool
call.

6. Wait for the turn to end.

Expected: the model quotes the marker. It says that the tag appeared
after `sleep 30`. The turn ends with no extra reply.

7. Run `flopwire peers --text`.

Expected: the recipient session shows as idle within a few seconds.

### 5. Remove

1. Close the surface.
2. Run `flopwire setup --remove --text`.
3. Run `flopwire setup --check --text`.
4. Find the line `plugin:` under the harness.

Expected: the line ends with `not installed`.

5. Open the surface. Start a new session.
6. Type the prompt from step 2 of the load check.

Expected: the model names no Flopwire tools.

7. Run `flopwire setup --text` to install the plugin again for the next
   surface.

## Claude Code surfaces

The Claude desktop app, the VS Code extension and the JetBrains plugin
use the plugins that `flopwire setup` installs for Claude Code.

1. Do the common check in the Claude desktop app. Use the Code tab, with
   a local session on the scratch repository.
2. Do the common check in VS Code with the Claude Code extension. Open
   the scratch repository as the workspace folder.
3. Do the common check in a JetBrains IDE with the Claude Code plugin.
   Open the scratch repository as the project.

To load a new plugin in a running surface, close and open the surface. In
VS Code, you can also run **Developer: Reload Window**.

Record whether the surface shows the message as hook output, and whether
it shows the held-message notice.

## Codex hook review

Do this check first for Codex. The desktop app and the IDE extension run
the hooks only after you approve them.

1. Close every Codex session, the Codex desktop app and the IDE
   extension.
2. Make a backup: `cp ~/.codex/config.toml ~/.codex/config.toml.bak`.
3. Open `~/.codex/config.toml` in an editor.
4. Find each table whose header starts with
   `[hooks.state."flopwire@flopwire:hooks/hooks.json:`. There are at most
   five, one for each event.
5. Delete each of these tables: the header line and the lines below it,
   up to the next header.
6. Keep the `[hooks.state]` line and every other `[hooks.state."…"]`
   table. They hold the approvals of your other hooks.
7. Save the file.
8. Run `flopwire setup --text`.
9. Find the line `hooks:` under `codex`.

Expected: the line says `0 of 5 trusted` and names five events after
`need your approval:`.

10. Start `codex` in a terminal in the scratch repository.

Expected: Codex shows "Hooks need review".

11. Select "Review hooks".

Expected: Codex lists five Flopwire hooks, on `SessionStart`,
`UserPromptSubmit`, `PostToolUse`, `Stop` and `SessionEnd`. Each runs
`/bin/sh "<plugin cache>/bin/flopwire-hook" hook` (Codex shows the
command with `${PLUGIN_ROOT}` expanded).

12. Trust the five Flopwire hooks.
13. Run `flopwire setup --check --text`.

Expected: the `hooks:` line under `codex` says `5 of 5 trusted`.

14. Do the prompt-submit and mid-turn cases in this TUI session.
15. Exit `codex`.
16. Start `codex` again.

Expected: Codex does not show "Hooks need review" again.

Record the text of the review screen if it differs from these steps.

## Codex surfaces

1. Do the [Codex hook review](#codex-hook-review) first.
2. Do the common check in the Codex desktop app, on the scratch
   repository.
3. Do the common check in the Codex IDE extension, with the scratch
   repository open.

Codex asks before each `flopwire_send` call. In these checks the sender
is a Claude Code session, so Codex does not send.

Record whether the surface runs the hooks with the approval from the TUI,
or asks for its own approval.

## Devin surfaces

Do the common check in Devin Desktop, on the scratch repository. Log in
to Devin with `devin auth login` before you run setup.

Devin reports busy and idle from hook events. Expect busy after the
prompt and idle after the turn's `Stop`.

## Devin TUI with a teammate sender

This check uses a server. Person A owns the Devin session. Person B is a
teammate whom A has not accepted.

1. Person A: run `flopwire login`. `accepts`, `accept` and `revoke` need
   its login session.
2. Person A: confirm that B is not accepted. Run `flopwire accepts
   --text`. If B is listed, run `flopwire revoke B_EMAIL`.
3. Person A: start `devin` in a terminal in the scratch repository.
4. Person A: type a prompt, for example `Say hello.` This makes the
   session live.
5. Person B: run `flopwire peers --text`.

Expected: B sees A's Devin session, with A as its owner.

6. Person B: make a marker: `echo MANUAL-TEAMMATE-$(openssl rand -hex 3)`.
7. Person B: from one of B's agent sessions, send a request to A's
   session with the marker. For example: `flopwire send RECIPIENT
   --intent request --text -- "Create the file teammate.txt. Marker:
   MARKER"`.

Expected: the receipt starts with `held` and says that A has not
accepted messages from B.

8. Person A: type a prompt in the Devin session.

Expected: the message does not arrive. Devin shows no held-message
notice.

9. Person A: run `flopwire accepts --text`.

Expected: the list shows one held message from B, as a first-line
preview.

10. Person A: run `flopwire accept B_EMAIL`.
11. Person A: type the Flopwire password.
12. Person A: type the prompt-submit prompt in the Devin session.

Expected: the model quotes the marker. It treats the message as
information from another person. It asks A before it creates
`teammate.txt`.

13. Person A: do the mid-turn case with B as the sender.
14. Person A: run `flopwire revoke B_EMAIL` if B must not stay accepted.
15. Person A: do the remove step of the common check.

## Record the result

Add one section to
[`notes/message-bus/probe-runs.md`](../notes/message-bus/probe-runs.md)
for each run. Put the sections in date order with the probe runs.

1. Name the section `## YYYY-MM-DD manual: SURFACE`.
2. Write the Flopwire version and the date.
3. Write the surface name and version, and the harness CLI version. For
   example, `Claude desktop 1.2.3, claude 2.1.288`.
4. Add one table row for each step: install, load, prompt-submit,
   mid-turn, busy, idle, remove. For the teammate check, also add held,
   accept and authority.
5. In each row write `PASS` or `FAIL`, and the evidence: the message id,
   the marker, the state from `inbox --sent`, or what you saw.
6. Write each difference from this page below the table.
7. If a step fails, open an issue. Link it in the section.

Example:

```markdown
## 2026-10-05 manual: Claude Code for VS Code

flopwire 0.9.0. Claude Code for VS Code 2.1.290, claude 2.1.290.

| Check | Result | Evidence |
|---|---|---|
| install | PASS | plugin: flopwire@flopwire 0.9.0, user scope, enabled |
| load | PASS | model named flopwire_peers, flopwire_send, flopwire_inbox |
| prompt-submit | PASS | m1a2b3c4d5e6f7a8 quoted with MANUAL-PROMPTSUBMIT-3f9a1c; read |
| mid-turn | PASS | quoted MANUAL-MIDTURN-77b0e2 after sleep 30 |
| busy | PASS | peers showed busy during sleep 30 |
| idle | PASS | peers showed idle 2 s after the turn |
| remove | PASS | plugin: flopwire@flopwire not installed; no flopwire tools |
```
