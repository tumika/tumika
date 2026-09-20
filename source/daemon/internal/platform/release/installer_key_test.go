package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The PEM block scripts/install-daemon.sh verifies a bill of materials against.
var installerKeyPattern = regexp.MustCompile(
	`(?s)-----BEGIN PUBLIC KEY-----.*?-----END PUBLIC KEY-----`)

// scripts/install-daemon.sh embeds the release public key, because it runs
// before any tumika binary exists and has nothing else to trust. It is the same
// key as the first entry of releaseKeyPEMs, and the two drifting is silent in
// both directions: a rotated compiled-in list leaves every new install
// verifying against a key the publisher no longer uses, and a rotated script
// leaves the daemon refusing the updates it was installed to follow.
func TestInstallerKeyMatchesReleaseKey(t *testing.T) {
	path := filepath.Join(repoRoot, "scripts", "install-daemon.sh")
	data, err := os.ReadFile(path) //nolint:gosec // a fixed path under the repo
	if err != nil {
		t.Fatalf("read %s: %v — it is what a first-time install trusts", path, err)
	}

	found := installerKeyPattern.FindAllString(string(data), -1)
	if len(found) != 1 {
		t.Fatalf("%s carries %d PEM public key blocks; it embeds exactly one", path, len(found))
	}

	embedded := strings.TrimSpace(found[0])
	compiled := strings.TrimSpace(releaseKeyPEMs[0])
	if embedded != compiled {
		t.Fatalf("install-daemon.sh embeds\n%s\nbut releaseKeyPEMs[0] is\n%s", embedded, compiled)
	}

	// The comparison above is on text, so it also catches a re-wrapped copy of
	// the same key. Parsing is what says the text is a key at all: openssl
	// refuses a block it cannot read, and an install that cannot verify is an
	// install that does nothing.
	key, err := ParsePublicKeyPEM([]byte(embedded + "\n"))
	if err != nil {
		t.Fatalf("the key embedded in install-daemon.sh does not parse: %v", err)
	}
	if !key.Equal(ReleaseKeys()[0]) {
		t.Fatal("the key embedded in install-daemon.sh is not the daemon's first release key")
	}
}
