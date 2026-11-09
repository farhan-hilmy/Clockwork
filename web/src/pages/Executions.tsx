import { Fragment, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api, executionStatuses, type Execution, type ExecutionStatus } from "../api";
import { formatDateTime, relativeTime } from "../format";
import { Badge, Button, Card, EmptyState, ErrorState, PageHeader, Spinner, inputClass } from "../ui";
import { ExecutionDetail } from "./ExecutionDetail";

const statusTone: Record<ExecutionStatus, "ok" | "danger" | "info" | "warn" | "neutral"> = {
  queued: "info",
  running: "info",
  succeeded: "ok",
  failed: "danger",
  retrying: "warn",
  interrupted: "warn",
  canceled: "neutral",
  skipped: "neutral",
};

/** How often the list refreshes itself while the page is open. */
const REFRESH_MS = 10_000;

export function Executions() {
  const [params, setParams] = useSearchParams();
  const taskId = params.get("task_id") ?? "";
  const status = (params.get("status") ?? "") as ExecutionStatus | "";
  const [items, setItems] = useState<Execution[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [state, setState] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState<Error | null>(null);
  const [nonce, setNonce] = useState(0);
  const [moreError, setMoreError] = useState("");
  const [open, setOpen] = useState<string | null>(null);
  // Once the user loads more pages, auto-refresh stops so it cannot drop those rows.
  const extended = useRef(false);

  useEffect(() => {
    setItems([]);
    setState("loading");
  }, [taskId, status]);

  // Loads the first page; refreshes it periodically so new scheduled runs appear.
  useEffect(() => {
    let cancelled = false;
    extended.current = false;
    const load = () =>
      api.listExecutions({ task_id: taskId, status, limit: 50 }).then(
        (p) => {
          if (cancelled) return;
          setItems(p.executions);
          setCursor(p.next_cursor);
          setState("ready");
        },
        (e: Error) => {
          if (cancelled) return;
          setError(e);
          setState("error");
        },
      );
    load();
    const timer = setInterval(() => !extended.current && load(), REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [taskId, status, nonce]);

  async function loadMore() {
    if (!cursor) return;
    setMoreError("");
    try {
      const p = await api.listExecutions({ task_id: taskId, status, cursor, limit: 50 });
      extended.current = true;
      setItems((xs) => [...xs, ...p.executions]);
      setCursor(p.next_cursor);
    } catch (e) {
      setMoreError((e as Error).message);
    }
  }

  function setFilter(key: "task_id" | "status", value: string) {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next);
  }

  const taskName = taskId && items[0]?.task_id === taskId ? items[0].task_name : taskId;
  const filtering = taskId !== "" || status !== "";

  return (
    <>
      <PageHeader
        title="Executions"
        subtitle={
          taskId ? (
            <>
              Runs of <Link to={`/tasks/${taskId}`} className="font-mono hover:underline">{taskName}</Link>, newest first.
            </>
          ) : (
            "Every attempt of every task, newest first."
          )
        }
        actions={<Button onClick={() => setNonce((n) => n + 1)}>Refresh</Button>}
      />

      <div className="flex flex-wrap items-center gap-2">
        <select
          aria-label="Filter by status"
          className={`${inputClass} max-w-[200px] bg-surface`}
          value={status}
          onChange={(e) => setFilter("status", e.target.value)}
        >
          <option value="">All statuses</option>
          {executionStatuses.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
        {taskId && (
          <Button variant="ghost" onClick={() => setFilter("task_id", "")}>
            Show all tasks ×
          </Button>
        )}
      </div>

      {moreError && (
        <p role="alert" className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          {moreError}
        </p>
      )}

      <Card>
        {state === "loading" && <Spinner />}
        {state === "error" && error && <ErrorState error={error} onRetry={() => setNonce((n) => n + 1)} />}
        {state === "ready" && items.length === 0 && (
          <EmptyState
            title={filtering ? "No executions match" : "No executions yet"}
            body={
              filtering
                ? "Try a different filter."
                : "Executions appear when a scheduled task falls due, or when you use Run now on a task."
            }
          />
        )}
        {state !== "loading" && items.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[760px] text-left text-[13px]">
              <thead className="bg-raised font-mono text-[10px] uppercase tracking-wide text-fg-3">
                <tr>
                  <th className="px-4 py-2.5 font-medium">Task</th>
                  <th className="px-4 py-2.5 font-medium">Status</th>
                  <th className="px-4 py-2.5 font-medium">Trigger</th>
                  <th className="px-4 py-2.5 font-medium">Scheduled for</th>
                  <th className="px-4 py-2.5 font-medium">Queued</th>
                  <th className="px-4 py-2.5 font-medium">Duration</th>
                </tr>
              </thead>
              <tbody>
                {items.map((e) => (
                  <Fragment key={e.id}>
                  <tr
                    className={`cursor-pointer border-t border-line align-middle hover:bg-raised/40 ${open === e.id ? "bg-raised/40" : ""}`}
                    onClick={() => setOpen((o) => (o === e.id ? null : e.id))}
                    aria-expanded={open === e.id}
                  >
                    <td className="max-w-[300px] px-4 py-2.5">
                      <Link to={`/tasks/${e.task_id}`} onClick={(ev) => ev.stopPropagation()} className="block truncate font-medium hover:underline">
                        {e.task_name}
                      </Link>
                      <span className="block truncate font-mono text-[11px] text-fg-3">{e.id}</span>
                    </td>
                    <td className="px-4 py-2.5">
                      <Badge tone={statusTone[e.status]}>{e.status}</Badge>
                      {e.status === "queued" && e.attempt > 1 && (
                        <span className="block font-mono text-[11px] text-fg-3">retry {relativeTime(e.run_after)}</span>
                      )}
                    </td>
                    <td className="px-4 py-2.5 font-mono text-xs text-fg-2">
                      {e.trigger}
                      {e.attempt > 1 && <span className="text-fg-3"> · attempt {e.attempt}</span>}
                    </td>
                    <td className="px-4 py-2.5 font-mono text-xs text-fg-2">{e.scheduled_for ? formatDateTime(e.scheduled_for) : "—"}</td>
                    <td className="px-4 py-2.5 font-mono text-xs text-fg-2" title={e.created_at}>
                      {relativeTime(e.created_at)}
                    </td>
                    <td className="px-4 py-2.5 font-mono text-xs text-fg-2">{e.duration_ms == null ? "—" : `${e.duration_ms} ms`}</td>
                  </tr>
                  {open === e.id && (
                    <tr className="border-t border-line bg-raised/20">
                      <td colSpan={6}>
                        <ExecutionDetail e={e} />
                      </td>
                    </tr>
                  )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {state === "ready" && cursor && (
          <div className="flex justify-center border-t border-line px-4 py-3">
            <Button onClick={loadMore}>Load more</Button>
          </div>
        )}
      </Card>
    </>
  );
}
