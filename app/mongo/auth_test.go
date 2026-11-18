package mongo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jelinden/rssfetcher/app/auth"
	"github.com/jelinden/rssfetcher/app/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	adminName = "seed-admin"
	adminPass = "test-admin-password-1"
)

func TestEnsureAdminUserStoresOnlyAHash(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	created, err := store.EnsureAdminUser(ctx, adminName, adminPass)
	if err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}
	if !created {
		t.Error("created = false for a brand new account")
	}

	var doc bson.M
	if err := store.collection(userCollection).FindOne(ctx, bson.M{"username": adminName}).Decode(&doc); err != nil {
		t.Fatalf("read back user: %v", err)
	}
	hash, _ := doc["passwordHash"].(string)
	if !strings.HasPrefix(hash, auth.HashPrefix+"$") {
		t.Fatalf("stored passwordHash = %q, want a self describing scrypt verifier", hash)
	}
	if strings.Contains(hash, adminPass) {
		t.Fatal("the plaintext password ended up in the stored hash")
	}
	// Nothing in the document may contain the plaintext password.
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), adminPass) {
		t.Fatal("the stored document contains the plaintext password")
	}

	// The seeded credentials have to work.
	if _, err := store.VerifyAdminPassword(ctx, adminName, adminPass); err != nil {
		t.Fatalf("VerifyAdminPassword with the seeded credentials: %v", err)
	}
}

// Seeding must be idempotent and must never clobber an existing password, or a
// restart with the default flags would silently undo a rotation.
func TestEnsureAdminUserDoesNotOverwrite(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}
	first, err := store.AdminUserByName(ctx, adminName)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.EnsureAdminUser(ctx, adminName, "another-password-1")
	if err != nil {
		t.Fatalf("second EnsureAdminUser: %v", err)
	}
	if created {
		t.Error("the account was created twice")
	}
	second, err := store.AdminUserByName(ctx, adminName)
	if err != nil {
		t.Fatal(err)
	}
	if first.PasswordHash != second.PasswordHash {
		t.Error("the stored password hash was replaced by the second seed")
	}
	if _, err := store.VerifyAdminPassword(ctx, adminName, adminPass); err != nil {
		t.Error("the original password stopped working after re-seeding")
	}
}

func TestEnsureAdminUserValidatesInput(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if _, err := store.EnsureAdminUser(ctx, "  ", adminPass); err == nil {
		t.Error("an empty username was accepted")
	}
	if _, err := store.EnsureAdminUser(ctx, adminName, "short"); err == nil {
		t.Error("a password below the minimum policy was accepted")
	}
}

func TestAdminUsernameIndexIsUnique(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}
	_, err := store.collection(userCollection).InsertOne(ctx, &domain.AdminUser{ID: bson.NewObjectID(), Username: adminName, PasswordHash: "x"})
	if err == nil {
		t.Fatal("a duplicate username was accepted, the unique index is missing")
	}
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("err = %v, want a duplicate key error", err)
	}
}

func TestVerifyAdminPassword(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}

	user, err := store.VerifyAdminPassword(ctx, adminName, adminPass)
	if err != nil {
		t.Fatalf("VerifyAdminPassword: %v", err)
	}
	if user.Username != adminName {
		t.Errorf("username = %q", user.Username)
	}
	if user.PasswordHash != "" {
		t.Error("the password hash left the store; do not hand it to callers that cannot need it")
	}

	for _, wrong := range []string{"", "Emerge008", strings.ToUpper(adminPass), adminPass + " "} {
		if _, err := store.VerifyAdminPassword(ctx, adminName, wrong); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("password %q: err = %v, want ErrInvalidCredentials", wrong, err)
		}
	}
	// An unknown username is reported with the very same error.
	if _, err := store.VerifyAdminPassword(ctx, "who-is-this", adminPass); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown user: err = %v, want ErrInvalidCredentials", err)
	}
}

func TestVerifyAdminPasswordRejectsUnparsableHash(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	hash, err := store.hasher.Hash(adminPass)
	if err != nil {
		t.Fatal(err)
	}
	user := domain.AdminUser{ID: bson.NewObjectID(), Username: adminName, PasswordHash: "plaintext-left-over-from-an-old-version"}
	if _, err := store.collection(userCollection).InsertOne(ctx, &user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyAdminPassword(ctx, adminName, adminPass); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials: a broken hash must never be a login bypass", err)
	}
	// Sanity: the same account with a proper hash does work.
	if err := store.setPasswordHash(ctx, user.ID, hash, user.PasswordHash); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyAdminPassword(ctx, adminName, adminPass); err != nil {
		t.Fatalf("VerifyAdminPassword after fixing the hash: %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}

	const nextPass = "a-brand-new-password"

	if err := store.ChangePassword(ctx, adminName, "wrong-current", nextPass); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v, want ErrInvalidCredentials", err)
	}
	if err := store.ChangePassword(ctx, adminName, adminPass, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("err = %v, want ErrWeakPassword", err)
	}

	// Sessions of the account must not survive a password change.
	if _, err := store.EnsureAdminUser(ctx, "rotator", adminPass); err != nil {
		t.Fatal(err)
	}
	rotator, err := store.AdminUserByName(ctx, "rotator")
	if err != nil {
		t.Fatal(err)
	}
	keepToken, keepSession, err := store.CreateSession(ctx, rotator.ID, rotator.Username, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, _, err := store.CreateSession(ctx, rotator.ID, rotator.Username, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Session(ctx, keepToken, DefaultSessionTTL); err != nil {
		t.Fatalf("session before the change: %v", err)
	}

	if err := store.ChangePassword(ctx, "rotator", adminPass, nextPass); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, _, err := store.Session(ctx, keepToken, DefaultSessionTTL); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("session %s survived the password change: %v", keepSession.ID.Hex(), err)
	}
	if _, _, err := store.Session(ctx, otherToken, DefaultSessionTTL); !errors.Is(err, ErrSessionExpired) {
		t.Error("the other session survived the password change")
	}
	if _, err := store.VerifyAdminPassword(ctx, "rotator", nextPass); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if _, err := store.VerifyAdminPassword(ctx, "rotator", adminPass); !errors.Is(err, ErrInvalidCredentials) {
		t.Error("the old password still works")
	}
}

// A password hash written with an older, cheaper policy is re-derived the next
// time the user logs in, without any admin action.
func TestVerifyAdminPasswordUpgradesWeakHash(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	weak, err := (&auth.Hasher{Params: auth.Params{N: 2, R: 1, P: 1, KeyLen: 16}}).Hash(adminPass)
	if err != nil {
		t.Fatal(err)
	}
	user := domain.AdminUser{ID: bson.NewObjectID(), Username: "upgrade-me", PasswordHash: weak, CreatedAt: time.Now()}
	if _, err := store.collection(userCollection).InsertOne(ctx, &user); err != nil {
		t.Fatal(err)
	}
	if !auth.NeedsUpgrade(weak) {
		t.Fatal("test fixture is not considered outdated")
	}

	// The current policy (this costs ~100ms, so it is only done here).
	store.WithHasher(auth.NewHasher())

	if _, err := store.VerifyAdminPassword(ctx, "upgrade-me", adminPass); err != nil {
		t.Fatalf("VerifyAdminPassword: %v", err)
	}
	stored, err := store.AdminUserByName(ctx, "upgrade-me")
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash == weak {
		t.Error("the outdated password hash was left in place")
	}
	if auth.NeedsUpgrade(stored.PasswordHash) {
		t.Errorf("the upgraded hash still needs an upgrade: %q", stored.PasswordHash)
	}
	if _, err := store.VerifyAdminPassword(ctx, "upgrade-me", adminPass); err != nil {
		t.Errorf("login with the re-hashed password: %v", err)
	}
}

func TestSessionsLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}
	user, err := store.AdminUserByName(ctx, adminName)
	if err != nil {
		t.Fatal(err)
	}

	token, session, err := store.CreateSession(ctx, user.ID, user.Username, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if token == "" {
		t.Fatal("empty session token")
	}

	var doc bson.M
	if err := store.collection(sessionCollection).FindOne(ctx, bson.M{"_id": session.ID}).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	// Only the digest of the cookie value is stored, so a leaked dump cannot be
	// replayed as a session cookie.
	if stored, _ := doc["tokenHash"].(string); stored != auth.TokenHash(token) {
		t.Errorf("stored tokenHash = %q", stored)
	}
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("the raw session token was stored in the database")
	}

	got, _, err := store.Session(ctx, token, DefaultSessionTTL)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.Username != user.Username || got.UserID != user.ID {
		t.Errorf("session = %+v, want it bound to %s", got, user.Username)
	}

	if _, _, err := store.Session(ctx, "forged-token", DefaultSessionTTL); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("err = %v, want ErrSessionExpired", err)
	}
	if _, _, err := store.Session(ctx, "", DefaultSessionTTL); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("empty token: err = %v, want ErrSessionExpired", err)
	}

	if err := store.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, _, err := store.Session(ctx, token, DefaultSessionTTL); !errors.Is(err, ErrSessionExpired) {
		t.Error("the session survived logout")
	}
}

func TestExpiredSessionIsRejectedEvenIfStored(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}
	user, err := store.AdminUserByName(ctx, adminName)
	if err != nil {
		t.Fatal(err)
	}
	token, session, err := store.CreateSession(ctx, user.ID, user.Username, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.collection(sessionCollection).UpdateOne(ctx,
		bson.M{"_id": session.ID},
		bson.D{{Key: "$set", Value: bson.D{{Key: "expiresAt", Value: time.Now().Add(-time.Minute)}}}},
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Session(ctx, token, DefaultSessionTTL); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v, want ErrSessionExpired", err)
	}
	// And the stale document is cleaned up right away.
	count, err := store.collection(sessionCollection).CountDocuments(ctx, bson.M{"_id": session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d expired sessions left in the collection", count)
	}
}

func TestSessionTokenIndexIsUnique(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	token, _, err := store.CreateSession(ctx, bson.NewObjectID(), adminName, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.collection(sessionCollection).InsertOne(ctx, &domain.Session{
		ID: bson.NewObjectID(), TokenHash: auth.TokenHash(token), Username: adminName, ExpiresAt: time.Now().Add(time.Hour),
	})
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("err = %v, want a duplicate key error", err)
	}
}

// The TTL index is what actually removes expired sessions from the collection.
func TestSessionTTLIndex(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes: %v", err)
	}
	cursor, err := store.collection(sessionCollection).Indexes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var specs []bson.M
	if err := cursor.All(ctx, &specs); err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		if spec["name"] != "ttl_expiresAt" {
			continue
		}
		expireAfter, ok := spec["expireAfterSeconds"]
		if !ok {
			t.Fatalf("index %v has no expireAfterSeconds", spec)
		}
		if reflect.ValueOf(expireAfter).Int() != 0 {
			t.Errorf("expireAfterSeconds = %v, want 0 (expiry is stored per document)", expireAfter)
		}
		return
	}
	t.Errorf("no ttl index found in %v", specs)
}

// Deleting an account has to take its sessions with it.
func TestDeleteUserSessions(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	userID := bson.NewObjectID()
	otherID := bson.NewObjectID()

	if _, _, err := store.CreateSession(ctx, userID, "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	keepToken, _, err := store.CreateSession(ctx, otherID, "b", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUserSessions(ctx, userID); err != nil {
		t.Fatal(err)
	}
	total, err := store.collection(sessionCollection).CountDocuments(ctx, bson.M{"userId": userID})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("%d sessions left for the deleted user", total)
	}
	if _, _, err := store.Session(ctx, keepToken, DefaultSessionTTL); err != nil {
		t.Errorf("an unrelated session was dropped: %v", err)
	}
}

// A session is renewed once less than half of the *configured* lifetime is
// left, and a request in the middle of it must not turn into a write.
func TestSessionIsRenewedOnlyNearTheDeadline(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const ttl = 2 * time.Hour
	user := seedAdmin(t, store, ctx)
	token, session := newSession(t, store, ctx, user, ttl)

	// Plenty of time left: no renewal. BSON dates only keep milliseconds, so the
	// comparison is done at that precision.
	got, extended, err := store.Session(ctx, token, ttl)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if extended {
		t.Error("a fresh session was reported as extended")
	}
	if delta := got.ExpiresAt.Sub(session.ExpiresAt.Truncate(time.Millisecond)); delta > time.Millisecond || delta < -time.Millisecond {
		t.Errorf("a fresh session was renewed: %v != %v", got.ExpiresAt, session.ExpiresAt)
	}

	// Inside the renewal window (less than half the lifetime left).
	forceExpiry(t, store, ctx, session.ID, time.Now().Add(10*time.Minute))
	got, extended, err = store.Session(ctx, token, ttl)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if !extended {
		t.Error("a session near its deadline must be reported as extended, so the cookie is re-issued")
	}
	if remaining := time.Until(got.ExpiresAt); remaining < ttl-time.Minute || remaining > ttl+time.Minute {
		t.Errorf("remaining lifetime after renewal = %s, want ~%s", remaining, ttl)
	}
}

// The renewal used to hardcode DefaultSessionTTL, so -session-ttl was silently
// ignored: a 30 minute session was extended to 12 hours.
func TestSessionRenewalHonoursTheConfiguredTTL(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const ttl = 30 * time.Minute
	user := seedAdmin(t, store, ctx)
	token, session := newSession(t, store, ctx, user, ttl)
	forceExpiry(t, store, ctx, session.ID, time.Now().Add(4*time.Minute))

	got, extended, err := store.Session(ctx, token, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if !extended {
		t.Fatal("want the session to be extended")
	}
	if remaining := time.Until(got.ExpiresAt); remaining > ttl+time.Minute {
		t.Errorf("session was extended to %s, want at most the configured %s", remaining, ttl)
	}
	if DefaultSessionTTL == ttl {
		t.Fatal("this test is only meaningful when the default differs from the configured ttl")
	}
}

// With a short TTL the renewal window has to shrink with it; the old absolute
// one hour threshold meant every single request wrote to the database.
func TestSessionRenewalIsNotTriggeredByAShortTTL(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const ttl = 10 * time.Minute
	user := seedAdmin(t, store, ctx)
	token, session := newSession(t, store, ctx, user, ttl)
	// 8 minutes left of a 10 minute session: far from the deadline.
	forceExpiry(t, store, ctx, session.ID, time.Now().Add(8*time.Minute))

	if _, extended, err := store.Session(ctx, token, ttl); err != nil {
		t.Fatal(err)
	} else if extended {
		t.Errorf("a session with 8 of %s left was renewed; the threshold must be relative to the ttl", ttl)
	}
}

// A password change must not overwrite a concurrent one.
func TestSetPasswordHashIsCompareAndSwap(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}
	user, err := store.AdminUserByName(ctx, adminName)
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := store.hasher.Hash("another-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setPasswordHash(ctx, user.ID, newHash, "hash-of-something-else"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict when the stored hash moved underneath us", err)
	}
	unchanged, err := store.AdminUserByName(ctx, adminName)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.PasswordHash != user.PasswordHash {
		t.Error("the hash was replaced even though the expected value did not match")
	}
	if err := store.setPasswordHash(ctx, user.ID, newHash, user.PasswordHash); err != nil {
		t.Fatalf("matching hash: %v", err)
	}
}

func seedAdmin(t *testing.T, store *Store, ctx context.Context) domain.AdminUser {
	t.Helper()
	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}
	user, err := store.AdminUserByName(ctx, adminName)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func newSession(t *testing.T, store *Store, ctx context.Context, user domain.AdminUser, ttl time.Duration) (string, domain.Session) {
	t.Helper()
	token, session, err := store.CreateSession(ctx, user.ID, user.Username, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return token, session
}

func forceExpiry(t *testing.T, store *Store, ctx context.Context, id bson.ObjectID, at time.Time) {
	t.Helper()
	if _, err := store.collection(sessionCollection).UpdateOne(ctx,
		bson.M{"_id": id},
		bson.D{{Key: "$set", Value: bson.D{{Key: "expiresAt", Value: at}}}},
	); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultSessionTTLIsUsedWhenUnset(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}
	user, err := store.AdminUserByName(ctx, adminName)
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := store.CreateSession(ctx, user.ID, user.Username, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ttl := session.ExpiresAt.Sub(session.CreatedAt); ttl != DefaultSessionTTL {
		t.Errorf("session ttl = %s, want %s", ttl, DefaultSessionTTL)
	}
}

func TestAuthOptionsIndexesAreCreatedByName(t *testing.T) {
	store := testStore(t)
	if len(store.authIndexes()) != 4 {
		t.Errorf("authIndexes() = %d specs, want 4", len(store.authIndexes()))
	}
	for _, spec := range store.authIndexes() {
		if spec.collection != userCollection && spec.collection != sessionCollection {
			t.Errorf("unexpected collection %q", spec.collection)
		}
	}
	// Sanity check that the driver option builders we rely on exist and are
	// configured (compile time protection for the v2 migration).
	if options.Index().SetUnique(true) == nil {
		t.Fatal("options.Index() returned nil")
	}
}

func TestHasAdminUsers(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	got, err := store.HasAdminUsers(ctx)
	if err != nil {
		t.Fatalf("HasAdminUsers on an empty collection: %v", err)
	}
	if got {
		t.Fatal("an empty usercollection reported an admin account")
	}

	if _, err := store.EnsureAdminUser(ctx, adminName, adminPass); err != nil {
		t.Fatal(err)
	}
	got, err = store.HasAdminUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("a seeded account was not reported")
	}
}
