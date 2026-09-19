import { describe, expect, it } from "vitest";

import { formatPolledAgo, instructionFor, pillFor } from "./daemon";

describe("pillFor", () => {
  it("labels every state, and only running reads as Running", () => {
    expect(pillFor("running")).toEqual({ label: "Running", tone: "ok" });
    expect(pillFor("degraded")).toEqual({
      label: "Degraded",
      tone: "attention",
    });
    expect(pillFor("stopped")).toEqual({ label: "Stopped", tone: "stopped" });
    expect(pillFor("needs_setup")).toEqual({
      label: "Needs setup",
      tone: "attention",
    });
    expect(pillFor("token_rejected")).toEqual({
      label: "Token rejected",
      tone: "attention",
    });
  });
});

describe("instructionFor", () => {
  it("names the command for the states the operator has to resolve", () => {
    expect(instructionFor("needs_setup")).toContain("tumika token rotate");
    expect(instructionFor("token_rejected")).toContain("tumika token rotate");
  });

  it("has nothing to say about a daemon that is answering", () => {
    expect(instructionFor("running")).toBeNull();
    expect(instructionFor("degraded")).toBeNull();
    expect(instructionFor("stopped")).toBeNull();
  });
});

describe("formatPolledAgo", () => {
  const polled = 1_700_000_000_000;

  it("counts seconds, then minutes, then hours", () => {
    expect(formatPolledAgo(polled, polled)).toBe("polled 0s ago");
    expect(formatPolledAgo(polled + 12_000, polled)).toBe("polled 12s ago");
    expect(formatPolledAgo(polled + 59_999, polled)).toBe("polled 59s ago");
    expect(formatPolledAgo(polled + 60_000, polled)).toBe("polled 1m ago");
    expect(formatPolledAgo(polled + 90_000, polled)).toBe("polled 1m ago");
    expect(formatPolledAgo(polled + 3_600_000, polled)).toBe("polled 1h ago");
  });

  it("reports no age at all for a poll the clock thinks is in the future", () => {
    expect(formatPolledAgo(polled - 5_000, polled)).toBe("polled 0s ago");
  });
});
