import type { ReactNode } from "react";
import type { Execution, ExecutionError, HttpOutput, ShellOutput } from "../api";
import { formatDateTime } from "../format";

function isHttp(o: unknown): o is HttpOutput {
  return typeof o === "object" && o !== null && "status_code" in o;
}

function isShell(o: unknown): o is ShellOutput {
  return typeof o === "object" && o !== null && "exit_code" in o;
}

function Pre({ label, text, truncated }: { label: string; text: string; truncated?: boolean }) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <span className="font-mono text-[10px] uppercase tracking-wide text-fg-3">
        {label}
        {truncated && " · truncated"}
      </span>
      <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded-md border border-line bg-canvas px-3 py-2 font-mono text-xs text-fg-2">
        {text === "" ? <span className="text-fg-3">(empty)</span> : text}
      </pre>
    </div>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-fg-3">{label}</dt>
      <dd className="font-mono">{children}</dd>
    </>
  );
}

/** Expanded view of one execution: timing, error and runner output. */
export function ExecutionDetail({ e }: { e: Execution }) {
  const err = e.error as ExecutionError | null;
  const out = e.output;
  return (
    <div className="flex flex-col gap-4 px-4 py-4 text-[13px]">
      <dl className="grid grid-cols-[120px_1fr] gap-x-4 gap-y-1.5 sm:grid-cols-[120px_1fr_120px_1fr]">
        <Fact label="Execution">{e.id}</Fact>
        <Fact label="Attempt">{e.attempt}</Fact>
        <Fact label="Queued">{formatDateTime(e.created_at)}</Fact>
        <Fact label="Not before">{formatDateTime(e.run_after)}</Fact>
        <Fact label="Started">{formatDateTime(e.started_at)}</Fact>
        <Fact label="Finished">{formatDateTime(e.finished_at)}</Fact>
      </dl>

      {err && (
        <div role="note" className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2">
          <p className="font-medium text-danger">
            {err.kind}: {err.message}
          </p>
          <p className="mt-0.5 text-xs text-fg-2">
            {err.retry_at
              ? `Retried as attempt ${e.attempt + 1}, not before ${formatDateTime(err.retry_at)}.`
              : err.retryable
                ? "Retryable, but this was the last attempt allowed by the task's retry policy."
                : "Not retryable."}
          </p>
        </div>
      )}

      {isHttp(out) && (
        <>
          <dl className="grid grid-cols-[120px_1fr] gap-x-4 gap-y-1.5">
            <Fact label="Status code">{out.status_code}</Fact>
            {out.content_type && <Fact label="Content type">{out.content_type}</Fact>}
            <Fact label="Body size">{out.body_bytes} bytes</Fact>
          </dl>
          <Pre label="Response body" text={out.body} truncated={out.body_truncated} />
        </>
      )}
      {isShell(out) && (
        <>
          <dl className="grid grid-cols-[120px_1fr] gap-x-4 gap-y-1.5">
            <Fact label="Exit code">{out.exit_code}</Fact>
          </dl>
          <div className="grid gap-3 lg:grid-cols-2">
            <Pre label="stdout" text={out.stdout} truncated={out.stdout_truncated} />
            <Pre label="stderr" text={out.stderr} truncated={out.stderr_truncated} />
          </div>
        </>
      )}
      {out != null && !isHttp(out) && !isShell(out) && <Pre label="Output" text={JSON.stringify(out, null, 2)} />}
      {out == null && !err && <p className="text-fg-3">No output recorded{e.status === "queued" || e.status === "running" ? " yet" : ""}.</p>}
    </div>
  );
}
