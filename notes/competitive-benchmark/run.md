# Run the pilot

Use Python 3.10 or later. The runners use the Python standard library.
Use a fresh output directory for each run. Existing output is preserved.

## Prepare the products

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

The runners never install plugins or change the user's agent configuration.
They create isolated homes and product state below the output directory.
Run them from a checkout that the Agent Mail server can access; its release
may discover a web asset folder from the current directory.

## Prepare Langfuse locally

Follow Langfuse's [Docker Compose guide](https://langfuse.com/self-hosting/deployment/docker-compose).
The pilot used the upstream compose file at commit
`3eb71e89be4fcb52e9b9015b1e4090982d826945`. The
[metadata](results/2026-10-13/metadata.json) records the measured image digests.

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

## Measure retrieval

Replace the example binary paths with the extracted binaries.

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
The runner stops its own AgentsView process after the run. Each CLI query
runs in a fresh process; the AgentsView service remains warm. Langfuse
uses direct HTTP requests to a persistent service. The runner preserves
command failures and mismatched attribution in `results.json`.

For this dated pilot, `rg` was included in the same randomized schedule.

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

## Validate and stop

1. Run the attribution and scoring checks.

   ```sh
   python3 scripts/competitive-pilot-test.py
   ```

2. Inspect every command failure and correctness failure in the results.
3. Keep unsupported cases separate from failures.
4. Record the product releases and image digests.
5. Stop the dedicated Langfuse compose project with `docker compose down`.
6. Retain its volumes and the scratch evidence until review is complete.

The pilot runners do not measure cold filesystem caches, model-visible
delivery, crash durability, relevance ranking or agent task success. Use
the [specification](README.md) for those experiments.
