import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { useAsync } from "./hooks";

describe("useAsync", () => {
  it("does not expose data from previous deps while new deps load", async () => {
    const resolvers: Record<string, (v: string) => void> = {};
    const load = (id: string) => () => new Promise<string>((r) => (resolvers[id] = r));

    const { result, rerender } = renderHook(({ id }) => useAsync(load(id), [id]), { initialProps: { id: "a" } });
    act(() => resolvers.a!("data-a"));
    await waitFor(() => expect(result.current.status).toBe("ready"));

    rerender({ id: "b" });
    await waitFor(() => expect(result.current.status).toBe("loading"));

    act(() => resolvers.b!("data-b"));
    await waitFor(() => expect(result.current).toMatchObject({ status: "ready", data: "data-b" }));
  });

  it("keeps showing current data during a reload", async () => {
    let n = 0;
    const { result } = renderHook(() => useAsync(() => Promise.resolve(++n), []));
    await waitFor(() => expect(result.current).toMatchObject({ status: "ready", data: 1 }));

    act(() => result.current.reload());
    expect(result.current.status).toBe("ready");
    await waitFor(() => expect(result.current).toMatchObject({ status: "ready", data: 2 }));
  });
});
