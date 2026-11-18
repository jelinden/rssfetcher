// Package auth contains the credential primitives of the admin UI: password
// hashing, login sessions and CSRF tokens. It deliberately has no knowledge of
// HTTP or storage.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	// HashPrefix marks the stored format: scrypt$N$r$p$salt$hash.
	HashPrefix = "scrypt"

	// Default parameters: 32 MiB of memory per hash, ~100 ms on a laptop.
	DefaultKeyLen  = 32
	DefaultSaltLen = 16

	// TokenLen is the entropy of session and CSRF tokens (in random bytes).
	TokenLen = 32

	// MinPasswordLen is enforced on password changes; the seeded admin
	// password has to satisfy it too.
	MinPasswordLen = 8
)

// ErrUnknownHashFormat is returned for stored hashes we cannot parse.
var ErrUnknownHashFormat = errors.New("auth: unknown password hash format")

// Params are the scrypt cost parameters of a hash.
type Params struct {
	N      int `json:"n" bson:"n"`
	R      int `json:"r" bson:"r"`
	P      int `json:"p" bson:"p"`
	KeyLen int `json:"keyLen" bson:"keyLen"`
}

// DefaultParams is the policy for newly created hashes.
func DefaultParams() Params {
	return Params{N: 1 << 15, R: 8, P: 1, KeyLen: DefaultKeyLen}
}

func (p Params) validate() error {
	switch {
	case p.N < 2 || p.N&(p.N-1) != 0:
		return fmt.Errorf("auth: scrypt N=%d must be a power of two >= 2", p.N)
	case p.R < 1, p.P < 1:
		return fmt.Errorf("auth: scrypt r/p must be >= 1 (got r=%d p=%d)", p.R, p.P)
	case p.KeyLen < 16:
		return fmt.Errorf("auth: scrypt key length must be >= 16 (got %d)", p.KeyLen)
	}
	return nil
}

// Hasher hashes passwords. Tests use a cheaper Params than the default.
type Hasher struct {
	Params Params
	Rand   io.Reader
}

func NewHasher() *Hasher { return &Hasher{Params: DefaultParams(), Rand: rand.Reader} }

func (h *Hasher) params() Params {
	p := h.Params
	if p.N == 0 {
		p = DefaultParams()
	}
	if h.Rand == nil {
		h.Rand = rand.Reader
	}
	return p
}

// Hash returns the encoded verifier for a plaintext password. The plaintext is
// never stored or logged.
func (h *Hasher) Hash(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	p := h.params()
	if err := p.validate(); err != nil {
		return "", err
	}
	salt := make([]byte, DefaultSaltLen)
	if _, err := io.ReadFull(h.Rand, salt); err != nil {
		return "", fmt.Errorf("auth: reading salt: %w", err)
	}
	key, err := scrypt.Key([]byte(password), salt, p.N, p.R, p.P, p.KeyLen)
	if err != nil {
		return "", fmt.Errorf("auth: scrypt: %w", err)
	}
	return fmt.Sprintf("%s$%d$%d$%d$%s$%s", HashPrefix, p.N, p.R, p.P,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify recomputes the hash of password and compares it in constant time.
func Verify(encoded, password string) (bool, error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	candidate, err := scrypt.Key([]byte(password), salt, p.N, p.R, p.P, p.KeyLen)
	if err != nil {
		return false, fmt.Errorf("auth: scrypt: %w", err)
	}
	return subtle.ConstantTimeCompare(candidate, key) == 1, nil
}

// NeedsUpgrade reports whether a stored hash was created with weaker
// parameters than the current policy, so it can be transparently re-hashed at
// the next successful login.
func NeedsUpgrade(encoded string) bool {
	return DefaultParams().weakerThanTarget(encoded)
}

// NeedsUpgrade reports whether this specific verifier should be re-derived with
// the receiver's parameters. It compares against the hasher that will do the
// re-hashing, not against DefaultParams: a test instance running cheap scrypt
// would otherwise "upgrade" every hash on every login and rewrite the stored
// value with a new salt, which makes a later compare-and-swap fail.
func (h *Hasher) NeedsUpgrade(encoded string) bool {
	if h == nil {
		return NeedsUpgrade(encoded)
	}
	return h.Params.weakerThanTarget(encoded)
}

// weakerThanTarget is true when the encoded verifier costs less than the target
// parameters (or cannot be parsed at all, which is treated as "replace it").
func (want Params) weakerThanTarget(encoded string) bool {
	p, err := parseHashParams(encoded)
	if err != nil {
		return true
	}
	return p.N < want.N || p.R < want.R || p.P < want.P || p.KeyLen < want.KeyLen
}

// parseHashParams reads the cost parameters of a stored hash without deriving
// the key, so it is cheap.
func parseHashParams(encoded string) (Params, error) {
	params, _, _, err := decodeHash(encoded)
	if err != nil {
		return Params{}, err
	}
	return params, nil
}

func decodeHash(encoded string) (Params, []byte, []byte, error) {
	fields := strings.Split(strings.TrimSpace(encoded), "$")
	if len(fields) != 6 || fields[0] != HashPrefix {
		return Params{}, nil, nil, ErrUnknownHashFormat
	}
	n, errN := strconv.Atoi(fields[1])
	r, errR := strconv.Atoi(fields[2])
	p, errP := strconv.Atoi(fields[3])
	salt, errS := base64.RawStdEncoding.DecodeString(fields[4])
	key, errK := base64.RawStdEncoding.DecodeString(fields[5])
	if errN != nil || errR != nil || errP != nil || errS != nil || errK != nil {
		return Params{}, nil, nil, ErrUnknownHashFormat
	}
	params := Params{N: n, R: r, P: p, KeyLen: len(key)}
	if err := params.validate(); err != nil {
		return Params{}, nil, nil, err
	}
	return params, salt, key, nil
}

// ValidatePassword enforces the minimum policy for the admin account.
func ValidatePassword(password string) error {
	switch {
	case len(password) < MinPasswordLen:
		return fmt.Errorf("auth: password must be at least %d characters", MinPasswordLen)
	case len(password) > 1024:
		return errors.New("auth: password is unreasonably long")
	}
	return nil
}

// NewToken returns a URL safe random token (session id, CSRF token).
func NewToken() (string, error) {
	buf := make([]byte, TokenLen)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("auth: reading token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewSecret returns a random CSRF signing secret.
func NewSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("auth: reading secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// passwordAlphabet is base32 without the glyphs that are easy to misread when
// a generated password has to be copied out of a log file: no 0/O, no 1/I/L.
const passwordAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// generatedPasswordLength is 26 characters of that alphabet, roughly 130 bits.
// It is meant to be pasted once, not memorised.
const generatedPasswordLength = 26

// GeneratePassword returns a random admin password, so the first start of the
// app can create an account without a credential being compiled into the
// binary or written into a script. The result always mixes letters and digits
// (which keeps it inside the composition rules shells, CI systems and password
// managers tend to impose) and always satisfies ValidatePassword.
func GeneratePassword() (string, error) {
	buf := make([]byte, generatedPasswordLength)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("auth: generating password: %w", err)
	}
	out := make([]byte, generatedPasswordLength)
	digits := 0
	for i, b := range buf {
		out[i] = passwordAlphabet[int(b)%len(passwordAlphabet)]
		if out[i] >= '2' { // the alphabet's digits start at 2
			digits++
		}
	}
	switch {
	case digits == 0:
		out[0] = passwordAlphabet[len(passwordAlphabet)-1]
	case digits == generatedPasswordLength:
		out[0] = passwordAlphabet[0]
	}
	return string(out), nil
}

// CSRFToken derives the form token of a session. It is an HMAC over the session
// id, so the token is verifiable without storing another secret in the database
// and it rotates automatically when the session does.
func CSRFToken(secret, sessionID string) (string, error) {
	if secret == "" {
		return "", errors.New("auth: empty CSRF secret")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(sessionID)); err != nil {
		return "", fmt.Errorf("auth: hmac: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// ValidCSRFToken compares a submitted token with the derived one in constant
// time.
func ValidCSRFToken(secret, sessionID, token string) bool {
	if token == "" || sessionID == "" {
		return false
	}
	want, err := CSRFToken(secret, sessionID)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(token))) == 1
}

// TokenHash is what gets stored for a session token: a leaked database dump
// must not hand out usable cookies.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
