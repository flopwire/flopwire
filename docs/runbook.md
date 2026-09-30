# Operations runbook

## Check health

Request `/livez` to confirm that the process is alive. Request `/readyz` (or
the alias `/healthz`) to probe Postgres and object storage; either failing
returns `503`. Compose runs
`flopwire healthcheck` against `/readyz` on the container's loopback address,
over HTTPS unless `FLOPWIRE_TLS` is `proxy` or `off`.

Open System health as an administrator. Confirm storage pressure, index queue
depth, index queue capacity, the last attributable transition, and the latest
completed backup. The console keeps the last known observation visible if a
refresh fails.

`GET /v1/admin/status` returns the same data as JSON, and more:

| Field | Means |
|---|---|
| `storage.used_bytes`, `storage.quota_bytes` | Stored bytes against the deployment quota |
| `index.queue_depth`, `index.queue_capacity` | Sources waiting to parse, and the backlog at which flushes get `503 parse_backlog` (20,000) |
| `index.parse_lag_seconds` | Age of the oldest pending parse |
| `index.failing_sources` | Sources whose parse failed and waits for a retry |
| `index.quarantined_sources`, `index.quarantined` | Sources that failed 12 parses in a row, with the last error (at most 20 listed) |
| `index.refused_sources` | Sources the server refused, or purged, because an admin path rule covers them |
| `index.hidden_sessions` | Stored sessions that a path-rule change hid and that wait for a purge |
| `last_backup` | The latest `backup.complete` event among the newest 500 audit events |

`/metrics` exports Go runtime and process metrics only. There are no
ingest or search metrics yet.

Inspect structured server logs for quota, parse and audit errors.

## Release a quarantined source

A source that fails to parse 12 times in a row is quarantined. Uploads to
it still succeed; it is not parsed again until you release it.

1. Read the error in `index.quarantined` of `GET /v1/admin/status`.
2. Fix the cause, for example by upgrading the server's parsers.
3. Release the source. The server parses it again:

   ```sh
   curl -X POST -H "Authorization: Bearer $ADMIN_SESSION" \
     https://flopwire.example.internal/v1/admin/sources/<source-uuid>/reparse
   ```

## Respond to quota pressure

1. Open Collection policy in the admin console.
2. Confirm the deployment and per-user byte limits.
3. Increase the intended limit or delete an approved conversation.
4. Let the device agents resume.

flopwire never deletes the oldest traces automatically.

## Manage admin sessions and servers

Administrative commands require a current login session. They never use the
enrolled device credential. Refresh an expired session when the URL
identifies the same server:

```sh
flopwire login --server https://flopwire.example.internal --email admin@example.com
```

On an enrolled device, the login also renews the device credential. The
device keeps its ID, and its 90-day login deadline restarts.

A login to the same server keeps the certificate fingerprint already saved in
the configuration. Pass `--fingerprint` to pin a server for the first time or
after its certificate changed.

Protect an administrator's mode-`0600` configuration as an administrative
secret until the session expires.

Treat a different scheme, host, port, or base path as a server switch. Login to
the new server clears the old device credential and any pending rotation. It
keeps your own path rules. Run `flopwire enroll` on the new server. Equivalent
URL spelling, such as a trailing slash or an explicit default port, does not
clear enrollment. Use a plain base path. Flopwire rejects encoded path
separators, backslashes, repeated separators, and dot segments because proxies
can route those spellings to different backends.

Password prompts do not echo on a terminal. In scripts, pipe the password (and
its confirmation for `bootstrap` and `claim`) on standard input, one per line.

## List devices

```sh
flopwire admin devices
```

The list shows each device's owner, label, kind, creation time, last seen
time, last IP address, expiry and scopes. Kinds:

- `device`: a laptop enrolled with `flopwire enroll`.
- `ephemeral`: a sandbox or CI job that used a minted token.
- `service`: a service account's upload device.

For an enrolled device, the expiry is the earlier of the 90-day login
deadline and 30 days after it was last seen. The server removes ephemeral
devices from the list 24 hours after their token expires. Add `--json` for
the full records.

## Revoke a device

```sh
flopwire admin revoke-device <device-uuid>
```

Revocation takes effect on the next request. The command also revokes the
tokens minted from the device. Existing central traces remain.
`flopwire revoke-device` is the same command.

## Revoke a user or service account

```sh
flopwire admin revoke-user <user-uuid>
```

The command revokes every credential of the user or service account:
login sessions, device credentials and minted tokens. It also revokes every
device the user owns. The account remains. A person can log in and enroll
again. `revoke-principal` is an alias. Find the user ID in
`flopwire admin devices --json` or the web console.

## Rotate this device credential

The device agent rotates the credential every 24 hours. To rotate by hand:

```sh
flopwire rotate-device
```

The command prepares an inactive token, saves it atomically, and then commits
the replacement. The old token remains active until commit. Rerun the command
after a network interruption. The saved pending state includes its state and
expiry. The command retries an idempotent commit before it discards an expired
capability, so a lost success response cannot strand the device. An
operating-system file lock serializes concurrent local credential changes.

## Respond to "re-login required"

`flopwire agent status` shows `credential: re-login required (REASON)` when
the server refused the device credential. The agent stops uploads and
continues to index locally. Reasons:

- `credential_rotated`: another holder of the token rotated it first.
  Treat the device token as stolen.
- `reauth_required`: the 90-day login deadline passed, or the device was
  not seen for 30 days.
- `credential_revoked`: an administrator revoked the credential or the
  user.

To recover:

1. Run `flopwire login --server <url> --email <you>`.
2. Check that the command prints `device credential renewed`.
3. Run `flopwire agent status`. Uploads resume without a restart.

The login re-authenticates the same device and revokes every other
credential of the device. If the command says the device was revoked, run
`flopwire enroll`.

For `credential_rotated`, also review the audit log for `auth.failed`
events with reason `rotated_credential_reused` and for the device's
`last_ip` in `flopwire admin devices`.

`flopwire agent status` warns seven days before the 90-day deadline. Run
`flopwire login` before it passes.

## Give a sandbox or CI job a token

On an enrolled device, mint a token:

```sh
flopwire token mint --label ci-build-1234 --ttl 2h --scope upload
```

The command prints the token on standard output. It prints the other
variables on standard error. In the sandbox, set:

```sh
export FLOPWIRE_TOKEN=<token>
export FLOPWIRE_SERVER=https://flopwire.example.internal
export FLOPWIRE_FINGERPRINT=sha256:...   # only for a self-signed server
```

Then run the agent once at the end of the job:

```sh
flopwire agent run --once
```

The agent indexes, uploads, waits for the upload to finish, and exits.
Set `--sync-timeout` to change the wait (default 5 minutes). The CLI
retrieval commands with `--server` use the same variables. Use
`--scope read` for a token that only searches, and `--scope upload,read`
for both. Mint one token for each sandbox: a token registers one
ephemeral device on first use.

The uploads belong to the user who minted the token. A service account
can mint upload tokens for shared CI.

## Set the minted-token TTL cap

```sh
flopwire admin token-ttl        # show the cap
flopwire admin token-ttl 8h     # set the cap
flopwire admin token-ttl 0      # restore the default (24h)
```

A mint request for a longer TTL fails with `ttl_exceeds_max`.

## Recover a locked-out administrator

Flopwire has no email, reset-link, or self-service password recovery. Keep at
least two human administrators. If every administrator is locked out, a
trusted database operator must perform a reviewed manual recovery or restore a
verified backup. The manual transaction must update the intended human
administrator's Argon2id password hash, revoke that user's session
credentials, and append an `admin.password.manual_recovery` audit event. Stop
the public proxy during this operation. Never put a plaintext password in SQL,
shell history, or an environment variable.

## Choose the transport

The server always speaks TLS. SECURITY.md describes the modes. Pick one:

- Self-signed (the default). Nothing to configure. Print the fingerprint on
  the server host:

  ```sh
  docker compose exec flopwire flopwire fingerprint
  ```

  Give it to `flopwire bootstrap --fingerprint` or `flopwire login
  --fingerprint`. `flopwire invite` adds it to the invite string for you. Back
  up the `flopwire-tls` volume. If you lose it, the server gets a new
  fingerprint and every device must log in again with `--fingerprint`.
- ACME. Set `FLOPWIRE_DOMAIN` to the public name and, optionally,
  `FLOPWIRE_ACME_EMAIL`. Publish the listener on port 443
  (`FLOPWIRE_PORT=443`, `FLOPWIRE_BIND=0.0.0.0`). Devices need no fingerprint.
- Your own reverse proxy. Set `FLOPWIRE_TLS=proxy`. The server logs a warning
  banner that it serves plain HTTP. `deploy/nginx.conf.example` shows the
  contract. Keep the flopwire listener reachable only by the proxy. Set
  `FLOPWIRE_TRUSTED_PROXY_CIDRS` to the proxy network only, so forwarded client
  addresses are believed for rate limiting and audit.

Tune the built-in login limits with `FLOPWIRE_AUTH_RATE_BURST` and
`FLOPWIRE_AUTH_RATE_REFILL`.

The device client gives each request a deadline: 90 seconds plus one second
per 64 KiB of request body, and the same again for the response body. It
waits at most 75 seconds for response headers.

Login, invite claim and rotation commit are limited on two independent
dimensions: per client address and per account (email, invite code or
rotation). Docker Desktop on macOS and Windows forwards published ports
through one gateway address, so without a trusted proxy the server sees every
client as the same address. The per-address limit then covers all clients
together: a burst of failed logins from one person also delays everyone
else's logins until the bucket refills. The per-account limit still applies
to each account from any address, so password guessing stays bounded. For a
shared deployment on Docker Desktop, put a reverse proxy in front and set
`FLOPWIRE_TRUSTED_PROXY_CIDRS`, or raise `FLOPWIRE_AUTH_RATE_BURST`.

## Change admin path rules

Admin path rules and the `unplaceable` floor apply to every device and
also on the server. Syntax: [agent.md](agent.md#keep-sessions-out-with-path-rules).

1. Edit the rules on the console's Collection policy page, or send
   `PUT /v1/admin/policy` with `path_rules` and `unplaceable`.
2. Wait for the parse queue's next sweep. It re-checks every stored
   session against the new rules.
3. Preview what the change hid:

   ```sh
   flopwire admin policy preview
   ```

   The output counts hidden sessions per user and per rule, and says when
   the oldest is purged. `--json` prints the same data.
4. If the preview shows sessions that should stay, change the rules
   again. The sweep restores each hidden session that no rule covers.
5. To purge the hidden sessions now, confirm:

   ```sh
   flopwire admin policy purge --yes
   ```

   Add `--rule RULE` (as the preview prints it) to purge only what one
   rule hid.

If you do nothing, the server purges each session 7 days after it was
hidden. A purge re-checks the current rules first and is permanent. It uses
the deletion path below. A backup taken while a session is hidden still
holds it; purge first if the next backup must leave it out.

New uploads that an admin rule covers are refused at parse time and never
stored. Each refusal is audited as `source.refused`.

## Delete a conversation

Deletion is manual. A transcript file that a harness deletes stays
searchable. The only automatic purge is of sessions that an admin path-rule
change hid; see [Change admin path rules](#change-admin-path-rules).

The conversation's owner or an administrator can delete it. An
administrator uses the Archive control page of the console, or the admin
route:

```sh
curl -X DELETE -H "Authorization: Bearer $ADMIN_SESSION" \
  https://flopwire.example.internal/v1/admin/conversations/<conversation-uuid>
```

The owner uses the member route with a login session:

```sh
curl -X DELETE -H "Authorization: Bearer $SESSION" \
  https://flopwire.example.internal/v1/conversations/<conversation-uuid>
```

The request answers `202` with a deletion job and its `Location`. The owner
polls `GET /v1/deletions/<job-id>`. A delete
forgets the session for good:

- The conversation and all its message rows disappear from search and
  context at once.
- The delete cascades to the conversation's subagent conversations.
- The tombstone belongs to the session for its owner, on every device. Lines
  that the harness later appends to the session are dropped, and a re-upload
  from any of the owner's devices does not restore it.
- Backups taken after the delete leave out the deleted chunks.

A background worker then removes the conversation's sources when nothing else
references them, and deletes every chunk object left without a manifest
reference. Poll
`GET /v1/admin/deletions/<job-id>`. After five failed attempts the job is
`failed`; retry it with `POST /v1/admin/deletions/<job-id>/retry`. A source
that still holds another conversation (a Devin database, for example) keeps
its raw bytes; only the deleted conversation's rows go.

A separate reconciler deletes chunk objects whose upload never committed a
manifest reference, with exponential backoff while object storage is down.

## Upgrade

1. Create a coordinated backup with `--encrypted-destination`.
2. Verify the backup.
3. Pull the new source tag or container image.
4. Run `docker compose up -d --build`.
5. Check `/healthz`.
6. Run one known provenance search.

Database migrations are forward-only and run in one transaction at startup.
Before the first release, migration files are edited in place, so the server
refuses a database made by an earlier pre-release build. Back it up and start
from an empty database. Restore the verified pre-upgrade backup to roll back
durable state.

A new build that changes a parser version re-parses on the devices: each
agent re-indexes affected sources in the background. A new local index
schema version makes each agent rebuild its local index from the
transcripts at its next start. Search works again when the rebuild ends.

## Recover

1. Stop writes to the damaged deployment.
2. Provision empty Postgres and object storage targets.
3. Run `flopwire backup-verify` against the selected backup.
4. Run `flopwire restore` against the new targets.
5. Start the server.
6. Run one known provenance search.

The bundled Compose volumes are not encrypted by flopwire. They can contain raw
trace text, PostgreSQL records, and object bytes. Use encrypted host storage
when the deployment threat model requires encryption at rest. Restrict host
and volume access. The backup command always requires an explicit
acknowledgement that its destination is encrypted.

The backup command takes one Postgres snapshot, lists the committed chunk
objects from it, dumps the database from the same snapshot, and copies each
object, checking it against its BLAKE3 content address. It holds a shared
purge lock: uploads continue and deletion requests take effect at once, but
the physical purge waits for the backup to finish. A backup is valid only
after `manifest.json` exists and `backup-verify` succeeds.

Restore refuses a database with any user objects or a bucket with objects.
It uploads and re-verifies every object, restores the dump, and checks that
the restored chunk inventory matches the manifest. Discard a partially
restored target; do not point traffic at it. Restore writes
`restore-state.json` beside the input manifest by default. Use
`--state-output` when the backup is mounted read-only. Treat `incomplete` and
`failed` targets as invalid and non-resumable.

### Upgrade for device-scoped withholding

Migration `006_device_withholds.sql` preserves existing tombstones as
user-wide and adds device-scoped tombstones for delegated upload credentials.
Stop all older server instances before starting the upgraded server. Older
ingesters interpret every tombstone as user-wide and cannot safely share this
migrated database while the new server accepts device-scoped requests.

`POST /v1/conversations/withhold` requires a bound device with upload scope.
A `202` records permanent intent, including before the first upload commits.
A member's enrolled credential with read and upload scopes retains user-wide
behavior. Minted tokens, service credentials, and upload-only device credentials
affect only their bound device. Read-only credentials and login sessions receive
`403`.
