import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api, type RunnerType, type Task } from "../api";
import { formatDateTime, runnerSummary } from "../format";
import { Badge, Button, Card, ConfirmDialog, EmptyState, ErrorState, PageHeader, Spinner, inputClass } from "../ui";

type Filter = "all" | "enabled" | "disabled";

export function Tasks() {
  const [params, setParams] = useSearchParams();
  const appId = params.get("application_id") ?? "";
  const [query, setQuery] = useState("");
  const [debounced, setDebounced] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [runner, setRunner] = useState<RunnerType | "">("");
  const [tasks, setTasks] = useState<Task[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState<Error | null>(null);
  const [nonce, setNonce] = useState(0);
  const [toDelete, setToDelete] = useState<Task | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [actionError, setActionError] = useState("");

  useEffect(() => {
    const t = setTimeout(() => setDebounced(query.trim()), 250);
    return () => clearTimeout(t);
  }, [query]);

  const filterParams = {
    q: debounced,
    application_id: appId,
    enabled: filter === "all" ? undefined : filter === "enabled",
    runner,
    limit: 25,
  };

  useEffect(() => {
    let cancelled = false;
    setStatus("loading");
    api.listTasks(filterParams).then(
      (p) => {
        if (cancelled) return;
        setTasks(p.tasks);
        setCursor(p.next_cursor);
        setStatus("ready");
      },
      (e: Error) => {
        if (cancelled) return;
        setError(e);
        setStatus("error");
      },
    );
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debounced, filter, runner, appId, nonce]);

  async function loadMore() {
    if (!cursor) return;
    try {
      const p = await api.listTasks({ ...filterParams, cursor });
      setTasks((ts) => [...ts, ...p.tasks]);
      setCursor(p.next_cursor);
    } catch (e) {
      setActionError((e as Error).message);
    }
  }

  async function toggle(t: Task) {
    setBusyId(t.id);
    setActionError("");
    try {
      const updated = await api.setEnabled(t.id, !t.enabled);
      setTasks((ts) => ts.map((x) => (x.id === t.id ? updated : x)));
    } catch (e) {
      setActionError((e as Error).message);
    } finally {
      setBusyId(null);
    }
  }

  async function confirmDelete() {
    if (!toDelete) return;
    setBusyId(toDelete.id);
    try {
      await api.deleteTask(toDelete.id);
      setTasks((ts) => ts.filter((x) => x.id !== toDelete.id));
      setToDelete(null);
    } catch (e) {
      setActionError((e as Error).message);
      setToDelete(null);
    } finally {
      setBusyId(null);
    }
  }

  const filtering = debounced !== "" || filter !== "all" || runner !== "" || appId !== "";

  return (
    <>
      <PageHeader
        title="Tasks"
        subtitle={appId ? <>Showing tasks owned by <span className="font-mono">{appId}</span></> : "Every automation Clockwork knows about."}
        actions={
          <Link to="/tasks/new">
            <Button variant="primary">+ New task</Button>
          </Link>
        }
      />

      <div className="flex flex-wrap items-center gap-2">
        <input
          type="search"
          placeholder="Search tasks by name…"
          aria-label="Search tasks"
          className={`${inputClass} max-w-xs bg-surface`}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <div className="flex rounded-md border border-line bg-surface p-0.5" role="group" aria-label="Filter by state">
          {(["all", "enabled", "disabled"] as Filter[]).map((f) => (
            <button
              key={f}
              onClick={() => setFilter(f)}
              aria-pressed={filter === f}
              className={`rounded px-3 py-1.5 text-[13px] capitalize ${filter === f ? "bg-raised text-fg" : "text-fg-2 hover:text-fg"}`}
            >
              {f}
            </button>
          ))}
        </div>
        <select
          aria-label="Filter by runner"
          className={`${inputClass} max-w-[160px] bg-surface`}
          value={runner}
          onChange={(e) => setRunner(e.target.value as RunnerType | "")}
        >
          <option value="">All runners</option>
          <option value="http">HTTP</option>
          <option value="shell">Shell</option>
        </select>
        {appId && (
          <Button variant="ghost" onClick={() => setParams({})}>
            Clear application filter ×
          </Button>
        )}
      </div>

      {actionError && (
        <p role="alert" className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          {actionError}
        </p>
      )}

      <Card>
        {status === "loading" && tasks.length === 0 && <Spinner />}
        {status === "error" && error && <ErrorState error={error} onRetry={() => setNonce((n) => n + 1)} />}
        {status !== "error" && !(status === "loading" && tasks.length === 0) && tasks.length === 0 && (
          <EmptyState
            title={filtering ? "No tasks match" : "No tasks yet"}
            body={filtering ? "Try a different search or filter." : "Create an HTTP or shell task and give it a cron schedule."}
            action={
              !filtering && (
                <Link to="/tasks/new">
                  <Button variant="primary">+ New task</Button>
                </Link>
              )
            }
          />
        )}
        {tasks.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[860px] text-left text-[13px]">
              <thead className="bg-raised font-mono text-[10px] uppercase tracking-wide text-fg-3">
                <tr>
                  <th className="px-4 py-2.5 font-medium">Task</th>
                  <th className="px-4 py-2.5 font-medium">Runner</th>
                  <th className="px-4 py-2.5 font-medium">Schedule</th>
                  <th className="px-4 py-2.5 font-medium">Application</th>
                  <th className="px-4 py-2.5 font-medium">Next run</th>
                  <th className="px-4 py-2.5 font-medium">State</th>
                  <th className="px-4 py-2.5" />
                </tr>
              </thead>
              <tbody>
                {tasks.map((t) => (
                  <tr key={t.id} className="border-t border-line align-middle hover:bg-raised/40">
                    <td className="max-w-[320px] px-4 py-2.5">
                      <Link to={`/tasks/${t.id}`} className="block truncate font-medium hover:underline">
                        {t.name}
                      </Link>
                      <span className="block truncate font-mono text-[11px] text-fg-3">
                        {runnerSummary(t.runner.type, t.runner.config as unknown as Record<string, unknown>)}
                      </span>
                    </td>
                    <td className="px-4 py-2.5 font-mono text-xs text-fg-2">{t.runner.type}</td>
                    <td className="px-4 py-2.5">
                      {t.schedule ? (
                        <>
                          <span className="font-mono text-xs">{t.schedule.cron}</span>
                          <span className="block font-mono text-[11px] text-fg-3">{t.schedule.timezone}</span>
                        </>
                      ) : (
                        <span className="text-xs text-fg-3">manual only</span>
                      )}
                    </td>
                    <td className="px-4 py-2.5 font-mono text-xs text-fg-2">{t.application_id.replace(/^app_/, "")}</td>
                    <td className="px-4 py-2.5 font-mono text-xs text-fg-2">{formatDateTime(t.next_run_at)}</td>
                    <td className="px-4 py-2.5">
                      {t.enabled ? <Badge tone="ok">Active</Badge> : <Badge tone={t.schedule ? "warn" : "neutral"}>{t.schedule ? "Paused" : "Disabled"}</Badge>}
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-right">
                      <Button variant="ghost" disabled={busyId === t.id} onClick={() => toggle(t)}>
                        {t.enabled ? "Pause" : "Resume"}
                      </Button>
                      <Button variant="ghost" className="hover:text-danger" disabled={busyId === t.id} onClick={() => setToDelete(t)}>
                        Delete
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {cursor && (
          <div className="flex justify-center border-t border-line px-4 py-3">
            <Button onClick={loadMore}>Load more</Button>
          </div>
        )}
      </Card>

      <ConfirmDialog
        open={!!toDelete}
        title={`Delete "${toDelete?.name}"?`}
        body="The task and its execution history will be permanently removed. This cannot be undone."
        confirmLabel="Delete task"
        busy={!!toDelete && busyId === toDelete.id}
        onConfirm={confirmDelete}
        onCancel={() => setToDelete(null)}
      />
    </>
  );
}
