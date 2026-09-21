import { useEffect, useState } from "react";

import { currentStatus, onStatus, quit } from "./bridge";
import {
  formatPolledAgo,
  instructionFor,
  pillFor,
  updateLine,
  type DaemonStatus,
} from "./daemon";

/**
 * Where the daemon listens unless it was told otherwise. A non-default address
 * is only known once a poll reports it.
 */
const DEFAULT_ADDRESS = "127.0.0.1:8737";

/** How often "polled Ns ago" is recomputed, which is what makes it count up. */
const TICK_MS = 1000;

export default function App() {
  const [status, setStatus] = useState<DaemonStatus | null>(null);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    let active = true;
    let unlisten: (() => void) | undefined;

    currentStatus()
      // The first poll can land between this call and its answer, so an older
      // status never replaces one that arrived by event.
      .then((initial) => {
        if (active && initial) {
          setStatus((previous) => previous ?? initial);
        }
      })
      .catch((err: unknown) => console.error("reading the current status", err));

    onStatus((next) => {
      if (active) {
        setStatus(next);
      }
    })
      .then((stop) => {
        if (active) {
          unlisten = stop;
        } else {
          stop();
        }
      })
      .catch((err: unknown) => console.error("subscribing to status", err));

    return () => {
      active = false;
      unlisten?.();
    };
  }, []);

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(timer);
  }, []);

  const pill = status ? pillFor(status.state) : null;
  const instruction = status ? instructionFor(status.state) : null;
  const detail = status && status.state !== "running" ? status.detail : null;

  return (
    <main className="popover">
      <header className="header">
        <h1 className="name">tumika</h1>
        {pill ? (
          <span className={`pill pill-${pill.tone}`} data-testid="state-pill">
            <span className="dot" aria-hidden="true" />
            {pill.label}
          </span>
        ) : (
          <span className="pill pill-checking" data-testid="state-pill">
            Checking…
          </span>
        )}
      </header>

      {status?.health ? (
        <p className="meta" data-testid="daemon-meta">
          tumika {status.health.version} · up {status.health.uptime}
        </p>
      ) : null}

      {detail ? (
        <p className="detail" data-testid="detail">
          {detail}
        </p>
      ) : null}

      {instruction ? (
        <p className="instruction" data-testid="instruction">
          {instruction}
        </p>
      ) : null}

      {status?.update ? (
        <p
          className={`update update-${status.update.state}`}
          data-testid="update"
        >
          {updateLine(status.update)}
        </p>
      ) : null}

      <footer className="footer">
        <span className="polled" data-testid="polled">
          {status
            ? `${formatPolledAgo(now, status.polled_at_ms)} · ${status.address}`
            : DEFAULT_ADDRESS}
        </span>
        <button
          type="button"
          className="quit"
          onClick={() => {
            quit().catch((err: unknown) => console.error("quitting", err));
          }}
        >
          Quit
        </button>
      </footer>
    </main>
  );
}
