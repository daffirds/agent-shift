// Typed client for the `agent-shift ui` JSON API.

export type Ref = { id: string; name?: string };

export type Trigger = {
  workspace: string;
  conversations: Ref[];
  from?: Ref[];
  contains?: string[];
  regex?: string;
};

export type Agent = {
  name: string;
  disabled?: boolean;
  engine: "claude" | "codex";
  model?: string;
  effort?: string;
  workdir: string;
  worktree?: boolean;
  autonomy?: "auto" | "full" | "readonly";
  system_prompt?: string;
  trigger: Trigger;
  thread_from?: Ref[];
  footer?: string;
  extra_args?: string[];
};

export type Workspace = {
  team_id: string;
  name: string;
  url?: string;
  user_id: string;
  user_name?: string;
  app_id?: string;
  token_hint?: string;
  token_error?: string;
  app_url?: string;
};

export type Settings = {
  max_sessions: number;
  idle_timeout: string;
  thread_ttl: string;
  catch_up: string;
  log_level: string;
};

export type EngineState = {
  id: string;
  label: string;
  installed: boolean;
  version?: string;
  logged_in: boolean;
  detail?: string;
  install: string;
  login: string;
};

export type WorkspaceStatus = {
  team_id: string;
  name: string;
  user: string;
  connected: boolean;
  since: string;
  error?: string;
  synced: string;
  poll?: PollStatus;
};

export type PollStatus = {
  last_poll?: string;
  last_ok?: string;
  covered?: string;
  conversations: number;
  threads: number;
  hot: number;
  search: string;
  sessions: number;
  replies: number;
  error?: string;
};

export type DaemonStatus = {
  pid: number;
  version: string;
  started: string;
  workspaces: WorkspaceStatus[] | null;
  sessions: Record<string, number> | null;
};

export type State = {
  slack: { create_app_url: string; manifest: string };
  workspaces: Workspace[];
  agents: Agent[];
  settings: Settings;
  engines: EngineState[];
  daemon: { running: boolean; pid?: number; status?: DaemonStatus; error?: string };
  autostart: { supported: boolean; enabled: boolean; detail?: string };
  paths: { config: string; log: string };
  version: string;
  home: string;
  warnings: string[];
};

export type Conversation = { id: string; label: string; name: string; kind: "self" | "channel" | "private" | "dm" | "group" };
export type Sender = { id: string; name: string; kind: "person" | "app" | "workflow" };
export type Effort = { level: string; description?: string };
export type Model = { id: string; name: string; description?: string; efforts: Effort[]; default_effort?: string };

export type SaveResult = { ok: boolean; reloaded: boolean; warnings: string[]; warning?: string; missing_scopes?: string[] };

export type SessionRecord = {
  id: string;
  agent: string;
  engine: string;
  team_id: string;
  channel: string;
  channel_name?: string;
  thread_ts: string;
  permalink?: string;
  from?: string;
  title?: string;
  workdir: string;
  state: "queued" | "running" | "idle" | "sleeping" | "ended" | "failed";
  last_error?: string;
  turns: number;
  created_at: string;
  last_active: string;
};

const TOKEN_KEY = "agent-shift-token";

function token(): string {
  const m = location.hash.match(/[#&]t=([0-9a-f]+)/);
  if (m) {
    try {
      sessionStorage.setItem(TOKEN_KEY, m[1]);
    } catch {
      /* private mode: keep it in the hash */
      return m[1];
    }
    history.replaceState(null, "", location.pathname + location.search);
    return m[1];
  }
  try {
    return sessionStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

export const hasToken = () => token() !== "";

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: { "X-Agent-Shift-Token": token(), ...(body !== undefined ? { "Content-Type": "application/json" } : {}) },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    /* not JSON */
  }
  if (!res.ok) {
    const msg = (data as { error?: string } | null)?.error ?? `HTTP ${res.status}`;
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

export const api = {
  state: () => call<State>("GET", "/api/state"),
  addWorkspace: (token: string) => call<SaveResult & { workspace: Workspace }>("POST", "/api/workspaces", { token }),
  removeWorkspace: (team: string) => call<SaveResult>("DELETE", `/api/workspaces/${encodeURIComponent(team)}`),
  conversations: (team: string) => call<Conversation[]>("GET", `/api/workspaces/${encodeURIComponent(team)}/conversations`),
  senders: (team: string, convs: string[]) =>
    call<Sender[]>("GET", `/api/workspaces/${encodeURIComponent(team)}/senders?c=${encodeURIComponent(convs.join(","))}`),
  models: (engine: string) => call<{ models: Model[]; error: string }>("GET", `/api/models/${engine}`),
  checkDir: (path: string) => call<{ path: string; exists: boolean; git: boolean }>("POST", "/api/check-dir", { path }),
  saveAgent: (agent: Agent, old_name: string) => call<SaveResult>("PUT", "/api/agents", { agent, old_name }),
  deleteAgent: (name: string) => call<SaveResult>("DELETE", `/api/agents/${encodeURIComponent(name)}`),
  saveSettings: (s: Settings) => call<SaveResult>("PUT", "/api/settings", s),
  daemon: (action: "start" | "stop" | "restart") => call<{ ok: boolean }>("POST", `/api/daemon/${action}`),
  autostart: (enabled: boolean) => call<{ ok: boolean }>("PUT", "/api/autostart", { enabled }),
  sessions: () => call<SessionRecord[]>("GET", "/api/sessions"),
};
