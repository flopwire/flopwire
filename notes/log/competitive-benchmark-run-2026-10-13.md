# Run the pilot

Record of 2026-10-13. The runner scripts it invokes (`scripts/competitive-pilot.py`,
`scripts/competitive-messaging.py`, `scripts/competitive-pilot-test.py`) and the
result files were not retained; this records the procedure as it was run.

Requires Python 3.10+, Flopwire, AgentsView and Agent Mail binaries, and a
dedicated local Langfuse project. Product flags are optional. Each run uses
a fresh output directory with isolated product state.

## Measure retrieval

```sh
python3 scripts/competitive-pilot.py \
  --root /tmp/competitive-retrieval-run-1 \
  --flopwire /tmp/flopwire-pilot \
  --agentsview /tmp/pilot-tools/agentsview \
  --langfuse-env /tmp/pilot-langfuse/.env \
  --langfuse-url http://127.0.0.1:43180 \
  --rg /opt/homebrew/bin/rg \
  --runs 30 --seed 1
```

Omit a product flag to skip that product. The output includes
`manifest.json`, `results.json`, first response files and a server log.
Each CLI query runs in a fresh process; services remain warm. Failures and
attribution mismatches are retained in `results.json`.

## Measure messaging

```sh
python3 scripts/competitive-messaging.py \
  --root /tmp/competitive-messaging-run-1 \
  --flopwire /tmp/flopwire-pilot \
  --agent-mail /tmp/pilot-tools/am \
  --runs 30 --seed 1
```

Keep the output path short enough for a Unix socket. The runner starts
both local servers. Agent Mail uses an available loopback port and a fresh
archive. Flopwire indexes generated transcripts and uses recent activity
as its presence signal. Synthetic hooks retrieve messages. Recovery uses
a separate Flopwire sender to respect the default hourly send allowance.

The output includes `messaging-results.json`, a corpus manifest and server
logs. The runner stops its own processes after the run. It does not run a
model or register sessions on the user's message network.

## Check results and stop Langfuse

```sh
python3 scripts/competitive-pilot-test.py
```

The runners stop their own processes. Inspect failures in the result files,
then run `docker compose down` for the dedicated Langfuse project. Keep its
volumes and scratch evidence until review is complete.

The pilot measures exact retrieval and transport. See the
[specification](competitive-benchmark-spec-2026-10-13.md) for relevance, model-visible delivery and agent
outcome experiments.

<details>
<summary>Install pilot binaries</summary>

1. Build Flopwire from the commit under test.

   ```sh
   go build -trimpath -o /tmp/flopwire-pilot ./cmd/flopwire
   ```

2. Download AgentsView `v0.45.0` from its
   [release](https://github.com/kenn-io/agentsview/releases/tag/v0.45.0).
3. Check its archive against the release's `SHA256SUMS`.
4. Extract the binary into a pilot tools directory.
5. Download Agent Mail `v0.3.38` from its
   [release](https://github.com/Dicklesworthstone/mcp_agent_mail_rust/releases/tag/v0.3.38).
6. Check its archive against the release's checksum file.
7. Extract the binaries into the same tools directory.

Run from a checkout that the Agent Mail server can access; its release
may discover a web asset folder from the current directory.

</details>

<details>
<summary>Set up the isolated Langfuse project</summary>

Follow Langfuse's [Docker Compose guide](https://langfuse.com/self-hosting/deployment/docker-compose).
The pilot used the upstream compose file at commit
`3eb71e89be4fcb52e9b9015b1e4090982d826945`. The
`metadata.json` result file recorded the measured image digests.

1. Download the upstream compose file into a separate scratch directory.
2. Give the compose project a unique pilot name.
3. Remove published ports from every service except `langfuse-web`.
4. Bind the web port to `127.0.0.1:43180:3000`.
5. Set `NEXTAUTH_URL=http://localhost:43180`.
6. Generate new database, Redis, object-store and application secrets.
7. Save the values in a mode-0600 `.env` file in the scratch directory.
8. Set `TELEMETRY_ENABLED=false`.
9. Set the `LANGFUSE_INIT_*` variables described in the
   [headless initialization guide](https://langfuse.com/self-hosting/administration/headless-initialization).
10. Use a new project ID and API key pair for each measured run.
11. Start the compose project.
12. Wait for `/api/public/health` to return `OK`.

The retrieval runner reads `LANGFUSE_INIT_PROJECT_PUBLIC_KEY` and
`LANGFUSE_INIT_PROJECT_SECRET_KEY` from the supplied `.env` file. It does
not print those values or copy them into results. It ingests synthetic
content through OTLP with `x-langfuse-ingestion-version: 4`, then queries
Observations API v2. Older deployments may require a different adapter.

Pass only the local pilot `.env` file. The runner will write synthetic
observations to the project named by those keys.

</details>
