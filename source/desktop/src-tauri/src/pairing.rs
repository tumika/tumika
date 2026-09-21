//! Which desktop component version this app must run, and where to get it.
//!
//! Pairing is the rule that the app runs exactly the desktop component version
//! named by the bill of materials of the release its daemon runs. It holds in
//! both directions: a daemon that moves to an older release pulls the app back
//! with it, so a component version that merely *differs* from the running one is
//! an update. Nothing here compares versions for order — the bill of materials
//! chooses, not the app.
//!
//! [`decide`] is the whole decision and touches nothing outside its arguments.
//! The document it is handed is the one [`crate::bomsig::verify`] accepted:
//! these bytes name an archive the app will download and run, so they are read
//! only after the publisher's signature over them verifies.

use std::collections::HashMap;
use std::sync::OnceLock;

use regex::Regex;
use serde::Deserialize;

/// Component name the desktop app is published under.
///
/// Source of truth: `DesktopComponent` in
/// `source/daemon/internal/platform/release/release.go`. The publisher and the
/// app must spell it identically.
const DESKTOP_COMPONENT: &str = "desktop";

/// The operating-system half of a platform key. The app ships for macOS alone.
const GOOS: &str = "darwin";

/// The shape a release label may have.
///
/// Source of truth: `releaseLabelPattern` in
/// `source/daemon/internal/platform/release/bom.go`, which this string is a copy
/// of; `the_release_label_pattern_matches_the_daemons` fails when the two drift.
/// A second spelling would let the app fetch a label the daemon would refuse, or
/// refuse one it publishes.
///
/// The pattern is strict because the label reaches a URL path: it arrives from
/// the daemon over HTTP, and no character that could escape a path segment
/// matches.
const RELEASE_LABEL_PATTERN: &str =
    r"^(?:[0-9]{4}\.[0-9]{2}\.[0-9]{2}(?:-beta\.[0-9]{1,6})?|edge\.[0-9]{1,10})$";

/// An asset's detached updater signature is a minisign document in base64: a few
/// hundred bytes. The cap bounds what a published document can make the app
/// carry around.
///
/// Counterpart of `validateAssetSignature` in
/// `source/daemon/internal/platform/release/bom.go`.
const MAX_ASSET_SIGNATURE_BYTES: usize = 4 << 10;

/// How much of an offending value an error message repeats.
const MAX_QUOTED_BYTES: usize = 64;

/// The architectures a release publishes the app for.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Arch {
    Arm64,
    Amd64,
}

impl Arch {
    /// The architecture this build runs on, or `None` on one no release
    /// publishes the app for.
    pub fn current() -> Option<Self> {
        match std::env::consts::ARCH {
            "aarch64" => Some(Self::Arm64),
            "x86_64" => Some(Self::Amd64),
            _ => None,
        }
    }

    /// The key an asset is published under: Go's `GOOS_GOARCH`.
    pub fn platform_key(self) -> String {
        format!("{GOOS}_{}", self.goarch())
    }

    /// Go's name for the architecture, which is the half of a platform key that
    /// differs between builds.
    pub const fn goarch(self) -> &'static str {
        match self {
            Self::Arm64 => "arm64",
            Self::Amd64 => "amd64",
        }
    }
}

/// What the app does about the release its daemon is running.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Decision {
    /// The app stays as it is, and why.
    NothingToDo { reason: Reason },
    /// The app installs this desktop component version.
    Update {
        component_version: String,
        /// Where the updater archive is published. A component the release
        /// carried over points at the earlier release's asset; the URL is used
        /// exactly as the document gives it.
        url: String,
        /// The updater signature the archive is verified against: base64 of the
        /// minisign document beside it, opaque here.
        signature: String,
    },
}

/// Why there is nothing to do.
///
/// Every one of these is a state a published release can legitimately be in, so
/// each is an outcome rather than an error: the app says what it is waiting for
/// and polls again.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Reason {
    /// The app already runs the component version the release names.
    AlreadyPaired { component_version: String },
    /// The release ships no desktop component at all.
    NoDesktopComponent,
    /// The release ships the component, but not for this machine.
    NoAssetForPlatform { platform: String },
    /// The asset carries no updater signature the app could verify it with.
    UnusableAssetSignature { platform: String },
}

impl std::fmt::Display for Reason {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::AlreadyPaired { component_version } => {
                write!(f, "the app is already at {component_version}")
            }
            Self::NoDesktopComponent => {
                write!(f, "the daemon's release ships no desktop app")
            }
            Self::NoAssetForPlatform { platform } => {
                write!(f, "the daemon's release publishes no {platform} app")
            }
            Self::UnusableAssetSignature { platform } => {
                write!(
                    f,
                    "the {platform} app is published without a usable signature"
                )
            }
        }
    }
}

/// A document that cannot be acted on at all.
///
/// Unlike a [`Reason`], each of these says the app was handed something it must
/// not follow: a label it will not put in a URL, a document it cannot read, or a
/// document describing a release other than the one that was asked for.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum PairingError {
    #[error("{0} is not a release label")]
    InvalidReleaseLabel(String),

    #[error("the bill of materials is malformed: {0}")]
    MalformedBom(String),

    #[error("the bill of materials describes release {found}, not {expected}")]
    ReleaseMismatch { expected: String, found: String },
}

/// What the app must do, given the release its daemon runs.
///
/// `bom_json` is the release's own bill of materials, exactly as published;
/// `running_component_version` is this build's own desktop component version.
pub fn decide(
    daemon_release: &str,
    bom_json: &[u8],
    running_component_version: &str,
    arch: Arch,
) -> Result<Decision, PairingError> {
    validate_release_label(daemon_release)?;

    let bom: Bom = serde_json::from_slice(bom_json)
        .map_err(|err| PairingError::MalformedBom(err.to_string()))?;

    // The document must be the one that was asked for. A release serving another
    // release's bill of materials would pair the app against a release its
    // daemon is not running.
    validate_release_label(&bom.release)?;
    if bom.release != daemon_release {
        return Err(PairingError::ReleaseMismatch {
            expected: quoted(daemon_release),
            found: quoted(&bom.release),
        });
    }

    let Some(component) = bom.components.get(DESKTOP_COMPONENT) else {
        return Ok(nothing(Reason::NoDesktopComponent));
    };

    let component_version = component.version.trim_start_matches('v');
    if component_version.is_empty() {
        return Err(PairingError::MalformedBom(format!(
            "component {DESKTOP_COMPONENT} of release {} names no version",
            quoted(&bom.release)
        )));
    }
    // Any component version that differs is an update, in either direction.
    if component_version == running_component_version.trim_start_matches('v') {
        return Ok(nothing(Reason::AlreadyPaired {
            component_version: component_version.to_string(),
        }));
    }

    let platform = arch.platform_key();
    let Some(asset) = component.assets.get(&platform) else {
        return Ok(nothing(Reason::NoAssetForPlatform { platform }));
    };
    if !usable_signature(&asset.signature) {
        return Ok(nothing(Reason::UnusableAssetSignature { platform }));
    }
    validate_asset_url(&asset.url)?;

    Ok(Decision::Update {
        component_version: component_version.to_string(),
        url: asset.url.clone(),
        signature: asset.signature.trim().to_string(),
    })
}

/// Reports whether a label may be used.
///
/// Call it on any label that reaches a URL, including the one the daemon reports
/// about itself — which is a string an attacker who can influence a build
/// controls.
pub fn validate_release_label(label: &str) -> Result<(), PairingError> {
    static PATTERN: OnceLock<Regex> = OnceLock::new();

    let pattern = PATTERN.get_or_init(|| {
        Regex::new(RELEASE_LABEL_PATTERN).expect("the release label pattern is a valid regex")
    });
    if pattern.is_match(label) {
        return Ok(());
    }
    Err(PairingError::InvalidReleaseLabel(quoted(label)))
}

const fn nothing(reason: Reason) -> Decision {
    Decision::NothingToDo { reason }
}

/// Whether an asset's signature is one the updater could work with.
///
/// The format belongs to the updater, which parses it; this refuses only what
/// cannot be a signature at all — an absent one, a blank one, an unbounded one,
/// and one carrying control characters a published document never needs.
fn usable_signature(signature: &str) -> bool {
    let trimmed = signature.trim();
    !trimmed.is_empty()
        && signature.len() <= MAX_ASSET_SIGNATURE_BYTES
        && !trimmed.chars().any(char::is_control)
}

/// Holds an asset's URL to an absolute `http(s)` one.
///
/// A relative URL would be resolved against whatever the app happened to fetch
/// the document from, and a `file:` one names something already on the machine.
fn validate_asset_url(url: &str) -> Result<(), PairingError> {
    let parsed = reqwest::Url::parse(url)
        .map_err(|err| PairingError::MalformedBom(format!("unreadable asset url: {err}")))?;
    if !matches!(parsed.scheme(), "http" | "https") || !parsed.has_host() {
        return Err(PairingError::MalformedBom(format!(
            "{} is not an absolute http(s) url",
            quoted(url)
        )));
    }
    Ok(())
}

/// A value as an error message repeats it: quoted, escaped, and clipped, so a
/// hostile document cannot choose the size or the control characters of a line
/// the app writes.
fn quoted(value: &str) -> String {
    let clipped: String = value.chars().take(MAX_QUOTED_BYTES).collect();
    if clipped.len() < value.len() {
        return format!("{clipped:?}…");
    }
    format!("{clipped:?}")
}

/// The half of a bill of materials pairing reads.
///
/// Unknown fields are accepted deliberately, as the daemon's parser accepts
/// them: an app must be able to read a document written by a later publisher in
/// order to update itself past the release that added a field.
#[derive(Debug, Deserialize)]
struct Bom {
    release: String,
    #[serde(default)]
    components: HashMap<String, Component>,
}

#[derive(Debug, Deserialize)]
struct Component {
    #[serde(default)]
    version: String,
    #[serde(default)]
    assets: HashMap<String, Asset>,
}

#[derive(Debug, Deserialize)]
struct Asset {
    #[serde(default)]
    url: String,
    /// The text of the `.sig` beside the archive. Absent for an asset published
    /// without one, which the daemon's own are.
    #[serde(default)]
    signature: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    const RELEASE: &str = "2026.09.01";
    const RUNNING: &str = "0.2.0";
    const SIGNATURE: &str = "dW50cnVzdGVkIGNvbW1lbnQ6IHNpZ25hdHVyZQo=";

    /// A release publishing the desktop app for both architectures.
    fn bom(release: &str, component_version: &str) -> String {
        format!(
            r#"{{
              "release": "{release}",
              "channel": "stable",
              "published_at": "2026-09-01T10:00:00Z",
              "components": {{
                "daemon": {{
                  "version": "1.4.0",
                  "assets": {{
                    "darwin_arm64": {{
                      "url": "https://get.tumika.org/releases/{release}/tumika_1.4.0_darwin_arm64",
                      "sha256": "{digest}"
                    }}
                  }}
                }},
                "desktop": {{
                  "version": "{component_version}",
                  "assets": {{
                    "darwin_arm64": {{
                      "url": "https://get.tumika.org/releases/{release}/tumika-desktop_{component_version}_darwin_arm64.app.tar.gz",
                      "sha256": "{digest}",
                      "signature": "{SIGNATURE}"
                    }},
                    "darwin_amd64": {{
                      "url": "https://get.tumika.org/releases/{release}/tumika-desktop_{component_version}_darwin_amd64.app.tar.gz",
                      "sha256": "{digest}",
                      "signature": "{SIGNATURE}"
                    }}
                  }}
                }}
              }}
            }}"#,
            digest = "0".repeat(64),
        )
    }

    fn decision(bom: &str) -> Decision {
        decide(RELEASE, bom.as_bytes(), RUNNING, Arch::Arm64).expect("a decision")
    }

    fn update(component_version: &str, platform: &str) -> Decision {
        Decision::Update {
            component_version: component_version.to_string(),
            url: format!(
                "https://get.tumika.org/releases/{RELEASE}/tumika-desktop_{component_version}_{platform}.app.tar.gz"
            ),
            signature: SIGNATURE.to_string(),
        }
    }

    #[test]
    fn the_component_version_the_app_runs_is_nothing_to_do() {
        assert_eq!(
            decision(&bom(RELEASE, RUNNING)),
            nothing(Reason::AlreadyPaired {
                component_version: RUNNING.to_string(),
            })
        );
    }

    #[test]
    fn a_leading_v_is_the_same_component_version() {
        assert_eq!(
            decision(&bom(RELEASE, "v0.2.0")),
            nothing(Reason::AlreadyPaired {
                component_version: RUNNING.to_string(),
            })
        );
    }

    #[test]
    fn a_later_component_version_is_an_update() {
        assert_eq!(
            decision(&bom(RELEASE, "0.3.0")),
            update("0.3.0", "darwin_arm64")
        );
    }

    #[test]
    fn an_earlier_component_version_is_an_update_too() {
        assert_eq!(
            decision(&bom(RELEASE, "0.1.0")),
            update("0.1.0", "darwin_arm64")
        );
    }

    #[test]
    fn each_architecture_takes_its_own_asset() {
        let document = bom(RELEASE, "0.3.0");

        let decided =
            decide(RELEASE, document.as_bytes(), RUNNING, Arch::Amd64).expect("a decision");

        assert_eq!(decided, update("0.3.0", "darwin_amd64"));
    }

    /// A component the release carried over already points at the earlier
    /// release's asset, so the URL is followed as the document gives it rather
    /// than rebuilt from the release that named it.
    #[test]
    fn a_carried_over_component_keeps_the_url_it_names() {
        let document = format!(
            r#"{{
              "release": "{RELEASE}",
              "channel": "stable",
              "published_at": "2026-09-01T10:00:00Z",
              "components": {{
                "desktop": {{
                  "version": "0.3.0",
                  "from_release": "2026.08.01",
                  "assets": {{
                    "darwin_arm64": {{
                      "url": "https://get.tumika.org/releases/2026.08.01/tumika-desktop_0.3.0_darwin_arm64.app.tar.gz",
                      "sha256": "{digest}",
                      "signature": "{SIGNATURE}"
                    }}
                  }}
                }}
              }}
            }}"#,
            digest = "0".repeat(64),
        );

        assert_eq!(
            decision(&document),
            Decision::Update {
                component_version: "0.3.0".to_string(),
                url: "https://get.tumika.org/releases/2026.08.01/tumika-desktop_0.3.0_darwin_arm64.app.tar.gz"
                    .to_string(),
                signature: SIGNATURE.to_string(),
            }
        );
    }

    #[test]
    fn a_release_without_a_desktop_component_is_nothing_to_do() {
        let document = format!(
            r#"{{
              "release": "{RELEASE}",
              "components": {{
                "daemon": {{
                  "version": "1.4.0",
                  "assets": {{"darwin_arm64": {{"url": "https://get.tumika.org/x", "sha256": "{digest}"}}}}
                }}
              }}
            }}"#,
            digest = "0".repeat(64),
        );

        assert_eq!(decision(&document), nothing(Reason::NoDesktopComponent));
    }

    #[test]
    fn a_component_without_an_asset_for_this_machine_is_nothing_to_do() {
        let document = bom(RELEASE, "0.3.0").replace("darwin_arm64", "darwin_riscv64");

        assert_eq!(
            decision(&document),
            nothing(Reason::NoAssetForPlatform {
                platform: "darwin_arm64".to_string(),
            })
        );
    }

    #[test]
    fn an_asset_without_a_usable_signature_is_nothing_to_do() {
        // The third is the JSON escape for a NUL, which a signature file never
        // carries and a document a shell installer also reads must not smuggle.
        for signature in ["", "   ", r"sig\u0000nature"] {
            let document = bom(RELEASE, "0.3.0").replace(SIGNATURE, signature);

            assert_eq!(
                decision(&document),
                nothing(Reason::UnusableAssetSignature {
                    platform: "darwin_arm64".to_string(),
                }),
                "{signature:?}"
            );
        }
    }

    #[test]
    fn an_asset_whose_signature_is_absent_is_nothing_to_do() {
        let document = format!(
            r#"{{
              "release": "{RELEASE}",
              "components": {{
                "desktop": {{
                  "version": "0.3.0",
                  "assets": {{
                    "darwin_arm64": {{
                      "url": "https://get.tumika.org/releases/{RELEASE}/tumika-desktop_0.3.0_darwin_arm64.app.tar.gz",
                      "sha256": "{digest}"
                    }}
                  }}
                }}
              }}
            }}"#,
            digest = "0".repeat(64),
        );

        assert_eq!(
            decision(&document),
            nothing(Reason::UnusableAssetSignature {
                platform: "darwin_arm64".to_string(),
            })
        );
    }

    #[test]
    fn an_oversized_signature_is_nothing_to_do() {
        let document =
            bom(RELEASE, "0.3.0").replace(SIGNATURE, &"A".repeat(MAX_ASSET_SIGNATURE_BYTES + 1));

        assert_eq!(
            decision(&document),
            nothing(Reason::UnusableAssetSignature {
                platform: "darwin_arm64".to_string(),
            })
        );
    }

    #[test]
    fn a_label_outside_the_published_shapes_never_reaches_a_url() {
        for label in [
            "",
            "../../etc/passwd",
            "2026.09.01/../2026.08.01",
            "2026.09.01 ",
            " 2026.09.01",
            "2026.09.01\n",
            "2026.09.01?x=1",
            "2026.09.01#f",
            "2026.09.01%2f",
            "latest",
            "edge.99999999999",
            "2026.9.1",
            &"9".repeat(4096),
        ] {
            let err = decide(
                label,
                bom(RELEASE, "0.3.0").as_bytes(),
                RUNNING,
                Arch::Arm64,
            )
            .expect_err("the label must be refused");

            assert!(
                matches!(err, PairingError::InvalidReleaseLabel(_)),
                "{label:?}: {err:?}"
            );
        }
    }

    #[test]
    fn the_published_label_shapes_are_accepted() {
        for label in [
            "2026.09.01",
            "2026.12.31-beta.4",
            "edge.1",
            "edge.1234567890",
        ] {
            assert_eq!(validate_release_label(label), Ok(()), "{label:?}");
        }
    }

    #[test]
    fn an_error_message_clips_what_it_repeats() {
        let PairingError::InvalidReleaseLabel(quoted) =
            validate_release_label(&"9".repeat(4096)).expect_err("refused")
        else {
            panic!("a bad label is an invalid release label");
        };

        assert!(quoted.len() < 128, "{quoted}");
    }

    #[test]
    fn a_document_describing_another_release_is_refused() {
        let err = decide(
            RELEASE,
            bom("2026.08.01", "0.3.0").as_bytes(),
            RUNNING,
            Arch::Arm64,
        )
        .expect_err("a substituted document must be refused");

        assert!(
            matches!(err, PairingError::ReleaseMismatch { .. }),
            "{err:?}"
        );
    }

    #[test]
    fn a_document_carrying_a_hostile_label_is_refused() {
        let document = bom(RELEASE, "0.3.0").replace(
            &format!(r#""release": "{RELEASE}""#),
            r#""release": "../../etc/passwd""#,
        );

        let err = decide(RELEASE, document.as_bytes(), RUNNING, Arch::Arm64)
            .expect_err("the document's own label must be refused");

        assert!(
            matches!(err, PairingError::InvalidReleaseLabel(_)),
            "{err:?}"
        );
    }

    #[test]
    fn a_document_that_is_not_a_bill_of_materials_is_refused() {
        for document in ["", "null", "[]", "{", r#"{"release": 7}"#] {
            let err = decide(RELEASE, document.as_bytes(), RUNNING, Arch::Arm64)
                .expect_err("an unreadable document must be refused");

            assert!(
                matches!(err, PairingError::MalformedBom(_)),
                "{document:?}: {err:?}"
            );
        }
    }

    #[test]
    fn a_component_without_a_version_is_refused() {
        let document = bom(RELEASE, "");

        let err = decide(RELEASE, document.as_bytes(), RUNNING, Arch::Arm64)
            .expect_err("a component with no version must be refused");

        assert!(matches!(err, PairingError::MalformedBom(_)), "{err:?}");
    }

    #[test]
    fn an_asset_url_that_is_not_absolute_http_is_refused() {
        for url in [
            "/releases/app.tar.gz",
            "file:///tmp/app.tar.gz",
            "not a url",
        ] {
            let document = bom(RELEASE, "0.3.0").replace(
                &format!("https://get.tumika.org/releases/{RELEASE}/tumika-desktop_0.3.0_darwin_arm64.app.tar.gz"),
                url,
            );

            let err = decide(RELEASE, document.as_bytes(), RUNNING, Arch::Arm64)
                .expect_err("an asset url the app cannot fetch must be refused");

            assert!(
                matches!(err, PairingError::MalformedBom(_)),
                "{url:?}: {err:?}"
            );
        }
    }

    #[test]
    fn a_platform_key_names_the_architecture_the_release_publishes() {
        assert_eq!(Arch::Arm64.platform_key(), format!("{GOOS}_arm64"));
        assert_eq!(Arch::Amd64.platform_key(), format!("{GOOS}_amd64"));
    }

    /// The daemon's pattern is the source of truth. A shape the publisher starts
    /// using and this copy refuses strands every installed app on the release
    /// before it.
    #[test]
    fn the_release_label_pattern_matches_the_daemons() {
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../daemon/internal/platform/release/bom.go"
        );
        let source = std::fs::read_to_string(path)
            .unwrap_or_else(|err| panic!("read the daemon's pattern at {path}: {err}"));

        let theirs = source
            .lines()
            .find_map(|line| {
                line.split_once("releaseLabelPattern = regexp.MustCompile(`")
                    .and_then(|(_, rest)| rest.split_once("`)"))
                    .map(|(pattern, _)| pattern)
            })
            .unwrap_or_else(|| panic!("no releaseLabelPattern found in {path}"));

        assert_eq!(
            theirs, RELEASE_LABEL_PATTERN,
            "the app's release label pattern differs from {path}"
        );
    }
}
