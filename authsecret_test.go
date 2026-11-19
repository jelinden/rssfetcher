package main

import (
	"flag"
	"strings"
	"testing"
)

// A start without a configured CSRF secret works, but every token is signed with
// a key that dies with the process - which is only acceptable if the operator
// chose it. The fallback used to be silent.
func TestMissingAuthSecretIsReported(t *testing.T) {
	cfg := config{}
	fs := flag.NewFlagSet("rssfetcher", flag.ContinueOnError)
	registerFlags(fs, &cfg)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	warning := cfg.csrfSecretWarning()
	if warning == "" {
		t.Fatal("an unset -auth-secret produced no warning")
	}
	for _, want := range []string{"auth-secret", "RSSFETCHER_AUTH_SECRET"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q does not mention %s", warning, want)
		}
	}
	if !strings.Contains(strings.ToLower(warning), "restart") {
		t.Errorf("warning %q does not explain the consequence (open forms break on restart)", warning)
	}

	// With a secret configured there is nothing to say.
	cfg.authSecret = "0123456789abcdef0123456789abcdef"
	if got := cfg.csrfSecretWarning(); got != "" {
		t.Errorf("with -auth-secret set the warning = %q, want empty", got)
	}
	// Whitespace is not a secret.
	cfg.authSecret = "   "
	if cfg.csrfSecretWarning() == "" {
		t.Error("a blank -auth-secret was treated as configured")
	}
}
