// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type WebhookEndpoint } from "../api";
import { Webhooks } from "./Webhooks";

const endpoint: WebhookEndpoint = {
  id: "whk_1", application_id: "app_billing", url: "https://hooks.example.com/x", description: "",
  events: [], enabled: true, created_at: "2026-11-11T10:00:00Z", updated_at: "2026-11-11T10:00:00Z",
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Webhooks", () => {
  it("creates an endpoint for chosen events and shows its secret once", async () => {
    vi.spyOn(api, "listWebhooks").mockResolvedValue([]);
    const create = vi.spyOn(api, "createWebhook").mockResolvedValue({ ...endpoint, events: ["execution.failed"], secret: "whsec_SECRET" });
    render(<Webhooks appId="app_billing" />);

    fireEvent.click(await screen.findByRole("button", { name: "+ Add endpoint" }));
    fireEvent.change(screen.getByLabelText(/Endpoint URL/), { target: { value: " https://hooks.example.com/x " } });
    fireEvent.click(screen.getByLabelText("All events")); // narrow to chosen events; execution.failed is preselected
    fireEvent.click(screen.getByRole("button", { name: "Add endpoint" }));

    expect(await screen.findByText("whsec_SECRET")).toBeTruthy();
    expect(create).toHaveBeenCalledWith({ application_id: "app_billing", url: "https://hooks.example.com/x", description: "", events: ["execution.failed"] });
  });

  it("shows the delivery log with failures and redelivers", async () => {
    vi.spyOn(api, "listWebhooks").mockResolvedValue([endpoint]);
    const list = vi.spyOn(api, "listDeliveries").mockResolvedValue([
      {
        id: "dlv_1", endpoint_id: "whk_1", event_id: "evt_1", event_type: "execution.failed", status: "failed", attempts: 10,
        next_attempt_at: null, last_attempt_at: "2026-11-11T10:00:00Z", last_status_code: 500, last_error: "endpoint answered 500",
        last_response: "", delivered_at: null, created_at: "2026-11-10T10:00:00Z",
      },
    ]);
    const redeliver = vi.spyOn(api, "redeliver").mockResolvedValue({} as never);
    render(<Webhooks appId="app_billing" />);

    fireEvent.click(await screen.findByRole("button", { name: "Deliveries" }));
    expect(await screen.findByText("endpoint answered 500")).toBeTruthy();
    expect(screen.getByText(/10 attempts · HTTP 500/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Redeliver" }));
    await waitFor(() => expect(redeliver).toHaveBeenCalledWith("whk_1", "dlv_1"));
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  });
});
