// Rocket.Chat import: reads a mongodump --archive of the `rocketchat`
// database and upserts it into gochatserver tables (REQUIREMENTS.md).
package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/gochathub/gochathub-server/internal/pwd"
	"github.com/gochathub/gochathub-server/internal/storage"
	"github.com/gochathub/gochathub-server/internal/store"
)

// Deterministic UUIDv5 namespaces: the same source id maps to the same target
// id on every run — this is what makes the delta (repeatable cutover) work.
var (
	nsUser = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gochathub.app/migrate/rc/user"))
	nsRoom = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gochathub.app/migrate/rc/room"))
	nsMsg  = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gochathub.app/migrate/rc/msg"))
	nsAtt  = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gochathub.app/migrate/rc/att"))
	nsRev  = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gochathub.app/migrate/rc/rev"))
)

const watermarkKey = "migrate.rc"

// Source users imported disabled: integration identities kept only so message
// history stays attributable (requirements decision 9).
var alwaysDisabled = map[string]bool{"rocket.cat": true, "mantisbt": true, "n8n": true}

// rcWatermark is the delta-cutover cursor. message_ts_max only advances in
// a run that imported files too: a --skip-files run marks files_pending so
// the next full run re-imports those messages with their attachments.
// Timestamps are RFC3339.
type rcWatermark struct {
	MessageTS    string `json:"message_ts_max,omitempty"`
	AvatarTS     string `json:"avatar_ts_max,omitempty"`
	FilesPending bool   `json:"files_pending,omitempty"`
}

type RcOpts struct {
	Archive   string
	DryRun    bool
	SkipFiles bool
	Log       *slog.Logger
}

type Summary struct {
	DryRun           bool `json:"dry_run"`
	Users            int  `json:"users"`
	UsersSkipped     int  `json:"users_skipped"` // marketplace app users (0 messages sourced)
	Tombstones       int  `json:"tombstones"`
	UsersNoPassword  int  `json:"users_without_password"`
	Rooms            int  `json:"rooms"`
	RoomsSkipped     int  `json:"rooms_skipped"`
	Members          int  `json:"members"`
	MembersSkipped   int  `json:"members_skipped"`
	Contacts         int  `json:"contacts"`
	Messages         int  `json:"messages"`
	MessagesSkipped  int  `json:"messages_skipped"`
	Revisions        int  `json:"revisions"`
	UsersWith2FA     int  `json:"users_with_2fa"`
	Reactions        int  `json:"reactions"`
	ReactionsSkipped int  `json:"reactions_skipped"`
	Mentions         int  `json:"mentions"`
	MentionsSkipped  int  `json:"mentions_skipped"`
	Files            int  `json:"files"`
	FilesOrphaned    int  `json:"files_orphaned"` // in GridFS but no imported message references them
	FilesSkipped     int  `json:"files_skipped"`  // missing GridFS body or uploader
	Avatars          int  `json:"avatars"`
	AvatarsSkipped   int  `json:"avatars_skipped"`
}

// RcMigrate imports one archive. st writes gochatserver tables; stg uploads
// files/avatars to S3 (nil is valid with DryRun or SkipFiles).
func RcMigrate(ctx context.Context, st *store.Store, stg *storage.S3, opts RcOpts) (Summary, error) {
	var sum Summary
	sum.DryRun = opts.DryRun

	wm, haveWatermark, err := loadWatermark(ctx, st)
	if err != nil {
		return sum, err
	}
	if !opts.DryRun && !opts.SkipFiles && stg == nil {
		return sum, fmt.Errorf("S3 not configured but files pending; set S3_* env or pass --skip-files")
	}
	if !haveWatermark { // first run demands an empty database
		var n int
		if err := st.Q.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
			return sum, fmt.Errorf("precondition check: %w", err)
		}
		if n > 0 {
			return sum, fmt.Errorf("users table is not empty and no migration watermark exists; refusing")
		}
	}

	rc := &rcState{
		opts:       opts,
		st:         st,
		stg:        stg,
		sum:        &sum,
		log:        opts.Log,
		wm:         wm,
		userSeen:   map[string]bool{},
		tombstones: map[string]string{},
		usedNames:  map[string]bool{},
		filesRef:   map[string]rcFileRef{},
		upChunks:   map[string][]chunkRef{},
		avChunks:   map[string][]chunkRef{},
		upFiles:    map[string]rcFileMeta{},
		avFiles:    map[string]rcFileMeta{},
		upMeta:     map[string]rcUploadMeta{},
		avMeta:     map[string]rcAvatarMeta{},
		idByRC:     map[string]string{},
		idByName:   map[string]string{},
		roomByRC:   map[string]string{},
		dmMembers:  map[string][]string{},
		pins:       map[string]rcPin{},
		imported:   map[string]string{},
	}
	if rc.log == nil {
		rc.log = slog.New(slog.DiscardHandler)
	}

	// Pass 1: GridFS metadata + chunk-doc offsets (the 361MB payload never
	// enters RAM; chunk docs are re-read by offset in the file phase).
	if err := rc.scanFiles(); err != nil {
		return sum, err
	}
	// Pass 2: collect data documents in memory, then flush in dependency
	// order (users → rooms → messages → subs → pins → files → avatars).
	// ponytail: all message docs in RAM (~25k small docs) — bounded by message
	// count; only GridFS chunk bytes stay out.
	if err := rc.collect(); err != nil {
		return sum, err
	}
	flushes := []func(context.Context) error{
		rc.flushUsers, rc.flushRooms, rc.flushMessages, rc.flushSubs,
		rc.flushPins, rc.flushFiles, rc.flushAvatars, rc.saveWatermark,
	}
	for _, fn := range flushes {
		if err := fn(ctx); err != nil {
			return sum, err
		}
	}
	rc.log.Info("rc migrate done", "summary", sum)
	return sum, nil
}

// --- streaming primitives ------------------------------------------------

// chunkRef locates one GridFS chunk doc inside the archive.
type chunkRef struct {
	n, off, length int64
}

// rcFileMeta is a GridFS files doc (Mongo 8 GridFS shape: only standard
// fields; RC's metadata lives in the bare rocketchat_uploads doc, and the
// original name/mime ride on the referencing message).
type rcFileMeta struct {
	id          string
	length      int64
	contentType string
	uploadDate  time.Time
}

// rcUploadMeta is RC's Uploads doc (name/room/uploader for a stored file).
type rcUploadMeta struct {
	fileRC, name, rid, ownerRC string
	uploadedAt                 time.Time
}

type rcAvatarMeta struct {
	fileRC, ownerRC string
	uploadedAt      time.Time
}

func fileMetaOf(m bson.M) rcFileMeta {
	meta := rcFileMeta{
		id:          astr(m["_id"]),
		length:      asInt(m["length"]),
		contentType: strings.TrimSpace(astr(m["contentType"])),
	}
	meta.uploadDate, _ = atime(m["uploadDate"])
	return meta
}

func (s *rcState) scanFiles() error {
	ar, err := OpenArchive(s.opts.Archive)
	if err != nil {
		return err
	}
	defer ar.Close()
	for {
		d, ok, err := ar.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		off, length := ar.LastDoc()
		switch d.KeyPath() {
		case "rocketchat_uploads": // bare RC Uploads doc: name/room/uploader
			m := rcUploadMetaOf(d.Data)
			s.upMeta[m.fileRC] = m
		case "rocketchat_uploads.files":
			m := fileMetaOf(d.Data)
			s.upFiles[m.id] = m
		case "rocketchat_uploads.chunks":
			id := astr(d.Data["files_id"])
			s.upChunks[id] = append(s.upChunks[id], chunkRef{n: asInt(d.Data["n"]), off: off, length: length})
		case "rocketchat_avatars": // bare RC Avatar doc: uploader id
			m := rcAvatarMetaOf(d.Data)
			s.avMeta[m.fileRC] = m
		case "rocketchat_avatars.files":
			afm := fileMetaOf(d.Data)
			s.avFiles[afm.id] = afm
			s.maxAvatar = maxTime(s.maxAvatar, afm.uploadDate)
		case "rocketchat_avatars.chunks":
			id := astr(d.Data["files_id"])
			s.avChunks[id] = append(s.avChunks[id], chunkRef{n: asInt(d.Data["n"]), off: off, length: length})
		}
	}
}

func loadWatermark(ctx context.Context, st *store.Store) (rcWatermark, bool, error) {
	var raw string
	err := st.Q.QueryRow(ctx, `SELECT value #>> '{}' FROM app_config WHERE key = $1`, watermarkKey).Scan(&raw)
	if err == store.ErrNotFound {
		return rcWatermark{}, false, nil
	}
	if err != nil {
		return rcWatermark{}, false, fmt.Errorf("watermark read: %w", err)
	}
	var wm rcWatermark
	if err := json.Unmarshal([]byte(raw), &wm); err != nil {
		return rcWatermark{}, false, fmt.Errorf("watermark decode: %w", err)
	}
	return wm, true, nil
}

func (s *rcState) saveWatermark(ctx context.Context) error {
	if s.opts.DryRun {
		return nil
	}
	wm := rcWatermark{MessageTS: rfc3339OrEmpty(s.maxMsgTS), AvatarTS: rfc3339OrEmpty(s.maxAvatar)}
	if s.opts.SkipFiles {
		// Files were not imported for these messages: do not advance the
		// message/avatar cursor, and flag the gap for the next full run.
		wm.MessageTS = s.wm.MessageTS
		wm.AvatarTS = s.wm.AvatarTS
		wm.FilesPending = true
	}
	raw, err := json.Marshal(wm)
	if err != nil {
		return err
	}
	if _, err := s.st.Q.Exec(ctx, `
		INSERT INTO app_config (key, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		watermarkKey, string(raw)); err != nil {
		return fmt.Errorf("watermark write: %w", err)
	}
	return nil
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func parseRFC3339(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// --- in-memory collection -------------------------------------------------

type rcUserRow struct {
	rcid, username, display, email, role, hash string
	enabled                                    bool
	created, lastSeen                          time.Time
	services2fa                                string // totp/email2fa snapshot for the future 2FA feature
}

type rcRoomRow struct {
	rcid, typ, creator string // creator: "id:<rcid>" or "username:<name>"
	name               *string
	description        string
	usernames          []string // direct rooms
	created, updated   time.Time
}

type rcSubRow struct {
	rid, uid string
	roles    []string
	ls, ts   time.Time
}

type rcEdit struct {
	body string
	ts   time.Time
}

type rcReact struct {
	emoji     string
	usernames []string
}

type rcPin struct {
	rcid string
	ts   time.Time
}

type rcMsgRow struct {
	rcid, rid, authorRC string
	body                string
	ts                  time.Time
	editedAt, deletedAt *time.Time
	tmid                string
	fileID              string
	edits               []rcEdit
	reactions           []rcReact
	mentions            []string
}

type rcFileRef struct {
	msgRC, authorRC, name, mime string
	dims                        [2]int
	hasDims                     bool
}

type rcState struct {
	opts       RcOpts
	st         *store.Store
	stg        *storage.S3
	sum        *Summary
	log        *slog.Logger
	wm         rcWatermark
	maxMsgTS   time.Time
	maxAvatar  time.Time
	users      []rcUserRow
	userSeen   map[string]bool
	tombstones map[string]string // rcid → username hint, discovered in message u blocks
	usedNames  map[string]bool
	rooms      []rcRoomRow
	subs       []rcSubRow
	msgs       []rcMsgRow
	filesRef   map[string]rcFileRef
	upChunks   map[string][]chunkRef
	avChunks   map[string][]chunkRef
	upFiles    map[string]rcFileMeta
	avFiles    map[string]rcFileMeta
	upMeta     map[string]rcUploadMeta
	avMeta     map[string]rcAvatarMeta
	idByRC     map[string]string
	idByName   map[string]string
	roomByRC   map[string]string
	dmMembers  map[string][]string // direct rooms: normalized usernames
	pins       map[string]rcPin
	imported   map[string]string // imported message rcid → uuid (file linking)
}

func (s *rcState) collect() error {
	ar, err := OpenArchive(s.opts.Archive)
	if err != nil {
		return err
	}
	defer ar.Close()
	for {
		d, ok, err := ar.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		switch d.KeyPath() {
		case "users":
			s.collectUser(d.Data)
		case "rocketchat_room":
			s.collectRoom(d.Data)
		case "rocketchat_subscription":
			s.collectSub(d.Data)
		case "rocketchat_message":
			s.collectMsg(d.Data)
		}
	}
}

func (s *rcState) collectUser(m bson.M) {
	rcid := astr(m["_id"])
	if rcid == "" || s.userSeen[rcid] {
		return
	}
	if astr(m["type"]) == "app" { // marketplace app users carry no content here
		s.sum.UsersSkipped++
		return
	}
	username := sanitizeName(astr(m["username"]), s.usedNames)
	if username == "" {
		return
	}
	s.userSeen[rcid] = true
	row := rcUserRow{
		rcid:        rcid,
		username:    username,
		email:       pickEmail(m),
		role:        "user",
		enabled:     asBool(m["active"], true) && !alwaysDisabled[username],
		hash:        legacyHash(m),
		created:     atimeOr(m["createdAt"], time.Now()),
		lastSeen:    atimeNil(m["lastLogin"]),
		services2fa: twoFAOf(m),
	}
	if row.display = astr(m["name"]); row.display == "" {
		row.display = row.username
	}
	if slices.Contains(asStrSlice(m["roles"]), "admin") {
		row.role = "admin"
	}
	if row.hash == "" {
		row.hash = randomHash()
		s.sum.UsersNoPassword++
	}
	s.users = append(s.users, row)
}

func legacyHash(m bson.M) string {
	if h, ok := sub(sub(m, "services"), "password")["bcrypt"].(string); ok && h != "" {
		return pwd.BCryptPrefix + h
	}
	return ""
}

func randomHash() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand: error only on a broken system
	h, _ := pwd.Hash(hex.EncodeToString(b))
	return h
}

func (s *rcState) collectRoom(m bson.M) {
	rcid := astr(m["_id"])
	skip := func(reason string) {
		s.sum.RoomsSkipped++
		s.log.Warn("room skipped", "rcid", rcid, "reason", reason)
	}
	if rcid == "" { // discussions: dropped per requirements
		skip("empty id")
		return
	}
	if astr(m["prid"]) != "" {
		skip("discussion")
		return
	}
	row := rcRoomRow{rcid: rcid}
	switch t := astr(m["t"]); t {
	case "c":
		row.typ = "public"
	case "p":
		row.typ = "private"
	case "d":
		un := asStrSlice(m["usernames"])
		if len(un) < 2 {
			skip("dm without 2 usernames")
			return
		}
		for i := range un {
			un[i] = strings.ToLower(un[i])
		}
		slices.Sort(un)
		un = slices.Compact(un)
		if len(un) > 2 {
			row.typ = "group_direct"
		} else {
			row.typ = "direct"
		}
		row.usernames = un
		row.description = astr(m["topic"])
		row.creator = "username:" + un[0] // lowest name = stable creator
	default:
		skip("unmappable type " + t)
		return
	}
	if row.typ != "direct" && row.typ != "group_direct" {
		n := strings.TrimSpace(astr(m["fname"]))
		if n == "" {
			n = strings.TrimSpace(astr(m["name"]))
		}
		if n == "" {
			n = "room-" + rcid
		}
		if len(n) > 200 {
			n = n[:200]
		}
		row.name = &n
		row.description = astr(m["topic"])
		if row.creator = "id:" + astr(sub(m, "u")["_id"]); row.creator == "id:" {
			skip("creator missing")
			return
		}
	}
	row.created = atimeOr(m["ts"], time.Now())
	row.updated = atimeNil(m["lm"])
	if row.updated.IsZero() {
		row.updated = row.created
	}
	s.rooms = append(s.rooms, row)
}

func (s *rcState) collectSub(m bson.M) {
	ridStr := astr(m["rid"])
	uid := astr(sub(m, "u")["_id"])
	if ridStr == "" || uid == "" {
		return
	}
	s.subs = append(s.subs, rcSubRow{
		rid: ridStr, uid: uid,
		roles: asStrSlice(m["roles"]),
		ls:    atimeNil(m["ls"]),
		ts:    atimeOr(m["ts"], time.Now()),
	})
}

// rcQuoteLink strips the Rocket.Chat jump-link prefix of quote replies
// ("[ ](https://host/group/slug?msg=<id>)"); the quoted text itself is
// appended as a markdown blockquote.
var rcQuoteLink = regexp.MustCompile(`\[\s*\]\(\s*https?://[^()\s]+\?msg=[^()\s]+\)`)

// quoteFold converts Rocket.Chat quote attachments (linked quoted text) into
// markdown blockquotes; file previews and oEmbed link previews are dropped.
func quoteFold(m bson.M) string {
	var b strings.Builder
	if arr := asAnySlice(m["attachments"]); arr != nil {
		for _, e := range arr {
			am := asM(e)
			if am == nil {
				continue
			}
			if am["type"] != nil || am["image_url"] != nil || am["video_url"] != nil || am["audio_url"] != nil {
				continue
			}
			if txt, ok := am["text"].(string); ok && strings.TrimSpace(txt) != "" {
				b.WriteString("\n> ")
				b.WriteString(strings.ReplaceAll(strings.TrimSpace(txt), "\n", "\n> "))
			}
		}
	}
	return b.String()
}

func (s *rcState) collectMsg(m bson.M) {
	rcid := astr(m["_id"])
	ridStr := astr(m["rid"])
	ts, ok := atime(m["ts"])
	if rcid == "" || ridStr == "" || !ok || ts.IsZero() {
		s.sum.MessagesSkipped++
		return
	}
	s.maxMsgTS = maxTime(s.maxMsgTS, ts)
	if s.wm.MessageTS != "" && !ts.After(parseRFC3339(s.wm.MessageTS)) {
		return // delta mode: already imported (before the junk filter, so old junk is not recounted)
	}
	if t := astr(m["t"]); t != "" { // system + livechat navigation junk
		s.sum.MessagesSkipped++
		return
	}
	author := sub(m, "u")
	authorRC := astr(author["_id"])
	if authorRC == "" {
		s.sum.MessagesSkipped++
		return
	}
	if !s.userSeen[authorRC] {
		s.tombstones[authorRC] = astr(author["username"])
	}
	row := rcMsgRow{
		rcid:     rcid,
		rid:      ridStr,
		authorRC: authorRC,
		body:     strings.TrimSpace(rcQuoteLink.ReplaceAllString(astr(m["msg"]), "") + quoteFold(m)),
		ts:       ts,
		tmid:     astr(m["tmid"]),
		fileID:   astr(sub(m, "file")["_id"]),
	}
	if t, ok := atime(m["editedAt"]); ok && !t.IsZero() {
		row.editedAt = &t
	}
	if t, ok := atime(m["deletedAt"]); ok && !t.IsZero() {
		row.deletedAt = &t
	}
	if asBool(m["pinned"], false) {
		pt, ok := atime(m["pinnedAt"])
		if !ok || pt.IsZero() {
			pt = ts
		}
		if p, dup := s.pins[ridStr]; !dup || pt.After(p.ts) {
			s.pins[ridStr] = rcPin{rcid: rcid, ts: pt}
		}
	}
	if arr := asAnySlice(m["edits"]); arr != nil {
		for _, e := range arr {
			em := asM(e)
			if em == nil {
				continue
			}
			if body := astr(em["msg"]); body != "" {
				et, ok := atime(em["ts"])
				if !ok {
					et = ts
				}
				row.edits = append(row.edits, rcEdit{body: body, ts: et})
			}
		}
	}
	if am := asM(m["reactions"]); am != nil {
		for emoji, v := range am {
			rv := asM(v)
			if rv == nil {
				continue
			}
			row.reactions = append(row.reactions, rcReact{emoji: emoji, usernames: asStrSlice(rv["usernames"])})
		}
	}
	if arr := asAnySlice(m["mentions"]); arr != nil {
		for _, e := range arr {
			em := asM(e)
			if em == nil {
				continue
			}
			switch id := astr(em["_id"]); id {
			case "", "all", "here":
				continue
			default:
				row.mentions = append(row.mentions, id)
			}
		}
	}
	if row.fileID != "" {
		ref := rcFileRef{msgRC: rcid, authorRC: authorRC}
		f := sub(m, "file")
		if f != nil {
			ref.name = astr(f["name"])
			ref.mime = astr(f["type"])
		}
		if arr := asAnySlice(m["attachments"]); arr != nil && len(arr) > 0 {
			if am := asM(arr[0]); am != nil {
				if d := sub(am, "image_dimensions"); d != nil {
					w, h := asInt(d["width"]), asInt(d["height"])
					if w > 0 && h > 0 {
						ref.dims, ref.hasDims = [2]int{int(w), int(h)}, true
					}
				}
			}
		}
		s.filesRef[row.fileID] = ref
	}
	s.msgs = append(s.msgs, row)
}

// --- flushes --------------------------------------------------------------

func (s *rcState) flushUsers(ctx context.Context) error {
	for _, u := range s.users {
		id := uuid.NewSHA1(nsUser, []byte(u.rcid)).String()
		if err := s.insertUser(ctx, u, id); err != nil {
			return err
		}
		s.idByRC[u.rcid] = id
		s.idByName[u.username] = id
		s.sum.Users++
		if err := s.sidecarUser(ctx, u, id); err != nil {
			return err
		}
	}
	// Deleted-in-source users: tombstones so message authorship survives.
	for rcid, hint := range s.tombstones {
		if _, known := s.idByRC[rcid]; known {
			continue
		}
		username := sanitizeName(hint, s.usedNames)
		if username == "" {
			username = sanitizeName("deleted-"+rcid, s.usedNames)
		}
		if username == "" {
			username = "deleted-user"
		}
		row := rcUserRow{
			rcid:     rcid,
			username: username,
			display:  firstNonEmpty(hint, username),
			role:     "user",
			enabled:  false,
			hash:     randomHash(),
			created:  time.Now(),
		}
		id := uuid.NewSHA1(nsUser, []byte(rcid)).String()
		if err := s.insertUser(ctx, row, id); err != nil {
			return err
		}
		s.idByRC[rcid] = id
		s.idByName[username] = id
		s.sum.Tombstones++
	}
	s.log.Info("users imported", "n", s.sum.Users, "tombstones", s.sum.Tombstones,
		"no_password", s.sum.UsersNoPassword)
	return nil
}

// sidecarUser preserves 2FA enrollment (TOTP secret/backup hashes, email-2FA
// flag) in migrate_rc_users for the future 2FA feature. Password, resume
// tokens and cloud credentials are deliberately not carried over.
func (s *rcState) sidecarUser(ctx context.Context, u rcUserRow, id string) error {
	if u.services2fa == "" {
		return nil
	}
	s.sum.UsersWith2FA++
	if s.opts.DryRun {
		return nil
	}
	if _, err := s.st.Q.Exec(ctx, `
		INSERT INTO migrate_rc_users (rcid, user_id, username, services)
		VALUES ($1,$2,$3,$4::jsonb)
		ON CONFLICT (rcid) DO UPDATE SET
			user_id = EXCLUDED.user_id, username = EXCLUDED.username, services = EXCLUDED.services`,
		u.rcid, id, u.username, u.services2fa); err != nil {
		return fmt.Errorf("sidecar %s: %w", u.username, err)
	}
	return nil
}

// twoFAOf extracts the auth-enrollment subdocs (totp, email2fa) that
// gochatserver has no schema for yet; everything else (password, resume,
// cloud, passwordHistory) is dead or secret material not carried over.
func twoFAOf(m bson.M) string {
	svc := sub(m, "services")
	if svc == nil {
		return ""
	}
	out := bson.M{}
	for _, k := range []string{"totp", "email2fa"} {
		if v := sub(svc, k); v != nil {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return ""
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(raw)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func rcUploadMetaOf(m bson.M) rcUploadMeta {
	meta := rcUploadMeta{
		fileRC:     astr(m["_id"]),
		name:       astr(m["name"]),
		rid:        astr(m["rid"]),
		ownerRC:    astr(m["userId"]),
		uploadedAt: atimeOr(m["uploadedAt"], time.Time{}),
	}
	if meta.ownerRC == "" { // older RC versions nest the uploader
		if meta.ownerRC = astr(sub(m, "u")["_id"]); meta.ownerRC == "" {
			meta.ownerRC = astr(m["userId"])
		}
	}
	return meta
}

func rcAvatarMetaOf(m bson.M) rcAvatarMeta {
	return rcAvatarMeta{
		fileRC:     astr(m["_id"]),
		ownerRC:    astr(m["userId"]),
		uploadedAt: atimeOr(m["uploadedAt"], time.Time{}),
	}
}

func (s *rcState) insertUser(ctx context.Context, u rcUserRow, id string) error {
	if s.opts.DryRun {
		return nil
	}
	var email *string
	if u.email != "" {
		email = &u.email
	}
	var lastSeen *time.Time
	if !u.lastSeen.IsZero() {
		lastSeen = &u.lastSeen
	}
	if _, err := s.st.Q.Exec(ctx, `
		INSERT INTO users (id, username, display_name, email, password_hash, role, enabled, created_at, last_seen_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (id) DO UPDATE SET
			username = EXCLUDED.username, display_name = EXCLUDED.display_name,
			email = EXCLUDED.email, role = EXCLUDED.role, enabled = EXCLUDED.enabled`,
		id, u.username, u.display, email, u.hash, u.role, u.enabled, u.created, lastSeen); err != nil {
		return fmt.Errorf("insert user %s: %w", u.username, err)
	}
	return nil
}

func (s *rcState) flushRooms(ctx context.Context) error {
	for _, r := range s.rooms {
		var creatorUUID string
		if base, ok := strings.CutPrefix(r.creator, "username:"); ok {
			creatorUUID = s.idByName[base]
		} else if base, ok := strings.CutPrefix(r.creator, "id:"); ok {
			creatorUUID = s.idByRC[base]
		}
		if creatorUUID == "" {
			s.sum.RoomsSkipped++
			s.log.Warn("room skipped: creator unknown", "rcid", r.rcid)
			continue
		}
		id := uuid.NewSHA1(nsRoom, []byte(r.rcid)).String()
		if !s.opts.DryRun {
			if _, err := s.st.Q.Exec(ctx, `
				INSERT INTO rooms (id, type, name, description, created_by, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT (id) DO UPDATE SET
					name = EXCLUDED.name, description = EXCLUDED.description, updated_at = EXCLUDED.updated_at`,
				id, r.typ, r.name, r.description, creatorUUID, r.created, r.updated); err != nil {
				return fmt.Errorf("insert room %s: %w", r.rcid, err)
			}
		}
		s.roomByRC[r.rcid] = id
		if len(r.usernames) > 0 {
			s.dmMembers[r.rcid] = r.usernames
		}
		s.sum.Rooms++
	}
	return nil
}

func (s *rcState) flushMessages(ctx context.Context) error {
	msgs := slices.Clone(s.msgs)
	slices.SortFunc(msgs, func(a, b rcMsgRow) int {
		if c := a.ts.Compare(b.ts); c != 0 {
			return c
		}
		return strings.Compare(a.rcid, b.rcid)
	})
	seen := map[string]bool{}
	for _, m := range msgs {
		roomUUID := s.roomByRC[m.rid]
		if roomUUID == "" {
			s.sum.MessagesSkipped++
			continue
		}
		authorUUID := s.idByRC[m.authorRC]
		if authorUUID == "" {
			s.sum.MessagesSkipped++
			continue
		}
		id := uuid.NewSHA1(nsMsg, []byte(m.rcid)).String()
		var reply *string
		if m.tmid != "" && seen[m.tmid] { // thread root must precede the reply
			r := uuid.NewSHA1(nsMsg, []byte(m.tmid)).String()
			reply = &r
		}
		if !s.opts.DryRun {
			if _, err := s.st.Q.Exec(ctx, `
				INSERT INTO messages (id, room_id, author_id, reply_to_message_id, body, format, created_at, edited_at, deleted_at)
				VALUES ($1,$2,$3,$4,$5,'markdown',$6,$7,$8)
				ON CONFLICT (id) DO UPDATE SET
					body = EXCLUDED.body, edited_at = EXCLUDED.edited_at, deleted_at = EXCLUDED.deleted_at`,
				id, roomUUID, authorUUID, reply, m.body, m.ts, m.editedAt, m.deletedAt); err != nil {
				return fmt.Errorf("insert message %s: %w", m.rcid, err)
			}
		}
		seen[m.rcid] = true
		s.imported[m.rcid] = id
		s.sum.Messages++
		for _, rx := range m.reactions {
			for _, un := range rx.usernames {
				uid, known := s.idByName[strings.ToLower(un)]
				if !known {
					s.sum.ReactionsSkipped++
					continue
				}
				if !s.opts.DryRun {
					if _, err := s.st.Q.Exec(ctx, `
						INSERT INTO message_reactions (message_id, user_id, emoji, created_at)
						VALUES ($1,$2,$3,$4) ON CONFLICT (message_id, user_id, emoji) DO NOTHING`,
						id, uid, rx.emoji, m.ts); err != nil {
						return fmt.Errorf("insert reaction %s: %w", m.rcid, err)
					}
				}
				s.sum.Reactions++
			}
		}
		for _, mid := range m.mentions {
			uid, known := s.idByRC[mid]
			if !known {
				s.sum.MentionsSkipped++
				continue
			}
			if !s.opts.DryRun {
				if _, err := s.st.Q.Exec(ctx, `
					INSERT INTO message_mentions (message_id, mentioned_user_id, created_at)
					VALUES ($1,$2,$3) ON CONFLICT (message_id, mentioned_user_id) DO NOTHING`,
					id, uid, m.ts); err != nil {
					return fmt.Errorf("insert mention %s: %w", m.rcid, err)
				}
			}
			s.sum.Mentions++
		}
		for _, e := range m.edits {
			if s.opts.DryRun {
				s.sum.Revisions++
				continue
			}
			rev := uuid.NewSHA1(nsRev, []byte(m.rcid+"|"+e.ts.UTC().Format(time.RFC3339Nano))).String()
			if _, err := s.st.Q.Exec(ctx, `
				INSERT INTO message_revisions (id, message_id, editor_id, body, format, created_at)
				VALUES ($1,$2,$3,$4,'markdown',$5) ON CONFLICT (id) DO NOTHING`,
				rev, id, authorUUID, e.body, e.ts); err != nil {
				return fmt.Errorf("insert revision %s: %w", m.rcid, err)
			}
			s.sum.Revisions++
		}
	}
	return nil
}

func (s *rcState) flushSubs(ctx context.Context) error {
	for _, sb := range s.subs {
		roomUUID := s.roomByRC[sb.rid]
		userUUID := s.idByRC[sb.uid]
		if roomUUID == "" || userUUID == "" {
			s.sum.MembersSkipped++
			continue
		}
		role := "member"
		if slices.Contains(sb.roles, "owner") {
			role = "admin"
		}
		if !s.opts.DryRun {
			if _, err := s.st.Q.Exec(ctx, `
				INSERT INTO room_members (room_id, user_id, role, joined_at)
				VALUES ($1,$2,$3,$4) ON CONFLICT (room_id, user_id) DO NOTHING`,
				roomUUID, userUUID, role, sb.ts); err != nil {
				return fmt.Errorf("insert member %s/%s: %w", sb.rid, sb.uid, err)
			}
			if !sb.ls.IsZero() {
				var last string
				if err := s.st.Q.QueryRow(ctx, `
					SELECT id FROM messages WHERE room_id = $1 AND created_at <= $2
					ORDER BY created_at DESC, id DESC LIMIT 1`, roomUUID, sb.ls).Scan(&last); err == nil {
					if _, err := s.st.Q.Exec(ctx, `
						UPDATE room_members SET last_read_message_id = $3
						WHERE room_id = $1 AND user_id = $2`, roomUUID, userUUID, last); err != nil {
						return fmt.Errorf("last read %s/%s: %w", sb.rid, sb.uid, err)
					}
				}
			}
		}
		s.sum.Members++
	}
	// DM pairs become mutual contacts (ADR-010).
	for _, un := range s.dmMembers {
		if len(un) != 2 {
			continue
		}
		a, b := s.idByName[un[0]], s.idByName[un[1]]
		if a == "" || b == "" {
			continue
		}
		for _, pair := range [][2]string{{a, b}, {b, a}} {
			if !s.opts.DryRun {
				if _, err := s.st.Q.Exec(ctx, `
					INSERT INTO contacts (owner_id, contact_user_id, created_at)
					VALUES ($1,$2,now()) ON CONFLICT (owner_id, contact_user_id) DO NOTHING`,
					pair[0], pair[1]); err != nil {
					return fmt.Errorf("insert contact %s->%s: %w", pair[0], pair[1], err)
				}
			}
			s.sum.Contacts++
		}
	}
	return nil
}

func (s *rcState) flushPins(ctx context.Context) error {
	for ridStr, p := range s.pins {
		roomUUID := s.roomByRC[ridStr]
		if roomUUID == "" {
			continue
		}
		msgUUID := uuid.NewSHA1(nsMsg, []byte(p.rcid)).String()
		if !s.opts.DryRun {
			if _, err := s.st.Q.Exec(ctx, `
				UPDATE rooms SET pinned_message_id = $2
				WHERE id = $1 AND EXISTS (SELECT 1 FROM messages WHERE id = $2::uuid)`,
				roomUUID, msgUUID); err != nil {
				return fmt.Errorf("pin %s: %w", ridStr, err)
			}
		}
	}
	return nil
}

var unsafeKeyChars = strings.NewReplacer("/", "%2F", "\\", "%5C", "%", "%25", "\x00", "")

func storageKeyName(attUUID, filename string) string {
	name := unsafeKeyChars.Replace(strings.TrimSpace(filename))
	if name == "" {
		name = "file-" + attUUID[:8]
	}
	if len(name) > 240 {
		name = name[len(name)-240:]
	}
	return "att/" + attUUID + "/" + name
}

// assembleChunks reassembles one GridFS file from its chunk docs (sorted by
// n, length-checked against the declared size). ponytail: whole file in RAM;
// stream chunks if a single file exceeds ~100MB.
func assembleChunks(ar *Archive, refs []chunkRef, declared int64) ([]byte, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("no chunks in archive")
	}
	clean := slices.Clone(refs)
	slices.SortFunc(clean, func(a, b chunkRef) int { return int(a.n - b.n) })
	var buf bytes.Buffer
	for _, cr := range clean {
		cd, err := ar.DocAt(cr.off, cr.length)
		if err != nil {
			return nil, err
		}
		bin, ok := cd["data"].(bson.Binary)
		if !ok {
			return nil, fmt.Errorf("chunk %d missing data", cr.n)
		}
		buf.Write(bin.Data)
	}
	if int64(buf.Len()) != declared {
		return nil, fmt.Errorf("assembled %d bytes != declared %d", buf.Len(), declared)
	}
	return buf.Bytes(), nil
}

func (s *rcState) flushFiles(ctx context.Context) error {
	if s.opts.DryRun || s.opts.SkipFiles {
		// Approximate counts (upload outcomes unknowable without S3): every
		// message-linked file is a candidate; GridFS entries without a
		// referencing message are the orphans.
		s.sum.Files = len(s.filesRef)
		s.sum.FilesOrphaned = len(s.upFiles) - len(s.filesRef)
		if s.sum.FilesOrphaned < 0 {
			s.sum.FilesOrphaned = 0
		}
		return nil
	}
	ar, err := OpenArchive(s.opts.Archive)
	if err != nil {
		return err
	}
	defer ar.Close()
	for fileID, ref := range s.filesRef {
		meta, ok := s.upFiles[fileID]
		if !ok {
			s.sum.FilesSkipped++
			continue
		}
		msgUUID, importedMsg := s.imported[ref.msgRC]
		if !importedMsg {
			continue // message for this file was skipped: orphan, not an error
		}
		// uploader: message author first, RC Uploads metadata second
		uploader := s.idByRC[ref.authorRC]
		if uploader == "" {
			if ul := s.upMeta[fileID]; ul.ownerRC != "" {
				uploader = s.idByRC[ul.ownerRC]
			}
		}
		if uploader == "" {
			s.sum.FilesSkipped++
			continue
		}
		attID := uuid.NewSHA1(nsAtt, []byte(fileID)).String()
		um := s.upMeta[fileID]
		filename := firstNonEmpty(ref.name, um.name, "file-"+fileID[:8])
		mime := firstNonEmpty(ref.mime, meta.contentType, "application/octet-stream")
		stamp := um.uploadedAt
		if stamp.IsZero() {
			stamp = meta.uploadDate
		}
		key := storageKeyName(attID, filename)
		if content, err := assembleChunks(ar, s.upChunks[fileID], meta.length); err != nil {
			s.sum.FilesSkipped++
			s.log.Warn("file reassembly failed", "fileid", fileID, "err", err)
			continue
		} else if err := s.stg.PutObject(ctx, key, mime, meta.length, bytes.NewReader(content)); err != nil {
			s.sum.FilesSkipped++
			s.log.Warn("file upload failed", "fileid", fileID, "err", err)
			continue
		}
		var w, h *int
		if ref.hasDims {
			w, h = &ref.dims[0], &ref.dims[1]
		}
		var createdAt *time.Time
		if !stamp.IsZero() {
			createdAt = &stamp
		}
		if _, err := s.st.Q.Exec(ctx, `
			INSERT INTO attachments (id, uploader_id, storage_key, filename, mime_type, size_bytes, width, height, status, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'ready',$9) ON CONFLICT (id) DO NOTHING`,
			attID, uploader, key, filename, mime, meta.length, w, h, createdAt); err != nil {
			return fmt.Errorf("insert attachment %s: %w", fileID, err)
		}
		if _, err := s.st.Q.Exec(ctx, `
			INSERT INTO message_attachments (message_id, attachment_id, sort_order)
			VALUES ($1,$2,0) ON CONFLICT (message_id, attachment_id) DO NOTHING`,
			msgUUID, attID); err != nil {
			return fmt.Errorf("link attachment %s: %w", fileID, err)
		}
		s.sum.Files++
	}
	// orphan = stored file no imported message links (deleted messages,
	// livechat uploads, orphans): exact because message_attachments persists.
	var linked int
	if err := s.st.Q.QueryRow(ctx, "SELECT count(*) FROM message_attachments").Scan(&linked); err == nil {
		s.sum.FilesOrphaned = len(s.upFiles) - linked
		if s.sum.FilesOrphaned < 0 {
			s.sum.FilesOrphaned = 0
		}
	}
	return nil
}

func (s *rcState) flushAvatars(ctx context.Context) error {
	// RC keeps every avatar upload; the newest version per user wins.
	type best struct {
		fileRC string
		ts     time.Time
	}
	pick := map[string]best{}
	for fileRC, av := range s.avMeta {
		if _, known := s.idByRC[av.ownerRC]; !known {
			s.sum.AvatarsSkipped++ // owner not imported (app user)
			continue
		}
		ts := av.uploadedAt
		if fm, ok := s.avFiles[fileRC]; ok {
			ts = maxTime(ts, fm.uploadDate)
		}
		if b, dup := pick[av.ownerRC]; !dup || ts.After(b.ts) {
			pick[av.ownerRC] = best{fileRC: fileRC, ts: ts}
		}
	}
	var ar *Archive
	if !s.opts.DryRun && !s.opts.SkipFiles {
		a, err := OpenArchive(s.opts.Archive)
		if err != nil {
			return err
		}
		defer a.Close()
		ar = a
	}
	for ownerRC, b := range pick {
		fm, ok := s.avFiles[b.fileRC]
		if !ok {
			s.sum.AvatarsSkipped++
			continue
		}
		if s.wm.AvatarTS != "" && !b.ts.After(parseRFC3339(s.wm.AvatarTS)) {
			continue // not newer than the previous run's watermark
		}
		if s.opts.DryRun || s.opts.SkipFiles {
			s.sum.Avatars++
			continue
		}
		attID := uuid.NewSHA1(nsAtt, []byte("avatar:"+b.fileRC)).String()
		key := storageKeyName(attID, "avatar-"+b.fileRC)
		upload := fm
		upload.contentType = firstNonEmpty(fm.contentType, "image/png")
		if content, err := assembleChunks(ar, s.avChunks[b.fileRC], fm.length); err != nil {
			s.sum.AvatarsSkipped++
			s.log.Warn("avatar reassembly failed", "fileid", b.fileRC, "err", err)
			continue
		} else if err := s.stg.PutObject(ctx, key, upload.contentType, fm.length, bytes.NewReader(content)); err != nil {
			s.sum.AvatarsSkipped++
			s.log.Warn("avatar upload failed", "fileid", b.fileRC, "err", err)
			continue
		}
		var createdAt *time.Time
		if !b.ts.IsZero() {
			createdAt = &b.ts
		}
		if _, err := s.st.Q.Exec(ctx, `
			INSERT INTO attachments (id, uploader_id, storage_key, filename, mime_type, size_bytes, status, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,'ready',$7) ON CONFLICT (id) DO NOTHING`,
			attID, s.idByRC[ownerRC], key, "avatar-"+b.fileRC, upload.contentType, fm.length, createdAt); err != nil {
			return fmt.Errorf("insert avatar attachment %s: %w", b.fileRC, err)
		}
		if _, err := s.st.Q.Exec(ctx, `
			UPDATE users SET avatar_attachment_id = $2, updated_at = now() WHERE id = $1`,
			s.idByRC[ownerRC], attID); err != nil {
			return fmt.Errorf("avatar link %s: %w", b.fileRC, err)
		}
		s.sum.Avatars++
	}
	return nil
}

// --- bson helpers ----------------------------------------------------------

func astr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bson.ObjectID:
		return x.Hex()
	default:
		return ""
	}
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case int32:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case bson.DateTime:
		return int64(x)
	default:
		return 0
	}
}

func asBool(v any, def bool) bool {
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

// atime returns (time, ok) for BSON DateTime (or a marshaled time.Time).
func atime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case bson.DateTime:
		return x.Time(), true
	case time.Time:
		return x, true
	default:
		return time.Time{}, false
	}
}

func atimeOr(v any, def time.Time) time.Time {
	if t, ok := atime(v); ok {
		return t
	}
	return def
}

// atimeNil maps absent timestamps to a zero time (NULL column).
func atimeNil(v any) time.Time {
	t, _ := atime(v)
	return t
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func asAnySlice(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case bson.A:
		return x
	default:
		return nil
	}
}

func asStrSlice(v any) []string {
	var arr []any
	switch x := v.(type) {
	case []any: // plain []interface{} (bson.A is a named type, not an alias)
		arr = x
	case bson.A:
		arr = x
	default:
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if str, ok := e.(string); ok && str != "" {
			out = append(out, str)
		}
	}
	return out
}

func sub(m bson.M, key string) bson.M {
	return asM(m[key])
}

// asM flattens an embedded BSON document (decoded as bson.D inside an
// "any" slot) into a bson.M view; pass-through for bson.M.
func asM(v any) bson.M {
	switch x := v.(type) {
	case bson.M:
		return x
	case bson.D:
		m := make(bson.M, len(x))
		for _, el := range x {
			m[el.Key] = el.Value
		}
		return m
	case bson.E:
		return bson.M{x.Key: x.Value}
	default:
		return nil
	}
}

func pickEmail(m bson.M) string {
	arr := asAnySlice(m["emails"])
	first, verified := "", ""
	for _, e := range arr {
		em := asM(e)
		if em == nil {
			continue
		}
		addr := strings.ToLower(strings.TrimSpace(astr(em["address"])))
		if addr == "" || !strings.Contains(addr, "@") {
			continue
		}
		if first == "" {
			first = addr
		}
		if verified == "" && asBool(em["verified"], false) {
			verified = addr
		}
	}
	return firstNonEmpty(verified, first)
}

// sanitizeName normalizes a source username to the users CHECK constraints
// (lowercase, [a-z0-9_.-], ≤64), suffixing collisions -2, -3, …
func sanitizeName(raw string, used map[string]bool) string {
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, strings.TrimSpace(raw))
	base = strings.Trim(base, "-._")
	if base == "" {
		return ""
	}
	if len(base) > 60 {
		base = base[:60]
	}
	name := base
	for i := 2; used[name]; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	used[name] = true
	return name
}
