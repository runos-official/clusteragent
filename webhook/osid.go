// Package webhook serves the cluster agent's local HTTP endpoints: the health
// probe, the presigned tarball upload server for CLI deploys and app-less
// build-image jobs, and the CLI deploy/pull executors. It also generates OSIDs
// (osid.go).
package webhook

import (
	"crypto/rand"
	"math/big"
)

const (
	letters      = "abcdefghijklmnopqrstuvwxyz"
	alphanumeric = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// GenerateOSID generates a unique OSID in the format: app-{letter}{alphanumeric}{alphanumeric}{alphanumeric}{letter}
// Must start with a letter, end with a letter, all lowercase
// Examples: app-a1b2c, app-xk9mz, app-hello
func GenerateOSID() (string, error) {
	// First character: must be a letter
	firstChar, err := randomChar(letters)
	if err != nil {
		return "", err
	}

	// Middle 3 characters: can be alphanumeric
	middle := make([]byte, 3)
	for i := 0; i < 3; i++ {
		char, err := randomChar(alphanumeric)
		if err != nil {
			return "", err
		}
		middle[i] = char
	}

	// Last character: must be a letter
	lastChar, err := randomChar(letters)
	if err != nil {
		return "", err
	}

	return "app-" + string(firstChar) + string(middle) + string(lastChar), nil
}

// randomChar returns a random character from the given charset
func randomChar(charset string) (byte, error) {
	max := big.NewInt(int64(len(charset)))
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return 0, err
	}
	return charset[n.Int64()], nil
}
