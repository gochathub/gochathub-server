package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	_ "github.com/jackc/pgx/v5/stdlib"

	migrations "github.com/gochathub/gochathub-server/db"
	"github.com/gochathub/gochathub-server/internal/app"
	"github.com/gochathub/gochathub-server/internal/config"
	"github.com/gochathub/gochathub-server/internal/pwd"
	"github.com/gochathub/gochathub-server/internal/store"
	"github.com/gochathub/gochathub-server/internal/testutil"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// rocketBcrypt builds the raw RC password value (the migration adds the
// bcrypt$ prefix): bcrypt over the hex SHA-256 digest string.
func rocketBcrypt(t *testing.T, password string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(password))
	h, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(sum[:])), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

type collDoc struct {
	coll string
	doc  bson.M
}

var collOrder = []string{
	"users", "rocketchat_room", "rocketchat_subscription", "rocketchat_message",
	"rocketchat_uploads", "rocketchat_uploads.files", "rocketchat_uploads.chunks",
	"rocketchat_avatars", "rocketchat_avatars.files", "rocketchat_avatars.chunks",
}

func writeFixture(t *testing.T, docs []collDoc) string {
	t.Helper()
	byColl := map[string][]bson.M{}
	for _, cd := range docs {
		byColl[cd.coll] = append(byColl[cd.coll], cd.doc)
	}
	var frames [][]byte
	frames = append(frames, marshDoc(t, bson.M{"version": "0.1", "server_version": "8.2"}))
	sentinel := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	for _, coll := range collOrder {
		list := byColl[coll]
		if len(list) == 0 {
			continue
		}
		frames = append(frames, marshDoc(t, metaDoc("rocketchat", coll)))
		for _, d := range list {
			frames = append(frames, marshDoc(t, d))
		}
		frames = append(frames, sentinel)
	}
	return writeArchiveFile(t, frames...)
}

func baseFixture(t *testing.T) []collDoc {
	t.Helper()
	T0 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	T1 := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	T2 := time.Date(2021, 1, 2, 0, 0, 0, 0, time.UTC)
	T3 := time.Date(2021, 1, 3, 0, 0, 0, 0, time.UTC)
	return []collDoc{
		{coll: "users", doc: bson.M{
			"_id": "aliceid", "username": "Alice", "name": "Alice A", "type": "user",
			"emails": []any{bson.M{"address": "ALICE@example.com", "verified": true}},
			"roles":  []any{"admin"},
			"active": true, "createdAt": T0, "lastLogin": T1,
			"services": bson.M{"password": bson.M{"bcrypt": rocketBcrypt(t, "hunter2")}},
		}},
		// bob: 2FA enrolled, deactivated, and his dead token material must not
		// land in the sidecar.
		{coll: "users", doc: bson.M{
			"_id": "bobby", "username": "bob", "name": "Bobby B", "type": "user", "active": false,
			"services": bson.M{
				"totp":     bson.M{"enabled": true, "secret": "BBBBBASE32", "hashedBackup": []any{"one", "two"}},
				"resume":   bson.M{"tokenSecrets": []any{"dead-token"}},
				"cloud":    bson.M{"accessToken": "dead-token"},
				"password": bson.M{"bcrypt": rocketBcrypt(t, "bopass")},
			},
		}},
		{coll: "users", doc: bson.M{"_id": "appid", "username": "clamav.bot", "type": "app"}},

		{coll: "rocketchat_room", doc: bson.M{"_id": "GENERAL", "t": "c", "fname": "General", "topic": "gen topic", "ts": T1, "u": bson.M{"_id": "aliceid", "username": "Alice"}}},
		{coll: "rocketchat_room", doc: bson.M{"_id": "alicebob", "t": "d", "usernames": []any{"BOB", "alice"}, "ts": T1}},
		{coll: "rocketchat_room", doc: bson.M{"_id": "disc1", "t": "p", "prid": "GENERAL", "fname": "discussion"}},
		{coll: "rocketchat_room", doc: bson.M{"_id": "live1", "t": "l", "ts": T1}},

		{coll: "rocketchat_subscription", doc: bson.M{"rid": "GENERAL", "u": bson.M{"_id": "aliceid"}, "roles": []any{"owner"}, "ts": T1, "ls": T2}},
		{coll: "rocketchat_subscription", doc: bson.M{"rid": "GENERAL", "u": bson.M{"_id": "bobby"}, "roles": []any{"user"}, "ts": T1}},
		{coll: "rocketchat_subscription", doc: bson.M{"rid": "alicebob", "u": bson.M{"_id": "aliceid"}, "ts": T1}},
		{coll: "rocketchat_subscription", doc: bson.M{"rid": "alicebob", "u": bson.M{"_id": "bobby"}, "ts": T1}},

		{coll: "rocketchat_message", doc: bson.M{
			"_id": "m1", "rid": "GENERAL", "msg": "Hello", "ts": T1,
			"u":      bson.M{"_id": "aliceid", "username": "alice"},
			"pinned": true, "pinnedAt": T2,
			"editedAt": T2, "edits": []any{bson.M{"msg": "old body", "ts": T0}},
			"reactions": bson.M{":thumbsup:": bson.M{"usernames": []any{"Alice", "bob"}}},
			"mentions":  []any{bson.M{"_id": "bobby", "username": "bob"}},
		}},
		{coll: "rocketchat_message", doc: bson.M{
			"_id": "m2", "rid": "GENERAL", "msg": "[ ](https://old.example/group/general?msg=m1) @alice check", "ts": T2,
			"u": bson.M{"_id": "bobby", "username": "bob"},
		}},
		{coll: "rocketchat_message", doc: bson.M{
			"_id": "m5", "rid": "GENERAL", "msg": "gone", "ts": T2, "deletedAt": T3,
			"u": bson.M{"_id": "bobby", "username": "bob"},
		}},
		{coll: "rocketchat_message", doc: bson.M{
			"_id": "m6f", "rid": "GENERAL", "msg": "see pic", "ts": T3,
			"u":           bson.M{"_id": "aliceid", "username": "alice"},
			"file":        bson.M{"_id": "fileX", "name": "pic.png", "type": "image/png"},
			"attachments": []any{bson.M{"type": "file", "image_url": "/file-upload/fileX/pic", "image_dimensions": bson.M{"width": int32(10), "height": int32(20)}}},
		}},
		{coll: "rocketchat_message", doc: bson.M{"_id": "m3", "rid": "skippedroom", "msg": "x", "ts": T3, "u": bson.M{"_id": "aliceid", "username": "alice"}}},
		{coll: "rocketchat_message", doc: bson.M{"_id": "m4junk", "rid": "GENERAL", "t": "uj", "ts": T3, "u": bson.M{"_id": "aliceid"}}},

		{coll: "rocketchat_uploads", doc: bson.M{"_id": "fileX", "name": "uploaded-name.png", "rid": "GENERAL", "userId": "bobby", "type": "image/png", "size": int32(5), "uploadedAt": T3, "complete": true}},
		{coll: "rocketchat_uploads.files", doc: bson.M{"_id": "fileX", "length": int32(5), "chunkSize": int32(5), "uploadDate": T3, "filename": "fileX", "contentType": "image/png"}},
		{coll: "rocketchat_uploads.chunks", doc: bson.M{"_id": "c0", "files_id": "fileX", "n": int32(0), "data": bson.Binary{Data: []byte("hel")}}},
		{coll: "rocketchat_uploads.chunks", doc: bson.M{"_id": "c1", "files_id": "fileX", "n": int32(1), "data": bson.Binary{Data: []byte("lo")}}},

		{coll: "rocketchat_avatars", doc: bson.M{"_id": "avbobOld", "userId": "bobby", "type": "image/png", "uploadedAt": T0}},
		{coll: "rocketchat_avatars.files", doc: bson.M{"_id": "avbobOld", "length": int32(1), "contentType": "image/png", "uploadDate": T0}},
		{coll: "rocketchat_avatars.chunks", doc: bson.M{"_id": "ac0", "files_id": "avbobOld", "n": int32(0), "data": bson.Binary{Data: []byte("z")}}},
		{coll: "rocketchat_avatars", doc: bson.M{"_id": "avbob", "userId": "bobby", "type": "image/png", "uploadedAt": T2}},
		{coll: "rocketchat_avatars.files", doc: bson.M{"_id": "avbob", "length": int32(3), "contentType": "image/png", "uploadDate": T2}},
		{coll: "rocketchat_avatars.chunks", doc: bson.M{"_id": "ac1", "files_id": "avbob", "n": int32(0), "data": bson.Binary{Data: []byte("abc")}}},
	}
}

func truncateAll(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.Q.Exec(context.Background(), `
		TRUNCATE users, attachments, rooms, app_config, audit_log, devices,
		contacts, sessions, api_tokens, push_endpoints, notification_preferences,
		messages, message_receipts, message_revisions, message_reactions,
		message_mentions, message_attachments, room_invites, room_members,
		migrate_rc_users CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func testStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	url := testutil.Database(t)
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := migrations.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sqlDB.Close()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	st := store.New(pool)
	truncateAll(t, st)
	return st, pool.Close
}

func countRows(t *testing.T, st *store.Store, stmt string) int {
	t.Helper()
	var n int
	if err := st.Q.QueryRow(context.Background(), stmt).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", stmt, err)
	}
	return n
}

func queryText(t *testing.T, st *store.Store, stmt, label string) string {
	t.Helper()
	var s string
	if err := st.Q.QueryRow(context.Background(), stmt).Scan(&s); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	return s
}

// rcRun wraps RcMigrate for the shared test DB: parallel test packages
// TRUNCATE concurrent tables and can deadlock our inserts. The import is
// idempotent, so truncating and retrying is a correct recovery
// (production runs against a dedicated empty DB, where this cannot happen).
func rcRun(t *testing.T, st *store.Store, opts RcOpts) (Summary, error) {
	t.Helper()
	for attempt := 0; ; attempt++ {
		sum, err := RcMigrate(t.Context(), st, nil, opts)
		if err == nil || !strings.Contains(err.Error(), "deadlock") || attempt == 2 {
			if err != nil {
				err = fmt.Errorf("attempt %d: %w", attempt+1, err)
			}
			return sum, err
		}
		truncateAll(t, st)
	}
}

func TestRcMigratePipeline(t *testing.T) {
	st, closeDB := testStore(t)
	defer closeDB()
	ctx := t.Context()
	archive := writeFixture(t, baseFixture(t))

	// precondition: refuse a non-empty database on the first run
	if _, err := st.Q.Exec(ctx, `
		INSERT INTO users (id, username, display_name, password_hash)
		VALUES (gen_random_uuid(), 'preexisting', 'x', 'argon2id$1,65536,4,32$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := rcRun(t, st, RcOpts{Archive: archive, DryRun: true}); err == nil {
		t.Fatal("non-empty DB accepted without watermark")
	}
	truncateAll(t, st)

	// dry run: counts only, nothing written
	sum, err := rcRun(t, st, RcOpts{Archive: archive, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if sum.Users != 2 || sum.UsersSkipped != 1 || sum.Rooms != 2 || sum.RoomsSkipped != 2 ||
		sum.Members != 4 || sum.Contacts != 2 || sum.Messages != 4 || sum.MessagesSkipped != 2 ||
		sum.Revisions != 1 || sum.Reactions != 2 || sum.Mentions != 1 ||
		sum.Files != 1 || sum.FilesOrphaned != 0 || sum.Avatars != 1 || sum.UsersWith2FA != 1 {
		t.Fatalf("dry summary mismatch: %+v", sum)
	}
	if countRows(t, st, "SELECT count(*) FROM users") != 0 {
		t.Fatal("dry run wrote users")
	}

	// real run without files
	sum, err = rcRun(t, st, RcOpts{Archive: archive, SkipFiles: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Tombstones != 0 || sum.UsersNoPassword != 0 {
		t.Fatalf("unexpected tombstones/no-password: %+v", sum)
	}

	// user content: role, normalization, disabled state
	alice := queryText(t, st, "SELECT id FROM users WHERE username = 'alice'", "alice id")
	if got := queryText(t, st, "SELECT role FROM users WHERE email = 'alice@example.com' AND password_hash LIKE 'bcrypt$%' AND enabled", "alice row"); got != "admin" {
		t.Fatalf("alice role %q want admin", got)
	}
	// dm room: direct type, creator = lowest username
	if got := queryText(t, st, "SELECT created_by::text FROM rooms WHERE type = 'direct'", "dm creator"); got != alice {
		t.Fatalf("dm creator %q want %q", got, alice)
	}
	if countRows(t, st, "SELECT count(*) FROM room_members WHERE role = 'admin'") != 1 {
		t.Fatal("owner not promoted to admin")
	}
	if countRows(t, st, "SELECT count(*) FROM contacts") != 2 {
		t.Fatal("contacts missing")
	}
	// quote-link stripping + trim
	if got := queryText(t, st, `SELECT body FROM messages WHERE body LIKE '@alice%'`, "m2 body"); got != "@alice check" {
		t.Fatalf("m2 body %q", got)
	}
	// edit history, tombstone, pin, cursor, reaction, mention
	var revs, dels, pins, cursors, rx, men int
	if err := st.Q.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM message_revisions),
		       (SELECT count(*) FROM messages WHERE deleted_at IS NOT NULL),
		       (SELECT count(*) FROM rooms WHERE pinned_message_id IS NOT NULL),
		       (SELECT count(*) FROM room_members WHERE last_read_message_id IS NOT NULL),
		       (SELECT count(*) FROM message_reactions),
		       (SELECT count(*) FROM message_mentions)`).Scan(&revs, &dels, &pins, &cursors, &rx, &men); err != nil {
		t.Fatalf("history counts: %v", err)
	}
	if revs != 1 || dels != 1 || pins != 1 || cursors != 1 || rx != 2 || men != 1 {
		t.Fatalf("history counts: revs=%d dels=%d pins=%d cursors=%d rx=%d men=%d", revs, dels, pins, cursors, rx, men)
	}
	// 2FA sidecar: enrollment kept, dead tokens dropped
	if got := queryText(t, st, `SELECT services::text FROM migrate_rc_users WHERE username = 'bob'`, "2fa sidecar"); !strings.Contains(got, "BBBBBASE32") || strings.Contains(got, "dead-token") {
		t.Fatalf("sidecar sanitize wrong: %s", got)
	}

	// re-run without files: messages re-import (files_pending watermark), rows unchanged
	again, err := rcRun(t, st, RcOpts{Archive: archive, SkipFiles: true})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if again.Messages != 4 || again.Rooms != 2 || again.Users != 2 {
		t.Fatalf("rerun replay mismatch: %+v", again)
	}

	// delta cursor as a files-mode run would leave it: base replays nothing
	if _, err := st.Q.Exec(ctx, `
		INSERT INTO app_config (key, value) VALUES ('migrate.rc', '{"message_ts_max":"2021-01-03T00:00:00Z"}')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if sum, err = rcRun(t, st, RcOpts{Archive: archive, SkipFiles: true}); err != nil || sum.Messages != 0 {
		t.Fatalf("watermarked rerun imported %d messages err=%v", sum.Messages, err)
	}

	// delta: a newer archive contributes only its newer document
	fx := append(baseFixture(t), collDoc{coll: "rocketchat_message", doc: bson.M{
		"_id": "m9", "rid": "GENERAL", "msg": "new arrival", "ts": time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC),
		"u": bson.M{"_id": "bobby", "username": "bob"},
	}})
	sum, err = rcRun(t, st, RcOpts{Archive: writeFixture(t, fx), SkipFiles: true})
	if err != nil {
		t.Fatalf("delta run: %v", err)
	}
	if sum.Messages != 1 {
		t.Fatalf("delta imported %d messages, want 1", sum.Messages)
	}
	if countRows(t, st, "SELECT count(*) FROM messages") != 5 {
		t.Fatal("delta duplicated rows")
	}
}

func TestLoginRehashesBcrypt(t *testing.T) {
	st, closeDB := testStore(t)
	defer closeDB()
	ctx := t.Context()
	archive := writeFixture(t, baseFixture(t))
	if _, err := rcRun(t, st, RcOpts{Archive: archive, SkipFiles: true}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	svc, _, err := app.Assemble(st, &config.Config{SessionTTL: time.Hour, RateLimitRPM: 10_000}, log)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	row, err := st.UserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("alice lookup: %v", err)
	}
	ok, verr := pwd.Verify(row.PasswordHash, "hunter2")
	t.Logf("probe verify ok=%v err=%v pwdHash=%q", ok, verr, row.PasswordHash)
	if !ok {
		t.Fatalf("pwd.Verify rejected the migrated hash")
	}
	token, _, err := svc.Auth.Login(ctx, "alice", "hunter2", "", nil)
	if err != nil || token == "" {
		t.Fatalf("login with migrated bcrypt: token=%q err=%v", token, err)
	}
	if got := queryText(t, st, "SELECT left(password_hash, 9) FROM users WHERE username = 'alice'", "rehash check"); got != "argon2id$" {
		t.Fatalf("password hash not re-hashed: %s", got)
	}
	if _, _, err := svc.Auth.Login(ctx, "alice", "hunter2", "", nil); err != nil {
		t.Fatalf("login after rehash: %v", err)
	}
	// deactivated users must not log in even with a valid password
	if _, _, err := svc.Auth.Login(ctx, "bob", "bopass", "", nil); err == nil {
		t.Fatal("deactivated user logged in")
	}
}
