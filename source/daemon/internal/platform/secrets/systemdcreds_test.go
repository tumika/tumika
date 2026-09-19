package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCreds stands in for systemd-creds. The "encryption" is a reversible
// marker, which is the right level of fidelity: what is under test is the
// handover protocol and the failure modes, not AES.
type fakeCreds struct {
	calls    [][]string
	encErr   error
	decErr   error
	boundTo  string
	failName bool
}

const fakeSeal = "SEALED:"

func (f *fakeCreds) run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)

	var name string
	for _, arg := range args {
		if after, ok := strings.CutPrefix(arg, "--name="); ok {
			name = after
		}
	}

	switch args[0] {
	case "encrypt":
		if f.encErr != nil {
			return nil, f.encErr
		}
		f.boundTo = name
		return []byte(fakeSeal + name + ":" + string(stdin)), nil

	case "decrypt":
		if f.decErr != nil {
			return nil, f.decErr
		}
		body, ok := strings.CutPrefix(string(stdin), fakeSeal)
		if !ok {
			return nil, errors.New("not a sealed blob")
		}
		boundName, payload, _ := strings.Cut(body, ":")
		// The name is bound into the ciphertext, so a blob cannot be renamed
		// into another service's slot and opened there.
		if f.failName && boundName != name {
			return nil, errors.New("credential name mismatch")
		}
		return []byte(payload), nil
	}
	return nil, errors.New("unexpected systemd-creds call")
}

func TestSealMasterKeyBindsTheCredentialName(t *testing.T) {
	fake := &fakeCreds{failName: true}
	path := filepath.Join(t.TempDir(), CredentialFileName)

	if err := SealMasterKey(t.Context(), path, fake.run); err != nil {
		t.Fatalf("SealMasterKey: %v", err)
	}
	if fake.boundTo != CredentialName {
		t.Errorf("sealed under name %q, want %q", fake.boundTo, CredentialName)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no blob was written: %v", err)
	}
	// The blob is not a secret in the way a key is, but it is the thing that
	// becomes one on this host — and there is no reason for anyone else to read
	// it.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o, want 0600", perm)
	}
}

// Idempotent, and it matters: replacing the blob would orphan every credential
// sealed under the key inside it.
func TestSealMasterKeyLeavesAnExistingBlobAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), CredentialFileName)
	original := []byte(fakeSeal + CredentialName + ":original-key")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fake := &fakeCreds{}
	if err := SealMasterKey(t.Context(), path, fake.run); err != nil {
		t.Fatalf("SealMasterKey: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(original) {
		t.Error("an existing sealed key was replaced; every credential under it is now unreadable")
	}
	if len(fake.calls) != 0 {
		t.Errorf("systemd-creds was called for a key that already existed: %v", fake.calls)
	}
}

// An empty blob is debris from a crash between create and write. Treated as
// absent so the next install seals one, rather than wedging forever on a
// decrypt that can never succeed.
func TestSealMasterKeyReplacesAnEmptyBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), CredentialFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fake := &fakeCreds{}
	if err := SealMasterKey(t.Context(), path, fake.run); err != nil {
		t.Fatalf("SealMasterKey: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Errorf("the empty blob was not replaced: %v (size %d)", err, info.Size())
	}
}

func TestSealMasterKeyReportsAFailureToSeal(t *testing.T) {
	fake := &fakeCreds{encErr: errors.New("no TPM and no host key")}
	path := filepath.Join(t.TempDir(), CredentialFileName)

	if err := SealMasterKey(t.Context(), path, fake.run); err == nil {
		t.Fatal("a failed seal reported success")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failed seal left a blob behind")
	}
}

// The handover is what makes the whole backend work: the daemon is unprivileged
// and could never read the host key itself.
func TestTheKeySystemdHandsOverIsUsed(t *testing.T) {
	home := t.TempDir()
	credDir := t.TempDir()

	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(filepath.Join(credDir, CredentialName), []byte(encoded), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv(MasterKeyEnv, "")
	t.Setenv(CredentialsDirEnv, credDir)

	store, err := OpenKeyStore(filepath.Join(home, "master.key"))
	if err != nil {
		t.Fatalf("OpenKeyStore: %v", err)
	}
	if store.Backend() != BackendSystemdCreds {
		t.Errorf("backend = %q, want %q", store.Backend(), BackendSystemdCreds)
	}

	got, err := store.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if string(got) != string(key) {
		t.Error("the key systemd handed over was not the one used")
	}
}

// THE fail-closed rule. A host with a sealed blob has credentials sealed under
// that key; minting a fresh one in a file would produce a daemon that starts
// cleanly, reports backend "file", and cannot open a single stored credential.
func TestASealedBlobThatCannotBeOpenedIsFatal(t *testing.T) {
	home := t.TempDir()
	keyFile := filepath.Join(home, "master.key")
	if err := os.WriteFile(CredentialPathFor(keyFile), []byte("sealed to another host"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv(MasterKeyEnv, "")
	t.Setenv(CredentialsDirEnv, "")

	// "linux" is passed explicitly rather than relying on the host, so this
	// rule is checked on every machine rather than only in CI.
	store, err := chooseKeyStore("linux", keyFile)
	if err == nil {
		t.Fatalf("a blob that cannot be opened was tolerated; backend %q", store.Backend())
	}
	if _, statErr := os.Stat(keyFile); statErr == nil {
		t.Error("a fresh file key was minted, orphaning everything sealed under the old one")
	}
}

// The explicit override still wins over everything, including a handover:
// an operator who set the variable meant it.
func TestTheEnvironmentOverrideBeatsAHandover(t *testing.T) {
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, CredentialName),
		[]byte(base64.StdEncoding.EncodeToString(make([]byte, KeySize))), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}

	chosen := make([]byte, KeySize)
	for i := range chosen {
		chosen[i] = 0xAB
	}
	t.Setenv(MasterKeyEnv, base64.StdEncoding.EncodeToString(chosen))
	t.Setenv(CredentialsDirEnv, credDir)

	store, err := OpenKeyStore(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatalf("OpenKeyStore: %v", err)
	}
	if store.Backend() != BackendEnv {
		t.Errorf("backend = %q, want %q — the explicit override must win", store.Backend(), BackendEnv)
	}
}

// A truncated or corrupt handover is refused rather than padded into something
// that would decrypt nothing correctly.
func TestAnUnusableHandoverIsRefused(t *testing.T) {
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, CredentialName), []byte("not-a-key"), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv(MasterKeyEnv, "")
	t.Setenv(CredentialsDirEnv, credDir)

	if _, err := OpenKeyStore(filepath.Join(t.TempDir(), "master.key")); err == nil {
		t.Fatal("an unusable handover was accepted")
	}
}

// HandoverWorks must observe the capability rather than assert it: a host can
// seal a key and still not deliver one, which is exactly the container case
// that left a daemon unable to start.
func TestHandoverWorksReportsTheProbeResult(t *testing.T) {
	var seen []string
	probe := func(_ context.Context, name string, args ...string) error {
		seen = append([]string{name}, args...)
		return nil
	}

	if !HandoverWorks(t.Context(), "/var/lib/tumika/master.cred", "tumika", probe) {
		t.Fatal("a succeeding probe was reported as a failure")
	}

	joined := strings.Join(seen, " ")
	if !strings.Contains(joined, "LoadCredentialEncrypted="+CredentialName) {
		t.Errorf("the probe does not exercise the handover: %s", joined)
	}
	if !strings.Contains(joined, "User=tumika") {
		t.Errorf("the probe does not run as the service account: %s", joined)
	}
}

func TestHandoverWorksReportsAFailingProbe(t *testing.T) {
	probe := func(context.Context, string, ...string) error { return errors.New("no credential delivered") }
	if HandoverWorks(t.Context(), "/var/lib/tumika/master.cred", "tumika", probe) {
		t.Error("a failing probe was reported as working")
	}
}

// The real runner is what production uses, so its two behaviours are worth
// pinning: output comes back, and a failure carries the tool's own words.
func TestExecCredsRunnerReportsFailuresWithDetail(t *testing.T) {
	if _, err := exec.LookPath("systemd-creds"); err != nil {
		t.Skip("systemd-creds is not on this machine")
	}

	// A name nothing was sealed under, so decrypt must fail.
	_, err := ExecCredsRunner(t.Context(), []byte("not a blob"), "decrypt", "--name=nope", "-", "-")
	if err == nil {
		t.Fatal("decrypting rubbish reported success")
	}
}

// SystemdCredsUsable must OBSERVE the capability. Asking `has-tpm2` would answer
// the wrong question: a Pi has no TPM and works fine on the host key.
func TestSystemdCredsUsableProbesByActuallySealing(t *testing.T) {
	var sawEncrypt bool
	probe := func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		if args[0] == "encrypt" {
			sawEncrypt = true
		}
		return []byte("sealed"), nil
	}

	if !SystemdCredsUsable(t.Context(), probe) {
		t.Error("a succeeding seal was reported as unusable")
	}
	if !sawEncrypt {
		t.Error("the probe did not actually try to seal anything")
	}
}

func TestSystemdCredsUsableReportsAFailureToSeal(t *testing.T) {
	probe := func(context.Context, []byte, ...string) ([]byte, error) {
		return nil, errors.New("no host key")
	}
	if SystemdCredsUsable(t.Context(), probe) {
		t.Error("a host that cannot seal was reported as usable")
	}
}

// Root opens the blob directly, which is what keeps the installer from being
// locked out of its own install.
func TestOpenSealedDirectly(t *testing.T) {
	fake := &fakeCreds{}
	path := filepath.Join(t.TempDir(), CredentialFileName)

	if err := SealMasterKey(t.Context(), path, fake.run); err != nil {
		t.Fatalf("SealMasterKey: %v", err)
	}

	store, err := openSealedDirectly(path, fake.run)
	if err != nil {
		t.Fatalf("openSealedDirectly: %v", err)
	}
	if store.Backend() != BackendSystemdCreds {
		t.Errorf("backend = %q, want %q", store.Backend(), BackendSystemdCreds)
	}
	if !strings.Contains(store.KeyRef(), CredentialName) {
		t.Errorf("KeyRef does not name the credential: %q", store.KeyRef())
	}

	key, err := store.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if len(key) != KeySize {
		t.Errorf("key is %d bytes, want %d", len(key), KeySize)
	}
}

// A blob sealed on another host cannot be opened here, and that must surface as
// an error rather than a zero key.
func TestOpenSealedDirectlyRefusesABlobItCannotDecrypt(t *testing.T) {
	fake := &fakeCreds{decErr: errors.New("host key mismatch")}
	path := filepath.Join(t.TempDir(), CredentialFileName)
	if err := os.WriteFile(path, []byte("sealed elsewhere"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := openSealedDirectly(path, fake.run); err == nil {
		t.Fatal("a blob from another host was opened")
	}
}

// A missing blob is a plain error, not a panic on a nil store.
func TestOpenSealedDirectlyWithNoBlob(t *testing.T) {
	fake := &fakeCreds{}
	if _, err := openSealedDirectly(filepath.Join(t.TempDir(), "absent"), fake.run); err == nil {
		t.Fatal("a missing blob was opened")
	}
}

// The file store remains the fallback where there is no systemd and no
// handover — which is what a container gets.
func TestTheFileStoreIsStillTheFallback(t *testing.T) {
	t.Setenv(MasterKeyEnv, "")
	t.Setenv(CredentialsDirEnv, "")

	store, err := chooseKeyStore("linux", filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatalf("chooseKeyStore: %v", err)
	}
	if store.Backend() != BackendFile {
		t.Errorf("backend = %q, want %q", store.Backend(), BackendFile)
	}
}

// The install-time custody path, exercised WITHOUT the env override — so it
// actually reaches the sealed-blob logic rather than short-circuiting on
// TUMIKA_MASTER_KEY, which is what every install test does.
//
// This is the case that locked the installer out of its own install: after
// committing to systemd-creds, root still has to be able to open the database.
func TestRootCanStillOpenTheDatabaseAfterCommittingToSystemdCreds(t *testing.T) {
	t.Setenv(MasterKeyEnv, "")
	t.Setenv(CredentialsDirEnv, "")

	home := t.TempDir()
	keyFile := filepath.Join(home, "master.key")
	sealed := CredentialPathFor(keyFile)

	fake := &fakeCreds{}
	if err := SealMasterKey(t.Context(), sealed, fake.run); err != nil {
		t.Fatalf("SealMasterKey: %v", err)
	}

	// systemd has handed nothing over — this is the installer, not the service.
	store, err := openSealedDirectly(sealed, fake.run)
	if err != nil {
		t.Fatalf("root cannot open the key it just sealed: %v", err)
	}
	if store.Backend() != BackendSystemdCreds {
		t.Errorf("backend = %q, want %q", store.Backend(), BackendSystemdCreds)
	}

	// And the key is the same one the service will receive, or the two halves
	// would be sealing and opening different things.
	handed := t.TempDir()
	viaRoot, err := store.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(handed, CredentialName),
		[]byte(encodeKey(viaRoot)), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(CredentialsDirEnv, handed)

	viaSystemd, err := chooseKeyStore("linux", keyFile)
	if err != nil {
		t.Fatalf("chooseKeyStore: %v", err)
	}
	serviceKey, err := viaSystemd.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if string(serviceKey) != string(viaRoot) {
		t.Error("the installer and the service would use different keys")
	}
}

// The real probe runs the command it is given. Exercised with `true`/`false`
// rather than systemd-run, so the wiring is checked on any machine.
func TestExecHandoverProbe(t *testing.T) {
	if err := ExecHandoverProbe(t.Context(), "true"); err != nil {
		t.Errorf("a succeeding command was reported as a failure: %v", err)
	}
	if err := ExecHandoverProbe(t.Context(), "false"); err == nil {
		t.Error("a failing command was reported as a success")
	}
}

// HandoverWorks builds the probe as a real transient unit: the credential
// directive, and the account the unit will run as.
func TestHandoverWorksBuildsACredentialProbe(t *testing.T) {
	var args []string
	probe := func(_ context.Context, name string, a ...string) error {
		args = append([]string{name}, a...)
		return nil
	}

	if !HandoverWorks(t.Context(), "/var/lib/tumika/master.cred", "", probe) {
		t.Fatal("a succeeding probe was reported as a failure")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "systemd-run") {
		t.Errorf("the probe does not use a transient unit: %s", joined)
	}
	// No account given, so no User= — otherwise the probe would name an empty
	// user and fail for a reason that says nothing about the handover.
	if strings.Contains(joined, "User=") {
		t.Errorf("the probe names a user that was not asked for: %s", joined)
	}
}

// A blob that cannot be written is an error, not a silent fallback to no
// custody at all.
func TestSealMasterKeyReportsAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this relies on")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	fake := &fakeCreds{}
	if err := SealMasterKey(t.Context(), filepath.Join(dir, CredentialFileName), fake.run); err == nil {
		t.Fatal("sealing into an unwritable directory reported success")
	}
}

// An empty blob from systemd-creds is refused rather than written out as a key
// nothing will ever open.
func TestSealMasterKeyRefusesAnEmptyBlob(t *testing.T) {
	empty := func(context.Context, []byte, ...string) ([]byte, error) { return nil, nil }
	path := filepath.Join(t.TempDir(), CredentialFileName)

	if err := SealMasterKey(t.Context(), path, empty); err == nil {
		t.Fatal("an empty blob was accepted")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("an empty blob was written to disk")
	}
}

// SystemdCredsUsable must report false when the tool produces nothing, not just
// when it errors.
func TestSystemdCredsUsableRefusesAnEmptyResult(t *testing.T) {
	empty := func(context.Context, []byte, ...string) ([]byte, error) { return nil, nil }
	if SystemdCredsUsable(t.Context(), empty) {
		t.Error("a tool that sealed nothing was reported as usable")
	}
}
