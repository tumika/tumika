//! What the tray reports, and how a poll produces it.

use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::Serialize;

use crate::health::{Health, HealthClient, HealthError};
use crate::keychain::{self, TokenSource};
use crate::update::UpdateStatus;

/// The fixed set of things the tray can say about the daemon.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum DaemonState {
    /// The daemon answered and reports itself healthy.
    Running,
    /// The daemon answered and reports a problem of its own.
    Degraded,
    /// Nothing answered on the address.
    Stopped,
    /// There is no API token to authenticate with.
    NeedsSetup,
    /// The daemon refused the token the Keychain holds.
    TokenRejected,
}

/// A single poll's outcome, as the popover receives it.
#[derive(Debug, Clone, Serialize)]
pub struct DaemonStatus {
    pub state: DaemonState,
    /// The daemon's own report, present whenever it answered with one.
    pub health: Option<Health>,
    /// Why the state is not `Running`, in the daemon's or the Keychain's words.
    pub detail: Option<String>,
    /// The address being polled.
    pub address: String,
    /// Wall-clock milliseconds of this poll, so a reader can age it against a
    /// clock of its own rather than trusting an elapsed count to stay fresh.
    pub polled_at_ms: u64,
    /// The app's own update status, or `None` before the first check has
    /// started. A poll leaves it empty; the publisher fills it from the update
    /// loop's shared status.
    pub update: Option<UpdateStatus>,
}

/// Which menu bar image a state calls for.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IconKind {
    AllClear,
    NeedsYou,
    Stopped,
}

/// Maps a state onto its icon, in precedence order: a daemon that is not there
/// outranks anything asking for the user's attention, which outranks all clear.
pub const fn icon_for(state: DaemonState) -> IconKind {
    match state {
        DaemonState::Stopped => IconKind::Stopped,
        DaemonState::NeedsSetup | DaemonState::TokenRejected | DaemonState::Degraded => {
            IconKind::NeedsYou
        }
        DaemonState::Running => IconKind::AllClear,
    }
}

/// Reads the token and polls the daemon, once per call.
pub struct Monitor {
    source: Box<dyn TokenSource>,
    client: HealthClient,
    address: String,
    read_timeout: Duration,
}

impl Monitor {
    pub fn new(source: Box<dyn TokenSource>, base_url: &str) -> Result<Self, reqwest::Error> {
        Ok(Self {
            source,
            client: HealthClient::new(base_url)?,
            read_timeout: keychain::READ_TIMEOUT,
            // The popover shows where it is looking, which is the base URL
            // without the scheme it has no use for.
            address: base_url
                .split_once("://")
                .map_or(base_url, |(_, rest)| rest)
                .to_string(),
        })
    }

    /// Shortens the Keychain read's bound, so a test for what happens when it
    /// expires runs in milliseconds.
    #[cfg(test)]
    fn with_read_timeout(mut self, read_timeout: Duration) -> Self {
        self.read_timeout = read_timeout;
        self
    }

    /// Produces the current status.
    ///
    /// The token is re-read every time, so a rotation is picked up without
    /// restarting the app.
    pub async fn poll(&self) -> DaemonStatus {
        let token = match keychain::load(self.source.as_ref(), self.read_timeout).await {
            Ok(Some(token)) => token,
            Ok(None) => {
                return self.status(
                    DaemonState::NeedsSetup,
                    None,
                    Some("no API token in the login Keychain".to_string()),
                )
            }
            // A Keychain that cannot be read — or that does not answer in time —
            // leaves the app exactly as unable to authenticate as an absent item
            // does, and the detail says which. Publishing it is what keeps the
            // tray from showing a state nothing is checking any more.
            Err(err) => return self.status(DaemonState::NeedsSetup, None, Some(err.to_string())),
        };

        match self.client.get(&token).await {
            Ok(health) => {
                let state = state_for(&health.status);
                let detail = (state != DaemonState::Running).then(|| health.warnings.join("; "));
                self.status(state, Some(health), detail)
            }
            Err(HealthError::Unauthorized) => self.status(
                DaemonState::TokenRejected,
                None,
                Some(HealthError::Unauthorized.to_string()),
            ),
            Err(err @ HealthError::Transport(_)) => {
                self.status(DaemonState::Stopped, None, Some(err.to_string()))
            }
            // The daemon answered, so it is there; it just did not answer with a
            // health report.
            Err(err) => self.status(DaemonState::Degraded, None, Some(err.to_string())),
        }
    }

    fn status(
        &self,
        state: DaemonState,
        health: Option<Health>,
        detail: Option<String>,
    ) -> DaemonStatus {
        DaemonStatus {
            state,
            health,
            detail: detail.filter(|d| !d.is_empty()),
            address: self.address.clone(),
            polled_at_ms: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map(|since| u64::try_from(since.as_millis()).unwrap_or(u64::MAX))
                .unwrap_or_default(),
            update: None,
        }
    }
}

/// A report the app does not recognise is not a claim of health.
fn state_for(status: &str) -> DaemonState {
    match status {
        "ok" => DaemonState::Running,
        _ => DaemonState::Degraded,
    }
}

#[cfg(test)]
mod tests {
    use base64::engine::general_purpose::STANDARD;
    use base64::Engine;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;
    use tokio::sync::oneshot;

    use super::*;
    use crate::keychain::{KeychainError, ReadFuture};

    const TOKEN: &str = "tk_a_test_token";

    /// A Keychain holding one token, so no test reaches the real one.
    struct FakeKeychain(Result<Option<String>, ()>);

    impl TokenSource for FakeKeychain {
        fn read(&self) -> ReadFuture<'_> {
            let answer = match &self.0 {
                Ok(value) => Ok(value.clone()),
                Err(()) => Err(KeychainError::Empty),
            };
            Box::pin(async move { answer })
        }
    }

    /// A Keychain that takes longer to answer than it is given, as a locked one
    /// waiting on an unlock dialog does.
    struct SlowKeychain(Duration);

    impl TokenSource for SlowKeychain {
        fn read(&self) -> ReadFuture<'_> {
            let delay = self.0;
            Box::pin(async move {
                tokio::time::sleep(delay).await;
                Ok(None)
            })
        }
    }

    fn stored_token() -> Box<dyn TokenSource> {
        Box::new(FakeKeychain(Ok(Some(format!(
            "go-keyring-base64:{}\n",
            STANDARD.encode(TOKEN)
        )))))
    }

    /// Serves one canned HTTP response on a loopback port, and hands back the
    /// request it was sent.
    async fn serve_once(response: String) -> (String, oneshot::Receiver<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let addr = listener.local_addr().expect("addr");
        let (tx, rx) = oneshot::channel();

        tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.expect("accept");
            let mut buf = [0u8; 2048];
            let read = stream.read(&mut buf).await.expect("read");
            let _ = tx.send(String::from_utf8_lossy(&buf[..read]).to_string());
            stream.write_all(response.as_bytes()).await.expect("write");
            stream.flush().await.expect("flush");
        });

        (format!("http://{addr}"), rx)
    }

    fn ok_response(body: &str) -> String {
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        )
    }

    fn health_body(status: &str) -> String {
        format!(
            r#"{{"status":"{status}","version":"1.2.3","uptime":"3m20s","started":"2026-09-19T09:00:00Z","database":{{"schema_version":4,"reachable":true}},"auth":{{"token_configured":true}},"secrets":{{"backend":"keychain"}},"warnings":["database is unreachable"]}}"#
        )
    }

    #[tokio::test]
    async fn an_ok_report_is_running_and_carries_the_daemons_fields() {
        let (base_url, request) = serve_once(ok_response(&health_body("ok"))).await;

        let status = Monitor::new(stored_token(), &base_url)
            .expect("monitor")
            .poll()
            .await;

        assert_eq!(status.state, DaemonState::Running);
        let health = status.health.expect("health");
        assert_eq!(health.version, "1.2.3");
        assert_eq!(health.uptime, "3m20s");
        assert_eq!(health.database.schema_version, 4);
        assert!(health.auth.token_configured);
        assert_eq!(health.secrets.backend, "keychain");
        assert_eq!(status.address, base_url.trim_start_matches("http://"));
        assert!(status.polled_at_ms > 0);

        let request = request.await.expect("request");
        assert!(
            request.contains(&format!("authorization: Bearer {TOKEN}"))
                || request.contains(&format!("Authorization: Bearer {TOKEN}")),
            "the token must reach the daemon as a bearer credential"
        );
        assert!(
            !request.to_ascii_lowercase().contains("origin:"),
            "an Origin header makes the daemon answer 403"
        );
    }

    #[tokio::test]
    async fn a_degraded_report_is_degraded() {
        let (base_url, _request) = serve_once(ok_response(&health_body("degraded"))).await;

        let status = Monitor::new(stored_token(), &base_url)
            .expect("monitor")
            .poll()
            .await;

        assert_eq!(status.state, DaemonState::Degraded);
        assert_eq!(status.detail.as_deref(), Some("database is unreachable"));
    }

    #[tokio::test]
    async fn a_refused_token_is_token_rejected() {
        let (base_url, _request) = serve_once(
            "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                .to_string(),
        )
        .await;

        let status = Monitor::new(stored_token(), &base_url)
            .expect("monitor")
            .poll()
            .await;

        assert_eq!(status.state, DaemonState::TokenRejected);
        assert!(status.health.is_none());
    }

    #[tokio::test]
    async fn a_refused_connection_is_stopped() {
        // Binding and dropping yields a port nothing is listening on.
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let addr = listener.local_addr().expect("addr");
        drop(listener);

        let status = Monitor::new(stored_token(), &format!("http://{addr}"))
            .expect("monitor")
            .poll()
            .await;

        assert_eq!(status.state, DaemonState::Stopped);
    }

    #[tokio::test]
    async fn an_absent_keychain_item_is_needs_setup() {
        let status = Monitor::new(Box::new(FakeKeychain(Ok(None))), "http://127.0.0.1:1")
            .expect("monitor")
            .poll()
            .await;

        assert_eq!(status.state, DaemonState::NeedsSetup);
    }

    #[tokio::test]
    async fn an_unreadable_keychain_is_needs_setup() {
        let status = Monitor::new(Box::new(FakeKeychain(Err(()))), "http://127.0.0.1:1")
            .expect("monitor")
            .poll()
            .await;

        assert_eq!(status.state, DaemonState::NeedsSetup);
        assert!(status.detail.is_some());
    }

    #[tokio::test]
    async fn a_keychain_that_does_not_answer_is_needs_setup() {
        let status = Monitor::new(
            Box::new(SlowKeychain(Duration::from_secs(60))),
            "http://127.0.0.1:1",
        )
        .expect("monitor")
        .with_read_timeout(Duration::from_millis(20))
        .poll()
        .await;

        assert_eq!(status.state, DaemonState::NeedsSetup);
        let detail = status.detail.expect("detail");
        assert!(detail.contains("did not answer"), "{detail}");
    }

    #[test]
    fn icons_follow_stopped_then_needs_you_then_all_clear() {
        assert_eq!(icon_for(DaemonState::Stopped), IconKind::Stopped);
        assert_eq!(icon_for(DaemonState::NeedsSetup), IconKind::NeedsYou);
        assert_eq!(icon_for(DaemonState::TokenRejected), IconKind::NeedsYou);
        assert_eq!(icon_for(DaemonState::Degraded), IconKind::NeedsYou);
        assert_eq!(icon_for(DaemonState::Running), IconKind::AllClear);
    }

    #[test]
    fn the_status_never_serialises_the_token() {
        let status = DaemonStatus {
            state: DaemonState::Running,
            health: None,
            detail: None,
            address: "127.0.0.1:8737".to_string(),
            polled_at_ms: 1,
            update: None,
        };

        let encoded = serde_json::to_string(&status).expect("serialise");

        assert!(!encoded.contains(TOKEN), "{encoded}");
        assert_eq!(
            encoded,
            r#"{"state":"running","health":null,"detail":null,"address":"127.0.0.1:8737","polled_at_ms":1,"update":null}"#
        );
    }

    #[test]
    fn the_update_status_serialises_with_the_daemon_status() {
        use crate::update::UpdateState;

        let status = DaemonStatus {
            state: DaemonState::Running,
            health: None,
            detail: None,
            address: "127.0.0.1:8737".to_string(),
            polled_at_ms: 1,
            update: Some(UpdateStatus {
                state: UpdateState::Failed,
                component_version: "0.1.0".to_string(),
                target_component_version: None,
                detail: Some("signature rejected".to_string()),
                checked_at_ms: 2,
            }),
        };

        let encoded = serde_json::to_value(&status).expect("serialise");

        assert_eq!(
            encoded["update"],
            serde_json::json!({
                "state": "failed",
                "component_version": "0.1.0",
                "target_component_version": null,
                "detail": "signature rejected",
                "checked_at_ms": 2
            })
        );
    }
}
