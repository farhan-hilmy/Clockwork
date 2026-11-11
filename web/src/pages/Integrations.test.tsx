// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type Application } from "../api";
import { Integrations } from "./Integrations";

const defaultApp: Application = {
  id: "app_clockwork", name: "clockwork", description: "", enabled: true, task_count: 2,
  created_at: "2026-11-11T10:00:00Z", updated_at: "2026-11-11T10:00:00Z",
};
const billing: Application = { ...defaultApp, id: "app_billing", name: "billing", task_count: 1 };

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Integrations", () => {
  it("shows a new API key once and lets the operator dismiss it", async () => {
    vi.spyOn(api, "applications").mockResolvedValue([defaultApp, billing]);
    const keys = vi.spyOn(api, "listKeys").mockResolvedValue([]);
    vi.spyOn(api, "listWebhooks").mockResolvedValue([]);
    vi.spyOn(api, "createKey").mockResolvedValue({
      id: "key_1", application_id: "app_billing", name: "prod", prefix: "cwk_abcdef",
      created_at: "2026-11-11T10:00:00Z", last_used_at: null, revoked_at: null, key: "cwk_abcdefSECRET",
    });
    render(<MemoryRouter><Integrations /></MemoryRouter>);

    await screen.findByText("billing");
    expect(screen.getByText(/has no API keys/)).toBeTruthy(); // the default application cannot have keys
    // An application with tasks cannot be deleted from here.
    expect((screen.getByRole("button", { name: "Delete" }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(screen.getByLabelText("New key name"), { target: { value: "prod" } });
    fireEvent.click(screen.getByRole("button", { name: "Create key" }));
    expect(await screen.findByText("cwk_abcdefSECRET")).toBeTruthy();
    expect(screen.getByText(/will not be shown again/)).toBeTruthy();
    await waitFor(() => expect(keys).toHaveBeenCalledTimes(2)); // the list reloads after creation

    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText("cwk_abcdefSECRET")).toBeNull();
  });
});
