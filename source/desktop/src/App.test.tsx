import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import App from "./App";
import type { DaemonState, DaemonStatus, Health } from "./daemon";

/** Every handler `App` has subscribed with, so a test can publish a poll. */
const subscribers: Array<(status: DaemonStatus) => void> = [];
const initialStatus = vi.fn<() => Promise<DaemonStatus | null>>();
const quit = vi.fn<() => Promise<void>>();

vi.mock("./bridge", () => ({
  currentStatus: () => initialStatus(),
  onStatus: async (handler: (status: DaemonStatus) => void) => {
    subscribers.push(handler);
    return () => {
      subscribers.splice(subscribers.indexOf(handler), 1);
    };
  },
  quit: () => quit(),
}));

const POLLED_AT = 1_700_000_000_000;

function health(overrides: Partial<Health> = {}): Health {
  return {
    status: "ok",
    version: "0.9.4",
    uptime: "6d4h",
    started: "2026-09-13T08:00:00Z",
    database: { schema_version: 4, reachable: true },
    auth: { token_configured: true },
    secrets: { backend: "keychain" },
    warnings: [],
    ...overrides,
  };
}

function status(overrides: Partial<DaemonStatus> = {}): DaemonStatus {
  return {
    state: "running",
    health: health(),
    detail: null,
    address: "127.0.0.1:8737",
    polled_at_ms: POLLED_AT,
    ...overrides,
  };
}

/** Renders and settles the subscription, leaving the popover pre-first-poll. */
async function renderPopover() {
  render(<App />);
  await act(async () => {});
}

async function publish(next: DaemonStatus) {
  await act(async () => {
    for (const subscriber of [...subscribers]) {
      subscriber(next);
    }
  });
}

beforeEach(() => {
  subscribers.length = 0;
  initialStatus.mockResolvedValue(null);
  quit.mockResolvedValue(undefined);
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(POLLED_AT);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("the state pill", () => {
  const labels: Array<[DaemonState, string]> = [
    ["running", "Running"],
    ["degraded", "Degraded"],
    ["stopped", "Stopped"],
    ["needs_setup", "Needs setup"],
    ["token_rejected", "Token rejected"],
  ];

  for (const [state, label] of labels) {
    it(`reads "${label}" for ${state}`, async () => {
      await renderPopover();

      await publish(status({ state, health: state === "running" ? health() : null }));

      const pill = screen.getByTestId("state-pill");
      expect(pill.textContent).toBe(label);
      if (state !== "running") {
        expect(pill.textContent).not.toContain("Running");
      }
    });
  }

  it("says Checking… until the first poll lands", async () => {
    await renderPopover();

    expect(screen.getByTestId("state-pill").textContent).toBe("Checking…");
    expect(screen.queryByTestId("daemon-meta")).toBeNull();

    await publish(status());

    expect(screen.getByTestId("state-pill").textContent).toBe("Running");
  });
});

describe("the daemon's own fields", () => {
  it("shows the version, the uptime and the address it polled", async () => {
    await renderPopover();

    await publish(status());

    expect(screen.getByTestId("daemon-meta").textContent).toBe(
      "tumika 0.9.4 · up 6d4h",
    );
    expect(screen.getByTestId("polled").textContent).toBe(
      "polled 0s ago · 127.0.0.1:8737",
    );
  });

  it("shows the default address before anything has been polled", async () => {
    await renderPopover();

    expect(screen.getByTestId("polled").textContent).toBe("127.0.0.1:8737");
  });

  it("ages the last poll between polls", async () => {
    await renderPopover();

    await publish(status());
    await act(async () => {
      vi.advanceTimersByTime(5_000);
    });

    expect(screen.getByTestId("polled").textContent).toBe(
      "polled 5s ago · 127.0.0.1:8737",
    );
  });
});

describe("the states the operator has to resolve", () => {
  it("shows the detail and names the command when there is no token", async () => {
    await renderPopover();

    await publish(
      status({
        state: "needs_setup",
        health: null,
        detail: "no API token in the login Keychain",
      }),
    );

    expect(screen.getByTestId("detail").textContent).toBe(
      "no API token in the login Keychain",
    );
    expect(screen.getByTestId("instruction").textContent).toContain(
      "tumika token rotate",
    );
  });

  it("names the same command when the daemon refused the token", async () => {
    await renderPopover();

    await publish(
      status({
        state: "token_rejected",
        health: null,
        detail: "the daemon rejected the API token",
      }),
    );

    expect(screen.getByTestId("instruction").textContent).toContain(
      "tumika token rotate",
    );
  });

  it("leaves a running daemon without a detail or an instruction", async () => {
    await renderPopover();

    await publish(status({ detail: "stale" }));

    expect(screen.queryByTestId("detail")).toBeNull();
    expect(screen.queryByTestId("instruction")).toBeNull();
  });
});

describe("quitting", () => {
  it("asks the app to exit", async () => {
    await renderPopover();

    fireEvent.click(screen.getByRole("button", { name: "Quit" }));

    expect(quit).toHaveBeenCalledTimes(1);
  });
});

describe("the fields no daemon data exists for", () => {
  it("renders none of them", async () => {
    await renderPopover();

    await publish(status());

    for (const absent of [
      "Stop",
      "Refresh",
      "Open Tumika",
      "Tasks",
      "Notifications",
      "Clear all",
    ]) {
      expect(screen.queryByText(absent)).toBeNull();
    }
    expect(screen.queryByText(/pid/i)).toBeNull();
    expect(screen.getAllByRole("button")).toHaveLength(1);
  });
});
