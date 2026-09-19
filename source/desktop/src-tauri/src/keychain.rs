//! Reading the API token the daemon leaves in the login Keychain.

use std::future::Future;
use std::pin::Pin;
use std::time::Duration;

use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use tokio::process::Command;

/// Keychain coordinates the daemon writes to (`platform/tokencustody`). An
/// operator looks the item up under exactly these names:
///
/// ```text
/// security find-generic-password -s tumika -a api-token -w
/// ```
pub const SERVICE: &str = "tumika";
pub const ACCOUNT: &str = "api-token";

/// go-keyring stores the value behind this marker, followed by standard base64.
const BASE64_PREFIX: &str = "go-keyring-base64:";

/// `security` exits with errSecItemNotFound when the item does not exist, which
/// is the state of every machine whose token has not been minted or rotated
/// since the daemon started writing one.
const ITEM_NOT_FOUND: i32 = 44;

/// How long a Keychain read is given before it is abandoned.
///
/// A locked Keychain answers only once someone has dealt with the unlock dialog,
/// which may be never. The bound is well inside the poll interval, so a poll
/// that cannot read still publishes a status of its own before the next one is
/// due, and the tray never shows a state it has stopped checking.
pub const READ_TIMEOUT: Duration = Duration::from_secs(3);

/// An API token held in memory.
///
/// It has no `Debug`, no `Display` and no `Serialize`, so the only way to the
/// secret is [`ApiToken::expose`]. That is what keeps an incidental `{:?}` of a
/// surrounding value, or a serialisation of it towards the webview, from
/// carrying the token with it.
pub struct ApiToken(String);

impl ApiToken {
    /// Returns the secret. Every call site is a place the token could escape, so
    /// there are two: the `Authorization` header, and this module's tests.
    pub fn expose(&self) -> &str {
        &self.0
    }
}

/// Everything that can go wrong between the Keychain and a usable token.
#[derive(Debug, thiserror::Error)]
pub enum KeychainError {
    #[error("run /usr/bin/security: {0}")]
    Spawn(#[from] std::io::Error),

    #[error("security exited {status}: {stderr}")]
    Security { status: String, stderr: String },

    /// The stored value's bytes are not text. Neither this variant nor the
    /// base64 one repeats any part of the value, since every part of it is the
    /// token.
    #[error("the stored value is not valid UTF-8")]
    NotUtf8,

    #[error("the stored value is not valid base64")]
    Base64,

    #[error("the stored value is empty")]
    Empty,

    /// The read was abandoned before the Keychain answered. It names no part of
    /// the value either, because there never was one.
    #[error("the login Keychain did not answer within {0:?}")]
    Timeout(Duration),
}

/// What a [`TokenSource`] read returns.
///
/// Boxed rather than an `async fn` in the trait, because the source is held
/// behind `dyn` and an `async fn` in a trait is not dyn-compatible.
pub type ReadFuture<'a> =
    Pin<Box<dyn Future<Output = Result<Option<String>, KeychainError>> + Send + 'a>>;

/// Source of the raw Keychain value.
///
/// A trait rather than a direct call so tests can exercise the decoding and the
/// polling without an entry in the Keychain of whoever ran them. It is async so
/// the read never occupies a runtime worker, and so abandoning it drops the work
/// rather than leaving it running.
pub trait TokenSource: Send + Sync {
    /// Returns the stored value, or `None` when the item does not exist.
    fn read(&self) -> ReadFuture<'_>;
}

/// Reads through `/usr/bin/security`.
///
/// That binary is the item's trusted reader: go-keyring writes without `-T`, so
/// reaching the same item through the Security framework raises an
/// authorisation prompt instead of answering. The value arrives on this
/// process's stdout rather than in any command line, so it is never in `ps`.
pub struct SecurityCli;

impl TokenSource for SecurityCli {
    fn read(&self) -> ReadFuture<'_> {
        Box::pin(async move {
            let output = Command::new("/usr/bin/security")
                .args(["find-generic-password", "-s", SERVICE, "-a", ACCOUNT, "-w"])
                // A read that is given up on kills the child with it. Without
                // this, a Keychain that never answers leaves one waiting
                // `security` behind per poll, forever.
                .kill_on_drop(true)
                .output()
                .await?;

            if output.status.code() == Some(ITEM_NOT_FOUND) {
                return Ok(None);
            }
            if !output.status.success() {
                return Err(KeychainError::Security {
                    status: output.status.to_string(),
                    stderr: String::from_utf8_lossy(&output.stderr).trim().to_string(),
                });
            }

            String::from_utf8(output.stdout)
                .map(Some)
                .map_err(|_| KeychainError::NotUtf8)
        })
    }
}

/// Reads and decodes the token, reporting `None` when there is no item to read.
///
/// The read is given `timeout` and then abandoned, so a caller polling on a
/// schedule always gets an answer in time to publish one.
pub async fn load(
    source: &dyn TokenSource,
    timeout: Duration,
) -> Result<Option<ApiToken>, KeychainError> {
    let stored = tokio::time::timeout(timeout, source.read())
        .await
        .map_err(|_| KeychainError::Timeout(timeout))??;

    match stored {
        Some(stored) => decode(&stored).map(Some),
        None => Ok(None),
    }
}

/// Turns a stored Keychain value into the token the daemon accepts.
pub fn decode(stored: &str) -> Result<ApiToken, KeychainError> {
    // `security -w` terminates the value with a newline of its own.
    let stored = stored.trim_end_matches(['\r', '\n']);
    if stored.is_empty() {
        return Err(KeychainError::Empty);
    }

    // A value without the marker was stored verbatim, which is what go-keyring
    // does for plain text.
    let Some(encoded) = stored.strip_prefix(BASE64_PREFIX) else {
        return Ok(ApiToken(stored.to_string()));
    };

    let decoded = STANDARD
        .decode(encoded)
        .map_err(|_| KeychainError::Base64)?;
    let token = String::from_utf8(decoded).map_err(|_| KeychainError::NotUtf8)?;
    if token.is_empty() {
        return Err(KeychainError::Empty);
    }
    Ok(ApiToken(token))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decodes_a_prefixed_value() {
        let stored = format!("{BASE64_PREFIX}{}", STANDARD.encode("tk_secret"));

        let token = decode(&stored).expect("decode");

        assert_eq!(token.expose(), "tk_secret");
    }

    #[test]
    fn strips_the_newline_security_appends() {
        let stored = format!("{BASE64_PREFIX}{}\n", STANDARD.encode("tk_secret"));

        let token = decode(&stored).expect("decode");

        assert_eq!(token.expose(), "tk_secret");
    }

    #[test]
    fn takes_an_unprefixed_value_verbatim() {
        let token = decode("tk_secret\n").expect("decode");

        assert_eq!(token.expose(), "tk_secret");
    }

    // `decode` returns an `ApiToken`, which has no `Debug`, so the failing cases
    // are matched rather than unwrapped.
    #[test]
    fn rejects_a_prefixed_value_that_is_not_base64() {
        let Err(err) = decode(&format!("{BASE64_PREFIX}not base64!")) else {
            panic!("a value that is not base64 must not decode");
        };

        assert!(matches!(err, KeychainError::Base64), "{err:?}");
    }

    #[test]
    fn rejects_an_empty_value() {
        for stored in ["", "\n", BASE64_PREFIX] {
            let Err(err) = decode(stored) else {
                panic!("{stored:?} must not decode");
            };
            assert!(matches!(err, KeychainError::Empty), "{stored:?}: {err:?}");
        }
    }

    #[tokio::test]
    async fn reports_an_absent_item_as_none() {
        struct Absent;
        impl TokenSource for Absent {
            fn read(&self) -> ReadFuture<'_> {
                Box::pin(async { Ok(None) })
            }
        }

        assert!(load(&Absent, READ_TIMEOUT).await.expect("load").is_none());
    }

    #[tokio::test]
    async fn gives_up_on_a_source_that_does_not_answer() {
        struct NeverAnswers;
        impl TokenSource for NeverAnswers {
            fn read(&self) -> ReadFuture<'_> {
                Box::pin(async {
                    std::future::pending::<()>().await;
                    unreachable!()
                })
            }
        }

        let timeout = Duration::from_millis(20);
        let Err(err) = load(&NeverAnswers, timeout).await else {
            panic!("a source that never answers must not produce a token");
        };

        assert!(
            matches!(err, KeychainError::Timeout(d) if d == timeout),
            "{err:?}"
        );
    }
}
