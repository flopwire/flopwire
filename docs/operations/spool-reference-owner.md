# Serial spool reference owner

The private `withReferences` API defines one process-local owner for an actual
Store and Spool pair. Capture holds it through publication and manifest commit
or abort. Nested salvage, cut, watermark changes, and conditional release use
the same scope. Upload joins it after a response to commit acknowledgement or
loss and release bytes. Startup sweep joins it before workers start.

The serial Syncer remains required. This change does not enable parallel workers
or qualify independent Syncers sharing a spool. Export materialization runs
outside reference ownership under the serial Syncer mutex. Has and Flush
transport also run outside reference ownership.

The first valid callback binds the Spool to its Store wrapper. Later callbacks
must use that same wrapper. Nil arguments and a different wrapper fail before
the callback runs. A newly opened wrapper needs a new Spool owner after the old
owner stops. The collector's process-level index lock remains required.

Use the supplied scope for publication, capture commit, and conditional release.
Keep it through commit or abort. Pass it explicitly to nested work. Do not
reacquire reference ownership inside a callback or retain the scope after the
callback returns. This is a private lexical contract, not a runtime lease or
reference cache.

The lock order is the reference mutex, then SQLite or the file mutex. Reference
queries complete before file inspection and unlink. No SQLite transaction spans
spool IO. No file bookkeeping lock spans a reference query. Neither mutex may
span network transport. Chunk and tail reads return independent byte copies
under the existing file mutex; this API adds no reader pins.

Owned chunk publication validates the supplied hash and any existing file before
reuse. Conditional chunk release queries only the supplied hashes in bounded
batches. Orphan manifests and invalid generation metadata stop release. Audit
rows beyond a cut generation's retained entry count are not pending references.
Regular file type, size, and body hash must validate before unlink and accounting
changes. Symlinks, directories, FIFOs, and corrupt bodies retain their charge.
A missing file causes no accounting decrement. Tail release retains a currently
needed identical hash and validates the immutable or canonical bytes before
removal. Cancellation and query failure do not authorize deletion.

Startup and cap-pressure tail cleanup use operation-local plans. File inventory
runs under the file mutex. Exact reference queries then run without that mutex.
Cleanup checks all reference decisions before deletion and retains the reference
owner throughout. The plan is discarded after that operation. Recognized corrupt
unreferenced chunks stop startup instead of being silently deleted. This is a
held initialization error, not automatic corruption repair.

Cleanup failure after a durable capture or acknowledgement leaves that commit
valid. It logs the error and retains charged bytes for a later safe cleanup.
Source-local retry reclamation still excludes chunks and other sources' files.
The owner does not guarantee progress at the cap.

The tests hold the owner at actual publication and reference-check boundaries.
They check real SQLite references, charged files, commit or guard rejection,
cleanup followed by republish, and lock-free export and transport callbacks.
Cross-process spool ownership, general reclamation, parallel capture, and
power-loss durability remain separate work. Stop and join workers before closing
the Syncer; ownership does not replace the collector's exclusive index lock.
