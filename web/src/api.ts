export type Health = {
  status: "ok" | "not_ready";
  database: string;
  object_store: string;
  search: string;
};

export type User = {
  id: string;
  name: string;
  email: string;
  role: "admin" | "member";
  identity_type: "human" | "service";
  disabled: boolean;
  created_at: string;
};
export type Device = {
  id: string;
  user_id: string;
  name: string;
  platform: string;
  last_seen_at?: string;
  revoked_at?: string;
  created_at: string;
};
export type AuditEvent = {
  id: string;
  actor_id?: string;
  device_id?: string;
  action: string;
  target_type?: string;
  target_id?: string;
  metadata?: Record<string, unknown>;
  created_at: string;
};
export type Policy = {
  max_storage_bytes: number;
  max_user_bytes: number;
  path_rules: string[];
  unplaceable: "" | "local" | "upload" | "exclude";
  updated_by?: string;
  updated_at?: string;
};
export type AdminStatus = {
  storage: { used_bytes: number; admin_bytes: number; quota_bytes: number };
  index: {
    queue_depth: number;
    queue_capacity: number;
    last_indexed_at?: string;
    error?: string;
  };
  last_transition?: AuditEvent;
  last_backup?: AuditEvent;
};
export type DeletionJob = {
  id: string;
  conversation_id: string;
  device_id: string;
  agent: string;
  session_id: string;
  requested_by: string;
  state: "queued" | "purging" | "retry_wait" | "complete" | "failed";
  attempts: number;
  next_attempt_at?: string;
  last_error?: string;
  requested_at: string;
  completed_at?: string;
};

export class APIError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

export async function request<T>(
  path: string,
  token = "",
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init.body && !headers.has("Content-Type"))
    headers.set("Content-Type", "application/json");
  const response = await fetch(path, { ...init, headers });
  if (!response.ok) {
    const problem = (await response.json().catch(() => ({}))) as {
      detail?: string;
    };
    if (response.status === 401 && token)
      window.dispatchEvent(new Event("flopwire:unauthorized"));
    throw new APIError(
      response.status,
      problem.detail ?? `Request failed with ${response.status}`,
    );
  }
  return response.json() as Promise<T>;
}

// Message bus acceptance (GET /v1/bus/held, /v1/bus/accepts). The held
// list carries previews only, never a whole message; a preview is text
// another person's agent wrote, shown as inert text.
export type HeldMessage = {
  id: string;
  agent: string;
  session: string;
  repo?: string;
  branch?: string;
  intent: "request" | "inform" | "done";
  addressed: "session" | "user";
  preview: string;
  bytes: number;
  refs?: number;
  sent: string;
  expires_at: string;
};
export type HeldGroup = {
  user: string;
  user_id: string;
  user_name?: string;
  count: number;
  oldest: string;
  newest: string;
  messages: HeldMessage[];
  more?: number;
};
export type Accepted = { user: string; user_id: string; accepted_at: string };
export type AcceptResult = {
  user: string;
  user_id: string;
  accepted: boolean;
  released?: number;
  reheld?: number;
};
