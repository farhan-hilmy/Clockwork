import { useState, type FormEvent } from "react";
import { api, ApiError, setToken } from "../api";
import { Button, Field, inputClass } from "../ui";

/** Asks for the admin token and verifies it against the API before storing it. */
export function TokenGate() {
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    const candidate = value.trim();
    try {
      await api.verifyToken(candidate);
      setToken(candidate); // only store a token the server accepted
    } catch (err) {
      setError(err instanceof ApiError && err.status === 401 ? "That token was rejected." : (err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex h-full items-center justify-center p-4">
      <form onSubmit={submit} className="w-[400px] max-w-full rounded-xl border border-line bg-surface p-6">
        <div className="mb-5 flex items-center gap-2.5">
          <svg width="24" height="24" viewBox="0 0 24 24" fill="none" aria-hidden>
            <circle cx="12" cy="12" r="9" stroke="var(--color-brass)" strokeWidth="2" />
            <path d="M12 7v5l3 2" stroke="var(--color-brass)" strokeWidth="2" strokeLinecap="round" />
          </svg>
          <h1 className="text-lg font-semibold">Sign in to Clockwork</h1>
        </div>
        <Field label="Admin token" hint="The value of CLOCKWORK_ADMIN_TOKEN on the server." error={error}>
          <input
            type="password"
            autoFocus
            autoComplete="current-password"
            className={inputClass}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            aria-invalid={!!error}
          />
        </Field>
        <Button variant="primary" type="submit" className="mt-5 w-full justify-center" disabled={busy || !value.trim()}>
          {busy ? "Checking…" : "Continue"}
        </Button>
      </form>
    </div>
  );
}
