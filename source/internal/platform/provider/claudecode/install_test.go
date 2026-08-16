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

	return Manifest{
		Version: version,
		Commit:  "0123456789abcdef",
		Platforms: map[string]Platform{
			b.platform: {Binary: BinaryName, Checksum: checksum, Size: int64(len(b.binary))},
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
	inst.release.keyring = openpgp.EntityList{b.entity}
	return inst, root
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
			inst.release.keyring = openpgp.EntityList{trusted}

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
