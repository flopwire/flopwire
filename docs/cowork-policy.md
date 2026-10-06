# Cowork policy metadata

The server accepts authenticated policy metadata before transcript content. The upload credential determines the device. A native session UUID on another device does not establish a policy association.

`GET /v1/sync/capabilities` currently advertises `policyplacements_version: 0` and one concurrent flush. Collection clients must keep Cowork uploads held until the advertised policy version is 1. Enabling that version requires qualification of the real client and server together.

`POST /v1/sync/policyplacements` accepts host folder placements, canonical native session and verified parent identities, captured-history scope, current mapping readiness, source-generation references, and an optional device directory snapshot. Its successful response contains the exact typed request digest. Clients must verify that digest and recheck their local capture revision before releasing content.

The ledger retains observed host placements and captured historical uncertainty. A current complete mapping does not prove the scope of older bytes. Metadata-only readiness can recover without tainting content that first arrives with a complete mapping. Actual folder restrictions and captured historical uncertainty propagate through verified same-device session components. Current unreadiness of an uncaptured placeholder holds only that source.

Selected folders use the strictest rule anywhere in their subtree. Worktree and main repository roots use direct placement rules. Restrictive repository rules with unresolved nested repository coverage hold sharing locally. Logical paths and physical aliases must come from the collecting host; the server does not resolve Mac paths on Linux.

Historical unmapped scope holds sharing locally. Configured unplaceable exclusion makes that history Deny, even when current host folders are known. Actual folder Deny takes precedence over uncertainty.

Recovered aliases require the exact stored source path, file identity, generation, native identity, original transcript path, and recovered-history provenance on the authenticated device. These references can add restrictions; they cannot provide folder grants. A separate recovery device requires an additional verified origin association and is outside this same-device contract.

Physical source ownership is immutable across generations. Parser versions and repository metadata can change, while agent, native owner, main or companion role, exact parent identity, and parser family cannot silently change. Conflicting metadata or uploads receive `409 policy_source_identity_conflict`.

The server limits each metadata array to 256 entries and request bodies to 1 MiB. A well-formed count, union, component, or reconciliation overflow durably holds the affected device component and hides existing copies before returning 413. Oversized bodies that cannot be parsed receive 413 without an inferred identity. Clients must first send a compact canonical `scope_status: limit-held` revocation, then bounded source and recovery reference batches. This capacity state does not invent unmapped history or permit expiry deletion. Ordinary complete requests cannot clear it; controlled scope review is required.

A recorded nonempty device home cannot change or clear while Cowork policy is bound to that identity. `409 device_home_conflict` requires a new device identity or a controlled home migration. Retrying the same conflicting snapshot cannot resolve it.

Policy registration and companion manifest commits take an exclusive device gate. Other manifest commits and parsing take the shared gate and recheck current policy before publishing. Rule reconciliation retains the collection-rule lock through restoration and acknowledgement. Hiding and purging preserve unrelated devices and already deleted history.
