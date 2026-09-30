Mode: Operate

Scope: The authenticated V1 administration console. It covers health, users,
devices, policy, audit, deletion, quotas, and backup status. It does not expose
corpus search.

Audience and job: A self-hosting operator needs to see whether traces are
flowing from collectors into durable storage and the CASS index, identify the
responsible user or device, and take a safe corrective action.

Primary task: Diagnose system health first. Enrollment and audit remain one
route away. Destructive actions must state their exact corpus scope.

Direction: Ref Log Control Plane. The memorable moment is a single causal relay
from Collector to Archive to Index to Retrieval, with current thresholds and
the most recent attributable transition visible in place.

Constraints: Restrained expression, no generic metric-card dashboard, WCAG 2.2
AA, useful on mobile, and honest empty/loading/degraded states. All displayed
data comes from the API; illustrative values must be labeled.
