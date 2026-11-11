import { useState, type FormEvent } from "react";
import { api, ApiError, webhookEventTypes, type WebhookDelivery, type WebhookEndpoint, type WebhookEventType } from "../api";
import { formatDateTime, relativeTime } from "../format";
import { useAsync } from "../hooks";
import { Badge, Button, ConfirmDialog, ErrorState, Field, Spinner, inputClass } from "../ui";

/** Shows a secret once, with a copy button. */
export function OneTimeSecret({ label, value, onDismiss }: { label: string; value: string; onDismiss: () => void }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
    } catch {
      /* clipboard blocked: the value is selectable */
    }
  }
  return (
    <div role="status" className="flex flex-col gap-2 rounded-md border border-ok/40 bg-ok/10 px-3 py-3">
      <p className="text-sm">{label} Copy it now: it will not be shown again.</p>
      <div className="flex flex-wrap items-center gap-2">
        <code className="min-w-0 flex-1 select-all break-all rounded border border-line bg-canvas px-2 py-1.5 font-mono text-xs">{value}</code>
        <Button onClick={copy}>{copied ? "Copied" : "Copy"}</Button>
        <Button variant="ghost" onClick={onDismiss}>
          Done
        </Button>
      </div>
    </div>
  );
}

/** An application's webhook endpoints. */
export function Webhooks({ appId }: { appId: string }) {
  const hooks = useAsync(() => api.listWebhooks(appId), [appId]);
  const [creating, setCreating] = useState(false);
  const [secret, setSecret] = useState<{ label: string; value: string } | null>(null);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <span className="flex-1 font-mono text-[10px] uppercase tracking-wide text-fg-3">Webhooks</span>
        {!creating && (
          <Button variant="ghost" onClick={() => setCreating(true)}>
            + Add endpoint
          </Button>
        )}
      </div>
      {secret && <OneTimeSecret label={secret.label} value={secret.value} onDismiss={() => setSecret(null)} />}
      {creating && (
        <NewEndpoint
          appId={appId}
          onCreated={(e) => {
            setCreating(false);
            setSecret({ label: `Signing secret for ${e.url}.`, value: e.secret ?? "" });
            hooks.reload();
          }}
          onCancel={() => setCreating(false)}
        />
      )}
      {hooks.status === "loading" && <Spinner label="Loading webhooks…" />}
      {hooks.status === "error" && <ErrorState error={hooks.error} onRetry={hooks.reload} />}
      {hooks.status === "ready" && hooks.data.length === 0 && !creating && (
        <p className="text-xs text-fg-3">No endpoints. Add one to be notified when this application's runs start, succeed or fail.</p>
      )}
      {hooks.status === "ready" &&
        hooks.data.map((e) => (
          <EndpointRow key={e.id} endpoint={e} onChange={hooks.reload} onSecret={(v) => setSecret({ label: `New signing secret for ${e.url}.`, value: v })} />
        ))}
    </div>
  );
}

function NewEndpoint({ appId, onCreated, onCancel }: { appId: string; onCreated: (e: WebhookEndpoint) => void; onCancel: () => void }) {
  const [url, setUrl] = useState("");
  const [all, setAll] = useState(true);
  const [events, setEvents] = useState<WebhookEventType[]>(["execution.failed"]);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);

  async function submit(ev: FormEvent) {
    ev.preventDefault();
    setBusy(true);
    setErrors({});
    try {
      onCreated(await api.createWebhook({ application_id: appId, url: url.trim(), description: "", events: all ? [] : events }));
    } catch (ex) {
      setErrors(ex instanceof ApiError && ex.status === 422 ? ex.details : { form: (ex as Error).message });
    } finally {
      setBusy(false);
    }
  }

  const toggle = (t: WebhookEventType) => setEvents((xs) => (xs.includes(t) ? xs.filter((x) => x !== t) : [...xs, t]));
  return (
    <form onSubmit={submit} className="flex flex-col gap-3 rounded-md border border-line px-3 py-3" noValidate>
      <Field label="Endpoint URL" hint="Receives a signed POST for each event." error={errors.url}>
        <input className={inputClass} value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://example.com/clockwork/webhooks" autoFocus aria-invalid={!!errors.url} />
      </Field>
      <fieldset className="flex flex-col gap-1.5 text-[13px]">
        <legend className="mb-1 text-xs font-medium text-fg-2">Events</legend>
        <label className="flex items-center gap-2">
          <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} className="accent-[var(--color-brass)]" />
          All events
        </label>
        {!all && (
          <div className="grid grid-cols-1 gap-1 pl-5 sm:grid-cols-2">
            {webhookEventTypes.map((t) => (
              <label key={t} className="flex items-center gap-2 font-mono text-xs">
                <input type="checkbox" checked={events.includes(t)} onChange={() => toggle(t)} className="accent-[var(--color-brass)]" />
                {t}
              </label>
            ))}
          </div>
        )}
        {errors.events && <span className="text-xs text-danger">{errors.events}</span>}
      </fieldset>
      {errors.form && (
        <p role="alert" className="text-sm text-danger">
          {errors.form}
        </p>
      )}
      <div className="flex justify-end gap-2">
        <Button type="button" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={busy || url.trim() === "" || (!all && events.length === 0)}>
          {busy ? "Adding…" : "Add endpoint"}
        </Button>
      </div>
    </form>
  );
}

function EndpointRow({ endpoint: e, onChange, onSecret }: { endpoint: WebhookEndpoint; onChange: () => void; onSecret: (s: string) => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [showLog, setShowLog] = useState(false);
  const [logNonce, setLogNonce] = useState(0);
  const [confirm, setConfirm] = useState<"delete" | "rotate" | null>(null);

  async function act(fn: () => Promise<unknown>) {
    setBusy(true);
    setError("");
    try {
      await fn();
    } catch (ex) {
      setError((ex as Error).message);
    } finally {
      setBusy(false);
      setConfirm(null);
    }
  }

  return (
    <div className="rounded-md border border-line">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2">
        <span className="min-w-0 flex-1 break-all font-mono text-xs">{e.url}</span>
        {e.enabled ? <Badge tone="ok">enabled</Badge> : <Badge tone="warn">paused</Badge>}
        <span className="text-xs text-fg-3">{e.events.length === 0 ? "all events" : e.events.join(", ")}</span>
      </div>
      <div className="flex flex-wrap gap-1 border-t border-line px-2 py-1">
        <Button
          variant="ghost"
          disabled={busy}
          onClick={() =>
            act(async () => {
              await api.testWebhook(e.id);
              setShowLog(true);
              setLogNonce((n) => n + 1);
            })
          }
        >
          Send test
        </Button>
        <Button variant="ghost" onClick={() => setShowLog((s) => !s)}>
          {showLog ? "Hide deliveries" : "Deliveries"}
        </Button>
        <Button variant="ghost" disabled={busy} onClick={() => act(() => api.updateWebhook(e.id, { enabled: !e.enabled }).then(onChange))}>
          {e.enabled ? "Pause" : "Resume"}
        </Button>
        <Button variant="ghost" disabled={busy} onClick={() => setConfirm("rotate")}>
          Rotate secret
        </Button>
        <Button variant="ghost" className="hover:text-danger" disabled={busy} onClick={() => setConfirm("delete")}>
          Delete
        </Button>
        {error && (
          <span role="alert" className="self-center text-xs text-danger">
            {error}
          </span>
        )}
      </div>
      {showLog && <Deliveries endpointId={e.id} nonce={logNonce} />}
      <ConfirmDialog
        open={confirm === "delete"}
        title="Delete this endpoint?"
        body="It stops receiving events immediately, and its delivery log is removed."
        confirmLabel="Delete endpoint"
        busy={busy}
        onConfirm={() => act(() => api.deleteWebhook(e.id).then(onChange))}
        onCancel={() => setConfirm(null)}
      />
      <ConfirmDialog
        open={confirm === "rotate"}
        title="Rotate the signing secret?"
        body="The old secret stops working immediately, including for retries of earlier events. Update the receiver right away."
        confirmLabel="Rotate secret"
        busy={busy}
        onConfirm={() => act(() => api.rotateWebhookSecret(e.id).then((r) => onSecret(r.secret ?? "")))}
        onCancel={() => setConfirm(null)}
      />
    </div>
  );
}

const deliveryTone = { pending: "info", delivered: "ok", failed: "danger" } as const;

function Deliveries({ endpointId, nonce }: { endpointId: string; nonce: number }) {
  const log = useAsync(() => api.listDeliveries(endpointId), [endpointId, nonce]);
  const [error, setError] = useState("");

  async function redeliver(d: WebhookDelivery) {
    setError("");
    try {
      await api.redeliver(endpointId, d.id);
      log.reload();
    } catch (ex) {
      setError((ex as Error).message);
    }
  }

  return (
    <div className="border-t border-line px-3 py-2">
      {log.status === "loading" && <Spinner label="Loading deliveries…" />}
      {log.status === "error" && <ErrorState error={log.error} onRetry={log.reload} />}
      {log.status === "ready" && log.data.length === 0 && <p className="py-2 text-xs text-fg-3">No deliveries yet.</p>}
      {log.status === "ready" && log.data.length > 0 && (
        <ul className="flex flex-col">
          {log.data.map((d) => (
            <li key={d.id} className="flex flex-wrap items-center gap-x-3 gap-y-0.5 border-t border-line py-1.5 text-xs first:border-t-0">
              <Badge tone={deliveryTone[d.status]}>{d.status}</Badge>
              <span className="font-mono">{d.event_type}</span>
              <span className="text-fg-3" title={d.created_at}>
                {relativeTime(d.created_at)}
              </span>
              <span className="text-fg-3">
                {d.attempts} {d.attempts === 1 ? "attempt" : "attempts"}
                {d.last_status_code != null && ` · HTTP ${d.last_status_code}`}
              </span>
              {d.last_error && <span className="text-danger">{d.last_error}</span>}
              {d.status === "pending" && d.next_attempt_at && d.attempts > 0 && (
                <span className="text-fg-3">next try {formatDateTime(d.next_attempt_at)}</span>
              )}
              <span className="flex-1" />
              {d.status !== "pending" && (
                <Button variant="ghost" onClick={() => redeliver(d)}>
                  Redeliver
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      {error && (
        <p role="alert" className="text-xs text-danger">
          {error}
        </p>
      )}
      <div className="flex justify-end">
        <Button variant="ghost" onClick={log.reload}>
          Refresh
        </Button>
      </div>
    </div>
  );
}
