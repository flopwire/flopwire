# Shared grep startup investigation, 2026-10-12

The reported broad case-insensitive grep exhausted its budget before returning
its first candidate page. The existing implementation already uses trigram
candidates, including finite literal alternatives, then verifies matches in Go.

Read-only production measurements found 1,693 visible sessions for the reported
repository filter. Session preselection completed in approximately 47 ms. Both
the existing ILIKE candidate shape and a PostgreSQL regular-expression variant,
using those session IDs, exceeded a bounded three-second execution budget. These
measurements do not isolate CPU, disk, or sort cost. They do not establish that a
native regular expression would improve latency.

This change removes ordering from session-ID preselection. That list has no
ordering semantics: all IDs are used up to the 5,000-session cap; above the cap,
the complete list is discarded. Candidate messages retain their time ordering.

An isolated A/B fixture measured preselection at 1.01 to 0.91 ms for a rare query
and 10.26 to 6.81 ms for a broad query. First-page retrieval remained approximately
1.35 seconds in the broad case. This is a modest startup improvement, not a
solution to all broad-query timeouts.

Timeout diagnostics now name the operation underway: acquiring the read
transaction, resolving filtered sessions, declaring the cursor, or fetching the
first page. Tests interrupt each stage and cover the exact cap, the cap plus one,
and hidden-session exclusion. No production data or configuration was changed.
