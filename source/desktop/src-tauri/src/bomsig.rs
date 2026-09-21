//! Verifying the detached signature published beside a bill of materials.
//!
//! A bill of materials names the bytes this app will download and run, so it is
//! read only once its own signature verifies against a key compiled in here.
//! That is a check of its own, independent of the updater signature Tauri
//! verifies over the archive: this one says the *document* is the publisher's,
//! the other says the *archive* is.
//!
//! The encoding is the publisher's, and the publisher is the daemon's
//! `platform/release` package: ECDSA P-256 over the SHA-256 of the document's
//! exact bytes, ASN.1 DER encoded and then standard base64 — the contents of the
//! `<doc>.json.sig` file beside it. Nothing re-encodes the document before
//! verification, because the signature covers the bytes as served.

use std::sync::OnceLock;

use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use p256::ecdsa::signature::Verifier;
use p256::ecdsa::{Signature, VerifyingKey};
use p256::pkcs8::DecodePublicKey;

/// The public halves of the keys that may sign a bill of materials.
///
/// Source of truth: `releaseKeyPEMs` in
/// `source/daemon/internal/platform/release/keys.go`. The daemon and the app are
/// separate binaries and each carries its own copy, so the two lists are kept
/// identical by `the_key_list_matches_the_daemons`, which fails when they drift.
///
/// It is a list rather than one key so that a key can be rotated: the release
/// that adds the next key is itself signed by a key already here.
const RELEASE_KEY_PEMS: &[&str] = &[concat!(
    "-----BEGIN PUBLIC KEY-----\n",
    "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEGgmfTHgWRmKZRGo7FrLeAEPcr7y1\n",
    "v7rAq2l1R6qbrMFeBxyiaU4+XvOuP1THEIJjk8y5dqM6zzMgh2LydpLZ/g==\n",
    "-----END PUBLIC KEY-----\n",
)];

/// A detached signature is base64 of an ASN.1 P-256 signature: under a hundred
/// bytes. The cap is what keeps a hostile host from choosing how much this
/// process decodes.
const MAX_SIGNATURE_BYTES: usize = 4 << 10;

/// Why a document was not accepted.
///
/// A tampered body and an unknown signer are the same answer, because neither is
/// distinguishable from the other.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum BomSignatureError {
    /// No signature accompanied the document. A document naming the bytes this
    /// app will run is refused unsigned exactly as it is refused tampered.
    #[error("the bill of materials is unsigned")]
    Unsigned,

    /// The signature is not base64 of an ASN.1 P-256 signature, or is past the
    /// size a signature has.
    #[error("the bill of materials signature is unreadable")]
    Unreadable,

    /// The signature verifies against none of the keys offered. An empty key
    /// list lands here too, so a build that lost its keys accepts nothing.
    #[error("the bill of materials signature matches none of the {0} release key(s)")]
    NoMatchingKey(usize),
}

/// Checks `signature` over `body`.
///
/// `signature` is the text of the detached `.sig` file, surrounding whitespace
/// included. Any key in `keys` verifying is enough: that is what makes rotation
/// possible.
pub fn verify(
    body: &[u8],
    signature: &str,
    keys: &[VerifyingKey],
) -> Result<(), BomSignatureError> {
    let text = signature.trim();
    if text.is_empty() {
        return Err(BomSignatureError::Unsigned);
    }
    if text.len() > MAX_SIGNATURE_BYTES {
        return Err(BomSignatureError::Unreadable);
    }

    let der = STANDARD
        .decode(text)
        .map_err(|_| BomSignatureError::Unreadable)?;
    let signature = Signature::from_der(&der).map_err(|_| BomSignatureError::Unreadable)?;

    // The verifier hashes `body` with SHA-256 itself, which is the digest the
    // publisher signs.
    if keys.iter().any(|key| key.verify(body, &signature).is_ok()) {
        return Ok(());
    }
    Err(BomSignatureError::NoMatchingKey(keys.len()))
}

/// The release-signing keys compiled into this app, in the order above.
///
/// The list is compiled in, so an entry that does not parse to an ECDSA P-256
/// public key is a defect in the binary rather than a condition to recover from:
/// it panics on first use, and `every_release_key_parses` makes that a failing
/// test instead of an app that cannot update itself.
pub fn release_keys() -> &'static [VerifyingKey] {
    static KEYS: OnceLock<Vec<VerifyingKey>> = OnceLock::new();

    KEYS.get_or_init(|| {
        RELEASE_KEY_PEMS
            .iter()
            .enumerate()
            .map(|(i, pem)| {
                VerifyingKey::from_public_key_pem(pem)
                    .unwrap_or_else(|err| panic!("release key {i} is not usable: {err}"))
            })
            .collect()
    })
}

#[cfg(test)]
mod tests {
    use p256::ecdsa::signature::Signer;
    use p256::ecdsa::SigningKey;

    use super::*;

    const BODY: &[u8] = br#"{"release":"2026.09.01","channel":"stable"}"#;

    /// A signer built from fixed bytes, so a test needs no randomness and no key
    /// material is committed. ECDSA signing here is deterministic (RFC 6979), so
    /// the same body always produces the same signature.
    fn signer(seed: u8) -> SigningKey {
        SigningKey::from_bytes(&[seed; 32].into()).expect("a non-zero scalar is a signing key")
    }

    /// The detached file's contents: base64 of the ASN.1 signature, with the
    /// trailing newline the publisher writes.
    fn sign(key: &SigningKey, body: &[u8]) -> String {
        let signature: Signature = key.sign(body);
        format!("{}\n", STANDARD.encode(signature.to_der().as_bytes()))
    }

    fn keys_of(key: &SigningKey) -> Vec<VerifyingKey> {
        vec![*key.verifying_key()]
    }

    #[test]
    fn accepts_a_document_signed_by_a_key_in_the_list() {
        let key = signer(1);

        assert_eq!(verify(BODY, &sign(&key, BODY), &keys_of(&key)), Ok(()));
    }

    #[test]
    fn accepts_a_document_signed_by_any_key_in_the_list() {
        let first = signer(1);
        let second = signer(2);
        let keys = vec![*first.verifying_key(), *second.verifying_key()];

        assert_eq!(verify(BODY, &sign(&second, BODY), &keys), Ok(()));
    }

    #[test]
    fn refuses_a_tampered_body() {
        let key = signer(1);
        let signature = sign(&key, BODY);
        let tampered = br#"{"release":"2026.09.02","channel":"stable"}"#;

        assert_eq!(
            verify(tampered, &signature, &keys_of(&key)),
            Err(BomSignatureError::NoMatchingKey(1))
        );
    }

    #[test]
    fn refuses_a_signature_from_a_key_that_is_not_in_the_list() {
        let publisher = signer(1);
        let stranger = signer(9);

        assert_eq!(
            verify(BODY, &sign(&stranger, BODY), &keys_of(&publisher)),
            Err(BomSignatureError::NoMatchingKey(1))
        );
    }

    #[test]
    fn refuses_every_document_when_there_are_no_keys() {
        let key = signer(1);

        assert_eq!(
            verify(BODY, &sign(&key, BODY), &[]),
            Err(BomSignatureError::NoMatchingKey(0))
        );
    }

    #[test]
    fn refuses_a_signature_that_is_not_base64_of_an_asn1_signature() {
        let key = signer(1);

        for signature in [
            "not base64!".to_string(),
            STANDARD.encode("not a DER signature"),
            format!("{}\n", "A".repeat(MAX_SIGNATURE_BYTES + 1)),
        ] {
            assert_eq!(
                verify(BODY, &signature, &keys_of(&key)),
                Err(BomSignatureError::Unreadable),
                "{signature:?}"
            );
        }
    }

    #[test]
    fn refuses_an_absent_signature() {
        let key = signer(1);

        for signature in ["", "\n", "   \t\n"] {
            assert_eq!(
                verify(BODY, signature, &keys_of(&key)),
                Err(BomSignatureError::Unsigned),
                "{signature:?}"
            );
        }
    }

    #[test]
    fn every_release_key_parses() {
        assert_eq!(release_keys().len(), RELEASE_KEY_PEMS.len());
    }

    /// The daemon's list is the source of truth. A key added or rotated there
    /// and not here leaves the app unable to verify the very release that
    /// carries the rotation.
    #[test]
    fn the_key_list_matches_the_daemons() {
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../daemon/internal/platform/release/keys.go"
        );
        let source = std::fs::read_to_string(path)
            .unwrap_or_else(|err| panic!("read the daemon's key list at {path}: {err}"));

        let theirs = pem_blocks(&source);
        let ours: Vec<String> = RELEASE_KEY_PEMS
            .iter()
            .map(|pem| pem_blocks(pem)[0].clone())
            .collect();

        assert!(!theirs.is_empty(), "no PEM block found in {path}");
        assert_eq!(theirs, ours, "the app's release keys differ from {path}");
    }

    /// Every PEM public-key block in `text`, each as its own lines joined by a
    /// newline. Reading the daemon's Go source this way needs no Go parser: a
    /// PEM block is delimited by its own markers wherever it is embedded.
    fn pem_blocks(text: &str) -> Vec<String> {
        let mut blocks = Vec::new();
        let mut current: Option<Vec<&str>> = None;

        // A PEM block embedded in Go source begins inside a raw string literal,
        // so the delimiters around it are trimmed along with the indentation.
        for line in text
            .lines()
            .map(|line| line.trim_matches(['`', '"', ' ', '\t', ',']))
        {
            match line {
                "-----BEGIN PUBLIC KEY-----" => current = Some(vec![line]),
                "-----END PUBLIC KEY-----" => {
                    if let Some(mut lines) = current.take() {
                        lines.push(line);
                        blocks.push(lines.join("\n"));
                    }
                }
                _ => {
                    if let Some(lines) = current.as_mut() {
                        lines.push(line);
                    }
                }
            }
        }
        blocks
    }
}
