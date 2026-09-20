// Command tumika-bom publishes the signed bills of materials a release host
// serves.
//
// It reads every release from the GitHub Releases API, hands them to bomgen,
// signs each document with the release signing key, verifies the signature it
// just made against the keys compiled into platform/release, and only then
// writes the pair of files the host publishes:
//
//	<out>/releases/<label>.json      <out>/releases/<label>.json.sig
//	<out>/channels/<channel>.json    <out>/channels/<channel>.json.sig
//
// The bytes signed are the bytes written: each document is read back off disk
// and verified again before the run is reported as a success. A daemon refuses
// anything that does not verify against a compiled-in key, so a run that
// published a document this binary cannot open would take the channel down
// until someone noticed.
//
// It is a second main in the daemon's module and ships nowhere: goreleaser
// builds ./cmd/tumika alone. Keeping it here is what lets it use the BOM types
// and the signing code the daemon reads them with, so publisher and reader
// cannot drift.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/bomgen"
	"github.com/tumika/tumika/source/daemon/internal/platform/release"
)

// signingKeyEnv names the environment variable holding the PEM private key.
//
// The key is read from the environment and from nowhere else: a flag would put
// it in the process table and in every shell history, and a path would tempt
// this command to name the file in an error a CI log keeps. Nothing here ever
// prints the value, the derived public key, or an error carrying either.
const signingKeyEnv = "TUMIKA_RELEASE_SIGNING_KEY"

// tokenEnvs are the environment variables a GitHub token may arrive in, in the
// order they are consulted. GH_TOKEN is what `gh` and this repo's workflows
// set; GITHUB_TOKEN is what a runner provides by default.
var tokenEnvs = []string{"GH_TOKEN", "GITHUB_TOKEN"}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := command(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "tumika-bom: %v\n", err)
		os.Exit(1)
	}
}

// command parses the flags and assembles what run needs.
//
// It is the only half that reads the environment, so run is drivable from a
// test with a key of its own and no secret anywhere near it.
func command(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("tumika-bom", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "site", "directory to write the published tree into")
	repo := fs.String("repo", "tumika/tumika", "owner/name of the repository to read releases from")
	api := fs.String("api", "https://api.github.com", "base URL of the GitHub API")
	allowSkips := fs.Bool("allow-skips", false,
		"exit zero even when a release could not be published; skips are always reported on stderr")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall deadline for reading the releases")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	key, err := signingKey()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	source, err := newGitHubSource(*api, *repo, token())
	if err != nil {
		return err
	}
	return run(ctx, options{
		source:     source,
		key:        key,
		trusted:    release.ReleaseKeys(),
		outDir:     *out,
		allowSkips: *allowSkips,
		stderr:     stderr,
	})
}

// releaseSource is where the releases to publish come from. The seam exists so
// the signing, self-verification and writing can be tested without a network
// and without the real key.
type releaseSource interface {
	Releases(ctx context.Context) ([]bomgen.Release, error)
}

// options is everything run needs. Nothing here is read from the environment.
type options struct {
	source releaseSource
	key    *ecdsa.PrivateKey
	// trusted is the key list a published document must verify against. In the
	// command it is the list compiled into the daemon, which is what makes
	// publishing with a key no daemon knows a failure here rather than a dead
	// channel later.
	trusted    []*ecdsa.PublicKey
	outDir     string
	allowSkips bool
	stderr     io.Writer
}

func run(ctx context.Context, o options) error {
	if err := keyIsTrusted(o.key, o.trusted); err != nil {
		return err
	}

	releases, err := o.source.Releases(ctx)
	if err != nil {
		return fmt.Errorf("read the releases: %w", err)
	}
	result, err := bomgen.Generate(releases)
	if err != nil {
		return err
	}

	for _, skip := range result.Skipped {
		_, _ = fmt.Fprintf(o.stderr, "tumika-bom: skipped %s: %s\n", skip.Tag, skip.Reason)
	}
	// Reported before the count is decided: a run that refuses to publish still
	// says which releases it could not process, because the reason is the whole
	// diagnosis.
	if len(result.Skipped) > 0 && !o.allowSkips {
		return fmt.Errorf("%d release(s) could not be published; pass -allow-skips to publish the rest anyway",
			len(result.Skipped))
	}

	documents := append(append([]bomgen.Document{}, result.Releases...), result.Channels...)
	for _, doc := range documents {
		if err := publish(o, doc); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(o.stderr, "tumika-bom: published %d release(s) and %d channel head(s) into %s\n",
		len(result.Releases), len(result.Channels), o.outDir)
	return nil
}

// publish signs one document, writes it beside its detached signature, and
// verifies the pair as it now sits on disk.
//
// The verification reads the files back rather than checking the values in
// memory. What a daemon fetches is the bytes on the host, so a truncated write
// or a signature written under the wrong name is exactly the failure this step
// exists to catch.
func publish(o options, doc bomgen.Document) error {
	signature, err := release.Sign(doc.Bytes, o.key)
	if err != nil {
		return fmt.Errorf("%s: %w", doc.Path, err)
	}

	body, err := destination(o.outDir, doc.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(body), 0o755); err != nil {
		return fmt.Errorf("%s: %w", doc.Path, err)
	}
	if err := os.WriteFile(body, doc.Bytes, 0o644); err != nil {
		return fmt.Errorf("%s: %w", doc.Path, err)
	}
	if err := os.WriteFile(body+signatureSuffix, signature, 0o644); err != nil {
		return fmt.Errorf("%s%s: %w", doc.Path, signatureSuffix, err)
	}

	writtenBody, err := os.ReadFile(body)
	if err != nil {
		return fmt.Errorf("%s: %w", doc.Path, err)
	}
	writtenSignature, err := os.ReadFile(body + signatureSuffix)
	if err != nil {
		return fmt.Errorf("%s%s: %w", doc.Path, signatureSuffix, err)
	}
	if _, err := release.VerifyBOM(writtenBody, writtenSignature, o.trusted); err != nil {
		return fmt.Errorf("%s does not verify as published: %w", doc.Path, err)
	}
	return nil
}

// signatureSuffix names the detached signature beside a document, matching what
// platform/release fetches.
const signatureSuffix = ".sig"

// destination turns a document's site-relative path into a file under out, and
// refuses one that would land outside it.
func destination(out, path string) (string, error) {
	full := filepath.Join(out, filepath.FromSlash(path))
	within, err := filepath.Rel(filepath.Clean(out), full)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q does not name a path inside %s", path, out)
	}
	return full, nil
}

// keyIsTrusted refuses a signing key whose public half is not among the keys a
// daemon verifies with.
//
// Publishing with an unknown key produces a tree that looks complete and that
// every daemon refuses, and the refusal surfaces as a channel that stopped
// offering anything — far from the run that caused it. The error names no key
// material, only which list was searched.
func keyIsTrusted(key *ecdsa.PrivateKey, trusted []*ecdsa.PublicKey) error {
	if key == nil || key.Curve != elliptic.P256() {
		return errors.New("a release is signed with an ECDSA P-256 key")
	}
	for _, candidate := range trusted {
		if candidate != nil && key.PublicKey.Equal(candidate) {
			return nil
		}
	}
	return fmt.Errorf("the signing key is not among the %d release key(s) daemons verify with; "+
		"a new key is published in a release signed by one already trusted", len(trusted))
}

// signingKey reads the private key out of the environment.
//
// Both PEM spellings openssl produces are accepted: SEC1 ("EC PRIVATE KEY",
// what `openssl ecparam -genkey` writes) and PKCS#8 ("PRIVATE KEY", what
// `openssl pkcs8 -topk8` writes and what most secret stores round-trip). No
// error here quotes the material it failed to parse.
func signingKey() (*ecdsa.PrivateKey, error) {
	text := os.Getenv(signingKeyEnv)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%s is not set: it holds the PEM release signing key", signingKeyEnv)
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, fmt.Errorf("%s is not a PEM block", signingKeyEnv)
	}

	var key *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s does not hold a readable SEC1 EC private key", signingKeyEnv)
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s does not hold a readable PKCS#8 private key", signingKeyEnv)
		}
		ecKey, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s holds a %T; a release is signed with an ECDSA P-256 key", signingKeyEnv, parsed)
		}
		key = ecKey
	default:
		return nil, fmt.Errorf("%s holds a %q PEM block; expected EC PRIVATE KEY or PRIVATE KEY",
			signingKeyEnv, block.Type)
	}

	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%s holds a key on another curve; a release is signed with ECDSA P-256", signingKeyEnv)
	}
	return key, nil
}

func token() string {
	for _, name := range tokenEnvs {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

// gitHubSource reads releases, and their assets' digests, from the Releases
// API.
//
// It is an HTTP client rather than a shell-out to `gh` because the digests come
// from each release's checksums.txt, which has to be downloaded whatever reads
// the release list: one client keeps the token, the timeouts and the response
// caps in one place, and the command then runs anywhere a token does rather
// than only on a runner with `gh` installed.
type gitHubSource struct {
	client *http.Client
	api    string
	repo   string
	token  string
}

// checksumsAsset and releaseYAMLAsset are the two assets every release carries
// that this command reads rather than merely lists.
const (
	checksumsAsset   = "checksums.txt"
	releaseYAMLAsset = "release.yaml"
)

// perPage is the Releases API's maximum, and maxPages bounds the walk. A
// repository with more releases than that has a prune problem, and silently
// publishing the first 2000 would hide it.
const (
	perPage  = 100
	maxPages = 20
)

// maxAssetBytes bounds a checksums.txt or a release.yaml, both of which are a
// few hundred bytes. The cap is what stops a wrong URL from deciding how much
// memory this command allocates.
const maxAssetBytes = 1 << 20

func newGitHubSource(api, repo, token string) (*gitHubSource, error) {
	base, err := url.Parse(api)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("%q is not an absolute API base URL", api)
	}
	if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("%q is not an owner/name repository", repo)
	}
	return &gitHubSource{
		client: &http.Client{Timeout: 2 * time.Minute},
		api:    strings.TrimSuffix(api, "/"),
		repo:   repo,
		token:  token,
	}, nil
}

// apiRelease is the subset of a Releases API entry this command reads.
type apiRelease struct {
	TagName     string     `json:"tag_name"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt time.Time  `json:"published_at"`
	Assets      []apiAsset `json:"assets"`
}

type apiAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Releases lists every release, newest first as the API returns them, with each
// asset's digest resolved.
//
// It fails rather than omitting a release it could not read. A release missing
// from the list is indistinguishable from one that was never published, and the
// channel heads are computed from the list — so an omission silently moves a
// head backwards.
func (s *gitHubSource) Releases(ctx context.Context) ([]bomgen.Release, error) {
	var releases []bomgen.Release
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("/repos/%s/releases?per_page=%d&page=%d", s.repo, perPage, page)
		body, err := s.get(ctx, s.api+path)
		if err != nil {
			return nil, err
		}
		var batch []apiRelease
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("read the release list: %w", err)
		}
		for _, rel := range batch {
			converted, err := s.convert(ctx, rel)
			if err != nil {
				return nil, fmt.Errorf("release %s: %w", rel.TagName, err)
			}
			releases = append(releases, converted)
		}
		if len(batch) < perPage {
			return releases, nil
		}
	}
	return nil, fmt.Errorf("more than %d releases: raise the page limit or prune", perPage*maxPages)
}

// convert resolves one API entry into what bomgen consumes.
//
// A draft's assets are not downloadable, so nothing is fetched for one: bomgen
// ignores it on the draft flag alone, and asking for its files would fail the
// whole run over a release nobody can publish.
func (s *gitHubSource) convert(ctx context.Context, rel apiRelease) (bomgen.Release, error) {
	converted := bomgen.Release{
		Tag:         rel.TagName,
		Draft:       rel.Draft,
		Prerelease:  rel.Prerelease,
		PublishedAt: rel.PublishedAt,
	}
	if rel.Draft {
		return converted, nil
	}

	// Digests come from the release's own checksums.txt rather than from
	// downloading each asset: goreleaser writes it over the same bytes it
	// uploads, and a publish run covering a dozen releases would otherwise pull
	// hundreds of megabytes of binaries to learn what that file already says.
	// It is not a trust decision — the digests are signed into the bill of
	// materials here, which is what a daemon checks its download against.
	digests := map[string]string{}
	for _, asset := range rel.Assets {
		switch asset.Name {
		case checksumsAsset:
			body, err := s.get(ctx, asset.URL)
			if err != nil {
				return bomgen.Release{}, err
			}
			digests = parseChecksums(body)
		case releaseYAMLAsset:
			body, err := s.get(ctx, asset.URL)
			if err != nil {
				return bomgen.Release{}, err
			}
			converted.ReleaseYAML = body
		}
	}

	for _, asset := range rel.Assets {
		converted.Assets = append(converted.Assets, bomgen.Asset{
			Name:   asset.Name,
			URL:    asset.URL,
			SHA256: digests[asset.Name],
		})
	}
	return converted, nil
}

// parseChecksums reads the `<digest>  <name>` lines sha256sum writes.
func parseChecksums(body []byte) map[string]string {
	digests := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		digest, name, ok := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if !ok {
			continue
		}
		// The separator is two spaces for a text-mode digest and " *" for a
		// binary-mode one; either way the name is what remains once the padding
		// is gone.
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		if digest == "" || name == "" {
			continue
		}
		digests[name] = digest
	}
	return digests
}

// get reads one document, capped, with the token attached when there is one.
func (s *gitHubSource) get(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", target, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxAssetBytes {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", target, maxAssetBytes)
	}
	return body, nil
}
