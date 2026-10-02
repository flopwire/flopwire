/**
 * THESIS: flopwire is an attributable ref log, not a metric-card dashboard.
 * OWN-WORLD: Cool paper, green-black ink, hairline structure, and state-only color.
 * STORY: See the ingest relay, locate the failing transition, act with provenance.
 * FIRST VIEWPORT: Health state and Collector → Archive → Index → Retrieval relay lead.
 * FORM: Ref Log Control Plane, health-first relay staging, seed 48d693fe.
 */
import {
  FormEvent,
  ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  Accepted,
  AcceptResult,
  AdminStatus,
  APIError,
  AuditEvent,
  DeletionJob,
  Device,
  Health,
  HeldGroup,
  Policy,
  request,
  User,
} from "./api";
import { ACCEPT_STATEMENT, plural } from "./messaging";

type Route = "health" | "people" | "policy" | "archive" | "audit" | "messages";
type Session = { token: string; user: User };
const SESSION_KEY = "flopwire.admin.session";
const DELETION_KEY_PREFIX = "flopwire.admin.deletion.";
type StoredDeletion = {
  job: DeletionJob;
  observed_at: string;
  stale: boolean;
};

function loadSession(): Session | null {
  try {
    return JSON.parse(
      sessionStorage.getItem(SESSION_KEY) ?? "null",
    ) as Session | null;
  } catch {
    return null;
  }
}

function loadDeletionTracking(
  key: string,
  expectedUserID: string,
): StoredDeletion | null {
  try {
    const value = JSON.parse(sessionStorage.getItem(key) ?? "null") as
      | StoredDeletion
      | null;
    if (
      !value ||
      !value.job ||
      !isNonEmptyString(value.job.id) ||
      !isNonEmptyString(value.job.conversation_id) ||
      value.job.requested_by !== expectedUserID ||
      !isDeletionState(value.job.state) ||
      !Number.isInteger(value.job.attempts) ||
      value.job.attempts < 0 ||
      !isValidDate(value.job.requested_at) ||
      !isOptionalDate(value.job.next_attempt_at) ||
      !isOptionalDate(value.job.completed_at) ||
      (value.job.last_error !== undefined &&
        typeof value.job.last_error !== "string") ||
      !isValidDate(value.observed_at) ||
      typeof value.stale !== "boolean"
    ) {
      sessionStorage.removeItem(key);
      return null;
    }
    return value;
  } catch {
    sessionStorage.removeItem(key);
    return null;
  }
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0;
}

function isValidDate(value: unknown): value is string {
  return typeof value === "string" && Number.isFinite(new Date(value).getTime());
}

function isOptionalDate(value: unknown): value is string | undefined {
  return value === undefined || isValidDate(value);
}

function isDeletionState(value: unknown): value is DeletionJob["state"] {
  return ["queued", "purging", "retry_wait", "complete", "failed"].includes(
    String(value),
  );
}

function saveDeletionTracking(key: string, value: StoredDeletion) {
  sessionStorage.setItem(key, JSON.stringify(value));
}

export function App() {
  const [session, setSession] = useState<Session | null>(loadSession);
  if (!session)
    return (
      <Login
        onLogin={(next) => {
          sessionStorage.setItem(SESSION_KEY, JSON.stringify(next));
          setSession(next);
        }}
      />
    );
  return (
    <Console
      session={session}
      onLogout={() => {
        sessionStorage.removeItem(SESSION_KEY);
        setSession(null);
      }}
    />
  );
}

function Login({ onLogin }: { onLogin: (session: Session) => void }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      onLogin(
        await request<Session>("/v1/login", "", {
          method: "POST",
          body: JSON.stringify({ email, password }),
        }),
      );
    } catch (reason) {
      setError(
        reason instanceof Error
          ? reason.message
          : "Login failed. Check the server and try again.",
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="login-shell">
      <section className="login-intro" aria-labelledby="login-title">
        <Brand />
        <h1 id="login-title">Operate the memory pipeline.</h1>
        <p>
          Inspect collection, archive durability, index freshness, access, and
          every administrative mutation.
        </p>
        <div className="mini-relay" aria-label="Collector to retrieval flow">
          <i />
          <i />
          <i />
          <i />
        </div>
      </section>
      <form className="login-form" onSubmit={submit}>
        <div>
          <span className="porcelain">ADMIN ACCESS</span>
          <h2>Sign in</h2>
          <p>Use a local account created by an administrator.</p>
        </div>
        <Field
          label="Email"
          type="email"
          value={email}
          onChange={setEmail}
          autoComplete="username"
        />
        <Field
          label="Password"
          type="password"
          value={password}
          onChange={setPassword}
          autoComplete="current-password"
        />
        {error && (
          <div className="error" role="alert">
            {error}
          </div>
        )}
        <button
          className="button primary"
          disabled={busy || !email || !password}
        >
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </main>
  );
}

function Console({
  session,
  onLogout,
}: {
  session: Session;
  onLogout: () => void;
}) {
  const [credentialMutationPending, setCredentialMutationPending] =
    useState(false);
  const [transitionWarning, setTransitionWarning] = useState("");
  // Every person reviews their own held messages; the other routes are
  // an administrator's.
  const isAdmin = session.user.role === "admin";
  const labels: Partial<Record<Route, string>> = isAdmin
    ? {
        health: "System health",
        people: "People & devices",
        policy: "Collection policy",
        archive: "Archive control",
        audit: "Audit log",
        messages: "Messaging",
      }
    : { messages: "Messaging" };
  const readRoute = (): Route => {
    const value = window.location.hash.slice(1);
    return value in labels ? (value as Route) : isAdmin ? "health" : "messages";
  };
  const [route, setRoute] = useState<Route>(readRoute);
  const routeRef = useRef(route);
  routeRef.current = route;
  useEffect(() => {
    const changed = () => {
      const next = readRoute();
      if (credentialMutationPending && next !== routeRef.current) {
        window.location.hash = routeRef.current;
        setTransitionWarning(
          "Finish creating the one-time credential before leaving this page.",
        );
        return;
      }
      setTransitionWarning("");
      setRoute(next);
    };
    window.addEventListener("hashchange", changed);
    window.addEventListener("flopwire:unauthorized", onLogout);
    return () => {
      window.removeEventListener("hashchange", changed);
      window.removeEventListener("flopwire:unauthorized", onLogout);
    };
  }, [credentialMutationPending, onLogout, isAdmin]);
  useEffect(() => {
    if (!credentialMutationPending) return;
    const blockUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", blockUnload);
    return () => window.removeEventListener("beforeunload", blockUnload);
  }, [credentialMutationPending]);
  useEffect(() => {
    const active = document.querySelector<HTMLElement>(
      '.mobile-nav [aria-current="page"]',
    );
    const nav = active?.parentElement;
    if (active && nav) {
      nav.scrollLeft = active.offsetLeft - (nav.clientWidth - active.clientWidth) / 2;
    }
  }, [route]);
  const navigation = (className = "") => (
    <nav className={className} aria-label="Administration">
      {(Object.keys(labels) as Route[]).map((item) => (
        <a
          key={item}
          href={`#${item}`}
          className={route === item ? "active" : ""}
          aria-current={route === item ? "page" : undefined}
          aria-disabled={credentialMutationPending || undefined}
          aria-describedby={
            credentialMutationPending ? "credential-mutation-guard" : undefined
          }
          onClick={(event) => {
            if (
              event.button !== 0 ||
              event.metaKey ||
              event.ctrlKey ||
              event.shiftKey ||
              event.altKey
            )
              return;
            if (credentialMutationPending) {
              event.preventDefault();
              setTransitionWarning(
                "Finish creating the one-time credential before leaving this page.",
              );
              return;
            }
            window.location.hash = item;
            setRoute(item);
          }}
        >
          <NavMark route={item} />
          {labels[item]}
        </a>
      ))}
    </nav>
  );
  return (
    <div className="app-shell">
      <aside className="rail">
        <Brand />
        {navigation()}
        <div className="account">
          <span>{session.user.name}</span>
          <small>{session.user.role}</small>
          <button disabled={credentialMutationPending} onClick={onLogout}>
            Sign out
          </button>
        </div>
      </aside>
      <main className="work">
        <header className="mobile-header">
          <div className="mobile-title">
            <Brand />
            <button
              className="button secondary"
              disabled={credentialMutationPending}
              onClick={onLogout}
            >
              Sign out
            </button>
          </div>
          {navigation("mobile-nav")}
        </header>
        {credentialMutationPending && (
          <div
            className="mutation-guard"
            id="credential-mutation-guard"
            role="status"
          >
            Creating a one-time credential. Navigation and sign-out are locked
            until the request finishes.
          </div>
        )}
        {transitionWarning && (
          <div className="sr-only" role="alert">
            {transitionWarning}
          </div>
        )}
        {route === "health" && <HealthRoute token={session.token} />}
        {route === "people" && (
          <PeopleRoute
            token={session.token}
            onCredentialMutationChange={setCredentialMutationPending}
          />
        )}
        {route === "policy" && <PolicyRoute token={session.token} />}
        {route === "archive" && (
          <ArchiveRoute token={session.token} userID={session.user.id} />
        )}
        {route === "audit" && <AuditRoute token={session.token} />}
        {route === "messages" && <MessagingRoute token={session.token} />}
      </main>
    </div>
  );
}

function HealthRoute({ token }: { token: string }) {
  const [health, setHealth] = useState<Health | null>(null);
  const [operations, setOperations] = useState<AdminStatus | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  const refreshGeneration = useRef(0);
  const refresh = useCallback(async () => {
    const generation = ++refreshGeneration.current;
    try {
      const [nextHealth, nextOperations] = await Promise.all([
        request<Health>("/healthz", token),
        request<AdminStatus>("/v1/admin/status", token),
      ]);
      if (generation !== refreshGeneration.current) return;
      setHealth(nextHealth);
      setOperations(nextOperations);
      setUpdatedAt(new Date());
      setError("");
    } catch (reason) {
      if (generation !== refreshGeneration.current) return;
      setError(message(reason));
    } finally {
      if (generation === refreshGeneration.current) setLoading(false);
    }
  }, [token]);
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    const poll = async () => {
      await refresh();
      if (!cancelled) timer = window.setTimeout(poll, 15_000);
    };
    void poll();
    return () => {
      cancelled = true;
      refreshGeneration.current++;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [refresh]);
  const queue = operations?.index;
  const pressure = operations?.storage.quota_bytes
    ? Math.round(
        (operations.storage.used_bytes / operations.storage.quota_bytes) * 100,
      )
    : 0;
  const stages = useMemo(
    () => [
      {
        name: "Collector",
        detail: "Upload telemetry unavailable",
        state: health ? "unknown" : "checking",
      },
      {
        name: "Archive",
        detail: "Postgres + objects",
        state:
          health?.database === "ready" && health.object_store === "ready"
            ? "ready"
            : health
              ? "failed"
              : "checking",
      },
      {
        name: "Index",
        detail:
          queue && queue.queue_capacity > 0
            ? `${queue.queue_depth} of ${queue.queue_capacity} jobs queued`
            : health?.search === "unavailable"
              ? "Search index is being rebuilt"
              : "Message rows",
        state:
          health?.search === "unavailable" || !health ? "checking" : "ready",
      },
      {
        name: "Retrieval",
        detail: health?.search ?? "Checking search",
        state: health ? "ready" : "checking",
      },
    ],
    [health, queue],
  );
  return (
    <Page
      title="System health"
      intro="Follow each trace from its enrolled device to durable archive and searchable evidence."
      action={
        <button className="button secondary" onClick={() => void refresh()}>
          Refresh state
        </button>
      }
    >
      {error && (
        <RecoveryError
          text={error}
          action={
            updatedAt
              ? `Showing last known state from ${updatedAt.toLocaleTimeString()}. Refresh when the API recovers.`
              : "Confirm the API is reachable, then refresh state."
          }
        />
      )}
      <section className="relay-region" aria-labelledby="relay-title">
        <div className="section-heading">
          <div>
            <span className="porcelain">LIVE PATH</span>
            <h2 id="relay-title">Ingest to retrieval</h2>
          </div>
          <State
            state={
              error && health
                ? "checking"
                : health?.status === "not_ready"
                ? "failed"
                : health
                  ? "ready"
                  : "checking"
            }
          >
            {error && health ? "stale" : (health?.status ?? "checking")}
          </State>
        </div>
        <ol className="relay">
          {stages.map((stage, index) => (
            <li key={stage.name} data-state={stage.state}>
              <div className="stage-index">{index + 1}</div>
              <div>
                <h3>{stage.name}</h3>
                <p>{stage.detail}</p>
              </div>
              <State state={stage.state}>{stage.state}</State>
            </li>
          ))}
        </ol>
      </section>
      <section className="record-list" aria-labelledby="transition-title">
        <div className="section-heading">
          <div>
            <span className="porcelain">CURRENT REF</span>
            <h2 id="transition-title">Operational truth</h2>
          </div>
          {updatedAt && (
            <span className="porcelain">
              OBSERVED {relative(updatedAt.toISOString())}
            </span>
          )}
        </div>
        {loading && !health ? (
          <LoadingRows />
        ) : health && operations ? (
          <>
            <div className="record">
              <div>
                <strong>{health.search}</strong>
                <p>
                  {health.search === "unavailable"
                    ? "Team search is being rebuilt on message rows and is not accepting queries yet."
                    : "The active retrieval backend is accepting authenticated searches."}
                </p>
              </div>
            </div>
            <div className="record">
              <div>
                <strong>Storage pressure</strong>
                <p>
                  {formatBytes(operations.storage.used_bytes)} archived
                  {operations.storage.quota_bytes
                    ? ` of ${formatBytes(operations.storage.quota_bytes)}`
                    : " · no deployment quota"}
                </p>
              </div>
              <State
                state={
                  operations.storage.quota_bytes && pressure >= 100
                    ? "failed"
                    : "ready"
                }
              >
                {operations.storage.quota_bytes
                  ? `${pressure}% of limit`
                  : "unlimited"}
              </State>
            </div>
            <div className="record">
              <div>
                <strong>Last attributable transition</strong>
                <p>
                  {operations.last_transition
                    ? `${humanAction(operations.last_transition.action)} · ${operations.last_transition.actor_id ? shortID(operations.last_transition.actor_id) : "SYSTEM"}`
                    : "No transition recorded yet."}
                </p>
              </div>
              <span className="porcelain">
                {operations.last_transition
                  ? relative(operations.last_transition.created_at)
                  : "NO REF"}
              </span>
            </div>
          </>
        ) : !error ? (
          <Empty text="No health state returned." />
        ) : null}
      </section>
    </Page>
  );
}

function PeopleRoute({
  token,
  onCredentialMutationChange,
}: {
  token: string;
  onCredentialMutationChange: (pending: boolean) => void;
}) {
  const [users, setUsers] = useState<User[]>([]);
  const [devices, setDevices] = useState<Device[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [panel, setPanel] = useState<"invite" | "service" | null>(null);
  const [panelBusy, setPanelBusy] = useState(false);
  const [revoking, setRevoking] = useState<Set<string>>(() => new Set());
  const refresh = useCallback(async () => {
    try {
      const [u, d] = await Promise.all([
        request<{ users: User[] }>("/v1/admin/users", token),
        request<{ devices: Device[] }>("/v1/admin/devices", token),
      ]);
      setUsers(u.users ?? []);
      setDevices(d.devices ?? []);
      setError("");
    } catch (reason) {
      setError(message(reason));
    } finally {
      setLoading(false);
    }
  }, [token]);
  useEffect(() => {
    void refresh();
  }, [refresh]);
  const names = new Map(users.map((user) => [user.id, user.name]));
  function setCredentialMutationBusy(pending: boolean) {
    setPanelBusy(pending);
    onCredentialMutationChange(pending);
  }
  async function revoke(device: Device) {
    if (revoking.has(device.id)) return;
    if (
      !window.confirm(
        `Revoke ${device.name}? Its credential will stop uploading and reading immediately. Existing traces remain archived.`,
      )
    )
      return;
    setRevoking((current) => new Set(current).add(device.id));
    try {
      await request(`/v1/admin/devices/${device.id}/revoke`, token, {
        method: "POST",
      });
      await refresh();
    } catch (reason) {
      setError(message(reason));
    } finally {
      setRevoking((current) => {
        const next = new Set(current);
        next.delete(device.id);
        return next;
      });
    }
  }
  return (
    <Page
      title="People & devices"
      intro="Canonical users own every uploaded trace. Device credentials bind collection to that identity."
      action={
        <div className="button-row">
          <button
            className="button secondary"
            disabled={panelBusy}
            onClick={() => setPanel("service")}
          >
            Create service
          </button>
          <button
            className="button primary"
            disabled={panelBusy}
            onClick={() => setPanel("invite")}
          >
            Create invite
          </button>
        </div>
      }
    >
      {error && (
        <RecoveryError
          text={error}
          action="Sign in again or verify your administrator role."
        />
      )}
      {panel === "invite" && (
        <InvitePanel
          token={token}
          onBusyChange={setCredentialMutationBusy}
          onClose={() => setPanel(null)}
        />
      )}
      {panel === "service" && (
        <ServicePanel
          token={token}
          onBusyChange={setCredentialMutationBusy}
          onClose={() => setPanel(null)}
        />
      )}
      <section className="record-list" aria-labelledby="users-title">
        <div className="section-heading">
          <div>
            <span className="porcelain">IDENTITIES · {users.length}</span>
            <h2 id="users-title">Members</h2>
          </div>
        </div>
        {loading ? (
          <LoadingRows />
        ) : error ? null : users.length === 0 ? (
          <Empty text="No members returned." />
        ) : (
          users.map((user) => (
            <div className="record" key={user.id}>
              <div className="identity">
                <Initials name={user.name} />
                <div>
                  <strong>{user.name}</strong>
                  <p>{user.email}</p>
                </div>
              </div>
              <div className="record-meta">
                <State state={user.disabled ? "failed" : "ready"}>
                  {user.disabled ? "disabled" : user.role}
                </State>
                <span className="porcelain">{shortID(user.id)}</span>
              </div>
            </div>
          ))
        )}
      </section>
      <section className="record-list" aria-labelledby="devices-title">
        <div className="section-heading">
          <div>
            <span className="porcelain">ENROLLMENTS · {devices.length}</span>
            <h2 id="devices-title">Devices</h2>
          </div>
        </div>
        {loading ? (
          <LoadingRows />
        ) : error ? null : devices.length === 0 ? (
          <Empty text="No devices enrolled yet." />
        ) : (
          devices.map((device) => (
            <div className="record" key={device.id}>
              <div>
                <strong>{device.name}</strong>
                <p>
                  {names.get(device.user_id) ?? "Unknown owner"} ·{" "}
                  {device.platform}
                </p>
              </div>
              <div className="record-meta">
                <State
                  state={
                    device.revoked_at
                      ? "failed"
                      : device.last_seen_at
                        ? "ready"
                        : "checking"
                  }
                >
                  {device.revoked_at
                    ? "revoked"
                    : device.last_seen_at
                      ? "active"
                      : "never seen"}
                </State>
                <span className="porcelain">
                  {device.last_seen_at
                    ? relative(device.last_seen_at)
                    : shortID(device.id)}
                </span>
                {!device.revoked_at && (
                  <button
                    className="text-button danger-text"
                    disabled={revoking.has(device.id)}
                    onClick={() => void revoke(device)}
                  >
                    {revoking.has(device.id) ? "Revoking…" : "Revoke"}
                  </button>
                )}
              </div>
            </div>
          ))
        )}
      </section>
    </Page>
  );
}

function ArchiveRoute({ token, userID }: { token: string; userID: string }) {
  const deletionStorageKey = `${DELETION_KEY_PREFIX}${userID}`;
  const restoredDeletion = useMemo(
    () => loadDeletionTracking(deletionStorageKey, userID),
    [deletionStorageKey, userID],
  );
  const [status, setStatus] = useState<AdminStatus | null>(null);
  const [sessionID, setSessionID] = useState(
    restoredDeletion?.job.conversation_id ?? "",
  );
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [deletion, setDeletion] = useState<DeletionJob | null>(
    restoredDeletion?.job ?? null,
  );
  const [deletionObservedAt, setDeletionObservedAt] = useState(
    restoredDeletion?.observed_at ?? "",
  );
  const [deletionStale, setDeletionStale] = useState(
    restoredDeletion?.stale ?? false,
  );
  const deletionRef = useRef<DeletionJob | null>(deletion);
  const deletionObservedAtRef = useRef(deletionObservedAt);
  const deletionStaleRef = useRef(deletionStale);
  deletionRef.current = deletion;
  deletionObservedAtRef.current = deletionObservedAt;
  deletionStaleRef.current = deletionStale;
  const [statusError, setStatusError] = useState("");
  const [mutationError, setMutationError] = useState("");
  const [pollError, setPollError] = useState("");
  const [statusLoading, setStatusLoading] = useState(true);
  const confirmationRef = useRef<HTMLElement>(null);
  const pollGeneration = useRef(0);
  const acceptDeletion = useCallback(
    (job: DeletionJob) => {
      const observedAt = new Date().toISOString();
      setDeletion(job);
      setSessionID(job.conversation_id);
      setDeletionObservedAt(observedAt);
      setDeletionStale(false);
      deletionStaleRef.current = false;
      setPollError("");
      saveDeletionTracking(deletionStorageKey, {
        job,
        observed_at: observedAt,
        stale: false,
      });
    },
    [deletionStorageKey],
  );
  useEffect(() => {
    request<AdminStatus>("/v1/admin/status", token)
      .then((next) => {
        setStatus(next);
        setStatusError("");
      })
      .catch((reason) => setStatusError(message(reason)))
      .finally(() => setStatusLoading(false));
  }, [token]);
  useEffect(() => {
    if (confirming) confirmationRef.current?.focus();
  }, [confirming]);
  const refreshDeletion = useCallback(async () => {
    const currentDeletion = deletionRef.current;
    if (!currentDeletion) return;
    const generation = ++pollGeneration.current;
    try {
      const out = await request<{ deletion: DeletionJob }>(
        `/v1/admin/deletions/${currentDeletion.id}`,
        token,
      );
      if (generation === pollGeneration.current) acceptDeletion(out.deletion);
    } catch (reason) {
      if (generation !== pollGeneration.current) return;
      const text = message(reason);
      setPollError(text);
      setDeletionStale(true);
      deletionStaleRef.current = true;
      saveDeletionTracking(deletionStorageKey, {
        job: currentDeletion,
        observed_at:
          deletionObservedAtRef.current || new Date().toISOString(),
        stale: true,
      });
    }
  }, [acceptDeletion, deletionStorageKey, token]);
  useEffect(() => {
    if (
      !deletion ||
      deletionStale ||
      deletion.state === "complete" ||
      deletion.state === "failed"
    )
      return;
    let cancelled = false;
    let timer: number | undefined;
    const poll = async () => {
      await refreshDeletion();
      const current = deletionRef.current;
      if (
        !cancelled &&
        current &&
        !deletionStaleRef.current &&
        current.state !== "complete" &&
        current.state !== "failed"
      )
        timer = window.setTimeout(poll, 2_000);
    };
    void poll();
    return () => {
      cancelled = true;
      pollGeneration.current++;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [deletion?.id, deletion?.state, deletionStale, refreshDeletion]);
  async function removeSession() {
    setBusy(true);
    setMutationError("");
    try {
      const out = await request<{ deletion: DeletionJob }>(
        `/v1/admin/conversations/${encodeURIComponent(sessionID)}`,
        token,
        { method: "DELETE" },
      );
      acceptDeletion(out.deletion);
      setConfirming(false);
    } catch (reason) {
      setMutationError(message(reason));
    } finally {
      setBusy(false);
    }
  }
  async function retryDeletion() {
    if (!deletion) return;
    setBusy(true);
    setMutationError("");
    try {
      const out = await request<{ deletion: DeletionJob }>(
        `/v1/admin/deletions/${deletion.id}/retry`,
        token,
        { method: "POST" },
      );
      acceptDeletion(out.deletion);
    } catch (reason) {
      setMutationError(message(reason));
    } finally {
      setBusy(false);
    }
  }
  function trackAnotherDeletion() {
    sessionStorage.removeItem(deletionStorageKey);
    setDeletion(null);
    setDeletionObservedAt("");
    setDeletionStale(false);
    deletionStaleRef.current = false;
    setPollError("");
    setMutationError("");
    setSessionID("");
    setConfirming(false);
  }
  function stopTrackingDeletion() {
    if (
      !window.confirm(
        "Stop tracking this deletion job? This only clears the local status reference. A queued or active purge may continue on the server.",
      )
    )
      return;
    trackAnotherDeletion();
  }
  const backup = status?.last_backup;
  return (
    <Page
      title="Archive control"
      intro="Review recovery state and perform explicit, attributable corpus deletion."
    >
      {statusError && (
        <RecoveryError
          text={statusError}
          action="Verify the administrator session, then reload recovery status."
        />
      )}
      <section className="record-list" aria-labelledby="backup-title">
        <div className="section-heading">
          <div>
            <span className="porcelain">RECOVERY REF</span>
            <h2 id="backup-title">Latest completed backup</h2>
          </div>
        </div>
        {statusLoading ? (
          <LoadingRows />
        ) : statusError ? null : !status ? (
          <Empty text="No recovery status returned." />
        ) : (
          <div className="record">
            <div>
              <strong>
                {backup ? backup.target_id : "No backup recorded"}
              </strong>
              <p>
                {backup
                  ? `${String(backup.metadata?.objects ?? 0)} chunk objects, database dump, and backup manifest.`
                  : "Run flopwire backup --encrypted-destination --output /safe/path on the server."}
              </p>
            </div>
            <span className="porcelain">
              {backup ? relative(backup.created_at) : "ACTION REQUIRED"}
            </span>
          </div>
        )}
      </section>
      <section
        className="action-panel danger-panel"
        aria-labelledby="delete-title"
      >
        <div className="section-heading">
          <div>
            <span className="porcelain">DESTRUCTIVE · AUDITED</span>
            <h2 id="delete-title">Delete one conversation</h2>
          </div>
        </div>
        <p>
          Removes the conversation from search at once and tombstones it so a
          re-upload cannot restore it. Its raw chunks are purged when nothing
          else references them. The deletion event remains in the audit log.
          Device-local files are unchanged.
        </p>
        <label>
          Exact conversation ID
          <input
            value={sessionID}
            disabled={Boolean(deletion)}
            onChange={(event) => {
              setSessionID(event.target.value);
              setConfirming(false);
              setMutationError("");
            }}
            placeholder="conversation UUID"
          />
        </label>
        {!deletion && confirming ? (
          <div className="confirm-row" aria-live="assertive">
            <strong
              id="delete-confirmation-title"
              ref={confirmationRef}
              tabIndex={-1}
            >
              Delete {sessionID} permanently?
            </strong>
            <div className="button-row">
              <button
                className="button danger"
                disabled={busy}
                onClick={() => void removeSession()}
              >
                {busy ? "Deleting…" : "Confirm deletion"}
              </button>
              <button
                className="button secondary"
                disabled={busy}
                onClick={() => setConfirming(false)}
              >
                Cancel
              </button>
            </div>
          </div>
        ) : !deletion ? (
          <button
            className="button secondary"
            disabled={!sessionID.trim()}
            onClick={() => setConfirming(true)}
          >
            Review deletion
          </button>
        ) : null}
        {mutationError && (
          <div className="error mutation-status" role="alert">
            {mutationError}
          </div>
        )}
        {deletion && (
          <div className="deletion-status">
            <div role="status" aria-live="polite">
              <strong>{deletionStatusTitle(deletion)}</strong>
              <p>{deletionStatusDetail(deletion)}</p>
              {deletionObservedAt && (
                <p className="porcelain">
                  LAST OBSERVED {relative(deletionObservedAt)}
                </p>
              )}
            </div>
            <div className="record-meta">
              <State
                state={
                  deletionStale ? "checking" : deletionStateTone(deletion.state)
                }
              >
                {deletionStale ? "stale" : deletion.state.replace("_", " ")}
              </State>
              <span className="porcelain">JOB {shortID(deletion.id)}</span>
              {deletion.state === "failed" && (
                <button
                  className="button secondary"
                  disabled={busy}
                  onClick={() => void retryDeletion()}
                >
                  {busy ? "Retrying…" : "Retry purge"}
                </button>
              )}
              {deletionStale && (
                <>
                  <button
                    className="button secondary"
                    disabled={busy}
                    onClick={() => void refreshDeletion()}
                  >
                    Retry status
                  </button>
                  {deletion.state !== "complete" &&
                    deletion.state !== "failed" && (
                      <button
                        className="text-button danger-text"
                        disabled={busy}
                        onClick={stopTrackingDeletion}
                      >
                        Stop tracking
                      </button>
                    )}
                </>
              )}
              {(deletion.state === "complete" || deletion.state === "failed") && (
                <button
                  className="text-button"
                  disabled={busy}
                  onClick={trackAnotherDeletion}
                >
                  Track another
                </button>
              )}
            </div>
          </div>
        )}
        {pollError && (
          <div className="error mutation-status" role="alert">
            {pollError}. The displayed job state is stale. Retry status or check
            the server connection.
          </div>
        )}
      </section>
    </Page>
  );
}

function AuditRoute({ token }: { token: string }) {
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    request<{ events: AuditEvent[] }>("/v1/admin/audit?limit=100", token)
      .then((data) => setEvents(data.events ?? []))
      .catch((reason) => setError(message(reason)))
      .finally(() => setLoading(false));
  }, [token]);
  return (
    <Page
      title="Audit log"
      intro="Every corpus read, upload, identity change, and administrative mutation remains attributable."
    >
      {error && (
        <RecoveryError
          text={error}
          action="Verify your administrator session and reload this route."
        />
      )}
      <section
        className="record-list audit-list"
        aria-labelledby="events-title"
      >
        <div className="section-heading">
          <div>
            <span className="porcelain">LATEST · {events.length}</span>
            <h2 id="events-title">Recorded actions</h2>
          </div>
        </div>
        {loading ? (
          <LoadingRows />
        ) : error ? null : events.length === 0 ? (
          <Empty text="No audit events returned." />
        ) : (
          events.map((event) => (
            <div className="record" key={event.id}>
              <div>
                <strong>{humanAction(event.action)}</strong>
                <p>
                  {event.target_type || "deployment"}
                  {event.target_id ? ` · ${shortID(event.target_id)}` : ""}
                </p>
              </div>
              <div className="record-meta">
                <span className="porcelain">
                  {event.actor_id ? shortID(event.actor_id) : "SYSTEM"}
                </span>
                <time dateTime={event.created_at}>
                  {relative(event.created_at)}
                </time>
              </div>
            </div>
          ))
        )}
      </section>
    </Page>
  );
}

function PolicyRoute({ token }: { token: string }) {
  const [policy, setPolicy] = useState<Policy | null>(null);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    request<Policy>("/v1/policy", token)
      .then(setPolicy)
      .catch((reason) => setError(message(reason)))
      .finally(() => setLoading(false));
  }, [token]);
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!policy) return;
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      const next = await request<Policy>("/v1/admin/policy", token, {
        method: "PUT",
        body: JSON.stringify(policy),
      });
      setPolicy(next);
      setSaved(true);
    } catch (reason) {
      setError(message(reason));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Page
      title="Collection policy"
      intro="Admin rules and private local rules combine before a trace can leave a device."
    >
      {error && (
        <RecoveryError
          text={error}
          action="Review the values and try saving again."
        />
      )}
      {loading ? (
        <LoadingRows />
      ) : !policy ? null : (
        <form className="policy-form" onSubmit={submit}>
          <section aria-labelledby="quota-title">
            <span className="porcelain">CAPACITY</span>
            <h2 id="quota-title">Pause before storage failure</h2>
            <p>
              Zero means unlimited. When a quota is reached, backfill pauses and
              new uploads fail visibly; flopwire never deletes old traces
              automatically.
            </p>
            <div className="field-pair">
              <label>
                Deployment quota, bytes
                <input
                  type="number"
                  min="0"
                  value={policy.max_storage_bytes}
                  onChange={(event) =>
                    setPolicy({
                      ...policy,
                      max_storage_bytes: Number(event.target.value),
                    })
                  }
                />
              </label>
              <label>
                Per-user quota, bytes
                <input
                  type="number"
                  min="0"
                  value={policy.max_user_bytes}
                  onChange={(event) =>
                    setPolicy({
                      ...policy,
                      max_user_bytes: Number(event.target.value),
                    })
                  }
                />
              </label>
            </div>
          </section>
          <section aria-labelledby="path-rules-title">
            <span className="porcelain">PATH RULES</span>
            <h2 id="path-rules-title">Keep matching paths off the server</h2>
            <p>
              One rule per line. A bare path pattern keeps matching sessions
              out of every index and off the server. <code>local:PATTERN</code>{" "}
              indexes them on the device but never uploads them. A path pattern
              matches the session's directory, its git worktree and the main
              checkout. <code>repo:github.com/acme/*</code> matches the
              repository's origin remote. Patterns are globs (<code>*</code>,{" "}
              <code>**</code>, <code>~</code>) and ignore case. Members can add
              stricter rules on their own devices.
            </p>
            <label>
              Path rules
              <textarea
                rows={7}
                value={policy.path_rules.join("\n")}
                onChange={(event) =>
                  setPolicy({
                    ...policy,
                    path_rules: event.target.value.split("\n"),
                  })
                }
              />
            </label>
            <label>
              Sessions with no directory or repo
              <select
                value={policy.unplaceable ?? ""}
                onChange={(event) =>
                  setPolicy({
                    ...policy,
                    unplaceable: event.target.value as Policy["unplaceable"],
                  })
                }
              >
                <option value="">No floor (each device decides; default local)</option>
                <option value="local">Local: index on the device, never upload</option>
                <option value="upload">Upload (same as no floor)</option>
                <option value="exclude">Exclude: neither index nor upload</option>
              </select>
            </label>
          </section>
          <div className="form-actions">
            <button className="button primary" disabled={busy}>
              {busy ? "Saving…" : "Save collection policy"}
            </button>
            {saved && <State state="ready">Policy saved</State>}
          </div>
        </form>
      )}
    </Page>
  );
}

function InvitePanel({
  token,
  onBusyChange,
  onClose,
}: {
  token: string;
  onBusyChange: (busy: boolean) => void;
  onClose: () => void;
}) {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<"member" | "admin">("member");
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [copyStatus, setCopyStatus] = useState<"" | "copied" | "failed">("");
  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    onBusyChange(true);
    setError("");
    try {
      const out = await request<{ code: string }>("/v1/admin/invites", token, {
        method: "POST",
        body: JSON.stringify({ email, role }),
      });
      setCode(out.code);
    } catch (reason) {
      setError(message(reason));
    } finally {
      setBusy(false);
      onBusyChange(false);
    }
  }
  return (
    <section className="action-panel" aria-labelledby="invite-title">
      <div className="section-heading">
        <div>
          <span className="porcelain">ONE-TIME CLAIM</span>
          <h2 id="invite-title">Create invitation</h2>
        </div>
        <button
          className="text-button"
          disabled={busy}
          onClick={onClose}
          aria-label="Close invitation panel"
        >
          Close
        </button>
      </div>
      {code ? (
        <div className="invite-result">
          <p>
            Send this code through a trusted channel. It expires in 72 hours and
            appears only once.
          </p>
          <code>{code}</code>
          <button
            className="button secondary"
            onClick={() =>
              void copyText(code)
                .then(() => {
                  setCopyStatus("copied");
                  window.setTimeout(() => setCopyStatus(""), 2_000);
                })
                .catch(() => setCopyStatus("failed"))
            }
          >
            {copyStatus === "copied" ? "Copied" : "Copy code"}
          </button>
          {copyStatus && (
            <span
              className={copyStatus === "failed" ? "copy-error" : "sr-only"}
              role={copyStatus === "failed" ? "alert" : "status"}
            >
              {copyStatus === "copied"
                ? "Invitation code copied"
                : "Could not copy the invitation code. Select and copy it manually."}
            </span>
          )}
        </div>
      ) : (
        <form className="inline-form" onSubmit={submit}>
          <Field
            label="Email"
            type="email"
            value={email}
            onChange={setEmail}
            autoComplete="off"
          />
          <label>
            Role
            <select
              value={role}
              onChange={(event) =>
                setRole(event.target.value as "member" | "admin")
              }
            >
              <option value="member">Member</option>
              <option value="admin">Administrator</option>
            </select>
          </label>
          {error && (
            <div className="error" role="alert">
              {error}
            </div>
          )}
          <button className="button primary" disabled={busy || !email}>
            {busy ? "Creating…" : "Create invitation"}
          </button>
        </form>
      )}
    </section>
  );
}

function ServicePanel({
  token,
  onBusyChange,
  onClose,
}: {
  token: string;
  onBusyChange: (busy: boolean) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState("");
  const [result, setResult] = useState<{ token: string; scope: string } | null>(
    null,
  );
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [copyStatus, setCopyStatus] = useState<"" | "copied" | "failed">("");
  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    onBusyChange(true);
    setError("");
    try {
      setResult(
        await request<{ token: string; scope: string }>(
          "/v1/admin/service-accounts",
          token,
          { method: "POST", body: JSON.stringify({ name }) },
        ),
      );
    } catch (reason) {
      setError(message(reason));
    } finally {
      setBusy(false);
      onBusyChange(false);
    }
  }
  return (
    <section className="action-panel" aria-labelledby="service-title">
      <div className="section-heading">
        <div>
          <span className="porcelain">UPLOAD-ONLY IDENTITY</span>
          <h2 id="service-title">Create service account</h2>
        </div>
        <button className="text-button" disabled={busy} onClick={onClose}>
          Close
        </button>
      </div>
      {result ? (
        <div className="invite-result">
          <p>
            Copy this credential now. It can upload traces but cannot search the
            corpus.
          </p>
          <code>{result.token}</code>
          <button
            className="button secondary"
            onClick={() =>
              void copyText(result.token)
                .then(() => {
                  setCopyStatus("copied");
                  window.setTimeout(() => setCopyStatus(""), 2_000);
                })
                .catch(() => setCopyStatus("failed"))
            }
          >
            {copyStatus === "copied" ? "Copied" : "Copy credential"}
          </button>
          {copyStatus && (
            <span
              className={copyStatus === "failed" ? "copy-error" : "sr-only"}
              role={copyStatus === "failed" ? "alert" : "status"}
            >
              {copyStatus === "copied"
                ? "Credential copied"
                : "Could not copy the credential. Select and copy it manually."}
            </span>
          )}
        </div>
      ) : (
        <form className="inline-form single" onSubmit={submit}>
          <Field
            label="Service name"
            type="text"
            value={name}
            onChange={setName}
            autoComplete="off"
          />
          {error && (
            <div className="error" role="alert">
              {error}
            </div>
          )}
          <button className="button primary" disabled={busy || !name}>
            {busy ? "Creating…" : "Create service account"}
          </button>
        </form>
      )}
    </section>
  );
}

// MessagingRoute is the signed-in person's review of senders (B7): messages
// held from people they have not accepted, grouped by sender, and the
// people they accept. Accepting is behind a statement of what it means;
// revoking is one step. Previews are text another person's agent wrote:
// React renders them as text, never as HTML or markdown.
function MessagingRoute({ token }: { token: string }) {
  const [held, setHeld] = useState<HeldGroup[]>([]);
  const [accepted, setAccepted] = useState<Accepted[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [confirming, setConfirming] = useState<string | null>(null);
  const [password, setPassword] = useState("");
  const [acceptError, setAcceptError] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [outcome, setOutcome] = useState("");
  // Opening or closing the accept panel starts it empty.
  const confirm = (id: string | null) => {
    setConfirming(id);
    setPassword("");
    setAcceptError("");
  };
  const refresh = useCallback(async () => {
    try {
      const [h, a] = await Promise.all([
        request<{ senders: HeldGroup[] }>("/v1/bus/held", token),
        request<{ accepted: Accepted[] }>("/v1/bus/accepts", token),
      ]);
      setHeld(h.senders ?? []);
      setAccepted(a.accepted ?? []);
      setError("");
    } catch (reason) {
      setError(message(reason));
    } finally {
      setLoading(false);
    }
  }, [token]);
  useEffect(() => {
    void refresh();
  }, [refresh]);
  // Accepting needs the person's password as well as the session: the
  // session `flopwire login` saves on a device can be read by an agent of
  // the same OS user, the password cannot.
  async function accept(group: HeldGroup) {
    setBusy(group.user_id);
    setOutcome("");
    setAcceptError("");
    const typed = password;
    setPassword("");
    try {
      const out = await request<AcceptResult>("/v1/bus/accepts", token, {
        method: "POST",
        body: JSON.stringify({ sender: group.user_id, password: typed }),
      });
      const n = out.released ?? 0;
      setOutcome(
        `Accepted ${out.user}. ${n} held ${plural(n, "message was", "messages were")} released to your sessions; their agents' next messages arrive without being held.`,
      );
      confirm(null);
      await refresh();
    } catch (reason) {
      if (reason instanceof APIError && reason.status === 403)
        setAcceptError(
          "Not accepted: the password was not correct. Type your own Flopwire password.",
        );
      else if (reason instanceof APIError && reason.status === 429)
        setAcceptError("Too many attempts. Wait a minute, then try again.");
      else setError(message(reason));
    } finally {
      setBusy(null);
    }
  }
  async function revoke(person: Accepted) {
    setBusy(person.user_id);
    setOutcome("");
    try {
      const out = await request<AcceptResult>(
        `/v1/bus/accepts/${encodeURIComponent(person.user_id)}`,
        token,
        { method: "DELETE" },
      );
      const n = out.reheld ?? 0;
      setOutcome(
        `Revoked ${out.user}. ${n} undelivered ${plural(n, "message is", "messages are")} held again, and their next messages are held until you accept them again. A message a session already received stays with it.`,
      );
      await refresh();
    } catch (reason) {
      setError(message(reason));
    } finally {
      setBusy(null);
    }
  }
  const total = held.reduce((n, g) => n + g.count, 0);
  return (
    <Page
      title="Messaging"
      kicker="FLOPWIRE / MESSAGING"
      intro="Messages from another person's agents wait here until you accept that person. Your agents do not see held messages or this page."
    >
      {error && (
        <RecoveryError
          text={error}
          action="Reload this page. If it persists, sign in again, or ask your administrator to check the server."
        />
      )}
      {outcome && (
        <div className="success messaging-outcome" role="status">
          {outcome}
        </div>
      )}
      <section className="record-list" aria-labelledby="held-title">
        <div className="section-heading">
          <div>
            <span className="porcelain">HELD · {total}</span>
            <h2 id="held-title">Waiting for you</h2>
          </div>
        </div>
        {loading ? (
          <LoadingRows />
        ) : error ? null : held.length === 0 ? (
          <Empty text="No one you have not accepted has messaged your agents. Held messages expire after 24 hours." />
        ) : (
          held.map((group) => (
            <div className="held-group" key={group.user_id}>
              <div className="record">
                <div className="identity">
                  <Initials name={group.user_name || group.user} />
                  <div>
                    <strong>{group.user_name || group.user}</strong>
                    <p>{group.user}</p>
                  </div>
                </div>
                <div className="record-meta">
                  <State state="checking">
                    {group.count} held
                  </State>
                  <time dateTime={group.newest}>{relative(group.newest)}</time>
                  {confirming !== group.user_id && (
                    <button
                      className="button secondary"
                      disabled={busy !== null}
                      onClick={() => confirm(group.user_id)}
                    >
                      Review and accept
                    </button>
                  )}
                </div>
              </div>
              <ul className="held-messages" aria-label={`Held from ${group.user}`}>
                {group.messages.map((m) => (
                  <li key={m.id}>
                    <p className="held-preview">{m.preview}</p>
                    <span className="porcelain">
                      {m.agent} · {m.repo ? `${m.repo}${m.branch ? "@" + m.branch : ""}` : "no repo"} · {m.intent} ·{" "}
                      {relative(m.sent)}
                    </span>
                  </li>
                ))}
                {(group.more ?? 0) > 0 && (
                  <li className="porcelain">and {group.more} more</li>
                )}
              </ul>
              {confirming === group.user_id && (
                <form
                  className="action-panel accept-panel"
                  role="region"
                  aria-label={`Accept ${group.user}`}
                  onSubmit={(event) => {
                    event.preventDefault();
                    void accept(group);
                  }}
                >
                  <h3>Accept messages from {group.user}?</h3>
                  <p className="accept-statement">{ACCEPT_STATEMENT}</p>
                  <p>
                    Accepting releases the {group.count} held{" "}
                    {plural(group.count, "message", "messages")} above to your
                    sessions now. Revoking later holds undelivered messages
                    again; it cannot recall one a session already received.
                  </p>
                  <Field
                    label="Your password, to confirm it is you"
                    type="password"
                    value={password}
                    onChange={setPassword}
                    autoComplete="current-password"
                  />
                  {acceptError && (
                    <div className="error" role="alert">
                      {acceptError}
                    </div>
                  )}
                  <div className="button-row">
                    <button
                      type="submit"
                      className="button primary"
                      disabled={busy !== null || password === ""}
                    >
                      {busy === group.user_id
                        ? "Accepting…"
                        : `Accept ${group.user}`}
                    </button>
                    <button
                      type="button"
                      className="button secondary"
                      disabled={busy !== null}
                      onClick={() => confirm(null)}
                    >
                      Cancel
                    </button>
                  </div>
                </form>
              )}
            </div>
          ))
        )}
      </section>
      <section className="record-list" aria-labelledby="accepted-title">
        <div className="section-heading">
          <div>
            <span className="porcelain">ACCEPTED · {accepted.length}</span>
            <h2 id="accepted-title">People whose agents can message yours</h2>
          </div>
        </div>
        {loading ? (
          <LoadingRows />
        ) : error ? null : accepted.length === 0 ? (
          <Empty text="You accept no one. Every message from another person is held until you accept them." />
        ) : (
          accepted.map((person) => (
            <div className="record" key={person.user_id}>
              <div>
                <strong>{person.user}</strong>
                <p>
                  Accepted{" "}
                  <time dateTime={person.accepted_at}>
                    {relative(person.accepted_at)}
                  </time>
                </p>
              </div>
              <div className="record-meta">
                <button
                  className="text-button danger-text"
                  disabled={busy !== null}
                  onClick={() => void revoke(person)}
                >
                  {busy === person.user_id ? "Revoking…" : `Revoke ${person.user}`}
                </button>
              </div>
            </div>
          ))
        )}
      </section>
    </Page>
  );
}

function Page({
  title,
  intro,
  action,
  kicker = "FLOPWIRE / ADMIN",
  children,
}: {
  title: string;
  intro: string;
  action?: ReactNode;
  kicker?: string;
  children: ReactNode;
}) {
  return (
    <div className="page">
      <header className="page-title">
        <div>
          <span className="porcelain">{kicker}</span>
          <h1>{title}</h1>
          <p>{intro}</p>
        </div>
        {action}
      </header>
      {children}
    </div>
  );
}
function Brand() {
  return (
    <div className="brand">
      <span className="brand-mark" aria-hidden="true">
        <i />
        <i />
        <i />
      </span>
      <span>flopwire</span>
    </div>
  );
}
function NavMark({ route }: { route: Route }) {
  const paths = {
    health: "M3 12h4l2-5 4 10 2-5h6",
    people:
      "M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75",
    policy:
      "M12 2v4M12 18v4M4.93 4.93l2.83 2.83M16.24 16.24l2.83 2.83M2 12h4M18 12h4M4.93 19.07l2.83-2.83M16.24 7.76l2.83-2.83",
    archive: "M4 7h16v13H4zM2 4h20v3H2zM9 11h6",
    audit: "M4 6h16M4 12h16M4 18h10",
    messages: "M4 5h16v11H9l-5 4z",
  };
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <path d={paths[route]} />
    </svg>
  );
}
function Field({
  label,
  type,
  value,
  onChange,
  autoComplete,
}: {
  label: string;
  type: string;
  value: string;
  onChange: (value: string) => void;
  autoComplete: string;
}) {
  return (
    <label>
      {label}
      <input
        required
        type={type}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        autoComplete={autoComplete}
      />
    </label>
  );
}
function State({ state, children }: { state: string; children: ReactNode }) {
  return <span className={`state ${state}`}>{children}</span>;
}
function Initials({ name }: { name: string }) {
  return (
    <span className="initials" aria-hidden="true">
      {name
        .split(/\s+/)
        .slice(0, 2)
        .map((part) => part[0])
        .join("")}
    </span>
  );
}
function RecoveryError({ text, action }: { text: string; action: string }) {
  return (
    <div className="recovery-error" role="alert">
      <strong>{text}</strong>
      <span>{action}</span>
    </div>
  );
}
function Empty({ text }: { text: string }) {
  return (
    <div className="empty">
      <strong>Nothing recorded</strong>
      <p>{text}</p>
    </div>
  );
}
function LoadingRows() {
  return (
    <div
      aria-label="Loading"
      aria-live="polite"
      className="loading-rows"
      role="status"
    >
      <i />
      <i />
      <i />
    </div>
  );
}
function message(reason: unknown) {
  if (reason instanceof APIError && reason.status === 401)
    return "Your session expired.";
  return reason instanceof Error ? reason.message : "The request failed.";
}
function shortID(id: string) {
  return id.slice(0, 8).toUpperCase();
}
function relative(value: string) {
  const time = new Date(value).getTime();
  if (!Number.isFinite(time)) return "UNKNOWN";
  const seconds = Math.round((Date.now() - time) / 1000);
  if (Math.abs(seconds) < 60) return "JUST NOW";
  const minutes = Math.round(seconds / 60);
  if (Math.abs(minutes) < 60) return `${minutes}M AGO`;
  const hours = Math.round(minutes / 60);
  if (Math.abs(hours) < 48) return `${hours}H AGO`;
  return new Date(value).toLocaleDateString();
}
function humanAction(action: string) {
  return action
    .split(".")
    .map((part) => part[0]?.toUpperCase() + part.slice(1))
    .join(" · ");
}
function formatBytes(value: number) {
  if (value < 1024) return `${value} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let current = value / 1024;
  let unit = 0;
  while (current >= 1024 && unit < units.length - 1) {
    current /= 1024;
    unit++;
  }
  return `${current.toFixed(current >= 10 ? 0 : 1)} ${units[unit]}`;
}
function deletionStateTone(state: DeletionJob["state"]) {
  if (state === "complete") return "ready";
  if (state === "failed") return "failed";
  return "checking";
}
function deletionStatusTitle(job: DeletionJob) {
  const id = job.conversation_id;
  if (job.state === "complete") return `Conversation ${id} was deleted`;
  if (job.state === "failed") return `Purge failed for ${id}`;
  if (job.state === "retry_wait") return `Purge will retry for ${id}`;
  if (job.state === "purging") return `Purging ${id}`;
  return `Deletion queued for ${id}`;
}
function deletionStatusDetail(job: DeletionJob) {
  if (job.state === "complete")
    return `Unshared raw chunks were purged after ${job.attempts} attempt${job.attempts === 1 ? "" : "s"}.`;
  if (job.state === "failed")
    return job.last_error
      ? `${job.last_error} Review server logs, then retry the purge.`
      : "Review server logs, then retry the purge.";
  if (job.state === "retry_wait" && job.next_attempt_at)
    return `Attempt ${job.attempts} did not complete. Next attempt ${relative(job.next_attempt_at)}.`;
  return `Attempt ${job.attempts}. The conversation is already gone from search; raw chunks purge next.`;
}
async function copyText(value: string) {
  await navigator.clipboard.writeText(value);
}
