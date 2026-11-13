/** Formats an ISO timestamp as a short local date-time, or "—" when absent. */
export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

/** Describes how far in the future (or past) a timestamp is, relative to now. */
export function relativeTime(iso: string | null | undefined, now: Date = new Date()): string {
  if (!iso) return "";
  const diff = new Date(iso).getTime() - now.getTime();
  const abs = Math.abs(diff);
  const units: [number, string][] = [
    [86_400_000, "d"],
    [3_600_000, "h"],
    [60_000, "m"],
  ];
  for (const [ms, label] of units) {
    if (abs >= ms) {
      const n = Math.round(abs / ms);
      return diff >= 0 ? `in ${n}${label}` : `${n}${label} ago`;
    }
  }
  return diff >= 0 ? "in <1m" : "just now";
}

/** Browser timezone, used as the default for new schedules. */
export function localTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

/** Summarises a runner config in one line for tables. */
export function runnerSummary(type: string, config: Record<string, unknown>): string {
  if (type === "http") return `${String(config.method ?? "GET")} ${String(config.url ?? "")}`;
  if (type === "shell") return String(config.command ?? "").split("\n")[0] ?? "";
  return type;
}

/** Formats milliseconds compactly: 840 ms, 2.4 s, 3m 05s, 1h 02m. */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  const s = Math.round(ms / 1000);
  if (s < 3600) return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(s / 3600)}h ${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}m`;
}
