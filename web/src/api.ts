// Typed client for the Clockwork /api/v1 endpoints.

export type RunnerType = "http" | "shell";

export interface HttpConfig {
  method: string;
  url: string;
  headers?: Record<string, string>;
  query?: Record<string, string>;
  body?: string;
  expected_status?: number[];
}

export interface ShellConfig {
  command: string;
  working_dir?: string;
  env?: Record<string, string>;
}

export interface Schedule {
  cron: string;
  timezone: string;
  misfire_policy: "skip" | "run_once";
  concurrency_policy: "forbid" | "allow";
}

export interface RetryPolicy {
  max_attempts: number;
  backoff_initial_seconds: number;
  backoff_max_seconds: number;
}

export interface Task {
  id: string;
  application_id: string;
  name: string;
  description: string;
  runner: { type: RunnerType; config: HttpConfig | ShellConfig };
  schedule: Schedule | null;
  timeout_seconds: number;
  retry: RetryPolicy;
  enabled: boolean;
  next_run_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface TaskInput {
  application_id?: string;
  name: string;
  description: string;
  runner: { type: RunnerType; config: HttpConfig | ShellConfig };
  schedule: Schedule | null;
  timeout_seconds: number;
  retry: RetryPolicy;
  enabled: boolean;
}

export interface Application {
  id: string;
  name: string;
  description: string;
  enabled: boolean;
  task_count: number;
  created_at: string;
}

export interface Meta {
  version: string;
  shell_enabled: boolean;
  runners: RunnerType[];
}

export interface TaskPage {
  tasks: Task[];
  next_cursor?: string;
}

export type ExecutionStatus = "queued" | "running" | "succeeded" | "failed" | "retrying" | "interrupted" | "canceled" | "skipped";
export const executionStatuses: ExecutionStatus[] = ["queued", "running", "succeeded", "failed", "retrying", "interrupted", "canceled", "skipped"];

export interface Execution {
  id: string;
  task_id: string;
  task_name: string;
  application_id: string;
  trigger: "scheduled" | "manual" | "api";
  status: ExecutionStatus;
  attempt: number;
  scheduled_for: string | null;
  output: unknown;
  error: unknown;
  created_at: string;
  run_after: string;
  started_at: string | null;
  finished_at: string | null;
  duration_ms: number | null;
}

/** executions.error: why an attempt failed, and when the next attempt runs if it was retried. */
export interface ExecutionError {
  kind: string;
  message: string;
  retryable: boolean;
  retry_at?: string;
}

/** executions.output for HTTP tasks. */
export interface HttpOutput {
  status_code: number;
  content_type?: string;
  body: string;
  body_truncated: boolean;
  body_bytes: number;
}

/** executions.output for shell tasks. */
export interface ShellOutput {
  exit_code: number;
  stdout: string;
  stdout_truncated: boolean;
  stderr: string;
  stderr_truncated: boolean;
}

export interface ExecutionPage {
  executions: Execution[];
  next_cursor?: string;
}

function query(params: Record<string, string | number | boolean | undefined>): string {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "") qs.set(k, String(v));
  const s = qs.toString();
  return s ? `?${s}` : "";
}

/** Error returned by the API, carrying per-field validation details when present. */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details: Record<string, string> = {},
  ) {
    super(message);
  }
}

const TOKEN_KEY = "clockwork.adminToken";

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setToken(token: string): void {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* storage unavailable: token lives only for this page load */
  }
  window.dispatchEvent(new Event("clockwork:auth"));
}

async function request<T>(method: string, path: string, body?: unknown, tokenOverride?: string): Promise<T> {
  const headers: Record<string, string> = { Authorization: `Bearer ${tokenOverride ?? getToken()}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";

  let res: Response;
  try {
    res = await fetch(`/api/v1${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, "network", "Cannot reach the Clockwork server.");
  }

  if (res.status === 401) {
    // A stored token that stops working signs the user out; a candidate token being verified does not.
    if (tokenOverride === undefined) setToken("");
    throw new ApiError(401, "unauthorized", "Your admin token was rejected.");
  }
  if (res.status === 204) return undefined as T;

  const data = await res.json().catch(() => null);
  if (!res.ok) {
    const e = data?.error ?? {};
    throw new ApiError(res.status, e.code ?? "error", e.message ?? `Request failed (${res.status})`, e.details ?? {});
  }
  return data as T;
}

export const api = {
  meta: () => request<Meta>("GET", "/meta"),
  /** Checks a candidate token without storing it. */
  verifyToken: (token: string) => request<Meta>("GET", "/meta", undefined, token),
  applications: () => request<{ applications: Application[] }>("GET", "/applications").then((r) => r.applications),
  listTasks: (params: { q?: string; enabled?: boolean; application_id?: string; cursor?: string; limit?: number } = {}) =>
    request<TaskPage>("GET", `/tasks${query(params)}`),
  getTask: (id: string) => request<Task>("GET", `/tasks/${encodeURIComponent(id)}`),
  createTask: (input: TaskInput) => request<Task>("POST", "/tasks", input),
  updateTask: (id: string, input: Partial<TaskInput>) => request<Task>("PATCH", `/tasks/${encodeURIComponent(id)}`, input),
  setEnabled: (id: string, enabled: boolean) =>
    request<Task>("POST", `/tasks/${encodeURIComponent(id)}/${enabled ? "enable" : "disable"}`),
  deleteTask: (id: string) => request<void>("DELETE", `/tasks/${encodeURIComponent(id)}`),
  /** Queues an immediate run of a task. */
  runTask: (id: string) => request<Execution>("POST", `/tasks/${encodeURIComponent(id)}/run`),
  listExecutions: (params: { task_id?: string; status?: ExecutionStatus | ""; cursor?: string; limit?: number } = {}) =>
    request<ExecutionPage>("GET", `/executions${query(params)}`),
};
