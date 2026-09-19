/** The daemon's self-report, as `GET /v1/health` returns it. */
export interface Health {
  status: string;
  version: string;
  uptime: string;
  started: string;
  database: { schema_version: number; reachable: boolean; error?: string };
  auth: { token_configured: boolean };
  secrets: { backend: string };
  warnings?: string[];
}

/** The fixed set of things the tray can say about the daemon. */
export type DaemonState =
  | "running"
  | "degraded"
  | "stopped"
  | "needs_setup"
  | "token_rejected";

/** A single poll's outcome, exactly as the Rust side serialises it. */
export interface DaemonStatus {
  state: DaemonState;
  health: Health | null;
  detail: string | null;
  address: string;
  polled_at_ms: number;
}

/**
 * How a state reads in the pill.
 *
 * `tone` drives the colour only. Only `running` carries the "Running" label:
 * every other state has a label of its own, so the pill never reports a daemon
 * that is not answering as one that is.
 */
export interface Pill {
  label: string;
  tone: "ok" | "attention" | "stopped";
}

const pills: Record<DaemonState, Pill> = {
  running: { label: "Running", tone: "ok" },
  degraded: { label: "Degraded", tone: "attention" },
  stopped: { label: "Stopped", tone: "stopped" },
  needs_setup: { label: "Needs setup", tone: "attention" },
  token_rejected: { label: "Token rejected", tone: "attention" },
};

export function pillFor(state: DaemonState): Pill {
  return pills[state];
}

/**
 * What the operator has to do, for the states only they can resolve.
 *
 * Both are the same command: `tumika token rotate` is what writes a token the
 * app can read into the login Keychain.
 */
export function instructionFor(state: DaemonState): string | null {
  switch (state) {
    case "needs_setup":
      return "Run tumika token rotate to put an API token in the login Keychain.";
    case "token_rejected":
      return "Run tumika token rotate to replace the token the daemon refused.";
    default:
      return null;
  }
}

/**
 * Ages a poll against the reader's own clock, so the footer keeps counting
 * between polls instead of freezing at whatever the last one reported.
 */
export function formatPolledAgo(nowMs: number, polledAtMs: number): string {
  const seconds = Math.max(0, Math.floor((nowMs - polledAtMs) / 1000));
  if (seconds < 60) {
    return `polled ${seconds}s ago`;
  }
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return `polled ${minutes}m ago`;
  }
  return `polled ${Math.floor(minutes / 60)}h ago`;
}
