package release

import (
	"crypto/ecdsa"
	"fmt"
	"sync"
)

// releaseKeyPEMs are the public halves of the keys that may sign a bill of
// materials. Their private halves are held by the publisher and never appear in
// this repository.
//
// It is a list rather than one key so that a key can be rotated: the release
// that adds the next key to this list is itself signed by a key already here,
// and a daemon that applies it can then verify documents signed either way.
// Dropping a key ends its authority, so a compromised key is removed in a
// release signed by the one replacing it.
var releaseKeyPEMs = []string{
	`-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEGgmfTHgWRmKZRGo7FrLeAEPcr7y1
v7rAq2l1R6qbrMFeBxyiaU4+XvOuP1THEIJjk8y5dqM6zzMgh2LydpLZ/g==
-----END PUBLIC KEY-----
`,
}

// ReleaseKeys returns the parsed release-signing keys, in the order above.
//
// The list is compiled in, so an entry that does not parse to an ECDSA P-256
// public key is a defect in the binary rather than a condition to recover from:
// it panics on first use, and TestReleaseKeysAllParse makes that a failing test
// instead of a failing daemon.
var ReleaseKeys = sync.OnceValue(func() []*ecdsa.PublicKey {
	keys := make([]*ecdsa.PublicKey, 0, len(releaseKeyPEMs))
	for i, text := range releaseKeyPEMs {
		key, err := ParsePublicKeyPEM([]byte(text))
		if err != nil {
			panic(fmt.Sprintf("release key %d is not usable: %v", i, err))
		}
		keys = append(keys, key)
	}
	return keys
})
