package rss

import (
	"fmt"
	"os"
	"testing"
)

// TestMain turns off the private address guard for the whole package: nearly every
// test here talks to an httptest server, which listens on loopback by definition.
// The guard itself has its own tests (see TestPrivateAddressesAreRejectedByDefault),
// which switch it back on for the duration of the case.
func TestMain(m *testing.M) {
	SetAllowPrivateURLs(true)
	code := m.Run()
	if err := recover(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
