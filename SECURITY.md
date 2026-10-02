# Security policy

## Security boundary

flopwire archives agent traces. Those traces can contain source code,
personal information, tool output, and prompts. Secrets are redacted on the
device before upload and again on the server, but redaction is pattern-
based: a credential it does not recognize is stored as written.

- Deployment administrators and anyone with host-level access are trusted to
  read the complete corpus.
- Every human member can search and retrieve the organization-wide corpus.
- Human device tokens grant upload and member read access. They never grant
  administrative access, even when the device belongs to an administrator.
- Minted tokens (`flopwire token mint`) grant only the scopes they name
  (`upload`, `read`, or both), never more than the minting credential
  holds, and never administrative access, device rotation, conversation
  deletion, message redaction or further minting.
- Administrative APIs require a human administrator's short-lived login
  session credential. The CLI stores that session separately from its device
  token in the mode-`0600` configuration file so collection never uses it.
- Service accounts are upload-only unless a future administrator explicitly
  grants broader access. A token a service account mints is upload-only
  too.
- Path rules (deny, local, repo: rules and the unplaceable setting) are
  enforced in two layers:
  - The device agent is the primary enforcer. It applies the administrator's
    rules and the member's own rules before indexing or transmission. It
    resolves each session's git worktree, main checkout, remote and
    symlinks on the device. A member's own rules are not sent to the
    server.
  - The server applies the administrator's rules again when it parses an
    upload, as a floor against an old, modified or faulty agent. A new
    session that a deny or local rule covers, or an unplaceable session
    under an `unplaceable` floor of `local` or `exclude`, is not stored: no
    conversation or message rows, and its raw evidence is dropped under the
    purge lock. The refusal is audited (`source.refused`, with the user,
    device, source and rule), counted in the admin status
    (`refused_sources`), and reported to the device in the flush answer
    (`flopwire agent status`). The server sees only what the transcript
    recorded: its working directory, the git remote Codex recorded, and the
    Claude project folder name. It cannot resolve worktrees, main
    checkouts or symlinks, so it can miss a session that the agent catches.
  - `~` in a rule is the home directory the device reports with each
    upload (with its Claude projects and Codex directories). Until a device
    has reported one, the server infers the home from the harness
    directory in the transcript's path (`/Users/me/.claude/...` is
    `/Users/me`). That inference is wrong when `CLAUDE_CONFIG_DIR` or
    `CODEX_HOME` lies outside the home: a `~` rule then misses sessions
    (or covers another tree) until the device reports its home. A device
    reports only its own home, so it cannot steer the rules for another
    device's or user's sessions.
  - When the administrator's rules change, stored sessions are hidden, not
    deleted. The server re-checks every stored conversation. A session the
    new rules cover is hidden at once, with its subagents and the same
    session on the user's other devices: search, grep, sessions, read and
    raw leave it out. The hide records the rule, the rules version, the
    time and the administrator, and is audited (`conversation.hidden`). If
    a later rule change no longer covers the session (the rule is removed,
    or edited so it no longer matches), the session is restored
    (`conversation.restored`). An administrator previews the hidden
    sessions per user and per rule (`flopwire admin policy preview`,
    `GET /v1/admin/policy/hidden`, and `hidden_sessions` in the admin
    status) and can confirm their purge now (`flopwire admin policy purge
    --yes`). Otherwise the server purges a session once it has been hidden
    for seven days. A purge re-checks the current rules, then runs the
    normal deletion path (tombstone, purge job, backups exclude the purged
    chunks) and is audited (`conversation.purged`, with the reason). A
    purge is permanent: removing the rule later does not bring the session
    back. New uploads to a hidden session are stored hidden, so a restore
    brings the session back whole.
  - Backups and path rules. A backup copies what the database and object
    store hold at that moment:
    - A backup taken between an upload and its parse can hold raw
      evidence that the parse then refuses. The refusal does not reach
      that backup.
    - A hidden session is still in the database and the object store until
      it is purged, so a backup taken while it is hidden holds it. To keep
      it out of the next backup, confirm the purge first.
- Local file deletion does not delete the central archive.
- Deletion is manual, except the purge of sessions a path-rule change hid
  (above). A conversation's owner or an administrator can
  delete it. A delete forgets the session for good: see
  [docs/runbook.md](docs/runbook.md#delete-a-conversation).
- The organization administrator, not each member, authorizes collection.
  There is no per-member consent gate. Administrators must tell members that
  eligible traces become team-visible and indefinitely retained, with only
  recognized secrets redacted.

### Messages between people

The message bus lets agent sessions message each other
([notes/message-bus/plan.md](notes/message-bus/plan.md)). A message from
another person's session is held until the recipient accepts that person
(B7). Accepting is the trust decision:

- An accepted person's agents can send messages to all of the recipient's
  agent sessions, including sessions that run with permission prompts
  turned off. The recipient's agents may act on those requests within
  each session's own permissions.
- Each delivered message is marked `sender="teammate"`, and a standing
  instruction tells the model to treat it as information and confirm with
  its human before consequential actions. That rule is guidance to the
  model, not a boundary. Smaller models act on a teammate's request,
  including one that claims to come from the recipient's own user (#77).
- A prompt-injected agent can pass the injection to every session whose
  owner accepted its owner.
- Accepting and revoking need the person's own login session (the web
  console, or the CLI with the session `flopwire login` saved). A device
  token, a minted token or a service account is refused. No route accepts
  for another person, so an administrator cannot accept for a member. The
  CLI refuses unless stdin is a terminal, and there is no MCP tool.
  Accepting also needs the person's password, checked by the server and
  limited like login: an agent that runs as the same OS user can read the
  saved login session from the configuration file (24 hours), or run the
  CLI under a pseudo-terminal, but it does not have the password. With
  the saved session alone such an agent can still list held previews and
  revoke a sender; it cannot accept one.
- Held messages are shown only to the recipient's login session, as a
  first-line preview; the whole body never leaves the server before
  acceptance. The hook tells the person about held messages through a
  channel the model does not see (Claude Code and Codex); it never puts
  them in model context. The notice is recorded in the harness
  transcript, which is uploaded like the rest of it.
- Revoking holds the sender's undelivered messages again, and the
  recipient's devices drop them at once. A message a session already
  received cannot be recalled.
- Accepts, revokes, and reads of the held and accepted lists are audited
  (`bus.accept`, `bus.revoke`, `bus.held` with the message ids shown,
  `bus.accepts`).

Audit events are retained indefinitely. User, device, member-policy, status,
and audit-log reads are audited. Failed authentication and authorization are
audited. Security-sensitive reads and identity mutations fail closed (`503`)
when an audit event cannot be stored. Identity mutations commit together with
their audit event and re-check the acting credential inside the same
transaction, so a revocation cannot race a privileged write. Retrieval
events record the full query and the result IDs, which makes the audit log
itself sensitive.

Flopwire does not encrypt live Postgres, MinIO, or Docker volumes at the
application layer. Host disk access, Docker volume access, storage snapshots,
or direct database access can expose raw traces and audit queries. Use
host-level access controls appropriate for that risk.

## Transport

The server always speaks TLS. `flopwire serve --tls` selects how:

- `auto` (the default) uses `acme` when a domain is configured and
  `self-signed` otherwise.
- `self-signed` generates an ECDSA P-256 certificate on first start and keeps
  it in the TLS directory (`FLOPWIRE_TLS_DIR`; the Compose volume
  `flopwire-tls`). The key file has mode `0600`. Devices trust the certificate
  by its SHA-256 fingerprint, not by a certificate authority. `flopwire serve`
  logs the fingerprint and `flopwire fingerprint` prints it on the server host.
  `flopwire invite` puts the fingerprint in the invite string, `flopwire claim`
  saves it in the device configuration, and every later connection (admin
  commands, `enroll`, `--server` retrieval, the raw fallback, the admin
  path-rules fetch and device sync) refuses a certificate with another
  fingerprint before any credential is sent. A lost TLS directory is a new server identity: every device must log
  in again with the new fingerprint.
- `acme` (`--domain` / `FLOPWIRE_DOMAIN`) obtains a Let's Encrypt certificate
  through the TLS-ALPN-01 challenge. The listener must be reachable on public
  port 443. Devices verify it against the system trust store; no pin is
  needed.
- `proxy` serves plain HTTP for an operator's own TLS-terminating reverse
  proxy and logs a warning banner at startup. The listener must then be
  reachable only by that proxy. `deploy/nginx.conf.example` shows the proxy
  contract.
- `off` serves plain HTTP and refuses any listen address that is not
  loopback. It exists for local tests.

The client refuses plain `http://` to any host that is not loopback, and it
refuses `http://` altogether once a fingerprint is pinned. The pin replaces
certificate-chain and hostname checks, so the self-signed certificate works
for any address the server is reached at, with no VPN or private DNS.

Configure `FLOPWIRE_TRUSTED_PROXY_CIDRS` with only the proxy network. flopwire
ignores `X-Forwarded-For` from other peers. Independent built-in token buckets limit
login, invite claim, and rotation commit by both trusted client IP and
account, invite, or rotation identifier. Both dimensions must allow a request.
Each per-process bucket set has bounded, idle-evicted storage. It uses four
protected ways for known clients and one shared overflow allowance for unseen
clients that collide with a full set. This prevents identity churn from
resetting a known depleted bucket and lets an occasional new client through.
Sustained churn can exhaust a set's overflow allowance and temporarily reject
a legitimate colliding client. Put an external edge limiter in front of flopwire
for deployment-wide enforcement and untrusted high-volume traffic.

## Credentials

The invite string holds the one-time invitation code. Send it to the member
over a channel you trust: whoever claims it first becomes that member. The
code and fingerprint ride in the URL fragment, which is never sent in a
request.

The network API cannot create the first administrator. Run `flopwire bootstrap`
on the server host with `DATABASE_URL`. Bootstrap, login, and invitation claim
never accept passwords in arguments or environment variables: they prompt
without echo on a terminal, and read one line per prompt from standard input
otherwise. Bootstrap atomically creates the admin, credential, and audit
event. There is no self-service password recovery.

Treat an administrator's local configuration as an administrative secret while
its login session is valid. Run `flopwire login` again when that session expires.
An enrolled configuration that has no separate session credential fails closed
for admin commands and also requires a fresh login.

### Device credentials (laptops)

An enrolled device holds one credential, bound to the device. The model
follows Tailscale's node keys:

- **Rotation.** The device agent rotates the credential every 24 hours
  (`flopwire rotate-device` does it by hand). Rotation takes two steps: the
  server issues an inactive replacement and a one-time commit token; the
  client saves both, then commits. The old credential stops working at
  commit, so a crash at any step leaves a working credential, and a copy
  of the old token is useless after the next rotation.
- **Theft detection.** If someone who copied the token rotates it first,
  the device's own next request is refused with `credential_rotated`. The
  agent then stops uploading (it keeps indexing locally), records the
  refusal in its configuration, and `flopwire agent status` says
  "re-login required". A rotated-away token presented more than two
  minutes after its rotation is audited as `auth.failed` with reason
  `rotated_credential_reused` and the device ID.
- **Interactive re-login.** A device credential lives 90 days from the
  `flopwire login` (or `flopwire enroll`) that issued it. Rotation keeps the
  deadline; it never extends it. A device unseen for 30 days is refused
  too. The server enforces both (`reauth_required`), from the credential's
  `expires_at` and the device's `last_seen_at`; the agent warns in
  `flopwire agent status` seven days ahead.
- **Recovery.** `flopwire login` on an enrolled device re-authenticates the
  same device (`POST /v1/devices/{id}/reauth`, audited as
  `device.reauth`): it issues a new credential with a new 90-day deadline
  and revokes every other credential of the device, including one a thief
  rotated to. The device keeps its ID, so its uploads stay its own. A
  revoked device cannot be re-authenticated; enroll a new one.

### Minted tokens (sandboxes and CI)

A short-lived scoped token for a machine that cannot hold a device
credential, in the manner of GitHub App installation tokens:

- `flopwire token mint --label NAME --ttl 2h --scope upload,read` mints one
  from an enrolled device, a service account, or a login session. A minted
  token cannot mint.
- The token belongs to the minting user: uploads are attributed to that
  user, or to the service account that minted it. Scopes are `upload`
  (sync and the admin path rules), `read` (search, grep, sessions, read,
  raw) or both.
- The TTL defaults to one hour. An administrator sets the cap
  (`flopwire admin token-ttl`, policy field `max_token_ttl_seconds`,
  default 24 hours). A token never outlives the credential that minted it.
- The sandbox sets `FLOPWIRE_TOKEN`, `FLOPWIRE_SERVER` and, for a
  self-signed server, `FLOPWIRE_FINGERPRINT`. The agent and the CLI then
  need no configuration file and no enrollment; nothing is written to disk
  except the local index and upload spool.
- The first use registers an ephemeral device that carries the label and
  belongs to the minting user (`device.register`). After the token expires
  plus a 24-hour grace, the server sweeps the device from the device list.
  The device row, its uploads and its audit events stay.
- Revoking the minting device revokes the tokens it minted and their
  ephemeral devices. Revoking a user revokes everything.

### Revocation and audit

`flopwire admin revoke-device` revokes a device, its credentials and the
tokens minted from it. `flopwire admin revoke-user` (alias
`revoke-principal`) revokes every credential a user or service account
holds or minted, and every device it owns (`principal.revoke`, with
counts). The account itself stays: a person can log in again. Mints are
audited as `token.mint`, expiries and idle lapses as `credential.expire`
(the server sweeps every five minutes), and sweeps as `device.sweep`.
`flopwire admin devices` lists each device's user, label, kind (`device`,
`ephemeral`, `service`), creation, last seen, last IP, expiry and scopes.

### Identity providers (future)

Login is by email and password today. The seam for a hosted OIDC login
is the login session: an OIDC callback would verify the ID token (issuer,
audience, nonce, `email_verified`), map the subject to a user, and issue
the same 24-hour session credential that `POST /v1/login` issues. Device
enrollment, re-authentication, minting and administration then work
unchanged, because they take a login session, not a password. A CI
system's own OIDC token (GitHub Actions, for one) could likewise be
exchanged for a minted token by a trust policy on issuer, repository and
branch, in place of a minted token stored as a CI secret. Neither is
implemented.

## Reporting vulnerabilities

Do not open a public issue for a suspected vulnerability. Until a dedicated
security contact is published, use GitHub's private vulnerability reporting on
the repository.

## Supported versions

Security fixes are provided for the latest tagged release only.
