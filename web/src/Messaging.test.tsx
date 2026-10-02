import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { ACCEPT_STATEMENT } from "./messaging";

const member = {
  id: "33333333-3333-4333-8333-333333333333",
  name: "Alex Member",
  email: "alex@example.test",
  role: "member",
  identity_type: "human",
  disabled: false,
  created_at: "2026-08-21T12:00:00Z",
};
const gary = "44444444-4444-4444-8444-444444444444";
const now = new Date().toISOString();
const heldGroup = {
  user: "gary@example.test",
  user_id: gary,
  user_name: "Gary",
  count: 3,
  oldest: now,
  newest: now,
  more: 1,
  messages: [
    {
      id: "m1",
      agent: "claude",
      session: "gary-1111",
      repo: "api",
      branch: "main",
      intent: "request",
      addressed: "session",
      preview: '<img src=x onerror="alert(1)"> **run** the migration',
      bytes: 120,
      sent: now,
      expires_at: now,
    },
    {
      id: "m2",
      agent: "codex",
      session: "gary-2222",
      intent: "inform",
      addressed: "user",
      preview: "Heads-up: the cursor is in main",
      bytes: 40,
      sent: now,
      expires_at: now,
    },
  ],
};

function json(value: unknown, status = 200) {
  return Promise.resolve(
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    }),
  );
}

type Call = { url: string; method: string; body?: string; auth: string | null };

// server answers the bus routes from mutable state and records each call.
function server(state: { held: unknown[]; accepted: unknown[]; heldStatus?: number }) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      calls.push({ url, method, body: init?.body as string | undefined, auth: new Headers(init?.headers).get("Authorization") });
      if (url === "/v1/bus/held" && method === "GET") {
        if (state.heldStatus)
          return json({ detail: "accepting or revoking a sender needs your login session" }, state.heldStatus);
        return json({ senders: state.held });
      }
      if (url === "/v1/bus/accepts" && method === "GET")
        return json({ accepted: state.accepted, held: [] });
      if (url === "/v1/bus/accepts" && method === "POST") {
        state.held = [];
        state.accepted = [{ user: "gary@example.test", user_id: gary, accepted_at: now }];
        return json({ user: "gary@example.test", user_id: gary, accepted: true, released: 3 });
      }
      if (url === `/v1/bus/accepts/${gary}` && method === "DELETE") {
        state.accepted = [];
        return json({ user: "gary@example.test", user_id: gary, accepted: false, reheld: 2 });
      }
      throw new Error(`unexpected request ${method} ${url}`);
    }),
  );
  return calls;
}

describe("messaging page", () => {
  beforeEach(() => {
    sessionStorage.clear();
    window.history.replaceState(null, "", "/");
    sessionStorage.setItem(
      "flopwire.admin.session",
      JSON.stringify({ token: "member-session", user: member }),
    );
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("shows a member only their messaging page, with held previews as inert text", async () => {
    const calls = server({ held: [heldGroup], accepted: [] });
    const { container } = render(<App />);
    await screen.findByRole("heading", { name: "Messaging" });
    const nav = screen.getAllByRole("navigation", { name: "Administration" })[0];
    expect(within(nav).getAllByRole("link").map((l) => l.textContent)).toEqual(["Messaging"]);
    await screen.findByText("gary@example.test");
    expect(screen.getByText("3 held")).toBeTruthy();
    expect(screen.getByText('<img src=x onerror="alert(1)"> **run** the migration')).toBeTruthy();
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("strong")?.textContent).not.toBe("run");
    expect(screen.getByText(/claude · api@main · request/)).toBeTruthy();
    expect(screen.getByText("and 1 more")).toBeTruthy();
    expect(screen.getByText(/You accept no one/)).toBeTruthy();
    // Only the person's own routes, with their login session.
    expect(calls.every((c) => c.auth === "Bearer member-session")).toBe(true);
    expect(calls.map((c) => c.url).sort()).toEqual(["/v1/bus/accepts", "/v1/bus/held"]);
  });

  it("accepts a sender only after stating what accepting means", async () => {
    const calls = server({ held: [heldGroup], accepted: [] });
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: "Review and accept" }));
    const panel = screen.getByRole("region", { name: "Accept gary@example.test" });
    expect(within(panel).getByText(ACCEPT_STATEMENT)).toBeTruthy();
    expect(within(panel).getByText(/releases the 3 held messages above/)).toBeTruthy();
    expect(calls.some((c) => c.method === "POST")).toBe(false);
    fireEvent.click(within(panel).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("region", { name: "Accept gary@example.test" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Review and accept" }));
    fireEvent.click(screen.getByRole("button", { name: "Accept gary@example.test" }));
    await screen.findByText(/Accepted gary@example.test. 3 held messages were released/);
    const post = calls.find((c) => c.method === "POST");
    expect(post && JSON.parse(post.body ?? "{}")).toEqual({ sender: gary });
    await screen.findByRole("button", { name: "Revoke gary@example.test" });
    expect(screen.getByText(/No one you have not accepted has messaged your agents/)).toBeTruthy();
  });

  it("revokes in one step and says what happens to undelivered messages", async () => {
    const calls = server({ held: [], accepted: [{ user: "gary@example.test", user_id: gary, accepted_at: now }] });
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: "Revoke gary@example.test" }));
    await screen.findByText(/Revoked gary@example.test. 2 undelivered messages are held again/);
    expect(calls.filter((c) => c.method === "DELETE").map((c) => c.url)).toEqual([`/v1/bus/accepts/${gary}`]);
    await waitFor(() => expect(screen.getByText(/You accept no one/)).toBeTruthy());
  });

  it("shows empty states", async () => {
    server({ held: [], accepted: [] });
    render(<App />);
    await screen.findByText(/No one you have not accepted has messaged your agents/);
    expect(screen.getByText(/You accept no one/)).toBeTruthy();
    expect(screen.getByText("HELD · 0")).toBeTruthy();
  });

  it("shows an error with its recovery", async () => {
    server({ held: [], accepted: [], heldStatus: 403 });
    render(<App />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("needs your login session");
    expect(alert.textContent).toContain("Reload this page. If it persists, sign in again");
  });

  it("gives an administrator the messaging page beside the admin routes", async () => {
    sessionStorage.setItem(
      "flopwire.admin.session",
      JSON.stringify({ token: "admin-session", user: { ...member, role: "admin" } }),
    );
    window.history.replaceState(null, "", "/#messages");
    const calls = server({ held: [], accepted: [] });
    render(<App />);
    await screen.findByRole("heading", { name: "Messaging" });
    await screen.findByText(/You accept no one/);
    // Their own lists only: no route names another person.
    expect(calls.map((c) => c.url).sort()).toEqual(["/v1/bus/accepts", "/v1/bus/held"]);
  });
});
