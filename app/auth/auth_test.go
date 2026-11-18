package auth

import (
	"regexp"
	"strings"
	"testing"
)

// cheapHasher keeps the scrypt cost negligible in unit tests.
func cheapHasher() *Hasher {
	return &Hasher{Params: Params{N: 2, R: 1, P: 1, KeyLen: 16}}
}

func TestHashAndVerify(t *testing.T) {
	const password = "correct horse battery staple"
	hash, err := cheapHasher().Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatal("the plaintext password appears in the stored hash")
	}
	if !strings.HasPrefix(hash, HashPrefix+"$") {
		t.Fatalf("hash %q does not use the expected self describing format", hash)
	}
	ok, err := Verify(hash, password)
	if err != nil || !ok {
		t.Fatalf("Verify = %v, %v; want true, nil", ok, err)
	}
	ok, err = Verify(hash, "wrong-password")
	if err != nil {
		t.Fatalf("Verify(wrong): %v", err)
	}
	if ok {
		t.Error("Verify accepted a wrong password")
	}
}

func TestHashUsesRandomSalt(t *testing.T) {
	h := cheapHasher()
	a, err := h.Hash("same-password")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash("same-password")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical, salt is missing or constant")
	}
	for _, hash := range []string{a, b} {
		if ok, _ := Verify(hash, "same-password"); !ok {
			t.Errorf("Verify(%q) = false", hash)
		}
	}
}

func TestHashRecordsItsParameters(t *testing.T) {
	h := &Hasher{Params: Params{N: 4, R: 2, P: 3, KeyLen: 16}}
	hash, err := h.Hash("password-12345")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^scrypt\$4\$2\$3\$`).MatchString(hash) {
		t.Errorf("hash %q does not encode N/r/p", hash)
	}
	params, err := parseHashParams(hash)
	if err != nil {
		t.Fatalf("parseHashParams: %v", err)
	}
	if params.N != 4 || params.R != 2 || params.P != 3 || params.KeyLen != 16 {
		t.Errorf("params = %+v, want {4 2 3 16}", params)
	}
}

func TestHashRejectsWeakParameters(t *testing.T) {
	for name, params := range map[string]Params{
		"N not a power of two": {N: 3, R: 8, P: 1, KeyLen: 32},
		"N too small":          {N: 1, R: 8, P: 1, KeyLen: 32},
		"r zero":               {N: 16, R: 0, P: 1, KeyLen: 32},
		"short key":            {N: 16, R: 8, P: 1, KeyLen: 4},
	} {
		if _, err := (&Hasher{Params: params}).Hash("password-12345"); err == nil {
			t.Errorf("%s: Hash accepted unsafe parameters", name)
		}
	}
}

func TestVerifyRejectsGarbageHashes(t *testing.T) {
	for _, hash := range []string{"", "plaintext-password", "bcrypt$1$2$a$b", "scrypt$x$y$z$a$b", "scrypt$2$1$1$not-base64$also-not-base64"} {
		if ok, err := Verify(hash, "anything"); err == nil || ok {
			t.Errorf("Verify(%q) = %v, %v; want an error", hash, ok, err)
		}
	}
}

func TestNeedsUpgrade(t *testing.T) {
	weak, err := (&Hasher{Params: Params{N: 2, R: 1, P: 1, KeyLen: 16}}).Hash("password-12345")
	if err != nil {
		t.Fatal(err)
	}
	if !NeedsUpgrade(weak) {
		t.Error("a hash with test parameters should be marked for upgrade")
	}
	if !NeedsUpgrade("garbage") {
		t.Error("an unparsable hash should be marked for upgrade")
	}
	strong, err := NewHasher().Hash("password-12345")
	if err != nil {
		t.Fatal(err)
	}
	if NeedsUpgrade(strong) {
		t.Error("a hash with the current policy should not need an upgrade")
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("a-passphrase-1"); err != nil {
		t.Errorf("the seeded admin password must satisfy the policy: %v", err)
	}
	if err := ValidatePassword("short"); err == nil {
		t.Error("a 5 character password was accepted")
	}
	if err := ValidatePassword(strings.Repeat("x", 2000)); err == nil {
		t.Error("an unreasonably long password was accepted")
	}
}

func TestNewTokenIsRandomAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		token, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) < 40 {
			t.Fatalf("token too short: %q", token)
		}
		if strings.ContainsAny(token, "+/=") {
			t.Errorf("token %q is not URL safe", token)
		}
		if seen[token] {
			t.Fatal("duplicate token generated")
		}
		seen[token] = true
	}
}

func TestTokenHashIsDeterministicAndOneWay(t *testing.T) {
	if TokenHash("abc") != TokenHash("abc") {
		t.Error("TokenHash is not stable")
	}
	if TokenHash("abc") == "abc" || len(TokenHash("abc")) != 64 {
		t.Errorf("TokenHash = %q, want a sha256 hex digest", TokenHash("abc"))
	}
	if strings.Contains(TokenHash("abc"), "abc") {
		t.Error("the token is recoverable from its hash")
	}
}

func TestNewSecret(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSecret()
	if a == b || len(a) < 32 {
		t.Errorf("secrets are not unique/long enough: %q %q", a, b)
	}
}

func TestCSRFToken(t *testing.T) {
	const secret = "test-secret"
	token, err := CSRFToken(secret, "session-1")
	if err != nil {
		t.Fatalf("CSRFToken: %v", err)
	}
	if !ValidCSRFToken(secret, "session-1", token) {
		t.Error("a valid token did not verify")
	}
	if ValidCSRFToken(secret, "session-2", token) {
		t.Error("a token leaked across sessions")
	}
	if ValidCSRFToken("other-secret", "session-1", token) {
		t.Error("a token signed with another secret was accepted")
	}
	if ValidCSRFToken(secret, "session-1", "") {
		t.Error("an empty token was accepted")
	}
	if ValidCSRFToken(secret, "session-1", token+"x") {
		t.Error("a tampered token was accepted")
	}
	if _, err := CSRFToken("", "session-1"); err == nil {
		t.Error("CSRFToken accepted an empty secret")
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 26 {
			t.Fatalf("len(%q) = %d, want 26", pw, len(pw))
		}
		if seen[pw] {
			t.Fatalf("GeneratePassword returned the same value twice: %q", pw)
		}
		seen[pw] = true

		var letters, digits int
		for _, r := range pw {
			switch {
			case r >= '2' && r <= '9':
				digits++
			case (r >= 'A' && r <= 'Z'):
				letters++
			default:
				t.Fatalf("unexpected character %q in %q", r, pw)
			}
		}
		if letters == 0 || digits == 0 {
			t.Fatalf("password %q is not mixed (letters=%d digits=%d)", pw, letters, digits)
		}
		if err := ValidatePassword(pw); err != nil {
			t.Fatalf("generated password rejected by the policy: %v", err)
		}
	}
}

// A generated password must round-trip through the verifier, and must not be
// guessable from the alphabet alone: every character of the alphabet is used.
func TestGeneratePasswordHashesAndCoversAlphabet(t *testing.T) {
	pw, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	h := NewHasher()
	sum, err := h.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := Verify(sum, pw)
	if err != nil || !ok {
		t.Fatalf("Verify(generated) = %v, %v", ok, err)
	}

	seen := make(map[rune]bool)
	for i := 0; i < 400; i++ {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p {
			seen[r] = true
		}
	}
	for _, r := range passwordAlphabet {
		if !seen[r] {
			t.Errorf("character %q of the alphabet never appeared", r)
		}
	}
}
