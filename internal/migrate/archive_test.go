package migrate

import (
	"os"
	"path/filepath"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// marshDoc BSON-marshals m; bson.Marshal output already carries its own
// int32 length prefix — exactly what Archive.readDoc expects.
func marshDoc(t *testing.T, m bson.M) []byte {
	t.Helper()
	raw, err := bson.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %v: %v", m, err)
	}
	return raw
}

func metaDoc(db, coll string) bson.M {
	return bson.M{"db": db, "collection": coll}
}

func writeArchiveFile(t *testing.T, frames ...[]byte) string {
	t.Helper()
	buf := []byte{0x6d, 0xe2, 0x99, 0x81} // mongodump archive magic
	for _, f := range frames {
		buf = append(buf, f...)
	}
	p := filepath.Join(t.TempDir(), "fixture.archive")
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestArchiveStream(t *testing.T) {
	sentinel := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	p := writeArchiveFile(t,
		marshDoc(t, bson.M{"version": "0.1", "server_version": "8.2"}),
		marshDoc(t, metaDoc("rocketchat", "users")),
		marshDoc(t, bson.M{"_id": "u1", "username": "u"}),
		sentinel,
		marshDoc(t, metaDoc("rocketchat", "rocketchat_message")),
		marshDoc(t, bson.M{"_id": "m1", "rid": "GENERAL"}),
		marshDoc(t, metaDoc("rocketchat", "rocketchat_uploads.files")),
		marshDoc(t, bson.M{"_id": "f1", "length": int32(5)}),
		marshDoc(t, metaDoc("rocketchat", "rocketchat_uploads.chunks")),
		marshDoc(t, bson.M{"_id": "c1", "files_id": "f1", "n": int32(0)}),
	)

	ar, err := OpenArchive(p)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()

	type want struct {
		keyPath string
		id      string
	}
	seq := []want{
		{"", ""}, // dump header doc (no db/collection): falls through with empty ns
		{"users", "u1"},
		{"rocketchat_message", "m1"},
		{"rocketchat_uploads.files", "f1"},
		{"rocketchat_uploads.chunks", "c1"},
	}
	for i, w := range seq {
		d, ok, err := ar.Next()
		if err != nil || !ok {
			t.Fatalf("doc %d: ok=%v err=%v", i, ok, err)
		}
		if got := d.KeyPath(); got != w.keyPath {
			t.Fatalf("doc %d keypath %q want %q", i, got, w.keyPath)
		}
		if astr(d.Data["_id"]) != w.id {
			t.Fatalf("doc %d id %v want %q", i, d.Data["_id"], w.id)
		}
	}
	if d, ok, err := ar.Next(); ok || err != nil {
		t.Fatalf("EOF: ok=%v err=%v (last %v)", ok, err, d.NS)
	}
}

// chunkAssemble: chunk reassembly must order chunks by n and honor the
// declared length (GridFS payload integrity).
func TestAssembleChunks(t *testing.T) {
	p := writeArchiveFile(t,
		marshDoc(t, metaDoc("rocketchat", "rocketchat_uploads.chunks")),
		marshDoc(t, bson.M{"_id": "c1", "files_id": "f", "n": int32(1), "data": bson.Binary{Data: []byte("lo")}}),
		marshDoc(t, bson.M{"_id": "c0", "files_id": "f", "n": int32(0), "data": bson.Binary{Data: []byte("hel")}}),
	)
	ar, err := OpenArchive(p)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	var refs []chunkRef
	for {
		d, ok, err := ar.Next()
		if err == nil && !ok {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		off, length := ar.LastDoc()
		refs = append(refs, chunkRef{n: asInt(d.Data["n"]), off: off, length: length})
	}
	if len(refs) != 2 {
		t.Fatalf("collected %d chunk refs", len(refs))
	}
	got, err := assembleChunks(ar, refs, 5)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("assembled %q want %q", got, "hello")
	}
	if _, err := assembleChunks(ar, refs, 6); err == nil {
		t.Fatal("length mismatch accepted")
	}
}
