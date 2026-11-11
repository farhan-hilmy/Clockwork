import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, DEFAULT_APP_ID, type ApiKey, type Application, type NewApiKey } from "../api";
import { formatDateTime, relativeTime } from "../format";
import { useAsync } from "../hooks";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorState, Field, PageHeader, Spinner, inputClass } from "../ui";

/** Applications that use Clockwork through the API, and their API keys. */
export function Integrations() {
  const apps = useAsync(() => api.applications(), []);
  const [creating, setCreating] = useState(false);

  return (
    <>
      <PageHeader
        title="Integrations"
        subtitle="Applications that use Clockwork through the /api/v1 API. Each has its own API keys and only sees its own tasks and executions."
        actions={
          !creating && (
            <Button variant="primary" onClick={() => setCreating(true)}>
              + New application
            </Button>
          )
        }
      />
      {creating && (
        <NewApplication
          onDone={() => {
            setCreating(false);
            apps.reload();
          }}
          onCancel={() => setCreating(false)}
        />
      )}
      {apps.status === "loading" && <Spinner />}
      {apps.status === "error" && <ErrorState error={apps.error} onRetry={apps.reload} />}
      {apps.status === "ready" &&
        (apps.data.length <= 1 && !creating ? (
          <Card>
            <EmptyState
              title="No applications yet"
              body="Register an application to give another service its own API key. It can then create and run its own tasks, isolated from everything else."
              action={
                <Button variant="primary" onClick={() => setCreating(true)}>
                  + New application
                </Button>
              }
            />
          </Card>
        ) : null)}
      {apps.status === "ready" && apps.data.map((a) => <ApplicationCard key={a.id} app={a} onChange={apps.reload} />)}
      <p className="text-xs text-fg-3">
        The API is described at{" "}
        <a href="/api/v1/openapi.json" className="font-mono underline" target="_blank" rel="noreferrer">
          /api/v1/openapi.json
        </a>
        . See <span className="font-mono">examples/integration</span> in the repository for a client that follows the recommended patterns.
      </p>
    </>
  );
}

function NewApplication({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [formError, setFormError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setErrors({});
    setFormError("");
    try {
      await api.createApplication({ name: name.trim(), description: description.trim() });
      onDone();
    } catch (ex) {
      if (ex instanceof ApiError && ex.status === 422) setErrors(ex.details);
      else setFormError((ex as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card title="New application">
      <form onSubmit={submit} className="flex flex-col gap-4 border-t border-line px-4 py-4" noValidate>
        <div className="grid gap-4 md:grid-cols-2">
          <Field label="Name" hint="Lowercase letters, digits, '.', '_' and '-'." error={errors.name}>
            <input className={inputClass} value={name} onChange={(e) => setName(e.target.value)} aria-invalid={!!errors.name} placeholder="billing-service" autoFocus />
          </Field>
          <Field label="Description" error={errors.description}>
            <input className={inputClass} value={description} onChange={(e) => setDescription(e.target.value)} placeholder="What this application uses Clockwork for" />
          </Field>
        </div>
        {formError && (
          <p role="alert" className="text-sm text-danger">
            {formError}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={busy}>
            {busy ? "Creating…" : "Create application"}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function ApplicationCard({ app, onChange }: { app: Application; onChange: () => void }) {
  const isDefault = app.id === DEFAULT_APP_ID;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);

  async function act(fn: () => Promise<unknown>) {
    setBusy(true);
    setError("");
    try {
      await fn();
      onChange();
    } catch (ex) {
      setError((ex as Error).message);
    } finally {
      setBusy(false);
      setConfirmDelete(false);
    }
  }

  return (
    <Card
      title={
        <span className="flex flex-wrap items-center gap-2.5">
          {app.name}
          {isDefault ? <Badge tone="neutral">default</Badge> : app.enabled ? <Badge tone="ok">enabled</Badge> : <Badge tone="warn">disabled</Badge>}
          <span className="font-mono text-[11px] font-normal text-fg-3">{app.id}</span>
        </span>
      }
      actions={
        !isDefault && (
          <div className="flex gap-1">
            <Button variant="ghost" disabled={busy} onClick={() => act(() => api.updateApplication(app.id, { enabled: !app.enabled }))}>
              {app.enabled ? "Disable" : "Enable"}
            </Button>
            <Button
              variant="ghost"
              className="hover:text-danger"
              disabled={busy || app.task_count > 0}
              title={app.task_count > 0 ? "Delete or move its tasks first" : undefined}
              onClick={() => setConfirmDelete(true)}
            >
              Delete
            </Button>
          </div>
        )
      }
    >
      <div className="flex flex-col gap-3 border-t border-line px-4 py-3 text-[13px]">
        <div className="flex flex-wrap items-center gap-x-6 gap-y-1 text-fg-2">
          {app.description && <span>{app.description}</span>}
          <Link to={`/tasks?application_id=${encodeURIComponent(app.id)}`} className="hover:underline">
            {app.task_count} {app.task_count === 1 ? "task" : "tasks"} →
          </Link>
          {!isDefault && !app.enabled && <span className="text-warn">Its API keys are refused and its tasks are not scheduled.</span>}
        </div>
        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}
        {isDefault ? (
          <p className="text-xs text-fg-3">Tasks created in this UI belong here. It is managed with the admin token and has no API keys.</p>
        ) : (
          <Keys appId={app.id} />
        )}
      </div>
      <ConfirmDialog
        open={confirmDelete}
        title={`Delete "${app.name}"?`}
        body="The application and all of its API keys are removed. Integrations using those keys stop working immediately."
        confirmLabel="Delete application"
        busy={busy}
        onConfirm={() => act(() => api.deleteApplication(app.id))}
        onCancel={() => setConfirmDelete(false)}
      />
    </Card>
  );
}

function Keys({ appId }: { appId: string }) {
  const keys = useAsync(() => api.listKeys(appId), [appId]);
  const [name, setName] = useState("");
  const [created, setCreated] = useState<NewApiKey | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [toRevoke, setToRevoke] = useState<ApiKey | null>(null);

  async function create(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      setCreated(await api.createKey(appId, name.trim()));
      setName("");
      keys.reload();
    } catch (ex) {
      setError(ex instanceof ApiError && ex.details.name ? `Name ${ex.details.name}` : (ex as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function revoke() {
    if (!toRevoke) return;
    setBusy(true);
    try {
      await api.revokeKey(appId, toRevoke.id);
      keys.reload();
    } catch (ex) {
      setError((ex as Error).message);
    } finally {
      setBusy(false);
      setToRevoke(null);
    }
  }

  return (
    <div className="flex flex-col gap-2">
      <span className="font-mono text-[10px] uppercase tracking-wide text-fg-3">API keys</span>
      {created && <NewKeyNotice k={created} onDismiss={() => setCreated(null)} />}
      {keys.status === "loading" && <Spinner label="Loading keys…" />}
      {keys.status === "error" && <ErrorState error={keys.error} onRetry={keys.reload} />}
      {keys.status === "ready" && keys.data.length === 0 && <p className="text-xs text-fg-3">No keys yet. Create one to let the application authenticate.</p>}
      {keys.status === "ready" && keys.data.length > 0 && (
        <ul className="rounded-md border border-line">
          {keys.data.map((k) => (
            <li key={k.id} className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-line px-3 py-2 first:border-t-0">
              <span className="min-w-[120px] font-medium">{k.name}</span>
              <span className="font-mono text-xs text-fg-2">{k.prefix}…</span>
              <span className="text-xs text-fg-3" title={k.created_at}>
                created {formatDateTime(k.created_at)}
              </span>
              <span className="text-xs text-fg-3">{k.last_used_at ? `used ${relativeTime(k.last_used_at)}` : "never used"}</span>
              <span className="flex-1" />
              {k.revoked_at ? (
                <Badge tone="neutral">revoked</Badge>
              ) : (
                <Button variant="ghost" className="hover:text-danger" disabled={busy} onClick={() => setToRevoke(k)}>
                  Revoke
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      <form onSubmit={create} className="flex flex-wrap items-center gap-2">
        <input
          className={`${inputClass} max-w-[240px]`}
          aria-label="New key name"
          placeholder="Key name, e.g. production"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <Button type="submit" disabled={busy || name.trim() === ""}>
          Create key
        </Button>
        {error && (
          <span role="alert" className="text-xs text-danger">
            {error}
          </span>
        )}
      </form>
      <ConfirmDialog
        open={!!toRevoke}
        title={`Revoke "${toRevoke?.name}"?`}
        body="Requests using this key are refused immediately. This cannot be undone. To rotate without downtime, create and deploy a new key first."
        confirmLabel="Revoke key"
        busy={busy}
        onConfirm={revoke}
        onCancel={() => setToRevoke(null)}
      />
    </div>
  );
}

/** Shows a freshly created key. It is the only time the key is visible. */
function NewKeyNotice({ k, onDismiss }: { k: NewApiKey; onDismiss: () => void }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(k.key);
      setCopied(true);
    } catch {
      /* clipboard blocked: the key is selectable */
    }
  }
  return (
    <div role="status" className="flex flex-col gap-2 rounded-md border border-ok/40 bg-ok/10 px-3 py-3">
      <p className="text-sm">
        Key <span className="font-medium">{k.name}</span> created. Copy it now: it will not be shown again.
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <code className="min-w-0 flex-1 select-all break-all rounded border border-line bg-canvas px-2 py-1.5 font-mono text-xs">{k.key}</code>
        <Button onClick={copy}>{copied ? "Copied" : "Copy"}</Button>
        <Button variant="ghost" onClick={onDismiss}>
          Done
        </Button>
      </div>
    </div>
  );
}
