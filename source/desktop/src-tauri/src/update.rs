//! Keeping the app on the desktop component version its daemon's release names.
//!
//! One pass is four steps, and each one is a gate on the next:
//!
//! 1. ask the daemon which release it runs, over the authenticated version
//!    endpoint — the same bearer token the health poll uses;
//! 2. fetch that release's bill of materials from the publishing host, together
//!    with the detached signature beside it;
//! 3. verify that signature with [`crate::bomsig`], because the document names
//!    the archive this app will install and run;
//! 4. hand the verified bytes to [`crate::pairing`], which says whether to
//!    install a component version and which archive carries it.
//!
//! Only then is the Tauri updater involved, and only with a decision already
//! made: it downloads the named archive, verifies it against the minisign public
//! key committed in `tauri.conf.json`, swaps the bundle, and the app relaunches.
//! Two independent signatures therefore stand between a published byte and a
//! running one — the publisher's over the document, the updater key's over the
//! archive.
//!
//! The updater takes its target from an HTTP endpoint and from nowhere else, so
//! the decision reaches it as a document served from a loopback socket this
//! process owns for one fetch (see [`install`]). That is why
//! `plugins.updater.dangerousInsecureTransportProtocol` is set in
//! `tauri.conf.json`, which JSON gives no room to explain: the flag governs only
//! which endpoint *schemes* the plugin accepts, and the sole endpoint this app
//! ever builds is `http://127.0.0.1:<ephemeral>/`. `plugins.updater.endpoints`
//! is deliberately absent, so no endpoint is ever taken from configuration.

use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use tauri::AppHandle;
use tauri_plugin_updater::UpdaterExt;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

use crate::bomsig;
use crate::keychain::{self, ApiToken, TokenSource};
use crate::pairing::{self, Arch, Decision, Reason};

/// Where release documents are published.
///
/// Source of truth: `DefaultBaseURL` in
/// `source/daemon/internal/platform/release/release.go`. The daemon and the app
/// read the same documents from the same host.
pub const RELEASE_BASE_URL: &str = "https://get.tumika.org";

/// Suffix of the detached signature published beside a document, as
/// `signatureSuffix` in the daemon's `platform/release` spells it.
const SIGNATURE_SUFFIX: &str = ".sig";

/// How often the daemon is asked which release it runs.
///
/// A release changes at most a few times a day, and pairing only matters within
/// minutes of the daemon having moved, so this is far rarer than the health
/// poll.
pub const CHECK_INTERVAL: Duration = Duration::from_secs(15 * 60);

/// Bound on one document fetch. Generous, because it competes with nothing: no
/// user is waiting on it.
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);

/// Bounds on the two documents read from the publishing host, matching
/// `maxBOMBytes` and `maxSignatureBytes` in the daemon's `platform/release`. A
/// host that answers with something unbounded is refused rather than read.
const MAX_BOM_BYTES: usize = 1 << 20;
const MAX_SIGNATURE_BYTES: usize = 4 << 10;

/// How long the endpoint carrying the decision stays up. It is fetched once,
/// immediately, by this process; the bound is what stops a failed check from
/// leaving a listening socket behind.
const MANIFEST_TIMEOUT: Duration = Duration::from_secs(30);

/// How much of a remote message is repeated in a detail line.
const MAX_DETAIL_BYTES: usize = 200;

/// What the app is doing about its own component version.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum UpdateState {
    /// A check is in flight.
    Checking,
    /// The app runs the component version its daemon's release names, or that
    /// release names no app this machine can install.
    UpToDate,
    /// An archive is being downloaded and installed.
    Updating,
    /// The last check or install did not finish. The next one runs on schedule.
    Failed,
}

/// The last thing an update check produced, as a reader receives it.
#[derive(Debug, Clone, Serialize)]
pub struct UpdateStatus {
    pub state: UpdateState,
    /// The desktop component version this build is.
    pub component_version: String,
    /// The component version being installed, present only while `Updating`.
    pub target_component_version: Option<String>,
    /// Why the state is what it is, in one short line. Never a secret, and
    /// clipped, since parts of it come from a remote document.
    pub detail: Option<String>,
    /// Wall-clock milliseconds of this check, so a reader can age it against a
    /// clock of its own.
    pub checked_at_ms: u64,
}

impl UpdateStatus {
    fn new(state: UpdateState, detail: Option<String>) -> Self {
        Self {
            state,
            component_version: running_component_version().to_string(),
            target_component_version: None,
            detail: detail.map(|detail| clip(&detail)),
            checked_at_ms: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .map(|since| u64::try_from(since.as_millis()).unwrap_or(u64::MAX))
                .unwrap_or_default(),
        }
    }

    fn installing(component_version: &str) -> Self {
        Self {
            target_component_version: Some(component_version.to_string()),
            ..Self::new(
                UpdateState::Updating,
                Some(format!("installing {component_version}")),
            )
        }
    }
}

/// The latest update check, shared with whatever renders it.
///
/// `None` until the first check has finished, exactly as the daemon status is
/// before its first poll.
#[derive(Clone, Default)]
pub struct SharedStatus(Arc<Mutex<Option<UpdateStatus>>>);

impl SharedStatus {
    pub fn get(&self) -> Option<UpdateStatus> {
        self.0.lock().expect("update status lock").clone()
    }

    fn set(&self, status: UpdateStatus) {
        *self.0.lock().expect("update status lock") = Some(status);
    }
}

/// This build's own desktop component version.
///
/// It is the crate's version, which `scripts/desktop-version.sh` holds equal to
/// `tauri.conf.json`'s — the one the updater compares against, and the one the
/// release's bill of materials names.
pub const fn running_component_version() -> &'static str {
    env!("CARGO_PKG_VERSION")
}

/// Why a pass produced no decision, or could not act on one.
///
/// A [`Reason`] is a release legitimately having no app for this machine; every
/// variant here is something that went wrong instead.
#[derive(Debug, thiserror::Error)]
pub enum UpdateError {
    #[error("the daemon could not be asked which release it runs: {0}")]
    Daemon(String),

    #[error("{0} could not be read: {1}")]
    Document(String, String),

    #[error("{0}")]
    Pairing(#[from] pairing::PairingError),

    #[error("the bill of materials for release {0} is not the publisher's: {1}")]
    Signature(String, bomsig::BomSignatureError),

    #[error("no app is published for this machine's architecture")]
    UnsupportedArch,

    #[error("the update could not be installed: {0}")]
    Install(String),
}

/// A document and the detached signature published beside it.
pub struct SignedDocument {
    pub body: Vec<u8>,
    /// The text of the `.sig` file, surrounding whitespace included.
    pub signature: String,
}

/// What a [`ReleaseSource`] read returns.
///
/// Boxed rather than an `async fn` in the trait, because the source is held
/// behind `dyn` and an `async fn` in a trait is not dyn-compatible.
pub type SourceFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T, UpdateError>> + Send + 'a>>;

/// Everything a pass reads from outside this process.
///
/// A trait so the decision can be exercised against documents a test writes,
/// with no daemon, no Keychain and no publishing host.
pub trait ReleaseSource: Send + Sync {
    /// The release label the daemon reports about itself.
    fn daemon_release(&self) -> SourceFuture<'_, String>;

    /// One release's own bill of materials. `label` has been validated against
    /// the release-label pattern before it arrives here.
    fn release_bom<'a>(&'a self, label: &'a str) -> SourceFuture<'a, SignedDocument>;
}

/// Runs the three reading steps and returns what the app must do.
///
/// Touches nothing but `source`, so the whole decision is testable: the Tauri
/// updater only ever sees a [`Decision::Update`] this produced.
pub async fn check(
    source: &dyn ReleaseSource,
    running_component_version: &str,
    arch: Arch,
) -> Result<Decision, UpdateError> {
    let release = source.daemon_release().await?;
    // The label goes into a URL, and it arrived over the network. Validated here
    // as well as inside `pairing::decide`, because the fetch happens first.
    pairing::validate_release_label(&release)?;

    let document = source.release_bom(&release).await?;
    bomsig::verify(&document.body, &document.signature, bomsig::release_keys())
        .map_err(|err| UpdateError::Signature(release.clone(), err))?;

    Ok(pairing::decide(
        &release,
        &document.body,
        running_component_version,
        arch,
    )?)
}

/// Starts the loop that keeps this app paired with its daemon.
///
/// The first pass runs immediately: an app launched after its daemon moved to
/// another release is the case pairing exists for.
pub fn start(app: AppHandle, status: SharedStatus, base_url: &str) -> Result<(), UpdateError> {
    let source = HttpSource::new(Box::new(crate::keychain::SecurityCli), base_url)?;

    tauri::async_runtime::spawn(async move {
        loop {
            pass(&app, &source, &status).await;
            tokio::time::sleep(CHECK_INTERVAL).await;
        }
    });

    Ok(())
}

/// One pass: decide, publish what was decided, and install when there is
/// something to install.
///
/// A failure is published and the loop continues. Nothing here is fatal to the
/// app: an app that cannot update itself is still an app that reports on its
/// daemon.
async fn pass(app: &AppHandle, source: &dyn ReleaseSource, status: &SharedStatus) {
    let arch = match Arch::current() {
        Some(arch) => arch,
        None => {
            status.set(UpdateStatus::new(
                UpdateState::Failed,
                Some(UpdateError::UnsupportedArch.to_string()),
            ));
            return;
        }
    };

    status.set(UpdateStatus::new(UpdateState::Checking, None));

    let decision = match check(source, running_component_version(), arch).await {
        Ok(decision) => decision,
        Err(err) => {
            status.set(UpdateStatus::new(
                UpdateState::Failed,
                Some(err.to_string()),
            ));
            return;
        }
    };

    let (component_version, url, signature) = match decision {
        Decision::NothingToDo { reason } => {
            status.set(UpdateStatus::new(
                UpdateState::UpToDate,
                detail_for(&reason),
            ));
            return;
        }
        Decision::Update {
            component_version,
            url,
            signature,
        } => (component_version, url, signature),
    };

    status.set(UpdateStatus::installing(&component_version));

    match install(app, &component_version, &url, &signature).await {
        // The install replaced the bundle this process runs out of, so the code
        // on disk is no longer the code in memory. Relaunching is what closes
        // that gap, and it never returns.
        Ok(()) => app.restart(),
        Err(err) => status.set(UpdateStatus::new(
            UpdateState::Failed,
            Some(err.to_string()),
        )),
    }
}

/// What a reader is told about a release that calls for no install.
///
/// Already being paired is the resting state and needs no explanation; the other
/// reasons are things the reader would otherwise wait for in silence.
fn detail_for(reason: &Reason) -> Option<String> {
    match reason {
        Reason::AlreadyPaired { .. } => None,
        other => Some(other.to_string()),
    }
}

/// Downloads and installs the archive the decision named.
///
/// The Tauri updater takes its target from an HTTP endpoint and from nowhere
/// else, so the decision is handed to it as a document served from a loopback
/// socket this process owns for the duration of one fetch. That is what keeps
/// the app from trusting a second, unauthenticated fetch from the publishing
/// host: every field of that document comes from the bill of materials whose
/// publisher signature [`check`] already verified.
async fn install(
    app: &AppHandle,
    component_version: &str,
    url: &str,
    signature: &str,
) -> Result<(), UpdateError> {
    let (endpoint, server) = serve_once(manifest(component_version, url, signature))
        .await
        .map_err(|err| UpdateError::Install(err.to_string()))?;

    let result = drive_updater(app, endpoint).await;
    // The endpoint is fetched once and is of no use afterwards, on either path.
    server.abort();
    result
}

async fn drive_updater(app: &AppHandle, endpoint: reqwest::Url) -> Result<(), UpdateError> {
    let updater = app
        .updater_builder()
        .endpoints(vec![endpoint])
        .and_then(|builder| {
            builder
                // The bill of materials chooses, so a component version that
                // merely differs from the running one is installed — which is
                // what pulls the app back when its daemon moves to an older
                // release. The plugin's own rule is "strictly newer".
                .version_comparator(|current, release| release.version != current)
                .timeout(REQUEST_TIMEOUT)
                .build()
        })
        .map_err(|err| UpdateError::Install(err.to_string()))?;

    let update = updater
        .check()
        .await
        .map_err(|err| UpdateError::Install(err.to_string()))?
        .ok_or_else(|| UpdateError::Install("the updater found nothing to install".to_string()))?;

    update
        .download_and_install(|_, _| {}, || {})
        .await
        .map_err(|err| UpdateError::Install(err.to_string()))
}

/// The updater's own document, in the dynamic shape its parser accepts: the
/// component version to announce, the archive, and the signature over it.
///
/// Built with a JSON encoder rather than a format string, so no field of a
/// published document can spill out of the value it belongs to.
fn manifest(component_version: &str, url: &str, signature: &str) -> String {
    serde_json::json!({
        "version": component_version,
        "url": url,
        "signature": signature,
    })
    .to_string()
}

/// Serves `body` as JSON to one caller on a loopback port, and reports the URL
/// it is at.
///
/// The body carries no secret — a published URL and a published signature — so
/// the exposure of the ephemeral port is that another local process could take
/// the one connection on offer, which costs this pass its update and nothing
/// else.
async fn serve_once(body: String) -> std::io::Result<(reqwest::Url, tokio::task::JoinHandle<()>)> {
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let endpoint = reqwest::Url::parse(&format!("http://{}/", listener.local_addr()?))
        .map_err(|err| std::io::Error::other(err.to_string()))?;

    let served = tokio::spawn(async move {
        let _ = tokio::time::timeout(MANIFEST_TIMEOUT, async move {
            let Ok((mut stream, _)) = listener.accept().await else {
                return;
            };
            // The request is read and discarded: this endpoint answers exactly
            // one document, whatever was asked for.
            let mut discarded = [0u8; 2048];
            let _ = stream.read(&mut discarded).await;

            let response = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                body.len()
            );
            let _ = stream.write_all(response.as_bytes()).await;
            let _ = stream.flush().await;
        })
        .await;
    });

    Ok((endpoint, served))
}

/// Reads the daemon's release over its API, and the documents from the
/// publishing host.
struct HttpSource {
    tokens: Box<dyn TokenSource>,
    http: reqwest::Client,
    version_url: String,
    release_base_url: String,
}

/// The half of `GET /v1/version` pairing needs.
///
/// The endpoint is behind the bearer token like every other route, and reports
/// the release label alongside the channel and the build identity; unknown
/// fields are ignored so a later daemon's report still reads.
#[derive(Deserialize)]
struct VersionReport {
    #[serde(default)]
    release: String,
}

impl HttpSource {
    /// `base_url` is the daemon's, as in [`crate::health::DEFAULT_BASE_URL`].
    fn new(tokens: Box<dyn TokenSource>, base_url: &str) -> Result<Self, UpdateError> {
        let http = reqwest::Client::builder()
            .timeout(REQUEST_TIMEOUT)
            // A redirect would carry the bearer token to whatever it named.
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .map_err(|err| UpdateError::Daemon(err.to_string()))?;

        Ok(Self {
            tokens,
            http,
            version_url: format!("{base_url}/v1/version"),
            release_base_url: RELEASE_BASE_URL.to_string(),
        })
    }

    /// Reads a whole document, refusing one past `max` rather than truncating it
    /// into a body whose signature could never verify.
    async fn document(&self, url: &str, max: usize) -> Result<Vec<u8>, UpdateError> {
        let failed = |detail: String| UpdateError::Document(url.to_string(), detail);

        let mut response = self
            .http
            .get(url)
            .send()
            .await
            .map_err(|err| failed(err.to_string()))?;
        if !response.status().is_success() {
            return Err(failed(format!("the host answered {}", response.status())));
        }

        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|err| failed(err.to_string()))?
        {
            body.extend_from_slice(&chunk);
            if body.len() > max {
                return Err(failed(format!("larger than the {max} byte cap")));
            }
        }
        Ok(body)
    }

    async fn token(&self) -> Result<ApiToken, UpdateError> {
        match keychain::load(self.tokens.as_ref(), keychain::READ_TIMEOUT).await {
            Ok(Some(token)) => Ok(token),
            Ok(None) => Err(UpdateError::Daemon(
                "no API token in the login Keychain".to_string(),
            )),
            Err(err) => Err(UpdateError::Daemon(err.to_string())),
        }
    }
}

impl ReleaseSource for HttpSource {
    fn daemon_release(&self) -> SourceFuture<'_, String> {
        Box::pin(async move {
            let token = self.token().await?;
            let response = self
                .http
                .get(&self.version_url)
                .bearer_auth(token.expose())
                .send()
                .await
                .map_err(|err| UpdateError::Daemon(err.to_string()))?;
            if !response.status().is_success() {
                return Err(UpdateError::Daemon(format!(
                    "the daemon answered {}",
                    response.status()
                )));
            }

            let report: VersionReport = response
                .json()
                .await
                .map_err(|err| UpdateError::Daemon(err.to_string()))?;
            if report.release.is_empty() {
                return Err(UpdateError::Daemon(
                    "the daemon reports no release".to_string(),
                ));
            }
            Ok(report.release)
        })
    }

    fn release_bom<'a>(&'a self, label: &'a str) -> SourceFuture<'a, SignedDocument> {
        Box::pin(async move {
            let url = format!("{}/releases/{label}.json", self.release_base_url);
            let body = self.document(&url, MAX_BOM_BYTES).await?;
            let signature = self
                .document(&format!("{url}{SIGNATURE_SUFFIX}"), MAX_SIGNATURE_BYTES)
                .await?;

            Ok(SignedDocument {
                body,
                signature: String::from_utf8(signature)
                    .map_err(|_| UpdateError::Document(url, "not valid UTF-8".to_string()))?,
            })
        })
    }
}

/// A message as a detail line repeats it: clipped, so a remote document cannot
/// choose the size of what the app carries around and renders.
fn clip(message: &str) -> String {
    let clipped: String = message.chars().take(MAX_DETAIL_BYTES).collect();
    if clipped.len() < message.len() {
        return format!("{clipped}…");
    }
    clipped
}

#[cfg(test)]
mod tests {
    use p256::ecdsa::signature::Signer;
    use p256::ecdsa::{Signature, SigningKey};
    use tokio::sync::oneshot;

    use base64::engine::general_purpose::STANDARD;
    use base64::Engine;

    use super::*;

    const RELEASE: &str = "2026.09.01";
    const RUNNING: &str = "0.2.0";
    const ARCHIVE_SIGNATURE: &str = "dW50cnVzdGVkIGNvbW1lbnQ6IHNpZ25hdHVyZQo=";

    fn bom(release: &str, component_version: &str) -> String {
        format!(
            r#"{{
              "release": "{release}",
              "channel": "stable",
              "components": {{
                "desktop": {{
                  "version": "{component_version}",
                  "assets": {{
                    "darwin_arm64": {{
                      "url": "https://get.tumika.org/releases/{release}/tumika-desktop_{component_version}_darwin_arm64.app.tar.gz",
                      "sha256": "{digest}",
                      "signature": "{ARCHIVE_SIGNATURE}"
                    }},
                    "darwin_amd64": {{
                      "url": "https://get.tumika.org/releases/{release}/tumika-desktop_{component_version}_darwin_amd64.app.tar.gz",
                      "sha256": "{digest}",
                      "signature": "{ARCHIVE_SIGNATURE}"
                    }}
                  }}
                }}
              }}
            }}"#,
            digest = "0".repeat(64),
        )
    }

    /// The publisher, built from fixed bytes so no key material is committed.
    fn publisher() -> SigningKey {
        SigningKey::from_bytes(&[7u8; 32].into()).expect("a non-zero scalar is a signing key")
    }

    fn sign(key: &SigningKey, body: &[u8]) -> String {
        let signature: Signature = key.sign(body);
        format!("{}\n", STANDARD.encode(signature.to_der().as_bytes()))
    }

    /// A source answering from documents a test wrote, with the publisher's
    /// signature over exactly the bytes it hands back.
    struct FakeSource {
        release: Result<String, String>,
        body: Vec<u8>,
        signature: String,
        /// Every label the source was asked for, so a test can assert what
        /// reached the URL.
        asked: Mutex<Vec<String>>,
    }

    impl FakeSource {
        fn new(release: &str, body: &str, key: &SigningKey) -> Self {
            Self {
                release: Ok(release.to_string()),
                body: body.as_bytes().to_vec(),
                signature: sign(key, body.as_bytes()),
                asked: Mutex::new(Vec::new()),
            }
        }
    }

    impl ReleaseSource for FakeSource {
        fn daemon_release(&self) -> SourceFuture<'_, String> {
            let answer = self.release.clone();
            Box::pin(async move { answer.map_err(UpdateError::Daemon) })
        }

        fn release_bom<'a>(&'a self, label: &'a str) -> SourceFuture<'a, SignedDocument> {
            self.asked.lock().expect("asked").push(label.to_string());
            Box::pin(async move {
                Ok(SignedDocument {
                    body: self.body.clone(),
                    signature: self.signature.clone(),
                })
            })
        }
    }

    /// `check` verifies against the compiled-in keys, which no test can sign
    /// for, so the signature step is exercised through `bomsig` directly and the
    /// orchestration is driven with the same shape.
    async fn check_with(source: &FakeSource, keys: &[p256::ecdsa::VerifyingKey]) -> Decision {
        let release = source.daemon_release().await.expect("a release");
        pairing::validate_release_label(&release).expect("a valid label");
        let document = source.release_bom(&release).await.expect("a document");
        bomsig::verify(&document.body, &document.signature, keys).expect("the publisher's");
        pairing::decide(&release, &document.body, RUNNING, Arch::Arm64).expect("a decision")
    }

    #[tokio::test]
    async fn a_release_naming_another_component_version_is_an_update() {
        let key = publisher();
        let source = FakeSource::new(RELEASE, &bom(RELEASE, "0.3.0"), &key);

        let decision = check_with(&source, &[*key.verifying_key()]).await;

        assert_eq!(
            decision,
            Decision::Update {
                component_version: "0.3.0".to_string(),
                url: format!(
                    "https://get.tumika.org/releases/{RELEASE}/tumika-desktop_0.3.0_darwin_arm64.app.tar.gz"
                ),
                signature: ARCHIVE_SIGNATURE.to_string(),
            }
        );
        assert_eq!(*source.asked.lock().expect("asked"), vec![RELEASE]);
    }

    #[tokio::test]
    async fn a_daemon_that_cannot_be_asked_fails_the_check() {
        struct Unreachable;
        impl ReleaseSource for Unreachable {
            fn daemon_release(&self) -> SourceFuture<'_, String> {
                Box::pin(async { Err(UpdateError::Daemon("connection refused".to_string())) })
            }
            fn release_bom<'a>(&'a self, _label: &'a str) -> SourceFuture<'a, SignedDocument> {
                panic!("no document is fetched when the daemon did not answer");
            }
        }

        let err = check(&Unreachable, RUNNING, Arch::Arm64)
            .await
            .expect_err("an unreachable daemon must fail the check");

        assert!(matches!(err, UpdateError::Daemon(_)), "{err:?}");
    }

    #[tokio::test]
    async fn a_label_the_daemon_reports_is_validated_before_it_reaches_a_url() {
        struct Hostile;
        impl ReleaseSource for Hostile {
            fn daemon_release(&self) -> SourceFuture<'_, String> {
                Box::pin(async { Ok("../../etc/passwd".to_string()) })
            }
            fn release_bom<'a>(&'a self, _label: &'a str) -> SourceFuture<'a, SignedDocument> {
                panic!("a label outside the published shapes must never be fetched");
            }
        }

        let err = check(&Hostile, RUNNING, Arch::Arm64)
            .await
            .expect_err("a hostile label must fail the check");

        assert!(
            matches!(
                err,
                UpdateError::Pairing(pairing::PairingError::InvalidReleaseLabel(_))
            ),
            "{err:?}"
        );
    }

    #[tokio::test]
    async fn a_document_signed_by_a_stranger_is_never_paired_against() {
        let key = publisher();
        let stranger = SigningKey::from_bytes(&[9u8; 32].into()).expect("a signing key");
        let document = bom(RELEASE, "0.3.0");
        let source = FakeSource::new(RELEASE, &document, &stranger);

        let err = bomsig::verify(&source.body, &source.signature, &[*key.verifying_key()])
            .expect_err("a stranger's signature must be refused");

        assert_eq!(err, bomsig::BomSignatureError::NoMatchingKey(1));
    }

    #[tokio::test]
    async fn the_compiled_in_keys_refuse_a_document_a_test_signed() {
        let key = publisher();
        let document = bom(RELEASE, "0.3.0");
        let source = FakeSource::new(RELEASE, &document, &key);

        let err = check(&source, RUNNING, Arch::Arm64)
            .await
            .expect_err("only the publisher's key verifies");

        assert!(matches!(err, UpdateError::Signature(_, _)), "{err:?}");
    }

    #[test]
    fn the_manifest_is_the_shape_the_updater_parses() {
        let encoded = manifest("0.3.0", "https://get.tumika.org/a.app.tar.gz", "c2ln");

        let parsed: serde_json::Value = serde_json::from_str(&encoded).expect("json");

        assert_eq!(parsed["version"], "0.3.0");
        assert_eq!(parsed["url"], "https://get.tumika.org/a.app.tar.gz");
        assert_eq!(parsed["signature"], "c2ln");
    }

    #[test]
    fn a_document_cannot_spill_out_of_the_field_it_belongs_to() {
        let encoded = manifest("0.3.0", r#"https://x/"," "#, "c2ln");

        let parsed: serde_json::Value = serde_json::from_str(&encoded).expect("json");

        assert_eq!(parsed["url"], r#"https://x/"," "#);
        assert_eq!(parsed["signature"], "c2ln");
    }

    #[tokio::test]
    async fn the_endpoint_serves_the_decision_once_on_loopback() {
        let body = manifest("0.3.0", "https://get.tumika.org/a.app.tar.gz", "c2ln");
        let (endpoint, served) = serve_once(body.clone()).await.expect("an endpoint");

        assert_eq!(endpoint.scheme(), "http");
        assert_eq!(endpoint.host_str(), Some("127.0.0.1"));

        let fetched: serde_json::Value = reqwest::Client::new()
            .get(endpoint)
            .send()
            .await
            .expect("fetch")
            .json()
            .await
            .expect("json");

        assert_eq!(fetched["version"], "0.3.0");
        served.abort();
    }

    /// The version endpoint is behind the bearer token like every other route.
    #[tokio::test]
    async fn the_daemons_release_is_read_with_the_token_as_a_bearer_credential() {
        const TOKEN: &str = "tk_a_test_token";

        struct FakeKeychain;
        impl TokenSource for FakeKeychain {
            fn read(&self) -> keychain::ReadFuture<'_> {
                Box::pin(async { Ok(Some(TOKEN.to_string())) })
            }
        }

        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let addr = listener.local_addr().expect("addr");
        let (tx, rx) = oneshot::channel();
        tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.expect("accept");
            let mut buf = [0u8; 2048];
            let read = stream.read(&mut buf).await.expect("read");
            let _ = tx.send(String::from_utf8_lossy(&buf[..read]).to_string());
            let body = format!(r#"{{"version":"1.4.0","release":"{RELEASE}","channel":"stable"}}"#);
            let response = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                body.len()
            );
            stream.write_all(response.as_bytes()).await.expect("write");
            stream.flush().await.expect("flush");
        });

        let source =
            HttpSource::new(Box::new(FakeKeychain), &format!("http://{addr}")).expect("source");

        let release = source.daemon_release().await.expect("a release");

        assert_eq!(release, RELEASE);
        let request = rx.await.expect("request");
        assert!(
            request.to_lowercase().contains("authorization: bearer"),
            "the token must reach the daemon as a bearer credential"
        );
        assert!(request.contains(TOKEN), "the request carries the token");
    }

    #[tokio::test]
    async fn a_daemon_that_reports_no_release_fails_the_check() {
        struct AbsentKeychain;
        impl TokenSource for AbsentKeychain {
            fn read(&self) -> keychain::ReadFuture<'_> {
                Box::pin(async { Ok(None) })
            }
        }

        let source =
            HttpSource::new(Box::new(AbsentKeychain), "http://127.0.0.1:1").expect("source");

        let err = source
            .daemon_release()
            .await
            .expect_err("no token is no release");

        assert!(matches!(err, UpdateError::Daemon(_)), "{err:?}");
    }

    #[test]
    fn the_status_carries_no_secret_and_clips_what_it_repeats() {
        let status = UpdateStatus::new(UpdateState::Failed, Some("x".repeat(4096)));

        let detail = status.detail.clone().expect("a detail");
        assert!(detail.chars().count() <= MAX_DETAIL_BYTES + 1, "{detail}");

        let encoded = serde_json::to_string(&status).expect("serialise");
        assert!(encoded.contains(r#""state":"failed""#), "{encoded}");
    }

    #[test]
    fn being_paired_needs_no_explanation_and_waiting_does() {
        assert_eq!(
            detail_for(&Reason::AlreadyPaired {
                component_version: RUNNING.to_string(),
            }),
            None
        );
        assert_eq!(
            detail_for(&Reason::NoDesktopComponent),
            Some(Reason::NoDesktopComponent.to_string())
        );
    }

    #[test]
    fn the_running_component_version_is_the_crates_own() {
        assert_eq!(running_component_version(), env!("CARGO_PKG_VERSION"));
    }
}
