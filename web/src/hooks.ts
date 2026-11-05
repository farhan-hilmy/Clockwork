import { useCallback, useEffect, useRef, useState } from "react";
import { getToken } from "./api";

export type AsyncState<T> =
  | { status: "loading" }
  | { status: "error"; error: Error }
  | { status: "ready"; data: T };

/**
 * Runs a loader on mount and whenever deps change; `reload` re-runs it.
 * When deps change the state resets to loading, so callers never see data
 * that belongs to the previous deps. A reload keeps showing the current data.
 */
export function useAsync<T>(load: () => Promise<T>, deps: unknown[]): AsyncState<T> & { reload: () => void } {
  const [state, setState] = useState<AsyncState<T>>({ status: "loading" });
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((n) => n + 1), []);
  const lastNonce = useRef(nonce);

  useEffect(() => {
    let cancelled = false;
    const isReload = lastNonce.current !== nonce;
    lastNonce.current = nonce;
    setState((s) => (isReload && s.status === "ready" ? s : { status: "loading" }));
    load().then(
      (data) => !cancelled && setState({ status: "ready", data }),
      (error: Error) => !cancelled && setState({ status: "error", error }),
    );
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce]);

  return { ...state, reload };
}

/** Tracks whether an admin token is stored; updates when it changes. */
export function useToken(): string {
  const [token, set] = useState(getToken);
  useEffect(() => {
    const update = () => set(getToken());
    window.addEventListener("clockwork:auth", update);
    window.addEventListener("storage", update);
    return () => {
      window.removeEventListener("clockwork:auth", update);
      window.removeEventListener("storage", update);
    };
  }, []);
  return token;
}
