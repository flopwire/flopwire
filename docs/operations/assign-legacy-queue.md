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
2. Create one backup while the collector remains stopped. Include the index,
   all search shards, and every SQLite WAL and SHM sidecar. Include the complete
   client config directory and spool. Include the installed collector binary
   and its launchd plist. Keep these files together as one matching snapshot.
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

## Roll back an assignment or upgrade

1. Stop the collector service and any foreground collector.
2. Save the current index, search shards, SQLite sidecars, config, and spool in
   a separate snapshot. Preserve this post-upgrade state for recovery.
3. Restore the complete matching stopped snapshot from before assignment.
4. Restore the binary and launchd plist from that same snapshot.
5. Restart the restored collector with its saved retention mode and settings.

Do not run an old binary against the newly scoped state. Restore the matching
state and binary together. A rollback can repeat uploads acknowledged after the
snapshot. Keep the post-upgrade snapshot until you have reconciled those changes.
