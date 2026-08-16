package claudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// The embedded key is the trust anchor, so it cannot be used to SIGN anything in
// a test. These tests therefore stand up a bucket signed by a throwaway key and
// swap the keyring — which also means every "bad signature" case below is a real
// cryptographic failure rather than a stubbed one.

// bucket is a fake release bucket.
type bucket struct {
	version  string
	binary   []byte
	entity   *openpgp.Entity
	platform string

	// tamperManifest corrupts the manifest after signing.
	tamperManifest bool
	// wrongChecksum publishes a checksum that does not match the binary.
	wrongChecksum bool
	// wrongSize publishes a size that does not match the binary, with a correct
	// checksum — the only way to reach the size check.
	wrongSize bool
	// omitSignature serves a 404 for the .sig.
	omitSignature bool
	// claimVersion overrides the version inside the manifest, simulating a
	// validly signed document being replayed for a different release.
	claimVersion string

	manifestRequests int
}

func newBucket(t *testing.T) *bucket {
	t.Helper()

	entity, err := openpgp.NewEntity("Test Signing", "", "test@example.com", nil)
	if err != nil {
		t.Fatalf("generate a signing key: %v", err)
	}

	platform, err := PlatformKey(runtimeGOOS(), runtimeGOARCH())
	if err != nil {
		t.Fatalf("PlatformKey: %v", err)
	}

	return &bucket{
		version:  "9.9.9",
		binary:   bytes.Repeat([]byte("claude-code-binary"), 64),
		entity:   entity,
		platform: platform,
	}
}

func (b *bucket) manifest() Manifest {
	sum := sha256.Sum256(b.binary)
	checksum := hex.EncodeToString(sum[:])
	if b.wrongChecksum {
		checksum = strings.Repeat("0", 64)
	}

	version := b.version
	if b.claimVersion != "" {
		version = b.claimVersion
	}

	size := int64(len(b.binary))
	if b.wrongSize {
		size += 1024
	}

	return Manifest{
		Version: version,
		Commit:  "0123456789abcdef",
		Platforms: map[string]Platform{
			b.platform: {Binary: BinaryName, Checksum: checksum, Size: size},
		},
	}
}

func (b *bucket) serve(t *testing.T) *httptest.Server {
	t.Helper()

	raw, err := json.Marshal(b.manifest())
	if err != nil {
		t.Fatalf("marshal the manifest: %v", err)
	}

	var sig bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&sig, b.entity, bytes.NewReader(raw), nil); err != nil {
		t.Fatalf("sign the manifest: %v", err)
	}

	served := raw
	if b.tamperManifest {
		// Signed, then altered: exactly what a compromised mirror would serve.
		served = append(bytes.Clone(raw), ' ')
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/stable", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(b.version + "\n"))
	})
	mux.HandleFunc("/"+b.version+"/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		b.manifestRequests++
		_, _ = w.Write(served)
	})
	mux.HandleFunc("/"+b.version+"/manifest.json.sig", func(w http.ResponseWriter, _ *http.Request) {
		if b.omitSignature {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(sig.Bytes())
	})
	mux.HandleFunc("/"+b.version+"/"+b.platform+"/"+BinaryName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(b.binary)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// installerFor builds an installer trusting the bucket's throwaway key.
func installerFor(t *testing.T, b *bucket) (*Installer, string) {
	t.Helper()

	server := b.serve(t)
	root := t.TempDir()

	inst, err := NewInstaller(root, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}
	trust(inst, b.entity)
	return inst, root
}

// trust substitutes a whole trust anchor: the keyring AND the fingerprint
// Manifest will accept. Swapping only the keyring would leave the two disagreeing,
// which is not a state production can reach.
func trust(inst *Installer, entity *openpgp.Entity) {
	inst.release.keyring = openpgp.EntityList{entity}
	inst.release.expectFingerprint = strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint))
}

func runtimeGOOS() string   { return runtime.GOOS }
func runtimeGOARCH() string { return runtime.GOARCH }

func TestInstallVerifiesAndPublishes(t *testing.T) {
	b := newBucket(t)
	inst, _ := installerFor(t, b)

	result, err := inst.Install(t.Context(), b.version)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.AlreadyPresent {
		t.Error("a fresh install reported AlreadyPresent")
	}

	raw, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read the installed binary: %v", err)
	}
	if !bytes.Equal(raw, b.binary) {
		t.Error("the installed binary does not match what was served")
	}

	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the installed binary is not executable: %#o", info.Mode().Perm())
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the installed binary is group- or world-accessible: %#o", info.Mode().Perm())
	}

	// Installing again is a no-op rather than a re-download: 307 MB is not
	// something to fetch twice.
	before := b.manifestRequests
	again, err := inst.Install(t.Context(), b.version)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if !again.AlreadyPresent {
		t.Error("re-installing did not report AlreadyPresent")
	}
	if b.manifestRequests != before {
		t.Error("re-installing fetched the manifest again")
	}
}

// The signature is the whole trust chain. Everything downstream believes the
// checksums in the manifest, so a manifest that does not verify must not be used
// to judge anything.
func TestInstallRefusesAnUntrustedManifest(t *testing.T) {
	tests := map[string]func(*bucket){
		"signed by a key we do not trust": func(b *bucket) {
			other, err := openpgp.NewEntity("Impostor", "", "impostor@example.com", nil)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			b.entity = other
		},
		"altered after signing": func(b *bucket) { b.tamperManifest = true },
		"no signature at all":   func(b *bucket) { b.omitSignature = true },
	}

	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			b := newBucket(t)
			trusted := b.entity
			corrupt(b)

			server := b.serve(t)
			root := t.TempDir()
			inst, err := NewInstaller(root, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
			if err != nil {
				t.Fatalf("NewInstaller: %v", err)
			}
			// Trust the ORIGINAL key, whatever the bucket did.
			trust(inst, trusted)

			if _, err := inst.Install(t.Context(), b.version); err == nil {
				t.Fatal("an untrusted manifest was accepted")
			}

			// Nothing unverified may be left where Installed would find it.
			installed, err := inst.Installed(t.Context())
			if err != nil {
				t.Fatalf("Installed: %v", err)
			}
			if len(installed) != 0 {
				t.Errorf("a failed install left %v behind", installed)
			}
		})
	}
}

// A validly signed manifest for a DIFFERENT version is a replay: serving an old
// signed document to install a build the operator did not ask for.
func TestInstallRefusesAReplayedManifest(t *testing.T) {
	b := newBucket(t)
	b.claimVersion = "1.0.0"
	inst, _ := installerFor(t, b)

	_, err := inst.Install(t.Context(), b.version)
	if !errors.Is(err, ErrUnsignedManifest) {
		t.Fatalf("= %v, want ErrUnsignedManifest", err)
	}
	if !strings.Contains(err.Error(), "describes") {
		t.Errorf("the error should name the mismatch, got: %v", err)
	}
}

func TestInstallRefusesAChecksumMismatch(t *testing.T) {
	b := newBucket(t)
	b.wrongChecksum = true
	inst, _ := installerFor(t, b)

	_, err := inst.Install(t.Context(), b.version)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("= %v, want ErrChecksumMismatch", err)
	}

	installed, _ := inst.Installed(t.Context())
	if len(installed) != 0 {
		t.Errorf("a binary that failed its checksum was installed: %v", installed)
	}
}

// Retention is about disk: 307 MB per version, on a Pi's SD card.
func TestPruneKeepsTheNewestAndTheProtected(t *testing.T) {
	root := t.TempDir()
	inst, err := NewInstaller(root)
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}

	for _, v := range []string{"2.1.230", "2.1.233", "2.1.240", "2.0.9"} {
		dir := filepath.Join(inst.root, v)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, BinaryName), []byte("x"), 0o700); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Keep two, and never lose the pinned version even though it is not newest.
	if err := inst.Prune(t.Context(), DefaultRetention, "2.1.230"); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	remaining, err := inst.Installed(t.Context())
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}

	kept := map[string]bool{}
	for _, v := range remaining {
		kept[v] = true
	}
	for _, want := range []string{"2.1.240", "2.1.233", "2.1.230"} {
		if !kept[want] {
			t.Errorf("%s was pruned; want the newest two plus the protected one, got %v", want, remaining)
		}
	}
	if kept["2.0.9"] {
		t.Errorf("the oldest version survived pruning: %v", remaining)
	}
}

// Deleting the version the daemon is configured to run would trade a full disk
// for a broken install.
func TestPruneNeverRemovesTheProtectedVersion(t *testing.T) {
	root := t.TempDir()
	inst, err := NewInstaller(root)
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}

	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		dir := filepath.Join(inst.root, v)
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, BinaryName), []byte("x"), 0o700)
	}

	if err := inst.Prune(t.Context(), 1, "1.0.0"); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	remaining, _ := inst.Installed(t.Context())
	found := false
	for _, v := range remaining {
		if v == "1.0.0" {
			found = true
		}
	}
	if !found {
		t.Errorf("the protected version was pruned: %v", remaining)
	}
}

func TestInstalledIgnoresIncompleteDirectories(t *testing.T) {
	root := t.TempDir()
	inst, err := NewInstaller(root)
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}

	// A directory with no binary is debris from an interrupted install, not a
	// version anyone can run.
	_ = os.MkdirAll(filepath.Join(inst.root, "2.1.233"), 0o700)

	installed, err := inst.Installed(t.Context())
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(installed) != 0 {
		t.Errorf("an incomplete install was reported as usable: %v", installed)
	}
}

func TestInstalledIsNewestFirst(t *testing.T) {
	root := t.TempDir()
	inst, _ := NewInstaller(root)

	for _, v := range []string{"2.1.9", "2.1.10", "2.2.0", "10.0.0"} {
		dir := filepath.Join(inst.root, v)
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, BinaryName), []byte("x"), 0o700)
	}

	got, err := inst.Installed(t.Context())
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	// Semver ordering, not lexicographic: 2.1.10 is newer than 2.1.9, and
	// 10.0.0 is newer than 2.2.0.
	want := []string{"10.0.0", "2.2.0", "2.1.10", "2.1.9"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Installed() = %v, want %v", got, want)
	}
}

// The embedded key must be the one the constant names, so swapping the file
// alone cannot silently change who tumika trusts.
func TestEmbeddedKeyMatchesTheDocumentedFingerprint(t *testing.T) {
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(string(signingKey)))
	if err != nil {
		t.Fatalf("read the embedded key: %v", err)
	}
	if err := assertFingerprint(keyring); err != nil {
		t.Fatalf("the embedded key does not match %s: %v", SigningKeyFingerprint, err)
	}

	var identities []string
	for _, e := range keyring {
		for name := range e.Identities {
			identities = append(identities, name)
		}
	}
	if len(identities) == 0 {
		t.Fatal("the embedded key has no identity, so nobody can tell whose it is")
	}
	t.Logf("embedded signing key: %s", strings.Join(identities, ", "))
}

func TestPlatformKey(t *testing.T) {
	tests := map[string]struct {
		goos, goarch string
		want         string
		wantErr      bool
	}{
		// Anthropic uses the Node convention, so amd64 must be translated. It is
		// the case nobody notices while developing on Apple silicon.
		"linux amd64":  {"linux", "amd64", "linux-x64", false},
		"linux arm64":  {"linux", "arm64", "linux-arm64", false},
		"darwin arm64": {"darwin", "arm64", "darwin-arm64", false},
		"darwin amd64": {"darwin", "amd64", "darwin-x64", false},
		"windows":      {"windows", "amd64", "", true},
		"riscv":        {"linux", "riscv64", "", true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := PlatformKey(tc.goos, tc.goarch)
			if tc.wantErr {
				if !errors.Is(err, ErrPlatformUnsupported) {
					t.Fatalf("= %q, %v; want ErrPlatformUnsupported", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlatformKey: %v", err)
			}
			if got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStableReportsTheBucketsCurrentVersion(t *testing.T) {
	b := newBucket(t)
	inst, _ := installerFor(t, b)

	got, err := inst.release.Stable(t.Context())
	if err != nil {
		t.Fatalf("Stable: %v", err)
	}
	// Informational only. The real bucket's stable was 2.1.224 while the pin was
	// 2.1.233 — tumika installs the pin, not whatever is current.
	if got != b.version {
		t.Errorf("Stable = %q, want %q", got, b.version)
	}
}

// A bucket that is unreachable, or that serves an error, must fail loudly rather
// than installing something unverified.
func TestFetchFailuresAreReported(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"manifest missing": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"bucket erroring": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	}

	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)

			inst, err := NewInstaller(t.TempDir(), WithBaseURL(server.URL), WithHTTPClient(server.Client()))
			if err != nil {
				t.Fatalf("NewInstaller: %v", err)
			}

			if _, err := inst.Install(t.Context(), "9.9.9"); err == nil {
				t.Error("a failing bucket produced no error")
			}
			if _, err := inst.release.Stable(t.Context()); err == nil {
				t.Error("Stable produced no error against a failing bucket")
			}
		})
	}
}

// The binary is fetched separately from the manifest, so it has its own failure
// path — and a 404 there must not leave a half-written install.
func TestBinaryDownloadFailureLeavesNothingBehind(t *testing.T) {
	b := newBucket(t)
	server := b.serve(t)

	// Re-serve everything except the binary.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+BinaryName) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Redirect(w, r, server.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	proxy := httptest.NewServer(mux)
	t.Cleanup(proxy.Close)

	inst, err := NewInstaller(t.TempDir(), WithBaseURL(proxy.URL), WithHTTPClient(proxy.Client()))
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}
	trust(inst, b.entity)

	if _, err := inst.Install(t.Context(), b.version); err == nil {
		t.Fatal("a missing binary produced no error")
	}

	installed, err := inst.Installed(t.Context())
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(installed) != 0 {
		t.Errorf("a failed download left %v behind", installed)
	}
}

// A manifest with no build for this machine is a clear refusal, not a confusing
// download failure.
func TestInstallRefusesAManifestWithoutThisPlatform(t *testing.T) {
	b := newBucket(t)
	b.platform = "solaris-sparc"
	inst, _ := installerFor(t, b)

	if _, err := inst.Install(t.Context(), b.version); !errors.Is(err, ErrPlatformUnsupported) {
		t.Fatalf("= %v, want ErrPlatformUnsupported", err)
	}
}

// A size that disagrees with the manifest means the manifest and the artifact
// describe different things, which is worth refusing on its own.
//
// The earlier version of this test appended bytes to the binary AFTER the
// manifest was signed, so the CHECKSUM failed first and the size branch never
// ran — it passed for the wrong reason. Lying about the size with the bytes
// unchanged is the only way to reach it.
func TestInstallRefusesASizeMismatch(t *testing.T) {
	b := newBucket(t)
	b.wrongSize = true
	inst, _ := installerFor(t, b)

	_, err := inst.Install(t.Context(), b.version)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("= %v, want ErrChecksumMismatch", err)
	}
	if !strings.Contains(err.Error(), "the manifest says") {
		t.Errorf("the error should be the SIZE check, not the checksum: %v", err)
	}
}

func TestInstallRejectsAnEmptyVersion(t *testing.T) {
	inst, err := NewInstaller(t.TempDir())
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}
	if _, err := inst.Install(t.Context(), ""); err == nil {
		t.Error("an empty version was accepted")
	}
}

// Unparseable directory names sort last rather than being dropped, so debris
// stays visible to Prune instead of accumulating unnoticed.
func TestVersionSortingHandlesDebris(t *testing.T) {
	versions := []string{"2.1.9", "not-a-version", "2.2.0", "also-bad"}
	sortVersionsNewestFirst(versions)

	if versions[0] != "2.2.0" || versions[1] != "2.1.9" {
		t.Errorf("valid versions did not sort first: %v", versions)
	}
	if len(versions) != 4 {
		t.Errorf("sorting dropped entries: %v", versions)
	}
}

// The fingerprint assertion is what stops a swapped key file silently changing
// who tumika trusts.
func TestFingerprintAssertionRejectsAForeignKey(t *testing.T) {
	other, err := openpgp.NewEntity("Impostor", "", "impostor@example.com", nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	err = assertFingerprint(openpgp.EntityList{other})
	if !errors.Is(err, ErrUnsignedManifest) {
		t.Fatalf("= %v, want ErrUnsignedManifest", err)
	}
	if !strings.Contains(err.Error(), SigningKeyFingerprint) {
		t.Errorf("the error should name the expected fingerprint, got: %v", err)
	}
}

// A providers root that is a file, not a directory, is what a botched volume
// mount looks like. It must be an error, not a panic or a silent skip.
func TestARootThatIsNotADirectory(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "providers")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	b := newBucket(t)
	server := b.serve(t)
	inst, err := NewInstaller(blocked, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}
	trust(inst, b.entity)

	if _, err := inst.Installed(t.Context()); err == nil {
		t.Error("Installed succeeded with a file where its root should be")
	}
	if _, err := inst.Install(t.Context(), b.version); err == nil {
		t.Error("Install succeeded with a file where its root should be")
	}
}

// A collision with debris must be an ERROR, never a phantom success.
//
// The earlier version of this test accepted AlreadyPresent here, which enshrined
// the bug: the verified download is discarded by the deferred cleanup, Path()
// points at a file that does not exist, and every later attempt repeats it — so
// the daemon is permanently stuck with a "present" version it cannot execute.
func TestACollisionWithDebrisIsAnErrorNotAPhantomInstall(t *testing.T) {
	b := newBucket(t)
	inst, _ := installerFor(t, b)

	// An interrupted install left the directory but no runnable binary.
	if err := os.MkdirAll(filepath.Join(inst.root, b.version, "debris"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	result, err := inst.Install(t.Context(), b.version)
	if err == nil {
		t.Fatalf("a collision with debris reported success: %+v", result)
	}
	if usable(result.Path) {
		t.Error("the reported path is usable, so this should not have been an error")
	}
	if !strings.Contains(err.Error(), "remove it and retry") {
		t.Errorf("the error should tell the operator what to do, got: %v", err)
	}
}

// A collision with a COMPLETE install is the real race, and is fine: whoever won
// it verified their copy the same way.
func TestACollisionWithACompleteInstallIsAccepted(t *testing.T) {
	b := newBucket(t)
	inst, _ := installerFor(t, b)

	dir := filepath.Join(inst.root, b.version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, BinaryName), b.binary, 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := inst.Install(t.Context(), b.version)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.AlreadyPresent {
		t.Error("a complete install was not recognised")
	}
}

// A download cut short mid-stream fails its checksum, which is the property that
// matters: a truncated 307 MB binary must never be published.
func TestATruncatedDownloadIsRejected(t *testing.T) {
	b := newBucket(t)
	server := b.serve(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+BinaryName) {
			// Half the bytes, then stop.
			_, _ = w.Write(b.binary[:len(b.binary)/2])
			return
		}
		http.Redirect(w, r, server.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	proxy := httptest.NewServer(mux)
	t.Cleanup(proxy.Close)

	inst, err := NewInstaller(t.TempDir(), WithBaseURL(proxy.URL), WithHTTPClient(proxy.Client()))
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}
	trust(inst, b.entity)

	if _, err := inst.Install(t.Context(), b.version); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("= %v, want ErrChecksumMismatch", err)
	}

	installed, _ := inst.Installed(t.Context())
	if len(installed) != 0 {
		t.Errorf("a truncated download was published: %v", installed)
	}
}

// An unusable base URL is a configuration error and must surface as one.
func TestAnUnusableBaseURL(t *testing.T) {
	inst, err := NewInstaller(t.TempDir(), WithBaseURL("://not a url"))
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}

	if _, err := inst.release.Stable(t.Context()); err == nil {
		t.Error("an unusable base URL produced no error")
	}
	if _, err := inst.Install(t.Context(), "9.9.9"); err == nil {
		t.Error("an unusable base URL produced no error from Install")
	}
}

// Path() joins the version into a filesystem path and the release client
// interpolates it into a URL, so an unchecked value escapes both. A traversing
// version used to reach the fast path, find an existing `claude` outside the
// providers root, and report an unverified binary as installed — precisely the
// hijack this package exists to prevent.
func TestVersionMustBeABareVersionToken(t *testing.T) {
	root := t.TempDir()

	// Something runnable outside the root, standing in for /usr/local/bin.
	outside := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, BinaryName), []byte("not ours"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}

	inst, err := NewInstaller(filepath.Join(root, "providers"))
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}

	for _, version := range []string{
		"../../elsewhere",
		"../elsewhere",
		"/etc",
		"2.1.233/../../elsewhere",
		"",
		"not-a-version",
		"2.1.233 ",
	} {
		t.Run(version, func(t *testing.T) {
			result, err := inst.Install(t.Context(), version)
			if !errors.Is(err, ErrInvalidVersion) {
				t.Fatalf("Install(%q) = %+v, %v; want ErrInvalidVersion", version, result, err)
			}
			if result.AlreadyPresent {
				t.Errorf("reported an unverified binary as installed at %s", result.Path)
			}
		})
	}
}

// Mere existence is not enough for the fast path: none of these can be executed,
// and accepting them makes Install a permanent no-op that reports success.
func TestTheFastPathRequiresARunnableBinary(t *testing.T) {
	b := newBucket(t)

	tests := map[string]func(dir string){
		"a directory named claude": func(dir string) {
			_ = os.MkdirAll(filepath.Join(dir, BinaryName), 0o700)
		},
		"an empty file": func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, BinaryName), nil, 0o700)
		},
		"not executable": func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, BinaryName), []byte("x"), 0o600)
		},
	}

	for name, seed := range tests {
		t.Run(name, func(t *testing.T) {
			inst, _ := installerFor(t, b)
			dir := filepath.Join(inst.root, b.version)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			seed(dir)

			// Not reported as installed...
			installed, err := inst.Installed(t.Context())
			if err != nil {
				t.Fatalf("Installed: %v", err)
			}
			if len(installed) != 0 {
				t.Errorf("an unrunnable binary was reported as installed: %v", installed)
			}

			// ...and not accepted by the fast path either.
			if result, err := inst.Install(t.Context(), b.version); err == nil && result.AlreadyPresent {
				t.Error("the fast path accepted a binary that cannot be executed")
			}
		})
	}
}

// Staging must live outside the directory Installed() enumerates. Inside it, a
// `claude` file created before a single byte is downloaded would be listed as a
// version, and Prune would delete the staging directory of an install in flight.
func TestStagingIsNotVisibleAsAnInstalledVersion(t *testing.T) {
	b := newBucket(t)
	inst, _ := installerFor(t, b)

	if _, err := inst.Install(t.Context(), b.version); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if strings.HasPrefix(inst.staging, inst.root) {
		t.Errorf("staging %q is inside the enumerated root %q", inst.staging, inst.root)
	}

	// Simulate an interrupted download: staging debris containing a binary.
	debris := filepath.Join(inst.root, ".staging-9.9.9-leftover")
	if err := os.MkdirAll(debris, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(debris, BinaryName), []byte("unverified"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}

	installed, err := inst.Installed(t.Context())
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	for _, v := range installed {
		if strings.HasPrefix(v, ".") {
			t.Errorf("staging debris was reported as an installed version: %q", v)
		}
	}
	if len(installed) != 1 || installed[0] != b.version {
		t.Errorf("Installed() = %v, want just the real version", installed)
	}
}

// The budget must count only protected versions that are actually installed, or
// retention silently keeps one more than asked — 307 MB more, on the SD card the
// policy exists to protect.
func TestPruneBudgetCountsOnlyInstalledProtectedVersions(t *testing.T) {
	root := t.TempDir()
	inst, err := NewInstaller(root)
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}

	for _, v := range []string{"3.0.0", "2.0.0", "1.0.0"} {
		dir := filepath.Join(inst.root, v)
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, BinaryName), []byte("x"), 0o700)
	}

	// 3.0.0 is protected AND is already among the newest, so it must not buy an
	// extra slot.
	if err := inst.Prune(t.Context(), 2, "3.0.0", "3.0.0"); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	remaining, err := inst.Installed(t.Context())
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(remaining) != 2 {
		t.Errorf("kept %v, want exactly 2 — a protected version already among the newest must not inflate the budget", remaining)
	}
}

// A keyring holding the right key PLUS another would have satisfied a
// contains-check while making the other key a trusted manifest signer.
func TestTheEmbeddedKeyringMustHoldExactlyOneKey(t *testing.T) {
	real, err := openpgp.ReadArmoredKeyRing(strings.NewReader(string(signingKey)))
	if err != nil {
		t.Fatalf("read the embedded key: %v", err)
	}

	impostor, err := openpgp.NewEntity("Impostor", "", "impostor@example.com", nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if err := assertFingerprint(append(real, impostor)); !errors.Is(err, ErrUnsignedManifest) {
		t.Errorf("a keyring with an extra key was accepted: %v", err)
	}
	if err := assertFingerprint(openpgp.EntityList{}); !errors.Is(err, ErrUnsignedManifest) {
		t.Errorf("an empty keyring was accepted: %v", err)
	}
}

// Verification must check WHO signed, not merely that the keyring contains the
// pinned key: CheckArmoredDetachedSignature accepts a signature from any entity
// in the ring.
func TestASignatureFromAnExtraKeyInTheRingIsRefused(t *testing.T) {
	b := newBucket(t)
	inst, _ := installerFor(t, b)

	// The ring contains the bucket's signer, but we expect a different one.
	inst.release.expectFingerprint = SigningKeyFingerprint

	_, err := inst.Install(t.Context(), b.version)
	if !errors.Is(err, ErrUnsignedManifest) {
		t.Fatalf("= %v, want ErrUnsignedManifest", err)
	}
	if !strings.Contains(err.Error(), "signed by") {
		t.Errorf("the error should name the actual signer, got: %v", err)
	}
}
