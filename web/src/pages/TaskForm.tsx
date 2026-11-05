import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, ApiError, type HttpConfig, type Meta, type RunnerType, type ShellConfig, type Task, type TaskInput } from "../api";
import { formatDateTime, localTimezone, relativeTime } from "../format";
import { useAsync } from "../hooks";
import { formatLines, parseLines, parseStatusList } from "../kv";
import { Badge, Button, Card, ConfirmDialog, ErrorState, Field, PageHeader, Spinner, inputClass } from "../ui";

/** Flat, string-based form state; converted to/from the API shape at the edges. */
interface FormState {
  name: string;
  description: string;
  runnerType: RunnerType;
  method: string;
  url: string;
  headers: string;
  query: string;
  body: string;
  expectedStatus: string;
  command: string;
  workingDir: string;
  env: string;
  scheduled: boolean;
  cron: string;
  timezone: string;
  misfire: "skip" | "run_once";
  concurrency: "forbid" | "allow";
  timeout: string;
  maxAttempts: string;
  backoffInitial: string;
  backoffMax: string;
  enabled: boolean;
}

const blank = (): FormState => ({
  name: "",
  description: "",
  runnerType: "http",
  method: "GET",
  url: "",
  headers: "",
  query: "",
  body: "",
  expectedStatus: "",
  command: "",
  workingDir: "",
  env: "",
  scheduled: true,
  cron: "*/5 * * * *",
  timezone: localTimezone(),
  misfire: "skip",
  concurrency: "forbid",
  timeout: "30",
  maxAttempts: "1",
  backoffInitial: "10",
  backoffMax: "300",
  enabled: true,
});

function fromTask(t: Task): FormState {
  const f = blank();
  f.name = t.name;
  f.description = t.description;
  f.runnerType = t.runner.type;
  if (t.runner.type === "http") {
    const c = t.runner.config as HttpConfig;
    f.method = c.method;
    f.url = c.url;
    f.headers = formatLines(c.headers, ":");
    f.query = formatLines(c.query, "=");
    f.body = c.body ?? "";
    f.expectedStatus = (c.expected_status ?? []).join(", ");
  } else {
    const c = t.runner.config as ShellConfig;
    f.command = c.command;
    f.workingDir = c.working_dir ?? "";
    f.env = formatLines(c.env, "=");
  }
  f.scheduled = !!t.schedule;
  if (t.schedule) {
    f.cron = t.schedule.cron;
    f.timezone = t.schedule.timezone;
    f.misfire = t.schedule.misfire_policy;
    f.concurrency = t.schedule.concurrency_policy;
  }
  f.timeout = String(t.timeout_seconds);
  f.maxAttempts = String(t.retry.max_attempts);
  f.backoffInitial = String(t.retry.backoff_initial_seconds);
  f.backoffMax = String(t.retry.backoff_max_seconds);
  f.enabled = t.enabled;
  return f;
}

/** Builds the API payload, or returns client-side errors keyed by API field names. */
function toInput(f: FormState): { input?: TaskInput; errors: Record<string, string> } {
  const errors: Record<string, string> = {};
  let config: HttpConfig | ShellConfig;
  if (f.runnerType === "http") {
    const headers = parseLines(f.headers, ":");
    const query = parseLines(f.query, "=");
    const status = parseStatusList(f.expectedStatus);
    if (headers.error) errors["runner.config.headers"] = headers.error;
    if (query.error) errors["runner.config.query"] = query.error;
    if (status.error) errors["runner.config.expected_status"] = status.error;
    config = { method: f.method, url: f.url.trim(), headers: headers.value, query: query.value, body: f.body, expected_status: status.value };
  } else {
    const env = parseLines(f.env, "=");
    if (env.error) errors["runner.config.env"] = env.error;
    config = { command: f.command, working_dir: f.workingDir.trim(), env: env.value };
  }
  const int = (v: string, field: string) => {
    const n = Number(v);
    if (!Number.isInteger(n)) errors[field] = "must be a whole number";
    return n;
  };
  const input: TaskInput = {
    name: f.name.trim(),
    description: f.description.trim(),
    runner: { type: f.runnerType, config },
    schedule: f.scheduled
      ? { cron: f.cron.trim(), timezone: f.timezone.trim(), misfire_policy: f.misfire, concurrency_policy: f.concurrency }
      : null,
    timeout_seconds: int(f.timeout, "timeout_seconds"),
    retry: {
      max_attempts: int(f.maxAttempts, "retry.max_attempts"),
      backoff_initial_seconds: int(f.backoffInitial, "retry.backoff_initial_seconds"),
      backoff_max_seconds: int(f.backoffMax, "retry.backoff_max_seconds"),
    },
    enabled: f.enabled,
  };
  return Object.keys(errors).length ? { errors } : { input, errors };
}

export function TaskForm() {
  const { id } = useParams();
  const isNew = !id;
  const meta = useAsync<Meta>(() => api.meta(), []);
  const existing = useAsync<Task | null>(() => (id ? api.getTask(id) : Promise.resolve(null)), [id]);

  if (meta.status === "loading" || existing.status === "loading") return <Spinner />;
  if (meta.status === "error") return <ErrorState error={meta.error} onRetry={meta.reload} />;
  if (existing.status === "error") {
    return (
      <>
        <PageHeader title="Task" crumb={<Link to="/tasks">Tasks</Link>} />
        <ErrorState error={existing.error} onRetry={existing.reload} />
      </>
    );
  }
  return <Editor key={id ?? "new"} task={existing.data} meta={meta.data} isNew={isNew} />;
}

function Editor({ task, meta, isNew }: { task: Task | null; meta: Meta; isNew: boolean }) {
  const navigate = useNavigate();
  const [f, setF] = useState<FormState>(() => (task ? fromTask(task) : blank()));
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [formError, setFormError] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState<Task | null>(task);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => setErrors({}), [f.runnerType]);

  const set = <K extends keyof FormState>(k: K, v: FormState[K]) => setF((s) => ({ ...s, [k]: v }));
  const err = (field: string) => errors[field];

  async function submit(e: FormEvent) {
    e.preventDefault();
    setFormError("");
    const { input, errors: clientErrors } = toInput(f);
    if (!input) {
      setErrors(clientErrors);
      return;
    }
    setSaving(true);
    try {
      const result = isNew ? await api.createTask(input) : await api.updateTask(saved!.id, input);
      setErrors({});
      if (isNew) {
        navigate(`/tasks/${result.id}`, { replace: true });
      } else {
        setSaved(result);
        setF(fromTask(result));
      }
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 422) {
        setErrors(ex.details);
        setFormError("Please fix the highlighted fields.");
      } else {
        setFormError((ex as Error).message);
      }
    } finally {
      setSaving(false);
    }
  }

  async function doDelete() {
    if (!saved) return;
    setDeleting(true);
    try {
      await api.deleteTask(saved.id);
      navigate("/tasks", { replace: true });
    } catch (ex) {
      setFormError((ex as Error).message);
      setConfirmDelete(false);
    } finally {
      setDeleting(false);
    }
  }

  const shellBlocked = f.runnerType === "shell" && !meta.shell_enabled;

  return (
    <form onSubmit={submit} className="flex flex-col gap-6" noValidate>
      <PageHeader
        crumb={
          <>
            <Link to="/tasks" className="hover:text-fg-2">
              Tasks
            </Link>{" "}
            / {isNew ? "new" : saved?.id}
          </>
        }
        title={
          <span className="flex items-center gap-3">
            {isNew ? "New task" : saved?.name}
            {saved && (saved.enabled ? <Badge tone="ok">Active</Badge> : <Badge tone="warn">Paused</Badge>)}
          </span>
        }
        subtitle={
          saved?.next_run_at
            ? `Next run ${formatDateTime(saved.next_run_at)} (${relativeTime(saved.next_run_at)})`
            : saved
              ? "No upcoming run: task is paused or has no schedule."
              : "Define what to run, when, and how failures are retried."
        }
        actions={
          <>
            {!isNew && (
              <Button type="button" variant="ghost" className="hover:text-danger" onClick={() => setConfirmDelete(true)}>
                Delete
              </Button>
            )}
            <Button type="submit" variant="primary" disabled={saving || shellBlocked}>
              {saving ? "Saving…" : isNew ? "Create task" : "Save changes"}
            </Button>
          </>
        }
      />

      {formError && (
        <p role="alert" className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          {formError}
        </p>
      )}

      <Section title="Basics">
        <Field label="Name" error={err("name")}>
          <input className={inputClass} value={f.name} onChange={(e) => set("name", e.target.value)} aria-invalid={!!err("name")} placeholder="health-check-api" />
        </Field>
        <Field label="Description" error={err("description")}>
          <input className={inputClass} value={f.description} onChange={(e) => set("description", e.target.value)} placeholder="What this task does and why" />
        </Field>
        <label className="flex items-center gap-2 text-[13px] text-fg-2">
          <input type="checkbox" checked={f.enabled} onChange={(e) => set("enabled", e.target.checked)} className="accent-[var(--color-brass)]" />
          Enabled
        </label>
      </Section>

      <Section title="Runner">
        <Field label="Type" error={err("runner.type")}>
          <select className={inputClass} value={f.runnerType} onChange={(e) => set("runnerType", e.target.value as RunnerType)}>
            <option value="http">HTTP request</option>
            <option value="shell">Shell command{meta.shell_enabled ? "" : " (disabled on this server)"}</option>
          </select>
        </Field>

        {f.runnerType === "http" ? (
          <>
            <div className="grid grid-cols-[120px_1fr] gap-3">
              <Field label="Method" error={err("runner.config.method")}>
                <select className={inputClass} value={f.method} onChange={(e) => set("method", e.target.value)}>
                  {["GET", "POST", "PUT", "PATCH", "DELETE"].map((m) => (
                    <option key={m}>{m}</option>
                  ))}
                </select>
              </Field>
              <Field label="URL" error={err("runner.config.url")}>
                <input className={inputClass} value={f.url} onChange={(e) => set("url", e.target.value)} aria-invalid={!!err("runner.config.url")} placeholder="https://api.example.com/healthz" />
              </Field>
            </div>
            <Field label="Headers" hint="One per line, Name: value. Secret headers are stored and shown as ********." error={err("runner.config.headers")}>
              <textarea rows={3} className={`${inputClass} font-mono`} value={f.headers} onChange={(e) => set("headers", e.target.value)} placeholder="Authorization: Bearer …" />
            </Field>
            <Field label="Query parameters" hint="One per line, name=value." error={err("runner.config.query")}>
              <textarea rows={2} className={`${inputClass} font-mono`} value={f.query} onChange={(e) => set("query", e.target.value)} />
            </Field>
            {f.method !== "GET" && f.method !== "DELETE" && (
              <Field label="Body" error={err("runner.config.body")}>
                <textarea rows={4} className={`${inputClass} font-mono`} value={f.body} onChange={(e) => set("body", e.target.value)} />
              </Field>
            )}
            <Field label="Expected status codes" hint="Comma separated. Empty means any 2xx." error={err("runner.config.expected_status")}>
              <input className={`${inputClass} font-mono`} value={f.expectedStatus} onChange={(e) => set("expectedStatus", e.target.value)} placeholder="200, 204" />
            </Field>
          </>
        ) : (
          <>
            {!meta.shell_enabled && (
              <p className="rounded-md border border-warn/40 bg-warn/10 px-3 py-2 text-[13px] text-warn">
                Shell tasks are disabled on this server. An operator must set <code className="font-mono">CLOCKWORK_ENABLE_SHELL=true</code>. Shell tasks run
                arbitrary commands as the Clockwork process user.
              </p>
            )}
            <Field label="Command" hint="Run with sh -c." error={err("runner.config.command")}>
              <textarea rows={4} className={`${inputClass} font-mono`} value={f.command} onChange={(e) => set("command", e.target.value)} placeholder="find /data/tmp -mtime +7 -delete" />
            </Field>
            <Field label="Working directory" error={err("runner.config.working_dir")}>
              <input className={`${inputClass} font-mono`} value={f.workingDir} onChange={(e) => set("workingDir", e.target.value)} />
            </Field>
            <Field label="Environment" hint="One per line, NAME=value. Secret-looking names are masked." error={err("runner.config.env")}>
              <textarea rows={3} className={`${inputClass} font-mono`} value={f.env} onChange={(e) => set("env", e.target.value)} />
            </Field>
          </>
        )}
        {err("runner.config") && <p className="text-xs text-danger">{err("runner.config")}</p>}
      </Section>

      <Section title="Schedule">
        <label className="flex items-center gap-2 text-[13px] text-fg-2">
          <input type="checkbox" checked={f.scheduled} onChange={(e) => set("scheduled", e.target.checked)} className="accent-[var(--color-brass)]" />
          Run on a schedule (otherwise manual and API triggers only)
        </label>
        {f.scheduled && (
          <>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Cron expression" hint="minute hour day-of-month month day-of-week, or @hourly / @daily" error={err("schedule.cron")}>
                <input className={`${inputClass} font-mono`} value={f.cron} onChange={(e) => set("cron", e.target.value)} aria-invalid={!!err("schedule.cron")} />
              </Field>
              <Field label="Timezone" hint="IANA name, e.g. Asia/Jakarta" error={err("schedule.timezone")}>
                <input className={`${inputClass} font-mono`} value={f.timezone} onChange={(e) => set("timezone", e.target.value)} aria-invalid={!!err("schedule.timezone")} />
              </Field>
              <Field label="If runs were missed while Clockwork was down" error={err("schedule.misfire_policy")}>
                <select className={inputClass} value={f.misfire} onChange={(e) => set("misfire", e.target.value as FormState["misfire"])}>
                  <option value="skip">Skip them</option>
                  <option value="run_once">Run once to catch up</option>
                </select>
              </Field>
              <Field label="If the previous run is still going" error={err("schedule.concurrency_policy")}>
                <select className={inputClass} value={f.concurrency} onChange={(e) => set("concurrency", e.target.value as FormState["concurrency"])}>
                  <option value="forbid">Skip this run</option>
                  <option value="allow">Run in parallel</option>
                </select>
              </Field>
            </div>
          </>
        )}
      </Section>

      <Section title="Timeout & retries">
        <div className="grid gap-3 sm:grid-cols-4">
          <Field label="Timeout (s)" error={err("timeout_seconds")}>
            <input inputMode="numeric" className={inputClass} value={f.timeout} onChange={(e) => set("timeout", e.target.value)} />
          </Field>
          <Field label="Max attempts" error={err("retry.max_attempts")}>
            <input inputMode="numeric" className={inputClass} value={f.maxAttempts} onChange={(e) => set("maxAttempts", e.target.value)} />
          </Field>
          <Field label="First backoff (s)" error={err("retry.backoff_initial_seconds")}>
            <input inputMode="numeric" className={inputClass} value={f.backoffInitial} onChange={(e) => set("backoffInitial", e.target.value)} />
          </Field>
          <Field label="Max backoff (s)" error={err("retry.backoff_max_seconds")}>
            <input inputMode="numeric" className={inputClass} value={f.backoffMax} onChange={(e) => set("backoffMax", e.target.value)} />
          </Field>
        </div>
      </Section>

      <ConfirmDialog
        open={confirmDelete}
        title={`Delete "${saved?.name}"?`}
        body="The task and its execution history will be permanently removed. This cannot be undone."
        confirmLabel="Delete task"
        busy={deleting}
        onConfirm={doDelete}
        onCancel={() => setConfirmDelete(false)}
      />
    </form>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card title={title}>
      <div className="flex flex-col gap-4 border-t border-line px-4 py-4">{children}</div>
    </Card>
  );
}
