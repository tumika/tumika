package release

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/mod/semver"
)

// Channel is a stream of releases a daemon follows.
//
// Channels are cumulative: edge receives what beta and stable publish, beta
// receives what stable publishes. That is a property of what a publisher writes
// into a channel's bill of materials, not of the value itself — this type only
// says which names exist.
type Channel string

// The channels a daemon may follow.
const (
	ChannelStable Channel = "stable"
	ChannelBeta   Channel = "beta"
	ChannelEdge   Channel = "edge"
)

// Channels lists every channel, most vetted first.
func Channels() []Channel { return []Channel{ChannelStable, ChannelBeta, ChannelEdge} }

// Errors a caller distinguishes when reading a bill of materials.
var (
	// ErrUnsignedBOM means no detached signature accompanied the document. A
	// bill of materials names the bytes the daemon will execute, so an absent
	// signature is refused exactly as a bad one is.
	ErrUnsignedBOM = errors.New("bill of materials is unsigned")
	// ErrBadSignature means the signature is unreadable, or verifies against
	// none of the keys offered — a tampered body and an unknown signer are the
	// same answer here, because neither is distinguishable from the other.
	ErrBadSignature = errors.New("bill of materials signature does not verify")
	// ErrMalformedBOM means the document verified but does not describe a
	// release: bad JSON, or a field outside its allowed shape.
	ErrMalformedBOM = errors.New("bill of materials is malformed")
	// ErrInvalidReleaseLabel means a label is outside the published shapes and
	// must not reach a URL.
	ErrInvalidReleaseLabel = errors.New("invalid release label")
	// ErrInvalidChannel means a name is not one of the channels.
	ErrInvalidChannel = errors.New("invalid channel")
)

// BOM is the published record of one release: which version of each component
// it contains, and where to fetch them.
//
// It is serialised as JSON and signed as a whole. The signature covers the
// exact bytes, so nothing in this type may be re-encoded before verification.
type BOM struct {
	// Release is the label people read and no client compares.
	Release string `json:"release"`
	// Channel is the stream this document is the head of.
	Channel Channel `json:"channel"`
	// PublishedAt orders releases. Recency, not the label, decides which
	// release a channel offers.
	PublishedAt time.Time `json:"published_at"`
	// Components is keyed by component name — "daemon", "desktop".
	Components map[string]Component `json:"components"`
}

// Component is one separately versioned thing a release ships.
type Component struct {
	// Version is the component's own semver, without a leading "v".
	Version string `json:"version"`
	// Assets is keyed by "<goos>_<goarch>".
	Assets map[string]Asset `json:"assets"`
	// FromRelease names the release whose assets this entry points at when the
	// component did not change. Empty when the release built the component
	// itself. Bytes are never republished under one component version, so a
	// carried-over entry is how a later release ships an unchanged component.
	FromRelease string `json:"from_release,omitempty"`
}

// Asset is one downloadable file and the digest it must hash to.
type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	// Signature is the text of the detached signature file published beside the
	// asset: minisign, for the desktop updater archive, because that is what the
	// app's updater verifies with. Absent for an asset published without one,
	// which the daemon's own are — a download is checked against SHA256, and the
	// digest is trustworthy because the signature over this document covers it.
	Signature string `json:"signature,omitempty"`
}

// Release labels are CalVer `YYYY.MM.NN`, zero-padded, optionally with a
// `-beta.N` suffix; an edge build is labelled `edge.<run number>`.
//
// The pattern is strict because a label taken from a bill of materials, or from
// a daemon reporting its own release, is placed in a URL path. No character
// that could escape a path segment matches.
var releaseLabelPattern = regexp.MustCompile(`^(?:[0-9]{4}\.[0-9]{2}\.[0-9]{2}(?:-beta\.[0-9]{1,6})?|edge\.[0-9]{1,10})$`)

// Component names key both the BOM and, in a later slice, a pairing lookup.
var componentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// A platform key is Go's own GOOS_GOARCH, lower-case and alphanumeric.
var platformKeyPattern = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9]+$`)

// SHA-256 digests are written lower-case hex, as sha256sum prints them.
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateReleaseLabel reports whether a label may be used, wrapping
// ErrInvalidReleaseLabel when it may not.
//
// Call it on any label that reaches a URL — including the one a daemon reads
// out of its own build stamp, which is a string an attacker who can influence a
// build controls.
func ValidateReleaseLabel(label string) error {
	if !releaseLabelPattern.MatchString(label) {
		return fmt.Errorf("%w: %q", ErrInvalidReleaseLabel, label)
	}
	return nil
}

// ValidateChannel reports whether name is a channel, wrapping
// ErrInvalidChannel when it is not.
func ValidateChannel(name Channel) error {
	for _, c := range Channels() {
		if name == c {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrInvalidChannel, string(name))
}

// PlatformKey is the key an asset is published under for a platform.
func PlatformKey(goos, goarch string) string { return goos + "_" + goarch }

// Asset returns the asset a component publishes for a platform.
func (b *BOM) Asset(component, goos, goarch string) (Asset, bool) {
	c, ok := b.Components[component]
	if !ok {
		return Asset{}, false
	}
	a, ok := c.Assets[PlatformKey(goos, goarch)]
	return a, ok
}

// ParseBOM decodes and validates a bill of materials.
//
// It does NOT check a signature: everything reaching a daemon goes through
// VerifyBOM, and this is the half that runs once the bytes are trusted.
func ParseBOM(data []byte) (*BOM, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// Unknown fields are accepted deliberately. A daemon must be able to read a
	// bill of materials written by a later publisher in order to update itself
	// past the release that added a field; refusing one would strand it.
	var bom BOM
	if err := dec.Decode(&bom); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedBOM, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing content after the document", ErrMalformedBOM)
	}
	if err := bom.validate(); err != nil {
		return nil, err
	}
	return &bom, nil
}

func (b *BOM) validate() error {
	if err := ValidateReleaseLabel(b.Release); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedBOM, err)
	}
	if err := ValidateChannel(b.Channel); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedBOM, err)
	}
	if b.PublishedAt.IsZero() {
		return fmt.Errorf("%w: published_at is missing", ErrMalformedBOM)
	}
	if len(b.Components) == 0 {
		return fmt.Errorf("%w: release %s names no components", ErrMalformedBOM, b.Release)
	}
	for name, c := range b.Components {
		if !componentNamePattern.MatchString(name) {
			return fmt.Errorf("%w: %q is not a component name", ErrMalformedBOM, name)
		}
		if err := c.validate(); err != nil {
			return fmt.Errorf("%w: component %s: %w", ErrMalformedBOM, name, err)
		}
	}
	return nil
}

func (c Component) validate() error {
	if !semver.IsValid("v" + strings.TrimPrefix(c.Version, "v")) {
		return fmt.Errorf("%q is not a component version", c.Version)
	}
	if c.FromRelease != "" {
		if err := ValidateReleaseLabel(c.FromRelease); err != nil {
			return err
		}
	}
	if len(c.Assets) == 0 {
		return errors.New("no assets")
	}
	for key, a := range c.Assets {
		if !platformKeyPattern.MatchString(key) {
			return fmt.Errorf("%q is not a <goos>_<goarch> key", key)
		}
		if err := a.validate(); err != nil {
			return fmt.Errorf("asset %s: %w", key, err)
		}
	}
	return nil
}

func (a Asset) validate() error {
	u, err := url.Parse(a.URL)
	if err != nil {
		return fmt.Errorf("unreadable url: %w", err)
	}
	// Absolute and host-bearing: a relative URL would be resolved against
	// whatever the caller happened to fetch the document from.
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an absolute http(s) url", a.URL)
	}
	if !sha256Pattern.MatchString(a.SHA256) {
		return fmt.Errorf("%q is not a sha256 digest", a.SHA256)
	}
	if a.Signature != "" {
		if err := validateAssetSignature(a.Signature); err != nil {
			return err
		}
	}
	return nil
}

// maxAssetSignatureBytes bounds an asset's detached signature. A minisign
// signature is an untrusted-comment line and a base64 line: a few hundred bytes.
const maxAssetSignatureBytes = 4 << 10

// validateAssetSignature holds an asset's signature to the shape a detached
// signature file has, and no further.
//
// The format belongs to whichever client verifies with it, and this package
// never parses one, so the check refuses only what cannot be a signature file at
// all: an unbounded one, a blank one, and one carrying control characters. A
// published document is read by a shell installer as well as by Go, and a NUL or
// an escape sequence in a value it echoes is not something a signature ever
// needs.
func validateAssetSignature(signature string) error {
	if len(signature) > maxAssetSignatureBytes {
		return fmt.Errorf("signature is %d bytes, past the %d byte cap", len(signature), maxAssetSignatureBytes)
	}
	if strings.TrimSpace(signature) == "" {
		return errors.New("signature is blank")
	}
	for _, r := range signature {
		switch r {
		case '\n', '\r', '\t':
			continue
		}
		if !unicode.IsPrint(r) {
			return fmt.Errorf("signature carries %q, which a signature file does not", r)
		}
	}
	return nil
}

// VerifyBOM checks a detached signature over body and returns the document it
// describes.
//
// The signature is ECDSA P-256 over the SHA-256 of body, ASN.1 DER encoded and
// then base64 (standard encoding, surrounding whitespace ignored) — which is
// what a detached `.sig` file beside the document contains.
//
// Any key in keys verifying is enough. That is what makes rotation possible: a
// release that adds a key to the compiled-in list is itself signed by a key
// already in it. An empty list verifies nothing, so a caller that loses its keys
// refuses every document rather than accepting any.
//
// body is parsed only after the signature verifies, so a malformed document
// from an unknown signer is reported as a signature failure — the first check
// that fails is the one the caller is told about.
func VerifyBOM(body, signature []byte, keys []*ecdsa.PublicKey) (*BOM, error) {
	sig, err := decodeSignature(signature)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	for _, key := range keys {
		if key == nil {
			continue
		}
		if ecdsa.VerifyASN1(key, digest[:], sig) {
			return ParseBOM(body)
		}
	}
	return nil, fmt.Errorf("%w: no match among %d release key(s)", ErrBadSignature, len(keys))
}

// OpenBOM verifies a document against the release keys compiled into the
// daemon.
func OpenBOM(body, signature []byte) (*BOM, error) {
	return VerifyBOM(body, signature, ReleaseKeys())
}

// Sign produces the detached signature VerifyBOM accepts.
//
// It is the counterpart of the verification above and exists so the publisher
// and the reader cannot disagree about the encoding.
func Sign(body []byte, key *ecdsa.PrivateKey) ([]byte, error) {
	if key == nil || key.Curve != elliptic.P256() {
		return nil, errors.New("a release is signed with an ECDSA P-256 key")
	}
	digest := sha256.Sum256(body)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign the bill of materials: %w", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n"), nil
}

// decodeSignature reads the detached file's contents.
func decodeSignature(signature []byte) ([]byte, error) {
	text := strings.TrimSpace(string(signature))
	if text == "" {
		return nil, ErrUnsignedBOM
	}
	sig, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable signature: %w", ErrBadSignature, err)
	}
	return sig, nil
}

// ParsePublicKeyPEM reads one PEM-encoded ECDSA P-256 public key.
func ParsePublicKeyPEM(data []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the public key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("release keys are ECDSA P-256, got %T", parsed)
	}
	return key, nil
}
