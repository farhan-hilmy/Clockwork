// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type Application } from "../api";
import { Executions, sinceFor } from "./Executions";

const app = (id: string, name: string): Application => ({
  id, name, description: "", enabled: true, task_count: 0, created_at: "", updated_at: "",
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("sinceFor", () => {
  it("turns a range into an RFC 3339 start time", () => {
    const now = Date.parse("2026-11-12T12:00:00.123Z");
    expect(sinceFor("", now)).toBeUndefined();
    expect(sinceFor("1h", now)).toBe("2026-11-12T11:00:00Z");
    expect(sinceFor("7d", now)).toBe("2026-11-05T12:00:00Z");
  });
});

describe("Executions filters", () => {
  it("queries by application and time range", async () => {
    vi.spyOn(api, "applications").mockResolvedValue([app("app_clockwork", "clockwork"), app("app_b", "billing")]);
    const list = vi.spyOn(api, "listExecutions").mockResolvedValue({ executions: [] });
    render(
      <MemoryRouter initialEntries={["/executions"]}>
        <Executions />
      </MemoryRouter>,
    );
    fireEvent.change(await screen.findByLabelText("Filter by application"), { target: { value: "app_b" } });
    fireEvent.change(screen.getByLabelText("Filter by time"), { target: { value: "24h" } });
    await waitFor(() => {
      const last = list.mock.calls.at(-1)![0]!;
      expect(last.application_id).toBe("app_b");
      expect(last.since).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
    });
    expect(await screen.findByText("No executions match")).toBeTruthy();
  });
});
