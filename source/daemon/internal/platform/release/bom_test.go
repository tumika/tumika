package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validBOM is the smallest document that passes validation. Tests that care
// about one field rewrite it with strings.Replace, so the rest stays valid.
const validBOM = `{
  "release": "2026.09.00",
  "channel": "stable",
  "published_at": "2026-09-20T07:28:00Z",
  "components": {
    "daemon": {
      "version": "0.0.1",
      "assets": {
        "linux_arm64": {
          "url": "https://get.tumika.org/download/2026.09.00/tumika_0.0.1_linux_arm64",
          "sha256": "4355a46b19d348dc2f57c046f8ef63d4538ebb936000f3c9ee954a27460dd865"
        }
      }
    }
  }
}`

// newSigningKey makes a throwaway key. Tests never use the release key: the
// private half of that one does not exist in this repository, and a test that
// needed it could not run at all.
func newSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a signing key: %v", err)
	}
	return key
}

func signBOM(t *testing.T, body string, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	sig, err := Sign([]byte(body), key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return sig
}

func TestVerifyBOMAcceptsASignatureFromAListedKey(t *testing.T) {
	key := newSigningKey(t)
	sig := signBOM(t, validBOM, key)

	bom, err := VerifyBOM([]byte(validBOM), sig, []*ecdsa.PublicKey{&key.PublicKey})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if bom.Release != "2026.09.00" || bom.Channel != ChannelStable {
		t.Fatalf("got release %q channel %q", bom.Release, bom.Channel)
	}
	if bom.PublishedAt.IsZero() {
		t.Fatal("published_at was not decoded")
	}
	asset, ok := bom.Asset("daemon", "linux", "arm64")
	if !ok {
		t.Fatal("the daemon asset for linux/arm64 is missing")
	}
	if asset.SHA256 != "4355a46b19d348dc2f57c046f8ef63d4538ebb936000f3c9ee954a27460dd865" {
		t.Fatalf("got digest %q", asset.SHA256)
	}
}

// A second key in the list verifies: this is the property rotation depends on.
func TestVerifyBOMAcceptsAnyKeyInTheList(t *testing.T) {
	first, second := newSigningKey(t), newSigningKey(t)
	sig := signBOM(t, validBOM, second)

	keys := []*ecdsa.PublicKey{&first.PublicKey, &second.PublicKey}
	if _, err := VerifyBOM([]byte(validBOM), sig, keys); err != nil {
		t.Fatalf("a signature from the second listed key must verify: %v", err)
	}
}

func TestVerifyBOMRejectsATamperedBody(t *testing.T) {
	key := newSigningKey(t)
	sig := signBOM(t, validBOM, key)
	tampered := strings.Replace(validBOM, "get.tumika.org", "get.tumika.example", 1)

	_, err := VerifyBOM([]byte(tampered), sig, []*ecdsa.PublicKey{&key.PublicKey})
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestVerifyBOMRejectsASignatureFromAnUnlistedKey(t *testing.T) {
	signer, listed := newSigningKey(t), newSigningKey(t)
	sig := signBOM(t, validBOM, signer)

	_, err := VerifyBOM([]byte(validBOM), sig, []*ecdsa.PublicKey{&listed.PublicKey})
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestVerifyBOMRejectsEveryDocumentWhenNoKeyIsOffered(t *testing.T) {
	key := newSigningKey(t)
	sig := signBOM(t, validBOM, key)

	for name, keys := range map[string][]*ecdsa.PublicKey{
		"empty":       {},
		"nil":         nil,
		"a nil entry": {nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyBOM([]byte(validBOM), sig, keys); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("want ErrBadSignature, got %v", err)
			}
		})
	}
}

func TestVerifyBOMRejectsAnUnsignedDocument(t *testing.T) {
	key := newSigningKey(t)
	for name, sig := range map[string][]byte{
		"no file":    nil,
		"empty":      []byte(""),
		"whitespace": []byte("  \n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyBOM([]byte(validBOM), sig, []*ecdsa.PublicKey{&key.PublicKey}); !errors.Is(err, ErrUnsignedBOM) {
				t.Fatalf("want ErrUnsignedBOM, got %v", err)
			}
		})
	}
}

func TestVerifyBOMRejectsAnUnreadableSignature(t *testing.T) {
	key := newSigningKey(t)
	for name, sig := range map[string][]byte{
		"not base64":      []byte("!!!!"),
		"base64 not DER":  []byte("aGVsbG8gd29ybGQ="),
		"truncated ASN.1": signBOM(t, validBOM, key)[:8],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyBOM([]byte(validBOM), sig, []*ecdsa.PublicKey{&key.PublicKey}); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("want ErrBadSignature, got %v", err)
			}
		})
	}
}

// A correctly signed document that is not a bill of materials is still refused:
// a signature proves who wrote the bytes, not that they mean anything.
func TestVerifyBOMRejectsAMalformedBody(t *testing.T) {
	key := newSigningKey(t)
	for name, body := range map[string]string{
		"not json":        "this is not json",
		"trailing junk":   validBOM + "\n{}",
		"empty object":    "{}",
		"wrong root":      "[]",
		"bad timestamp":   strings.Replace(validBOM, `"2026-09-20T07:28:00Z"`, `"yesterday"`, 1),
		"no published_at": strings.Replace(validBOM, `"published_at": "2026-09-20T07:28:00Z",`, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			sig := signBOM(t, body, key)
			_, err := VerifyBOM([]byte(body), sig, []*ecdsa.PublicKey{&key.PublicKey})
			if !errors.Is(err, ErrMalformedBOM) {
				t.Fatalf("want ErrMalformedBOM, got %v", err)
			}
		})
	}
}

func TestParseBOMRejectsABadReleaseLabelOrChannel(t *testing.T) {
	for name, body := range map[string]string{
		"unpadded label":   strings.Replace(validBOM, "2026.09.00", "2026.9.0", 1),
		"label with path":  strings.Replace(validBOM, "2026.09.00", "../../etc/passwd", 1),
		"empty label":      strings.Replace(validBOM, "2026.09.00", "", 1),
		"unknown channel":  strings.Replace(validBOM, `"stable"`, `"nightly"`, 1),
		"empty channel":    strings.Replace(validBOM, `"stable"`, `""`, 1),
		"upper channel":    strings.Replace(validBOM, `"stable"`, `"Stable"`, 1),
		"bad from_release": strings.Replace(validBOM, `"version": "0.0.1",`, `"version": "0.0.1", "from_release": "latest",`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBOM([]byte(body)); !errors.Is(err, ErrMalformedBOM) {
				t.Fatalf("want ErrMalformedBOM, got %v", err)
			}
		})
	}
}

func TestParseBOMRejectsABadComponentEntry(t *testing.T) {
	for name, body := range map[string]string{
		"empty entry":  strings.Replace(validBOM, `"daemon"`, `"desktop": {}, "daemon"`, 1),
		"bad version":  strings.Replace(validBOM, `"0.0.1"`, `"one"`, 1),
		"bad platform": strings.Replace(validBOM, `"linux_arm64"`, `"linux-arm64"`, 1),
		"relative url": strings.Replace(validBOM, "https://get.tumika.org/download/2026.09.00/tumika_0.0.1_linux_arm64", "/download/x", 1),
		"file url":     strings.Replace(validBOM, "https://get.tumika.org/download/2026.09.00/tumika_0.0.1_linux_arm64", "file:///etc/passwd", 1),
		"short digest": strings.Replace(validBOM, "4355a46b19d348dc2f57c046f8ef63d4538ebb936000f3c9ee954a27460dd865", "4355a46b", 1),
		"upper digest": strings.Replace(validBOM, "4355a46b19d348dc2f57c046f8ef63d4538ebb936000f3c9ee954a27460dd865", strings.ToUpper("4355a46b19d348dc2f57c046f8ef63d4538ebb936000f3c9ee954a27460dd865"), 1),
		"component has no assets": strings.Replace(validBOM,
			`"assets": {
        "linux_arm64": {
          "url": "https://get.tumika.org/download/2026.09.00/tumika_0.0.1_linux_arm64",
          "sha256": "4355a46b19d348dc2f57c046f8ef63d4538ebb936000f3c9ee954a27460dd865"
        }
      }`, `"assets": {}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBOM([]byte(body)); !errors.Is(err, ErrMalformedBOM) {
				t.Fatalf("want ErrMalformedBOM, got %v", err)
			}
		})
	}
}

// The daemon's own assets carry no signature, so the document a daemon-only
// release publishes is the one this reader has always accepted.
func TestParseBOMAcceptsAnAssetWithNoSignature(t *testing.T) {
	bom, err := ParseBOM([]byte(validBOM))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	asset, ok := bom.Asset(DaemonComponent, "linux", "arm64")
	if !ok {
		t.Fatal("the daemon asset for linux/arm64 is missing")
	}
	if asset.Signature != "" {
		t.Fatalf("got signature %q", asset.Signature)
	}
}

// An asset's signature is text this package never parses — the client that
// verifies with it owns the format — so what is refused is a value that cannot be
// a signature file at all.
func TestParseBOMRejectsASignatureThatIsNotAFile(t *testing.T) {
	withSignature := func(signature string) string {
		return strings.Replace(validBOM, `"sha256":`, `"signature": "`+signature+`",
          "sha256":`, 1)
	}

	for name, signature := range map[string]string{
		"blank":      " \\n ",
		"a nul byte": "untrusted comment\\u0000",
		"an escape":  "untrusted comment\\u001b[2J",
		"past the byte cap": "untrusted comment: " +
			strings.Repeat("A", maxAssetSignatureBytes),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBOM([]byte(withSignature(signature))); !errors.Is(err, ErrMalformedBOM) {
				t.Fatalf("want ErrMalformedBOM, got %v", err)
			}
		})
	}

	// Two lines of base64 under a comment: what a minisign `.sig` file holds.
	valid := `untrusted comment: signature from minisign key DCEC313987A2C979\\nRWR5yaKHOTHs3Hy94MYfQLY+kxs7C36ALLmGWmI5Gn86eT57fCREZzc8\\n`
	if _, err := ParseBOM([]byte(withSignature(valid))); err != nil {
		t.Fatalf("a minisign signature must be accepted: %v", err)
	}
}

// A field a later publisher adds must not strand a daemon on an old release.
func TestParseBOMAcceptsUnknownFields(t *testing.T) {
	body := strings.Replace(validBOM, `"release":`, `"notes_url": "https://get.tumika.org/notes", "release":`, 1)
	if _, err := ParseBOM([]byte(body)); err != nil {
		t.Fatalf("an unknown field must be ignored: %v", err)
	}
}

func TestParseBOMReadsACarriedOverComponent(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "bom.json"))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	bom, err := ParseBOM(body)
	if err != nil {
		t.Fatalf("parse the fixture: %v", err)
	}
	desktop, ok := bom.Components["desktop"]
	if !ok {
		t.Fatal("the desktop component is missing")
	}
	if desktop.FromRelease != "2026.09.00" {
		t.Fatalf("got from_release %q", desktop.FromRelease)
	}
	asset, ok := bom.Asset(DesktopComponent, "darwin", "arm64")
	if !ok {
		t.Fatal("the desktop asset for darwin/arm64 is missing")
	}
	if !strings.HasPrefix(asset.Signature, "untrusted comment:") {
		t.Fatalf("the app archive's signature did not survive the parse: %q", asset.Signature)
	}
	if _, ok := bom.Asset("desktop", "linux", "arm64"); ok {
		t.Fatal("desktop publishes no linux/arm64 asset")
	}
	if _, ok := bom.Asset("workflow-engine", "darwin", "arm64"); ok {
		t.Fatal("an absent component must not resolve to an asset")
	}
}

func TestValidateReleaseLabel(t *testing.T) {
	valid := []string{"2026.09.00", "2026.09.01", "2026.12.99", "2026.09.01-beta.1", "2026.09.00-beta.12", "edge.147", "edge.1"}
	for _, label := range valid {
		t.Run(label, func(t *testing.T) {
			if err := ValidateReleaseLabel(label); err != nil {
				t.Fatalf("%q must be accepted: %v", label, err)
			}
		})
	}

	// Anything here would reach a URL path if it were accepted.
	invalid := []string{
		"", "dev", "latest", "2026.9.0", "2026-09-00", "v2026.09.00", "2026.09.00 ",
		"2026.09.00/../../etc/passwd", "../2026.09.00", "2026.09.00?x=1", "2026.09.00#f",
		"2026.09.00-alpha.1", "2026.09.00-beta", "2026.09.00-beta.", "edge", "edge.", "edge.x",
		"EDGE.1", "2026.09.00\n", "2026.09.001",
	}
	for _, label := range invalid {
		t.Run("rejects "+label, func(t *testing.T) {
			if err := ValidateReleaseLabel(label); !errors.Is(err, ErrInvalidReleaseLabel) {
				t.Fatalf("%q must be rejected, got %v", label, err)
			}
		})
	}
}

func TestValidateChannel(t *testing.T) {
	for _, c := range Channels() {
		if err := ValidateChannel(c); err != nil {
			t.Fatalf("%q must be accepted: %v", c, err)
		}
	}
	for _, name := range []Channel{"", "nightly", "Stable", "stable ", "stable/beta"} {
		if err := ValidateChannel(name); !errors.Is(err, ErrInvalidChannel) {
			t.Fatalf("%q must be rejected, got %v", name, err)
		}
	}
}

func TestSignRefusesAKeyOffTheP256Curve(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a P-384 key: %v", err)
	}
	if _, err := Sign([]byte(validBOM), key); err == nil {
		t.Fatal("a P-384 key must be refused")
	}
	if _, err := Sign([]byte(validBOM), nil); err == nil {
		t.Fatal("a nil key must be refused")
	}
}

func TestParsePublicKeyPEMRefusesAnythingElse(t *testing.T) {
	for name, text := range map[string]string{
		"empty":        "",
		"not pem":      "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE",
		"wrong block":  "-----BEGIN CERTIFICATE-----\nMFkwEwYHKoZIzj0CAQ==\n-----END CERTIFICATE-----\n",
		"bad contents": "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQ==\n-----END PUBLIC KEY-----\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePublicKeyPEM([]byte(text)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// The compiled-in list is what OpenBOM trusts, so a key that does not parse is
// a failing test rather than a panicking daemon.
func TestReleaseKeysAllParse(t *testing.T) {
	keys := ReleaseKeys()
	if len(keys) != len(releaseKeyPEMs) {
		t.Fatalf("got %d keys for %d PEM blocks", len(keys), len(releaseKeyPEMs))
	}
	if len(keys) == 0 {
		t.Fatal("a daemon with no release keys can never update")
	}
	for i, key := range keys {
		if key == nil || key.Curve != elliptic.P256() {
			t.Fatalf("key %d is not an ECDSA P-256 key", i)
		}
	}
}

// OpenBOM is the production entry point, and a document this repository can
// sign is exactly the document it must refuse.
func TestOpenBOMRefusesADocumentSignedByAnyOtherKey(t *testing.T) {
	key := newSigningKey(t)
	sig := signBOM(t, validBOM, key)
	if _, err := OpenBOM([]byte(validBOM), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}
