//go:build !darwin

package tokencustody

// New returns the Custodian for this platform.
//
// Off macOS there is no keystore tumika can write to without a dependency it
// refuses to take: a Secret Service keyring needs a desktop session and D-Bus,
// neither of which a headless Linux install or a container has. Storing
// nothing is better than storing the token in a file nobody knows about, since
// the token is printed when it is minted.
func New() Custodian { return NewNoop() }
