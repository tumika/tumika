/**
 * The webview's only channel to the daemon.
 *
 * Every field the popover renders arrives through `invoke` and `listen`: the
 * webview holds no API token and makes no HTTP call, because the daemon's Origin
 * middleware answers 403 to any request carrying an `Origin` header, which a
 * webview `fetch` always does.
 *
 * Keeping the two Tauri entry points in one module is also what lets the
 * component be tested without a Tauri runtime.
 */
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";

import type { DaemonStatus } from "./daemon";

/** The event the Rust side emits after every poll. */
const STATUS_EVENT = "daemon-status";

/** The most recent poll, or `null` when none has finished yet. */
export async function currentStatus(): Promise<DaemonStatus | null> {
  return (await invoke<DaemonStatus | null>("daemon_status")) ?? null;
}

/** Subscribes to every subsequent poll; resolves to the unsubscribe function. */
export async function onStatus(
  handler: (status: DaemonStatus) => void,
): Promise<() => void> {
  return listen<DaemonStatus>(STATUS_EVENT, (event) => handler(event.payload));
}

/**
 * Ends the app.
 *
 * A webview cannot exit the process itself, so this is a Rust command.
 */
export async function quit(): Promise<void> {
  await invoke("quit");
}
