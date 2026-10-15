# Assign an ownerless upload queue

Older collectors did not record the destination of saved upload state. Current
collectors refuse to start a connected agent with that state until you assign
its owner. The owner covers source watermarks, generations, manifests, known
hashes, acknowledgments, and captured bytes.

Establish the historical server and device from your enrollment records.
The command cannot infer provenance from an ownerless queue. Matching the
current enrollment does not prove the historical owner. Assign only when you
know that the saved state belongs to that server and device.

1. Stop the collector service and any foreground collector.
2. Back up the index, its SQLite sidecars, and the client config directory.
3. Run the command with the existing index and the historical enrollment:

   ```sh
   flopwire agent assign-legacy-queue \
     --db /absolute/path/index.db \
     --server https://your-server.example \
     --device-id YOUR-ENROLLED-DEVICE-ID
   ```

4. Restart the collector with its existing retention mode and settings.

The command requires the current enrollment to match the explicit server and
device. It holds the index writer lock and the config lock throughout assignment.
It makes no network requests. It does not migrate the index or change retention,
source state, acknowledgments, or capture formats. It adds only the authority
metadata needed to bind that state.

Legacy captures under the config directory's `spool/chunks` and `spool/tails`
are copied into a new private spool scope. Files and directories are synced
before the authority transaction commits. The originals remain available for
recovery. Do not use an older connected collector against these originals after
assignment. Do not remove them until you have verified recovery and completed
your normal backup policy.

An interrupted assignment leaves a small `.assign-*.json` journal in `spool`.
Run the same command again to resume its recorded destination and spool scope.
A retry verifies existing copied bytes and refuses conflicting files. A completed
assignment is a no-op for the same owner, so a retry cannot restore captures that
have since been acknowledged. Another owner is refused. Do not edit or delete an
unfinished journal to change ownership.
