import { describe, expect, it } from "vitest";
import { formatLines, parseLines, parseStatusList } from "./kv";
import { relativeTime, runnerSummary } from "./format";

describe("parseLines", () => {
  it("parses headers, ignoring blanks and comments, keeping colons in values", () => {
    const r = parseLines("Authorization: Bearer a:b\n\n# note\nAccept:application/json", ":");
    expect(r.error).toBeUndefined();
    expect(r.value).toEqual({ Authorization: "Bearer a:b", Accept: "application/json" });
  });

  it("parses env vars with = in the value", () => {
    expect(parseLines("DSN=postgres://u@h/db?x=1", "=").value).toEqual({ DSN: "postgres://u@h/db?x=1" });
  });

  it("reports malformed and duplicate lines with their line number", () => {
    expect(parseLines("ok: 1\nbroken", ":").error).toMatch(/Line 2/);
    expect(parseLines("A=1\nA=2", "=").error).toMatch(/set twice/);
  });

  it("round-trips through formatLines", () => {
    const map = { "X-One": "1", "X-Two": "two" };
    expect(parseLines(formatLines(map, ":"), ":").value).toEqual(map);
  });
});

describe("parseStatusList", () => {
  it("accepts comma separated codes", () => {
    expect(parseStatusList("200, 201,204").value).toEqual([200, 201, 204]);
    expect(parseStatusList("").value).toEqual([]);
  });
  it("rejects non-codes", () => {
    expect(parseStatusList("200, abc").error).toBeDefined();
    expect(parseStatusList("700").error).toBeDefined();
  });
});

describe("format helpers", () => {
  const now = new Date("2026-11-05T14:00:00Z");
  it("describes relative times", () => {
    expect(relativeTime("2026-11-05T14:05:00Z", now)).toBe("in 5m");
    expect(relativeTime("2026-11-05T12:00:00Z", now)).toBe("2h ago");
    expect(relativeTime(null, now)).toBe("");
  });
  it("summarises runners", () => {
    expect(runnerSummary("http", { method: "POST", url: "https://x.dev/a" })).toBe("POST https://x.dev/a");
    expect(runnerSummary("shell", { command: "echo hi\necho there" })).toBe("echo hi");
  });
});
