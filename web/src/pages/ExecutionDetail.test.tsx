// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { Execution } from "../api";
import { ExecutionDetail } from "./ExecutionDetail";

const base: Execution = {
  id: "exe_1",
  task_id: "tsk_1",
  task_name: "health",
  application_id: "app_clockwork",
  trigger: "scheduled",
  status: "succeeded",
  attempt: 1,
  scheduled_for: null,
  output: null,
  error: null,
  created_at: "2026-11-09T10:00:00Z",
  run_after: "2026-11-09T10:00:00Z",
  started_at: "2026-11-09T10:00:00Z",
  finished_at: "2026-11-09T10:00:01Z",
  duration_ms: 120,
};

afterEach(cleanup);

describe("ExecutionDetail", () => {
  it("shows the HTTP status code and response body", () => {
    render(
      <ExecutionDetail
        e={{ ...base, output: { status_code: 200, content_type: "application/json", body: '{"ok":true}', body_truncated: false, body_bytes: 11 } }}
      />,
    );
    expect(screen.getByText("200")).toBeTruthy();
    expect(screen.getByText('{"ok":true}')).toBeTruthy();
  });

  it("shows shell streams and a retried failure", () => {
    render(
      <ExecutionDetail
        e={{
          ...base,
          status: "retrying",
          output: { exit_code: 3, stdout: "partial", stdout_truncated: false, stderr: "", stderr_truncated: false },
          error: { kind: "exit_code", message: "command exited with status 3", retryable: true, retry_at: "2026-11-09T10:00:11Z" },
        }}
      />,
    );
    expect(screen.getByText("exit_code: command exited with status 3")).toBeTruthy();
    expect(screen.getByText(/Retried as attempt 2/)).toBeTruthy();
    expect(screen.getByText("partial")).toBeTruthy();
    expect(screen.getByText("(empty)")).toBeTruthy();
  });

  it("explains a skipped run without presenting it as a failure", () => {
    const message = "skipped: a previous run of this task is still active (concurrency_policy=forbid)";
    render(<ExecutionDetail e={{ ...base, status: "skipped", error: { kind: "concurrency", message, retryable: false } }} />);
    expect(screen.getByText(message)).toBeTruthy();
    expect(screen.queryByText("Not retryable.")).toBeNull();
  });

  it("says when nothing has been recorded yet", () => {
    render(<ExecutionDetail e={{ ...base, status: "queued", started_at: null, finished_at: null, duration_ms: null }} />);
    expect(screen.getByText("No output recorded yet.")).toBeTruthy();
  });
});
