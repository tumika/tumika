//go:build !darwin

package secrets

import "errors"

// errKeychainUnavailable exists on every platform so the selection logic reads
// the same everywhere; off macOS there is simply no keychain to try.
var errKeychainUnavailable = errors.New("keychain is unavailable")

// newKeychainKeyStore is never reached off macOS — OpenKeyStore guards on GOOS —
// but it keeps the package compiling without build tags leaking into the
// selection logic. The Linux answer is systemd-creds, which arrives with the
// service-manager step; until then Linux uses the file store, as a container
// would.
func newKeychainKeyStore() (KeyStore, error) { return nil, errKeychainUnavailable }
