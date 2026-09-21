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

/** What the app is doing about its own component version. */
export type UpdateState = "checking" | "up_to_date" | "updating" | "failed";

/** The app's update status, exactly as the Rust side serialises it. */
export interface UpdateStatus {
  state: UpdateState;
  component_version: string;
  target_component_version: string | null;
  detail: string | null;
  checked_at_ms: number;
}

/** A single poll's outcome, exactly as the Rust side serialises it. */
export interface DaemonStatus {
  state: DaemonState;
  health: Health | null;
  detail: string | null;
  address: string;
  polled_at_ms: number;
  update: UpdateStatus | null;
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

export function pillFor(state: DaemonState): Pill {
  switch (state) {
    case "running":
      return { label: "Running", tone: "ok" };
    case "degraded":
      return { label: "Degraded", tone: "attention" };
    case "stopped":
      return { label: "Stopped", tone: "stopped" };
    case "needs_setup":
      return { label: "Needs setup", tone: "attention" };
    case "token_rejected":
      return { label: "Token rejected", tone: "attention" };
  }
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

/** One line saying where the app stands against its daemon's release. */
export function updateLine(update: UpdateStatus): string {
  switch (update.state) {
    case "checking":
      return "Checking the release…";
    case "up_to_date":
      return `Up to date with the release · component version ${update.component_version}`;
    case "updating":
      return update.target_component_version
        ? `Updating to component version ${update.target_component_version}…`
        : "Updating…";
    case "failed":
      return update.detail ? `Update failed: ${update.detail}` : "Update failed";
  }
}
