import { Link } from "react-router-dom";
import { api, type Application, type Execution, type ExecutionStats, type Task } from "../api";
import { useAsync } from "../hooks";
import { formatDateTime, formatDuration, relativeTime } from "../format";
import { Button, Card, EmptyState, ErrorState, PageHeader, Spinner } from "../ui";

interface DashboardData {
  tasks: Task[];
  truncated: boolean;
  apps: Application[];
  stats: ExecutionStats;
  failures: Execution[];
}

async function load(): Promise<DashboardData> {
  const [page, apps, stats, failed] = await Promise.all([
    api.listTasks({ limit: 200 }),
    api.applications(),
    api.stats("24h"),
    api.listExecutions({ status: "failed", limit: 5 }),
  ]);
  return { tasks: page.tasks, truncated: !!page.next_cursor, apps, stats: stats.executions, failures: failed.executions };
}

function Stat({ label, value, meta }: { label: string; value: string | number; meta: string }) {
  return (
    <div className="flex flex-col gap-1.5 rounded-[10px] border border-line bg-surface px-4 py-3.5">
      <span className="text-xs font-medium text-fg-2">{label}</span>
      <span className="text-[28px] font-semibold leading-none">{value}</span>
      <span className="font-mono text-[11px] text-fg-3">{meta}</span>
    </div>
  );
}

export function Dashboard() {
  const state = useAsync(load, []);

  return (
    <>
      <PageHeader
        title="Dashboard"
        subtitle="Everything Clockwork is running, at a glance."
        actions={
          <Link to="/tasks/new">
            <Button variant="primary">+ New task</Button>
          </Link>
        }
      />
      {state.status === "loading" && <Spinner />}
      {state.status === "error" && <ErrorState error={state.error} onRetry={state.reload} />}
      {state.status === "ready" && <Body data={state.data} />}
    </>
  );
}

function Body({ data }: { data: DashboardData }) {
  const { tasks, apps, truncated, stats, failures } = data;
  const failed24h = stats.by_status.failed ?? 0;
  const enabled = tasks.filter((t) => t.enabled);
  const scheduled = enabled.filter((t) => t.schedule);
  const paused = tasks.filter((t) => !t.enabled && t.schedule);
  const upcoming = scheduled
    .filter((t) => t.next_run_at)
    .sort((a, b) => a.next_run_at!.localeCompare(b.next_run_at!))
    .slice(0, 8);
  const plus = truncated ? "+" : "";

  return (
    <>
      <div className="grid grid-cols-2 gap-4 md:grid-cols-3 xl:grid-cols-6">
        <Stat label="Runs · 24h" value={stats.total} meta={`${stats.by_status.succeeded ?? 0} succeeded · ${stats.by_status.queued ?? 0} queued`} />
        <Stat
          label="Success rate · 24h"
          value={stats.success_rate == null ? "—" : `${Math.round(stats.success_rate * 100)}%`}
          meta={stats.success_rate == null ? "no finished runs yet" : "of finished runs"}
        />
        <Link to="/executions?status=failed" className="contents">
          <Stat label="Failures · 24h" value={failed24h} meta={failed24h === 0 ? "no failed runs" : "view failed runs →"} />
        </Link>
        <Stat label="Typical duration" value={formatDuration(stats.avg_duration_ms)} meta={`p95 ${formatDuration(stats.p95_duration_ms)}`} />
        <Stat label="Tasks" value={`${tasks.length}${plus}`} meta={`${scheduled.length} scheduled · ${paused.length} paused`} />
        <Stat label="Applications" value={apps.length} meta={`${apps.filter((a) => a.task_count > 0).length} with tasks`} />
      </div>

      <div className="grid gap-4 lg:grid-cols-[1fr_360px]">
        <Card title="Upcoming runs" actions={<span className="text-xs text-fg-3">your local time</span>}>
          {upcoming.length === 0 ? (
            <EmptyState
              title="Nothing scheduled"
              body="Tasks with a cron schedule appear here with their next run time."
              action={
                <Link to="/tasks/new">
                  <Button>Create a scheduled task</Button>
                </Link>
              }
            />
          ) : (
            <ul>
              {upcoming.map((t) => (
                <li key={t.id} className="flex items-center gap-4 border-t border-line px-4 py-2.5">
                  <span className="w-28 shrink-0 font-mono text-[13px] font-medium text-brass">{formatDateTime(t.next_run_at)}</span>
                  <div className="min-w-0 flex-1">
                    <Link to={`/tasks/${t.id}`} className="block truncate text-[13px] font-medium hover:underline">
                      {t.name}
                    </Link>
                    <span className="font-mono text-[11px] text-fg-3">
                      {t.schedule!.cron} · {t.schedule!.timezone}
                    </span>
                  </div>
                  <span className="font-mono text-xs text-fg-3">{relativeTime(t.next_run_at)}</span>
                </li>
              ))}
            </ul>
          )}
        </Card>

        <div className="flex flex-col gap-4">
        <Card title="Recent failures" actions={<Link to="/executions?status=failed" className="text-xs text-fg-3 hover:underline">all →</Link>}>
          {failures.length === 0 ? (
            <p className="border-t border-line px-4 py-6 text-center text-[13px] text-fg-3">No failed runs.</p>
          ) : (
            <ul>
              {failures.map((e) => (
                <li key={e.id} className="flex items-center gap-3 border-t border-line px-4 py-2.5">
                  <div className="min-w-0 flex-1">
                    <Link to={`/executions?task_id=${encodeURIComponent(e.task_id)}`} className="block truncate text-[13px] font-medium hover:underline">
                      {e.task_name}
                    </Link>
                    <span className="block truncate font-mono text-[11px] text-danger">{failureReason(e)}</span>
                  </div>
                  <span className="shrink-0 font-mono text-xs text-fg-3" title={e.finished_at ?? e.created_at}>
                    {relativeTime(e.finished_at ?? e.created_at)}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </Card>
        <Card title="Tasks by application">
          <ul>
            {apps.map((a) => (
              <li key={a.id} className="flex items-center gap-3 border-t border-line px-4 py-3">
                <div className="min-w-0 flex-1">
                  <Link to={`/tasks?application_id=${a.id}`} className="text-[13px] font-semibold hover:underline">
                    {a.name}
                  </Link>
                  <div className="font-mono text-[11px] text-fg-3">{a.id === "app_clockwork" ? "default context" : a.id}</div>
                </div>
                <span className="text-xs text-fg-2">
                  {a.task_count} {a.task_count === 1 ? "task" : "tasks"}
                </span>
              </li>
            ))}
          </ul>
        </Card>
        </div>
      </div>
    </>
  );
}

function failureReason(e: Execution): string {
  const err = e.error as { kind?: string; message?: string } | null;
  return err?.message ? `${err.kind}: ${err.message}` : "failed";
}
