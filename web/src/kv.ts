/**
 * Converts between a map and editable text with one entry per line.
 * Headers use "Name: value"; environment variables use "NAME=value".
 */
export function parseLines(text: string, sep: ":" | "="): { value: Record<string, string>; error?: string } {
  const out: Record<string, string> = {};
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!.trim();
    if (!line || line.startsWith("#")) continue;
    const at = line.indexOf(sep);
    if (at <= 0) return { value: out, error: `Line ${i + 1}: expected ${sep === ":" ? "Name: value" : "NAME=value"}` };
    const key = line.slice(0, at).trim();
    const val = line.slice(at + 1).trim();
    if (key in out) return { value: out, error: `Line ${i + 1}: "${key}" is set twice` };
    out[key] = val;
  }
  return { value: out };
}

export function formatLines(map: Record<string, string> | undefined, sep: ":" | "="): string {
  if (!map) return "";
  return Object.entries(map)
    .map(([k, v]) => (sep === ":" ? `${k}: ${v}` : `${k}=${v}`))
    .join("\n");
}

/** Parses "200, 201,204" into [200, 201, 204]. Returns an error for anything else. */
export function parseStatusList(text: string): { value: number[]; error?: string } {
  const parts = text.split(",").map((s) => s.trim()).filter(Boolean);
  const out: number[] = [];
  for (const p of parts) {
    const n = Number(p);
    if (!Number.isInteger(n) || n < 100 || n > 599) return { value: out, error: `"${p}" is not an HTTP status code` };
    out.push(n);
  }
  return { value: out };
}
