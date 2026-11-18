package mongo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	userCollection    = "usercollection"
	sessionCollection = "sessioncollection"

	// sessionRenewThreshold is how close to the deadline a session has to be
	// before an active request slides its expiry forward. Renewing on literally
	// every request would turn each page view into a write.
	sessionRenewThreshold = time.Hour
)

// ErrConflict reports that the document changed between reading and writing.
var ErrConflict = errors.New("mongo: conflict")

// ErrInvalidCredentials is returned for an unknown username or a wrong
// password. It is intentionally the same error in both cases.
var ErrInvalidCredentials = errors.New("mongo: invalid username or password")

// ErrSessionExpired is returned when a cookie points at an expired or deleted
// session.
var ErrSessionExpired = errors.New("mongo: session expired")

// ErrWeakPassword is returned when a new password does not meet the policy.
var ErrWeakPassword = errors.New("mongo: password does not meet the minimum policy")

// dummyHash is verified when a username does not exist, so a wrong username and
// a wrong password take the same amount of time. It is derived lazily: the hash
// itself costs ~100 ms and must not be paid at package init.
var dummyHash = sync.OnceValue(func() string {
	hash, err := auth.NewHasher().Hash("placeholder-password-never-used")
	if err != nil {
		// scrypt is unusable; any verification will fail as well.
		return "scrypt$2$8$1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}
	return hash
})

// HasAdminUsers reports whether the database holds at least one admin account.
// Used at start-up to tell "nothing configured yet" apart from "configured
// elsewhere", so a missing RSSFETCHER_ADMIN_USER can be reported accurately.
func (s *Store) HasAdminUsers(ctx context.Context) (bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	// Existence, not an exact count: the limit keeps this a one document scan.
	total, err := s.collection(userCollection).CountDocuments(ctx, bson.D{}, options.Count().SetLimit(1))
	if err != nil {
		return false, fmt.Errorf("mongo: counting admin users: %w", err)
	}
	return total > 0, nil
}

// AdminUserByName loads an account by username.
func (s *Store) AdminUserByName(ctx context.Context, username string) (domain.AdminUser, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	var user domain.AdminUser
	err := s.collection(userCollection).FindOne(ctx, bson.M{"username": username}).Decode(&user)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return domain.AdminUser{}, ErrNotFound
	}
	if err != nil {
		return domain.AdminUser{}, fmt.Errorf("mongo: load user %q: %w", username, err)
	}
	return user, nil
}

// EnsureAdminUser creates the admin account (and its hashed password) when it
// does not exist yet. It never overwrites an existing password: rotating a
// password must be an explicit action.
func (s *Store) EnsureAdminUser(ctx context.Context, username, password string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, errors.New("mongo: admin username is required")
	}
	if err := auth.ValidatePassword(password); err != nil {
		return false, err
	}

	existing, err := s.AdminUserByName(ctx, username)
	switch {
	case err == nil:
		if s.hasher.NeedsUpgrade(existing.PasswordHash) {
			// We cannot re-hash without the plaintext here; log it so the
			// operator knows to rotate the password.
			log.Printf("mongo: password hash of %q uses outdated scrypt parameters, rotate it from the admin UI", username)
		}
		return false, nil
	case !errors.Is(err, ErrNotFound):
		return false, err
	}

	hash, err := s.hasher.Hash(password)
	if err != nil {
		return false, err
	}
	now := time.Now()
	user := domain.AdminUser{
		ID:           bson.NewObjectID(),
		Username:     username,
		PasswordHash: hash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if _, err := s.collection(userCollection).InsertOne(ctx, &user); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// Another instance seeded it first, which is fine.
			return false, nil
		}
		return false, fmt.Errorf("mongo: insert admin user %q: %w", username, err)
	}
	log.Printf("mongo: created admin user %q", username)
	return true, nil
}

// VerifyAdminPassword checks credentials. The returned user never carries the
// password hash.
func (s *Store) VerifyAdminPassword(ctx context.Context, username, password string) (domain.AdminUser, error) {
	user, err := s.verifyAdminPassword(ctx, username, password)
	user.PasswordHash = ""
	return user, err
}

// verifyAdminPassword is the internal variant, which keeps the hash so that
// ChangePassword can compare-and-swap against it.
func (s *Store) verifyAdminPassword(ctx context.Context, username, password string) (domain.AdminUser, error) {
	user, err := s.AdminUserByName(ctx, strings.TrimSpace(username))
	if errors.Is(err, ErrNotFound) {
		// Constant-ish work factor whether or not the user exists.
		_, _ = auth.Verify(dummyHash(), password)
		return domain.AdminUser{}, ErrInvalidCredentials
	}
	if err != nil {
		return domain.AdminUser{}, err
	}

	ok, err := auth.Verify(user.PasswordHash, password)
	if err != nil {
		// A stored hash we cannot parse must not become a login bypass.
		log.Printf("mongo: password hash of %q is unusable: %v", user.Username, err)
		return domain.AdminUser{}, ErrInvalidCredentials
	}
	if !ok {
		return domain.AdminUser{}, ErrInvalidCredentials
	}

	if s.hasher.NeedsUpgrade(user.PasswordHash) {
		if hash, err := s.hasher.Hash(password); err == nil {
			if err := s.setPasswordHash(ctx, user.ID, hash, user.PasswordHash); err != nil {
				log.Printf("mongo: upgrading password hash of %q: %v", user.Username, err)
			} else {
				// Keep the value we just wrote: a caller that compare-and-swaps
				// against the old one would otherwise fail on its own upgrade.
				log.Printf("mongo: upgraded password hash parameters for %q", user.Username)
				user.PasswordHash = hash
			}
		}
	}
	// The hash stays in the returned value on purpose: ChangePassword needs it to
	// compare-and-swap against. VerifyAdminPassword clears it for its callers.
	return user, nil
}

func (s *Store) ChangePassword(ctx context.Context, username, currentPassword, newPassword string) error {
	username = strings.TrimSpace(username)
	if err := auth.ValidatePassword(newPassword); err != nil {
		return fmt.Errorf("%w: %v", ErrWeakPassword, err)
	}
	// One lookup, and it carries the hash we are replacing: verifying and then
	// re-reading the user would leave a window in which a concurrent change is
	// silently overwritten.
	user, err := s.verifyAdminPassword(ctx, username, currentPassword)
	if err != nil {
		return err
	}
	hash, err := s.hasher.Hash(newPassword)
	if err != nil {
		return err
	}
	if err := s.setPasswordHash(ctx, user.ID, hash, user.PasswordHash); err != nil {
		return err
	}
	// Any other session of this account is now stale: log them out.
	if err := s.DeleteUserSessions(ctx, user.ID); err != nil {
		log.Printf("mongo: dropping sessions of %q: %v", username, err)
	}
	return nil
}

// setPasswordHash writes the new verifier only if the account still holds
// expectedHash, which makes verify-then-write a compare-and-swap: two concurrent
// password changes cannot both land.
func (s *Store) setPasswordHash(ctx context.Context, userID bson.ObjectID, hash, expectedHash string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	res, err := s.collection(userCollection).UpdateOne(ctx,
		bson.M{"_id": userID, "passwordHash": expectedHash},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "passwordHash", Value: hash},
			{Key: "updatedAt", Value: time.Now()},
		}}},
	)
	if err != nil {
		return fmt.Errorf("mongo: update password hash: %w", err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("%w: password of %s changed concurrently", ErrConflict, userID.Hex())
	}
	return nil
}

// CreateSession starts a login session and returns the cookie value. Only the
// SHA-256 of that value is stored.
func (s *Store) CreateSession(ctx context.Context, userID bson.ObjectID, username string, ttl time.Duration) (token string, session domain.Session, err error) {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	token, err = auth.NewToken()
	if err != nil {
		return "", domain.Session{}, err
	}
	now := time.Now()
	session = domain.Session{
		ID:        bson.NewObjectID(),
		TokenHash: auth.TokenHash(token),
		UserID:    userID,
		Username:  username,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}

	opCtx, cancel := s.opCtx(ctx)
	defer cancel()
	if _, err := s.collection(sessionCollection).InsertOne(opCtx, &session); err != nil {
		return "", domain.Session{}, fmt.Errorf("mongo: create session: %w", err)
	}
	return token, session, nil
}

// Session resolves a cookie value into a live session. When the deadline gets
// close (less than half the lifetime left) the expiry is slid forward by ttl,
// which is what the caller configured with -session-ttl; the second result says
// whether that happened, so the handler can re-issue the browser cookie - the
// database alone would not extend anything the browser can see.
func (s *Store) Session(ctx context.Context, token string, ttl time.Duration) (domain.Session, bool, error) {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return domain.Session{}, false, ErrSessionExpired
	}
	hash := auth.TokenHash(token)

	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	var session domain.Session
	err := s.collection(sessionCollection).FindOne(ctx, bson.M{"tokenHash": hash}).Decode(&session)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return domain.Session{}, false, ErrSessionExpired
	}
	if err != nil {
		return domain.Session{}, false, fmt.Errorf("mongo: load session: %w", err)
	}
	if session.Expired(time.Now()) {
		_, _ = s.collection(sessionCollection).DeleteOne(ctx, bson.M{"_id": session.ID})
		return domain.Session{}, false, ErrSessionExpired
	}

	// The threshold is relative to the configured lifetime: with -session-ttl
	// 30m a fixed one hour window would renew on literally every request.
	if remaining := time.Until(session.ExpiresAt); remaining > 0 && remaining < ttl/2 {
		newExpiry := time.Now().Add(ttl)
		if _, err := s.collection(sessionCollection).UpdateOne(ctx,
			bson.M{"_id": session.ID},
			bson.D{{Key: "$set", Value: bson.D{{Key: "expiresAt", Value: newExpiry}}}},
		); err != nil {
			// Not fatal: the session is still valid, it simply will not slide.
			log.Printf("mongo: extending session: %v", err)
			return session, false, nil
		}
		session.ExpiresAt = newExpiry
		return session, true, nil
	}
	return session, false, nil
}

// DeleteSession logs one cookie out.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	if _, err := s.collection(sessionCollection).DeleteMany(ctx, bson.M{"tokenHash": auth.TokenHash(token)}); err != nil {
		return fmt.Errorf("mongo: delete session: %w", err)
	}
	return nil
}

// DeleteUserSessions logs every session of an account out (password change).
func (s *Store) DeleteUserSessions(ctx context.Context, userID bson.ObjectID) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()

	if _, err := s.collection(sessionCollection).DeleteMany(ctx, bson.M{"userId": userID}); err != nil {
		return fmt.Errorf("mongo: delete sessions of user: %w", err)
	}
	return nil
}

// authIndexes is appended to EnsureIndexes: unique usernames, unique session
// tokens and a TTL so expired sessions disappear by themselves.
func (s *Store) authIndexes() []indexSpec {
	return []indexSpec{
		namedIndex(userCollection, "uniq_username", bson.D{{Key: "username", Value: 1}}, func(o *options.IndexOptionsBuilder) { o.SetUnique(true) }),
		namedIndex(sessionCollection, "uniq_tokenHash", bson.D{{Key: "tokenHash", Value: 1}}, func(o *options.IndexOptionsBuilder) { o.SetUnique(true) }),
		namedIndex(sessionCollection, "ttl_expiresAt", bson.D{{Key: "expiresAt", Value: 1}}, func(o *options.IndexOptionsBuilder) { o.SetExpireAfterSeconds(0) }),
		namedIndex(sessionCollection, "idx_userId", bson.D{{Key: "userId", Value: 1}}, nil),
	}
}
