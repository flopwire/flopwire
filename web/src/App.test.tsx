import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";

const admin = {
  id: "11111111-1111-1111-1111-111111111111",
  name: "Ada Admin",
  email: "ada@example.test",
  role: "admin",
  identity_type: "human",
  disabled: false,
  created_at: "2026-08-21T12:00:00Z",
};

describe("admin console", () => {
  beforeEach(() => {
    sessionStorage.clear();
    window.history.replaceState(null, "", "/");
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("logs in and renders the causal health relay", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "/v1/login")
          return json({ token: "test-token", user: admin });
        if (url === "/healthz")
          return json({
            status: "ok",
            database: "ready",
            object_store: "ready",
            search: "messages",
          });
        if (url === "/v1/admin/status")
          return json({
            storage: { used_bytes: 256, admin_bytes: 256, quota_bytes: 1024 },
            index: { queue_depth: 0, queue_capacity: 1024 },
          });
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    fireEvent.change(screen.getByLabelText("Email"), {
      target: { value: "ada@example.test" },
    });
    fireEvent.change(screen.getByLabelText("Password"), {
      target: { value: "correct horse battery staple" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("heading", { name: "System health" });
    expect(
      screen.getByRole("heading", { name: "Ingest to retrieval" }),
    ).toBeTruthy();
    for (const stage of ["Collector", "Archive", "Index", "Retrieval"])
      expect(screen.getByRole("heading", { name: stage })).toBeTruthy();
  });

  it("renders attributable users and devices without metric cards", async () => {
    sessionStorage.setItem(
      "flopwire.admin.session",
      JSON.stringify({ token: "test-token", user: admin }),
    );
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "/healthz")
          return json({
            status: "ok",
            database: "ready",
            object_store: "ready",
            search: "unavailable",
          });
        if (url === "/v1/admin/status")
          return json({
            storage: { used_bytes: 0, admin_bytes: 0, quota_bytes: 0 },
            index: { queue_depth: 0, queue_capacity: 1024 },
          });
        if (url === "/v1/admin/users") return json({ users: [admin] });
        if (url === "/v1/admin/devices")
          return json({
            devices: [
              {
                id: "22222222-2222-2222-2222-222222222222",
                user_id: admin.id,
                name: "ada-mac",
                platform: "darwin-arm64",
                created_at: "2026-08-21T12:00:00Z",
              },
            ],
          });
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    fireEvent.click(
      screen.getAllByRole("link", { name: "People & devices" })[0],
    );
    await waitFor(() => expect(screen.getByText("ada-mac")).toBeTruthy());
    expect(screen.getAllByText("Ada Admin").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("darwin-arm64", { exact: false })).toBeTruthy();
  });

  it("restores an admin route from the URL hash", async () => {
    sessionStorage.setItem(
      "flopwire.admin.session",
      JSON.stringify({ token: "test-token", user: admin }),
    );
    window.location.hash = "archive";
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        json({
          storage: { used_bytes: 0, admin_bytes: 0, quota_bytes: 0 },
          index: { queue_depth: 0, queue_capacity: 1024 },
          last_backup: {
            id: "backup-event",
            action: "backup.complete",
            target_id: "backup-2026-08-22",
            metadata: { objects: 4 },
            created_at: "2026-08-22T12:00:00Z",
          },
        }),
      ),
    );
    render(<App />);
    await screen.findByRole("heading", { name: "Archive control" });
    expect(
      screen.getByRole("heading", { name: "Latest completed backup" }),
    ).toBeTruthy();
    expect(
      screen.getByText("4 chunk objects, database dump, and backup manifest."),
    ).toBeTruthy();
  });

  it("clears an expired administrator session on a 401", async () => {
    sessionStorage.setItem(
      "flopwire.admin.session",
      JSON.stringify({ token: "expired", user: admin }),
    );
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        json({ detail: "credential is invalid or expired" }, 401),
      ),
    );
    render(<App />);
    await screen.findByRole("heading", { name: "Sign in" });
    expect(sessionStorage.getItem("flopwire.admin.session")).toBeNull();
  });

  it("keeps the full administration route order in the mobile navigation", async () => {
    installSession();
    mockHealth();
    render(<App />);
    await screen.findByRole("heading", { name: "System health" });

    const navigation = screen.getAllByRole("navigation", {
      name: "Administration",
    });
    expect(navigation).toHaveLength(2);
    expect(
      within(navigation[1])
        .getAllByRole("link")
        .map((link) => link.textContent),
    ).toEqual([
      "System health",
      "People & devices",
      "Collection policy",
      "Archive control",
      "Audit log",
    ]);

    fireEvent.click(
      within(navigation[1]).getByRole("link", { name: "Archive control" }),
    );
    expect(window.location.hash).toBe("#archive");
    await screen.findByRole("heading", { name: "Archive control" });
  });

  it("labels last-known health as stale after refresh fails", async () => {
    installSession();
    let failing = false;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (failing) return json({ detail: "upstream unavailable" }, 503);
        const url = String(input);
        if (url === "/healthz")
          return json({
            status: "ok",
            database: "ready",
            object_store: "ready",
            search: "messages",
          });
        if (url === "/v1/admin/status") return json(adminStatus());
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    await waitFor(() => expect(screen.getAllByText("messages")).toHaveLength(2));

    failing = true;
    fireEvent.click(screen.getByRole("button", { name: "Refresh state" }));

    await screen.findByRole("alert");
    expect(screen.getByText("stale")).toBeTruthy();
    expect(screen.getAllByText("messages")).toHaveLength(2);
    expect(screen.queryByLabelText("Loading")).toBeNull();
  });

  it("accepts only the newest overlapping health refresh", async () => {
    installSession();
    const pending: Array<{
      url: string;
      response: ReturnType<typeof deferred<Response>>;
    }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const response = deferred<Response>();
        pending.push({ url: String(input), response });
        return response.promise;
      }),
    );
    render(<App />);
    await waitFor(() => expect(pending).toHaveLength(2));

    fireEvent.click(screen.getByRole("button", { name: "Refresh state" }));
    await waitFor(() => expect(pending).toHaveLength(4));
    pending[2].response.resolve(
      response({
        status: "ok",
        database: "ready",
        object_store: "ready",
        search: "unavailable",
      }),
    );
    pending[3].response.resolve(response(adminStatus()));
    await waitFor(() =>
      expect(screen.getAllByText("unavailable")).toHaveLength(2),
    );

    pending[0].response.resolve(
      response({
        status: "ok",
        database: "ready",
        object_store: "ready",
        search: "messages",
      }),
    );
    pending[1].response.resolve(response(adminStatus()));
    await Promise.resolve();
    await Promise.resolve();
    expect(screen.queryByText("messages")).toBeNull();
    expect(screen.getByText(/OBSERVED/)).toBeTruthy();
  });

  it("waits for a slow health poll to settle before scheduling another", async () => {
    vi.useFakeTimers();
    installSession();
    const health = deferred<Response>();
    const status = deferred<Response>();
    let requests = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        requests++;
        return String(input) === "/healthz" ? health.promise : status.promise;
      }),
    );
    render(<App />);
    await act(async () => Promise.resolve());
    expect(requests).toBe(2);

    await vi.advanceTimersByTimeAsync(16_000);
    expect(requests).toBe(2);
    await act(async () => {
      health.resolve(
        response({
          status: "ok",
          database: "ready",
          object_store: "ready",
          search: "messages",
        }),
      );
      status.resolve(response(adminStatus()));
      await Promise.resolve();
    });
    await vi.advanceTimersByTimeAsync(14_999);
    expect(requests).toBe(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(requests).toBe(4);
  });

  it("does not render an empty corpus state when people loading fails", async () => {
    installSession();
    window.location.hash = "people";
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => json({ detail: "database unavailable" }, 503)),
    );
    render(<App />);

    await screen.findByRole("alert");
    await waitFor(() => expect(screen.queryByLabelText("Loading")).toBeNull());
    expect(screen.queryByText("Nothing recorded")).toBeNull();
  });

  it("tracks asynchronous session deletion until physical purge completes", async () => {
    installSession();
    window.location.hash = "archive";
    const queued = deletion("queued");
    const complete = {
      ...queued,
      state: "complete",
      attempts: 1,
      completed_at: "2026-08-22T12:01:00Z",
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/v1/admin/status") return json(adminStatus());
        if (url === "/v1/admin/conversations/session-123" && init?.method === "DELETE")
          return json({ deletion: queued }, 202);
        if (url === `/v1/admin/deletions/${queued.id}`)
          return json({ deletion: complete });
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    await screen.findByRole("heading", { name: "Archive control" });
    fireEvent.change(screen.getByLabelText("Exact conversation ID"), {
      target: { value: "session-123" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Review deletion" }));
    expect(document.activeElement?.textContent).toBe(
      "Delete session-123 permanently?",
    );
    fireEvent.click(screen.getByRole("button", { name: "Confirm deletion" }));

    expect(
      await screen.findByText("Conversation session-123 was deleted"),
    ).toBeTruthy();
    expect(screen.getByText("complete")).toBeTruthy();
  });

  it("persists active deletion tracking through route changes and reload", async () => {
    installSession();
    window.location.hash = "archive";
    const queued = deletion("queued");
    const poll = deferred<Response>();
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/v1/admin/status") return response(adminStatus());
        if (url === "/v1/admin/audit?limit=100")
          return response({ events: [] });
        if (url === "/v1/admin/conversations/session-123" && init?.method === "DELETE")
          return response({ deletion: queued }, 202);
        if (url === `/v1/admin/deletions/${queued.id}`) return poll.promise;
        throw new Error(`unexpected request ${url}`);
      }),
    );
    const rendered = render(<App />);
    await screen.findByRole("heading", { name: "Archive control" });
    fireEvent.change(screen.getByLabelText("Exact conversation ID"), {
      target: { value: "session-123" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Review deletion" }));
    fireEvent.click(screen.getByRole("button", { name: "Confirm deletion" }));
    await screen.findByText("Deletion queued for session-123");
    expect(
      (screen.getByLabelText("Exact conversation ID") as HTMLInputElement)
        .disabled,
    ).toBe(true);

    fireEvent.click(screen.getAllByRole("link", { name: "Audit log" })[0]);
    await screen.findByRole("heading", { name: "Audit log" });
    fireEvent.click(screen.getAllByRole("link", { name: "Archive control" })[0]);
    await screen.findByText("Deletion queued for session-123");

    rendered.unmount();
    render(<App />);
    await screen.findByText("Deletion queued for session-123");
    expect(
      (screen.getByLabelText("Exact conversation ID") as HTMLInputElement)
        .value,
    ).toBe("session-123");
  });

  it("marks deletion tracking stale after poll failure and retries manually", async () => {
    installSession();
    window.location.hash = "archive";
    const queued = deletion("queued");
    sessionStorage.setItem(
      `flopwire.admin.deletion.${admin.id}`,
      JSON.stringify({
        job: queued,
        observed_at: "2026-08-22T12:00:00Z",
        stale: false,
      }),
    );
    let pollCount = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "/v1/admin/status") return response(adminStatus());
        if (url === `/v1/admin/deletions/${queued.id}`) {
          pollCount++;
          if (pollCount === 1)
            return response({ detail: "worker unavailable" }, 503);
          return response({
            deletion: {
              ...queued,
              state: "complete",
              attempts: 1,
              completed_at: "2026-08-22T12:01:00Z",
            },
          });
        }
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    await screen.findByText("stale");
    expect(screen.getByText(/displayed job state is stale/)).toBeTruthy();
    expect(screen.getByText(/LAST OBSERVED/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Retry status" }));
    expect(
      await screen.findByText("Conversation session-123 was deleted"),
    ).toBeTruthy();
  });

  it("serializes deletion polling when a request exceeds the interval", async () => {
    vi.useFakeTimers();
    installSession();
    window.location.hash = "archive";
    const queued = deletion("queued");
    sessionStorage.setItem(
      `flopwire.admin.deletion.${admin.id}`,
      JSON.stringify({
        job: queued,
        observed_at: "2026-08-22T12:00:00Z",
        stale: false,
      }),
    );
    const firstPoll = deferred<Response>();
    const secondPoll = deferred<Response>();
    let pollRequests = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "/v1/admin/status")
          return Promise.resolve(response(adminStatus()));
        if (url === `/v1/admin/deletions/${queued.id}`) {
          pollRequests++;
          return pollRequests === 1 ? firstPoll.promise : secondPoll.promise;
        }
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    await act(async () => Promise.resolve());
    expect(pollRequests).toBe(1);
    await vi.advanceTimersByTimeAsync(2_500);
    expect(pollRequests).toBe(1);

    await act(async () => {
      firstPoll.resolve(response({ deletion: queued }));
      await Promise.resolve();
    });
    await vi.advanceTimersByTimeAsync(1_999);
    expect(pollRequests).toBe(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(pollRequests).toBe(2);
  });

  it.each([
    { label: "unknown state", patch: { state: "forgotten" } },
    { label: "invalid attempts", patch: { attempts: "one" } },
    { label: "wrong owner", patch: { requested_by: "another-user" } },
    { label: "invalid request date", patch: { requested_at: "yesterday" } },
  ])("discards persisted deletion tracking with $label", async ({ patch }) => {
    installSession();
    window.location.hash = "archive";
    const key = `flopwire.admin.deletion.${admin.id}`;
    sessionStorage.setItem(
      key,
      JSON.stringify({
        job: { ...deletion("queued"), ...patch },
        observed_at: "2026-08-22T12:00:00Z",
        stale: false,
      }),
    );
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input) === "/v1/admin/status")
          return response(adminStatus());
        throw new Error(`unexpected request ${String(input)}`);
      }),
    );
    render(<App />);
    await screen.findByRole("heading", { name: "Archive control" });
    const field = screen.getByLabelText(
      "Exact conversation ID",
    ) as HTMLInputElement;
    expect(field.disabled).toBe(false);
    expect(field.value).toBe("");
    expect(sessionStorage.getItem(key)).toBeNull();
  });

  it("can stop tracking a missing nonterminal job without claiming cancellation", async () => {
    installSession();
    window.location.hash = "archive";
    const queued = deletion("queued");
    const key = `flopwire.admin.deletion.${admin.id}`;
    sessionStorage.setItem(
      key,
      JSON.stringify({
        job: queued,
        observed_at: "2026-08-22T12:00:00Z",
        stale: false,
      }),
    );
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "/v1/admin/status") return response(adminStatus());
        if (url === `/v1/admin/deletions/${queued.id}`)
          return response({ detail: "deletion job not found" }, 404);
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    const stop = await screen.findByRole("button", { name: "Stop tracking" });
    fireEvent.click(stop);
    expect(confirm.mock.calls[0][0]).toContain("purge may continue");
    const field = screen.getByLabelText(
      "Exact conversation ID",
    ) as HTMLInputElement;
    expect(field.disabled).toBe(false);
    expect(field.value).toBe("");
    expect(sessionStorage.getItem(key)).toBeNull();
  });

  it.each([
    {
      open: "Create invite",
      field: "Email",
      value: "grace@example.test",
      submit: "Create invitation",
      endpoint: "/v1/admin/invites",
      result: { code: "invite-once" },
      resultText: "invite-once",
    },
    {
      open: "Create service",
      field: "Service name",
      value: "CI collector",
      submit: "Create service account",
      endpoint: "/v1/admin/service-accounts",
      result: { token: "service-once", scope: "upload" },
      resultText: "service-once",
    },
  ])("locks one-time $open controls while creating", async (scenario) => {
    installSession();
    window.location.hash = "people";
    const mutation = deferred<Response>();
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "/v1/admin/users") return response({ users: [admin] });
        if (url === "/v1/admin/devices") return response({ devices: [] });
        if (url === scenario.endpoint) return mutation.promise;
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    await screen.findAllByText("Ada Admin");
    fireEvent.click(screen.getByRole("button", { name: scenario.open }));
    fireEvent.change(screen.getByLabelText(scenario.field), {
      target: { value: scenario.value },
    });
    fireEvent.click(screen.getByRole("button", { name: scenario.submit }));

    expect((screen.getByRole("button", { name: /Close/ }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Create invite" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Create service" }) as HTMLButtonElement).disabled).toBe(true);
    expect(
      screen
        .getAllByRole("link", { name: "Audit log" })
        .every((link) => link.getAttribute("aria-disabled") === "true"),
    ).toBe(true);
    expect(
      screen
        .getAllByRole("button", { name: "Sign out" })
        .every((button) => (button as HTMLButtonElement).disabled),
    ).toBe(true);
    fireEvent.click(screen.getAllByRole("link", { name: "Audit log" })[0]);
    expect(window.location.hash).toBe("#people");
    expect(
      screen.getByRole("heading", { name: "People & devices" }),
    ).toBeTruthy();
    window.history.pushState(null, "", "#audit");
    fireEvent(window, new HashChangeEvent("hashchange"));
    await waitFor(() => expect(window.location.hash).toBe("#people"));
    const unload = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(unload);
    expect(unload.defaultPrevented).toBe(true);

    mutation.resolve(response(scenario.result));
    await screen.findByText(scenario.resultText);
    expect((screen.getByRole("button", { name: /Close/ }) as HTMLButtonElement).disabled).toBe(false);
    expect(
      screen
        .getAllByRole("link", { name: "Audit log" })
        .every((link) => link.getAttribute("aria-disabled") === null),
    ).toBe(true);
  });

  it("locks a device revoke mutation against duplicate submission", async () => {
    installSession();
    window.location.hash = "people";
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const revokeResponse = deferred<Response>();
    let revoked = false;
    let revokeRequests = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/v1/admin/users") return response({ users: [admin] });
        if (url === "/v1/admin/devices")
          return response({
            devices: [
              {
                id: "22222222-2222-2222-2222-222222222222",
                user_id: admin.id,
                name: "ada-mac",
                platform: "darwin-arm64",
                revoked_at: revoked ? "2026-08-22T12:00:00Z" : undefined,
                created_at: "2026-08-21T12:00:00Z",
              },
            ],
          });
        if (url.includes("/revoke") && init?.method === "POST") {
          revokeRequests++;
          return revokeResponse.promise;
        }
        throw new Error(`unexpected request ${url}`);
      }),
    );
    render(<App />);
    const revoke = await screen.findByRole("button", { name: "Revoke" });
    fireEvent.click(revoke);
    fireEvent.click(revoke);
    expect(revokeRequests).toBe(1);
    expect(screen.getByRole("button", { name: "Revoking…" })).toBeTruthy();

    revoked = true;
    revokeResponse.resolve(response({ revoked_at: "2026-08-22T12:00:00Z" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull());
  });

  it("reports clipboard failure with a manual recovery action", async () => {
    installSession();
    window.location.hash = "people";
    const clipboardDescriptor = Object.getOwnPropertyDescriptor(
      navigator,
      "clipboard",
    );
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: vi.fn().mockRejectedValue(new Error("denied")) },
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url === "/v1/admin/users") return json({ users: [admin] });
        if (url === "/v1/admin/devices") return json({ devices: [] });
        if (url === "/v1/admin/invites") return json({ code: "invite-once" });
        throw new Error(`unexpected request ${url}`);
      }),
    );
    try {
      render(<App />);
      await screen.findByText("Ada Admin");
      fireEvent.click(screen.getByRole("button", { name: "Create invite" }));
      fireEvent.change(screen.getByLabelText("Email"), {
        target: { value: "grace@example.test" },
      });
      fireEvent.click(
        screen.getByRole("button", { name: "Create invitation" }),
      );
      await screen.findByText("invite-once");
      fireEvent.click(screen.getByRole("button", { name: "Copy code" }));
      expect(
        await screen.findByText(
          "Could not copy the invitation code. Select and copy it manually.",
        ),
      ).toBeTruthy();
    } finally {
      if (clipboardDescriptor)
        Object.defineProperty(navigator, "clipboard", clipboardDescriptor);
      else Reflect.deleteProperty(navigator, "clipboard");
    }
  });
});

function installSession() {
  sessionStorage.setItem(
    "flopwire.admin.session",
    JSON.stringify({ token: "test-token", user: admin }),
  );
}

function adminStatus() {
  return {
    storage: { used_bytes: 256, admin_bytes: 256, quota_bytes: 1024 },
    index: { queue_depth: 0, queue_capacity: 1024 },
  };
}

function mockHealth() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/healthz")
        return json({
          status: "ok",
          database: "ready",
          object_store: "ready",
          search: "messages",
        });
      if (url === "/v1/admin/status") return json(adminStatus());
      throw new Error(`unexpected request ${url}`);
    }),
  );
}

function deletion(state: "queued" | "complete" | "failed") {
  return {
    id: "44444444-4444-4444-4444-444444444444",
    conversation_id: "session-123",
    device_id: "55555555-5555-4555-8555-555555555555",
    agent: "codex",
    session_id: "rollout-1",
    requested_by: admin.id,
    state,
    attempts: 0,
    requested_at: "2026-08-22T12:00:00Z",
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function response(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function json(value: unknown, status = 200) {
  return Promise.resolve(response(value, status));
}
