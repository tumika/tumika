package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BackendSystemdCreds names the Linux host-bound custody.
const BackendSystemdCreds = "systemd-creds"

// CredentialName is what systemd calls the key, on both sides of the handover.
//
// It is bound into the ciphertext by `systemd-creds encrypt --name=`, so a blob
// cannot be renamed into another service's slot and decrypted there.
const CredentialName = "tumika-master-key" // #nosec G101 -- the NAME systemd binds a credential to, written into a world-readable unit file; not a secret

// CredentialsDirEnv is what systemd sets for a unit with LoadCredentialEncrypted=.
// The directory is a private tmpfs, readable only by the service account.
const CredentialsDirEnv = "CREDENTIALS_DIRECTORY"

// CredentialFileName is the sealed blob under the tumika home.
const CredentialFileName = "master.cred"

// credsTimeout bounds a systemd-creds call. It is local work against the TPM or
// the host key; slower than this means stuck, not slow.
const credsTimeout = 20 * time.Second

// The split below is the whole design, and it is not obvious from either half.
//
// SEALING happens at install time, as ROOT: `systemd-creds encrypt` reads
// /var/lib/systemd/credential.secret (or the TPM), and that is root-only. The
// blob lands under the tumika home as master.cred.
//
// OPENING never runs systemd-creds at all. The unit declares
// LoadCredentialEncrypted=, so systemd decrypts the blob during startup — while
// it is still privileged — and drops the plaintext into a private tmpfs the
// service account can read. The daemon just reads a file.
//
// Decrypting at runtime instead is the obvious design, and it does not work: the
// daemon runs unprivileged, cannot read the host key, and silently fell back to
// a file store reporting backend "file". That was found by running it under a
// real systemd in a container, not by reasoning about it.

// credsKeyStore reads the key systemd handed over.
type credsKeyStore struct {
	path string
	key  []byte
}

// CredentialPathFor is where the sealed blob lives, beside the plaintext key
// file it replaces.
//
// A different name on purpose: handing a systemd-creds blob to the file store
// would fail to decode, and the reverse would look like a key nobody can open.
func CredentialPathFor(keyFile string) string {
	return filepath.Join(filepath.Dir(keyFile), CredentialFileName)
}

// handedOverKeyPath is the plaintext key inside systemd's credentials tmpfs, or
// "" when this process was not started with one.
func handedOverKeyPath() string {
	dir := os.Getenv(CredentialsDirEnv)
	if dir == "" {
		return ""
	}
	// The directory comes from the environment, which gosec treats as tainted.
	// It is systemd's own variable, set for this process by the supervisor that
	// started it: anything able to forge it is already inside the daemon's
	// process. The leaf is a package constant, so nothing caller-influenced
	// reaches the path either.
	path := filepath.Join(dir, CredentialName)
	if _, err := os.Stat(path); err != nil { // #nosec G703 -- $CREDENTIALS_DIRECTORY is set by systemd for this process; the leaf is a constant
		return ""
	}
	return path
}

func newCredsKeyStore(path string) (KeyStore, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is systemd's own credentials directory
	if err != nil {
		return nil, fmt.Errorf("read the key systemd handed over at %s: %w", path, err)
	}

	key, err := decodeKey(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("the key systemd handed over at %s is unusable: %w", path, err)
	}
	return &credsKeyStore{path: path, key: key}, nil
}

func (s *credsKeyStore) Key() ([]byte, error) { return s.key, nil }
func (s *credsKeyStore) Backend() string      { return BackendSystemdCreds }
func (s *credsKeyStore) KeyRef() string       { return BackendSystemdCreds + ":" + CredentialName }

// openSealedDirectly decrypts the blob in this process.
//
// Only root can: systemd-creds reads the host key, which is 0600 root-only. That
// asymmetry is deliberate and is what makes the two callers behave correctly
// without either knowing about the other.
//
//   - `tumika install` and `tumika token rotate` run as root, so they open the
//     same key the service will get. Without this they could not touch the
//     database at all on a host that had sealed a key — which is how the first
//     version of this locked the installer out of its own install.
//   - The daemon runs unprivileged, so this fails and the handover is the only
//     way in. That is the correct answer for it: if systemd did not provide the
//     key, something is wrong with the unit and inventing a new key would be
//     data loss dressed up as resilience.
func openSealedDirectly(path string, run CredsRunner) (KeyStore, error) {
	if run == nil {
		run = ExecCredsRunner
	}

	sealed, err := os.ReadFile(path) // #nosec G304 -- path is tumika's own layout
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), credsTimeout)
	defer cancel()

	out, err := run(ctx, sealed, "decrypt", "--name="+CredentialName, "-", "-")
	if err != nil {
		return nil, err
	}

	key, err := decodeKey(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, err
	}
	return &credsKeyStore{path: path, key: key}, nil
}

// CredsRunner executes systemd-creds. Exported for the installer, and an
// indirection so sealing is testable on a machine with no systemd — which is
// every machine this was written on.
type CredsRunner func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

// ExecCredsRunner runs the real systemd-creds.
func ExecCredsRunner(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "systemd-creds", args...) // #nosec G204 -- args are constructed by this package, never caller-supplied
	cmd.Stdin = bytes.NewReader(stdin)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// stderr is read only on FAILURE. systemd-creds writes an advisory line
		// ("not located on encrypted media, using anyway") on success whenever
		// the root filesystem is unencrypted — which includes a stock Raspberry
		// Pi — so treating any stderr output as an error would refuse the
		// backend on exactly the deployment it is meant for.
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, detail)
	}
	return stdout.Bytes(), nil
}

// SystemdCredsUsable reports whether this host can seal a key.
//
// Probed by actually sealing something, not by asking `systemd-creds has-tpm2`:
// a Pi has no TPM and works fine on the host key, so the TPM question answers
// something else. Only the installer calls this, and only as root.
func SystemdCredsUsable(ctx context.Context, run CredsRunner) bool {
	if run == nil {
		run = ExecCredsRunner
	}
	if _, err := exec.LookPath("systemd-creds"); err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, credsTimeout)
	defer cancel()

	out, err := run(ctx, []byte("probe"), "encrypt", "--name=tumika-probe", "-", "-")
	return err == nil && len(out) > 0
}

// HandoverProbe runs a command and reports whether it succeeded. An
// indirection so the check is testable without systemd.
type HandoverProbe func(ctx context.Context, name string, args ...string) error

// ExecHandoverProbe runs the real command.
func ExecHandoverProbe(ctx context.Context, name string, args ...string) error {
	// Both scanners flag the variable command. The audit: the only caller is
	// HandoverWorks, which passes the literal "systemd-run" and arguments it
	// builds from package constants plus a path and account name that
	// servicemgr.Config already validated. Nothing here comes from a request.
	//
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- see the audit above: called only with "systemd-run" and package-built args
	return cmd.Run()
}

// HandoverWorks reports whether systemd will actually deliver the sealed key to
// a service.
//
// Checked rather than assumed, because being ABLE to seal a key and being able
// to RECEIVE one are different capabilities, and a host can have the first
// without the second. In a container without the mounts systemd needs for its
// credentials tmpfs, `systemd-creds encrypt` succeeds, the unit is accepted,
// $CREDENTIALS_DIRECTORY is even set — and the directory does not exist. Install
// would seal a key, write a unit naming it, and leave a daemon that can never
// start.
//
// The probe is a real transient unit doing exactly what the real one will do:
// read the credential as an unprivileged user. Anything less would be asserting
// the capability rather than observing it.
func HandoverWorks(ctx context.Context, sealedPath, user string, probe HandoverProbe) bool {
	if probe == nil {
		probe = ExecHandoverProbe
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, credsTimeout)
	defer cancel()

	args := []string{
		"--quiet", "--wait", "--collect",
		"--unit=tumika-credential-probe",
		"--property=LoadCredentialEncrypted=" + CredentialName + ":" + sealedPath,
	}
	if user != "" {
		args = append(args, "--property=User="+user)
	}
	args = append(args, "/bin/sh", "-c",
		`test -s "$`+CredentialsDirEnv+`/`+CredentialName+`"`)

	return probe(ctx, "systemd-run", args...) == nil
}

// SealMasterKey mints a master key and seals it to this host.
//
// Called by `tumika install`, as root, BEFORE the unit is written. Idempotent:
// an existing blob is left exactly as it is, because replacing it would orphan
// every credential already sealed under the key inside.
//
// The host binding is the point, and its cost is real (ADR-0002): a stolen SD
// card yields a database and a blob that open nowhere else — and so does a
// backup restored onto a new machine. Recovery there is re-submitting the
// credentials, which for a subscription token is one paste.
func SealMasterKey(ctx context.Context, path string, run CredsRunner) error {
	if run == nil {
		run = ExecCredsRunner
	}

	info, err := os.Stat(path)
	switch {
	case err == nil && info.Size() > 0:
		return nil
	case err == nil:
		// A zero-length blob is debris from a crash between create and write.
		// It is not a key, nothing will ever decrypt it, and leaving it makes
		// every later start fail closed on a file that can never work — so it
		// is cleared here rather than tripping the ErrExist branch below and
		// being silently kept.
		if rmErr := os.Remove(path); rmErr != nil {
			return fmt.Errorf("remove the empty sealed key %s: %w", path, rmErr)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("stat %s: %w", path, err)
	}

	key, err := newKey()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, credsTimeout)
	defer cancel()

	sealed, err := run(ctx, []byte(encodeKey(key)), "encrypt", "--name="+CredentialName, "-", "-")
	if err != nil {
		return fmt.Errorf("seal a master key with systemd-creds: %w", err)
	}
	if len(sealed) == 0 {
		return errors.New("systemd-creds produced an empty blob")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	// Staged, then linked with O_EXCL semantics — the same care the file store
	// takes, for the same reason: two installs racing must not each seal a key
	// and leave one set of credentials unreadable. os.Rename would silently
	// replace, which is precisely what must never happen to a key.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+CredentialFileName+".*")
	if err != nil {
		return fmt.Errorf("stage the sealed key in %s: %w", filepath.Dir(path), err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set permissions on %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(sealed); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}

	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Someone else won the race; their key is the key.
			return nil
		}
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}
