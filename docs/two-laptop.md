# Two-laptop setup

This runbook sets up the two-laptop phase. Laptop A runs the server and a
device agent. Laptop B runs a device agent. Both laptops reach the server
over the network. The server speaks TLS with a self-signed certificate, and
both laptops pin its fingerprint. Both agents keep a local index and upload
transcripts to the server.

`scripts/e2e-sync.sh` tests this setup on one machine. See
[Test the setup on one machine](#test-the-setup-on-one-machine).

## Before you start

You need:

- Laptop B can reach laptop A over the network: the same LAN, a VPN, or any
  other route. This runbook needs no VPN.
- Docker Desktop on laptop A.
- Go 1.26 on both laptops, or a built `flopwire` binary.
- A clone of this repository on laptop A.

## Install the binary

Do these steps on both laptops.

1. Build the binary:

   ```sh
   go build -trimpath -o ~/.local/bin/flopwire ./cmd/flopwire
   ```

2. Make sure `~/.local/bin` is on your `PATH`.
3. Run `flopwire version` to check the binary.

## Start the server on laptop A

1. Find the address at which laptop B reaches laptop A. On a LAN:

   ```sh
   ipconfig getifaddr en0
   ```

   This runbook uses `192.168.1.20`. Use your own address or host name.

2. Go to the repository directory.
3. Create a file `.env` with two passwords:

   ```sh
   cat > .env <<'EOF'
   POSTGRES_PASSWORD=<long random password>
   MINIO_ROOT_PASSWORD=<long random password>
   FLOPWIRE_BIND=192.168.1.20
   FLOPWIRE_PORT=8080
   EOF
   chmod 600 .env
   ```

4. Start the stack:

   ```sh
   docker compose -f compose.yaml -f compose.dev.yaml up -d --build --wait
   ```

   `compose.dev.yaml` publishes Postgres on `127.0.0.1:55432` and MinIO
   on `127.0.0.1:59000`. The bootstrap step needs Postgres. The server
   publishes only on `FLOPWIRE_BIND`, not on loopback.

5. Print the server's certificate fingerprint:

   ```sh
   docker compose -f compose.yaml -f compose.dev.yaml exec -T flopwire flopwire fingerprint
   ```

   The output looks like `sha256:3f9a…`. The server made the certificate on
   first start and keeps it in the `flopwire-tls` volume. Keep the
   fingerprint for the next steps.

6. Check the server. `-k` skips certificate checks; this request sends no
   credential:

   ```sh
   curl -k https://192.168.1.20:8080/readyz
   ```

   The answer must contain `"status":"ok"`.

If Docker cannot bind the address, the address is not on laptop A. Check it,
then run step 4 again. To test on laptop A only, set
`FLOPWIRE_BIND=127.0.0.1`. Laptop B cannot reach that address.

## Create the administrator on laptop A

The first administrator is also the owner of both devices.

1. Load the passwords:

   ```sh
   set -a; . ./.env; set +a
   ```

2. Create the administrator. Use the fingerprint from step 5 above. Enter
   a password twice when prompted:

   ```sh
   DATABASE_URL="postgres://flopwire:$POSTGRES_PASSWORD@127.0.0.1:55432/flopwire?sslmode=disable" \
     flopwire bootstrap --server https://192.168.1.20:8080 --fingerprint sha256:<fingerprint> \
     --name "Your Name" --email you@example.com
   ```

3. Enroll laptop A as a device:

   ```sh
   flopwire enroll --name laptop-a
   ```

The configuration is in `~/Library/Application Support/flopwire/config.json`
with mode 0600. It holds the device token, a login session and the pinned
fingerprint. The login session expires after 24 hours. The device token does
not expire. Run `flopwire login` again before an administrative command.

Every connection from this laptop checks the server's certificate against
the pinned fingerprint: admin commands, `--server` queries, the raw fallback
and the agent's uploads. A different certificate is refused before any
credential is sent.

To keep the administrator apart from your own data, invite a member
instead. See [Enroll laptop B](#enroll-laptop-b), option 1.

## Enroll laptop B

Use one of these options.

Option 1: claim an invite. This option works for any member.

1. On laptop A, create an invite:

   ```sh
   flopwire invite --email you+member@example.com
   ```

   Copy the `invite` value from the output. It looks like
   `https://192.168.1.20:8080#code=…&pin=sha256%3A…`. It holds the server
   address, a one-time code and the fingerprint. Send it over a channel you
   trust.

2. On laptop B, claim the invite. Enter a new password twice:

   ```sh
   flopwire claim --invite '<invite string>' --name "Your Name"
   ```

3. Enroll laptop B:

   ```sh
   flopwire enroll --name laptop-b
   ```

Option 2: log in as the administrator.

1. On laptop B, log in with the fingerprint from laptop A. Enter the
   administrator password:

   ```sh
   flopwire login --server https://192.168.1.20:8080 --fingerprint sha256:<fingerprint> \
     --email you@example.com
   ```

2. Enroll laptop B:

   ```sh
   flopwire enroll --name laptop-b
   ```

If a command reports `does not match the pinned fingerprint`, the server has
a different certificate than the one you pinned. Do not continue. Check the
fingerprint on laptop A with `flopwire fingerprint`. If the `flopwire-tls` volume
was replaced, log in again with the new fingerprint.

## Install the agent as a launchd user agent

Do these steps on both laptops.

1. Copy the template:

   ```sh
   cp deploy/launchd/com.flopwire.agent.plist ~/Library/LaunchAgents/
   ```

   On laptop B, copy the file from laptop A or from a clone.

2. Open `~/Library/LaunchAgents/com.flopwire.agent.plist`.
3. Replace `/Users/YOU` with your home directory in every path.
4. Check the file:

   ```sh
   plutil -lint ~/Library/LaunchAgents/com.flopwire.agent.plist
   ```

5. Load the agent:

   ```sh
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.flopwire.agent.plist
   ```

6. Check that the agent runs:

   ```sh
   launchctl print gui/$(id -u)/com.flopwire.agent | grep state
   ```

   The state must be `running`.

7. Read the log:

   ```sh
   tail -f ~/Library/Logs/flopwire-agent.log
   ```

   Look for `agent: running` with `sync=true`.

The first pass indexes every transcript on the laptop. On an 18GB corpus
it takes about 7 minutes. After a large first pass the agent restarts
itself once. The first upload of all history takes longer. Hook flushes
go ahead of that backlog.

## Connect the harness hooks

Do these steps on both laptops. The hooks make uploads immediate. Without
them, a new line reaches the server within about 2 seconds while the
server is up.

1. Open `~/.claude/settings.json`.
2. Add these entries under `hooks`. Use the full path of the binary:

   ```json
   {
     "hooks": {
       "Stop": [
         { "hooks": [{ "type": "command", "command": "/Users/YOU/.local/bin/flopwire agent flush", "timeout": 10 }] }
       ],
       "PostToolUse": [
         { "matcher": "*", "hooks": [{ "type": "command", "command": "/Users/YOU/.local/bin/flopwire agent flush", "timeout": 10 }] }
       ]
     }
   }
   ```

3. Open `~/.codex/config.toml`.
4. Add this line at the top level:

   ```toml
   notify = ["/Users/YOU/.local/bin/flopwire", "agent", "flush"]
   ```

A hook never fails because of the agent. See `docs/agent.md` for details.

## Verify

1. On laptop A, start a Claude Code session.
2. Type a prompt with a unique word, for example `zebra-4411`.
3. On laptop B, search the server:

   ```sh
   flopwire grep --server zebra-4411
   ```

   The session header's `who=` field must name the teammate's user.
   `--json` shows the device too.

4. Read the hit. Use the address the hit prints first, for example
   `0b7e2c1a/28672:1`:

   ```sh
   flopwire read --server 0b7e2c1a/28672:1 -B 2
   ```

5. On laptop A, run the same search with `--device laptop-b` for a
   prompt from laptop B.

6. Search the local index of laptop A. Leave out `--server`:

   ```sh
   flopwire grep zebra-4411
   ```

   The hit comes from the agent's local index, which holds only laptop A's
   transcripts. It works while the server is down.

## What happens when a laptop is offline

Do nothing. The agent indexes locally and keeps a queue. When the server
is reachable again, the agent uploads what is missing. The retry interval
grows to at most 30 seconds, so catch-up starts up to 30 seconds after the
server returns. A hook flush retries at once.

If laptop A sleeps, laptop B also waits. Laptop B loses nothing.

## Stop

1. Stop the agent on a laptop:

   ```sh
   launchctl bootout gui/$(id -u)/com.flopwire.agent
   ```

2. Stop the server on laptop A. Keep the volumes:

   ```sh
   docker compose -f compose.yaml -f compose.dev.yaml down
   ```

   Do not add `--volumes`. That flag deletes the database and the
   archive.

3. Start the server again with the command in
   [Start the server on laptop A](#start-the-server-on-laptop-a), step 4.

## Back up the server

1. Create a directory that the container user (uid 10001) can write:

   ```sh
   mkdir -p ~/flopwire-backups && chmod 777 ~/flopwire-backups
   ```

2. Put the directory on encrypted storage. FileVault counts.
3. Create a backup:

   ```sh
   docker compose -f compose.yaml -f compose.dev.yaml run --rm --no-deps \
     -v ~/flopwire-backups:/backup flopwire backup --encrypted-destination --output /backup/$(date +%Y%m%d)
   ```

4. Verify the backup:

   ```sh
   docker compose -f compose.yaml -f compose.dev.yaml run --rm --no-deps \
     -v ~/flopwire-backups:/backup flopwire backup-verify --input /backup/$(date +%Y%m%d)
   ```

To restore, follow "Recover" in `docs/runbook.md`. The server image holds
`pg_dump` and `pg_restore`; run `restore` with `docker compose run` the
same way.

## Test the setup on one machine

`scripts/e2e-sync.sh` runs the whole setup on one laptop. It starts the
server on the laptop's LAN address (or on `127.0.0.1` when that bind fails).
It pins the server's fingerprint through the invite, the same way as
[Enroll laptop B](#enroll-laptop-b). It simulates two laptops with two
agents that have separate homes, credentials, indexes and queues.

1. Start Docker Desktop.
2. Run the test:

   ```sh
   scripts/e2e-sync.sh
   ```

3. Read the scenario table at the end of the output.

Set `FLOPWIRE_E2E_CORPUS=25` to add copies of the 25 newest Claude, Codex
and Devin sessions of this machine to the first simulated laptop. The test
only reads the harness directories. Set `FLOPWIRE_E2E_KEEP=1` to keep the
work directory and the Compose project for inspection.
