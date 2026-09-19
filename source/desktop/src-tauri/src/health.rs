//! The `GET /v1/health` client.

use std::time::Duration;

use reqwest::{redirect::Policy, StatusCode};
use serde::{Deserialize, Serialize};

use crate::keychain::ApiToken;

/// Where the daemon listens unless it was told otherwise. A non-default listen
/// address lives in the daemon's own database, which is unreadable without the
/// API this client is trying to reach.
pub const DEFAULT_BASE_URL: &str = "http://127.0.0.1:8737";

/// Long enough for a daemon busy with a migration, short enough that a poll
/// never outlives the interval between polls.
const REQUEST_TIMEOUT: Duration = Duration::from_secs(5);

/// The daemon's self-report, exactly as `/v1/health` returns it.
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Health {
    /// `ok` or `degraded`.
    pub status: String,
    pub version: String,
    pub uptime: String,
    pub started: String,
    pub database: DatabaseHealth,
    pub auth: AuthHealth,
    pub secrets: SecretsHealth,
    /// Names everything that made `status` `degraded`.
    #[serde(default)]
    pub warnings: Vec<String>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct DatabaseHealth {
    pub schema_version: i64,
    pub reachable: bool,
    #[serde(default)]
    pub error: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct AuthHealth {
    pub token_configured: bool,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct SecretsHealth {
    pub backend: String,
}

/// Why a health poll produced no report.
#[derive(Debug, thiserror::Error)]
pub enum HealthError {
    /// Nothing answered on the address, so there is no daemon listening there.
    #[error("{0}")]
    Transport(String),

    /// The daemon answered and refused the token.
    #[error("the daemon rejected the API token")]
    Unauthorized,

    /// The daemon answered with something other than a health report.
    #[error("the daemon answered {0}")]
    Status(u16),

    #[error("the health response could not be read: {0}")]
    Body(String),
}

/// Polls one daemon's health endpoint.
pub struct HealthClient {
    http: reqwest::Client,
    url: String,
}

impl HealthClient {
    /// `base_url` is a scheme, host and port without a trailing slash, as in
    /// [`DEFAULT_BASE_URL`]. It is a parameter so tests can point the client at
    /// a loopback server of their own.
    pub fn new(base_url: &str) -> Result<Self, reqwest::Error> {
        let http = reqwest::Client::builder()
            .timeout(REQUEST_TIMEOUT)
            // A redirect would carry the bearer token to whatever it named, and
            // the health endpoint never issues one.
            .redirect(Policy::none())
            .build()?;

        Ok(Self {
            http,
            url: format!("{base_url}/v1/health"),
        })
    }

    /// Requests the daemon's health report.
    ///
    /// No `Origin` header is set, and none must be: the daemon's Origin
    /// middleware answers 403 to any request carrying one while no origin is
    /// allowed, which is the default configuration.
    pub async fn get(&self, token: &ApiToken) -> Result<Health, HealthError> {
        let response = self
            .http
            .get(&self.url)
            .bearer_auth(token.expose())
            .send()
            .await
            .map_err(|err| HealthError::Transport(err.to_string()))?;

        match response.status() {
            StatusCode::OK => response
                .json()
                .await
                .map_err(|err| HealthError::Body(err.to_string())),
            StatusCode::UNAUTHORIZED => Err(HealthError::Unauthorized),
            other => Err(HealthError::Status(other.as_u16())),
        }
    }
}
