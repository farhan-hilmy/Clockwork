import { describe, expect, it } from "vitest";
import { formatDuration, relativeTime } from "./format";

describe("formatDuration", () => {
  it.each([
    [null, "—"],
    [840, "840 ms"],
    [2400, "2.4 s"],
    [185_000, "3m 05s"],
    [3_720_000, "1h 02m"],
  ])("%s -> %s", (ms, want) => expect(formatDuration(ms)).toBe(want));
});

describe("relativeTime", () => {
  const now = new Date("2026-11-12T12:00:00Z");
  it("describes past and future", () => {
    expect(relativeTime("2026-11-12T11:00:00Z", now)).toBe("1h ago");
    expect(relativeTime("2026-11-12T12:05:00Z", now)).toBe("in 5m");
    expect(relativeTime("2026-11-12T12:00:10Z", now)).toBe("in <1m");
    expect(relativeTime(null, now)).toBe("");
  });
});
