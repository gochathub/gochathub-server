// Package migrate imports foreign chat data into gochatserver.
package migrate

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Doc is one data document from a mongodump archive.
type Doc struct {
	NS   string // full namespace, e.g. "rocketchat.rocketchat_message"
	Data bson.M
}

// KeyPath returns the namespace minus the db prefix: a GridFS bucket
// "rocketchat.rocketchat_uploads.files" keys as "rocketchat_uploads.files";
// a plain collection keys as itself.
func (d Doc) KeyPath() string {
	if i := strings.IndexByte(d.NS, '.'); i >= 0 {
		return d.NS[i+1:]
	}
	return d.NS
}

// countReader tracks bytes consumed so Archive can report absolute doc offsets.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Archive streams a mongodump --archive file (raw BSON, no gzip).
//
// Layout (verified against server 8.2.12 / tools 100.18.0 output): 4-byte
// magic, then [int32 LE size][BSON doc] repeating. A doc carrying db and
// collection but no _id is metadata (collection header/index docs) and only
// switches the current namespace.
type Archive struct {
	f   *os.File
	cr  *countReader
	ns  string
	off int64 // byte offset of the size prefix of the last doc returned
	len int64 // its full serialized length (prefix included)
}

func OpenArchive(path string) (*Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	a := &Archive{f: f}
	a.cr = &countReader{r: bufio.NewReaderSize(f, 1<<20)}
	if _, err := io.CopyN(io.Discard, a.cr, 4); err != nil {
		f.Close()
		return nil, fmt.Errorf("archive header: %w", err)
	}
	return a, nil
}

func (a *Archive) Close() error { return a.f.Close() }

// LastDoc reports the absolute offset/length (size prefix included) of the
// most recently returned document, for by-offset re-reads of GridFS chunks.
func (a *Archive) LastDoc() (int64, int64) { return a.off, a.len }

func (a *Archive) readDoc() (bson.M, error) {
	start := a.cr.n
	var lenBuf [4]byte
	if _, err := io.ReadFull(a.cr, lenBuf[:]); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("doc size: %w", err)
	}
	size := int64(binary.LittleEndian.Uint32(lenBuf[:]))
	if size == 0xFFFFFFFF { // end-of-section sentinel written by mongodump
		return a.readDoc()
	}
	if size < 5 || size > 48<<20 { // BSON caps at 16MB; GridFS chunk docs stay far below
		return nil, fmt.Errorf("doc size %d out of range at offset %d", size, start)
	}
	raw := make([]byte, size)
	copy(raw[:4], lenBuf[:])
	if _, err := io.ReadFull(a.cr, raw[4:]); err != nil {
		return nil, fmt.Errorf("doc body at offset %d: %w", start, err)
	}
	a.off, a.len = start, size
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("bson decode at offset %d: %w", start, err)
	}
	return m, nil
}

func isMetaDoc(m bson.M) bool {
	_, db := m["db"]
	_, coll := m["collection"]
	_, id := m["_id"]
	return db && coll && !id
}

// Next returns the next data document, advancing the namespace over metadata
// docs. ok=false marks clean EOF.
func (a *Archive) Next() (d Doc, ok bool, err error) {
	for {
		m, err := a.readDoc()
		if err == io.EOF {
			return Doc{}, false, nil
		}
		if err != nil {
			return Doc{}, false, err
		}
		if isMetaDoc(m) {
			a.ns = fmt.Sprint(m["db"]) + "." + fmt.Sprint(m["collection"])
			continue
		}
		return Doc{NS: a.ns, Data: m}, true, nil
	}
}

// DocAt re-reads the doc previously visited at (off, len) — used by the file
// phase to fetch individual GridFS chunks without a second full stream pass.
func (a *Archive) DocAt(off, length int64) (bson.M, error) {
	raw := make([]byte, length)
	if _, err := a.f.ReadAt(raw, off); err != nil {
		return nil, fmt.Errorf("chunk read at offset %d: %w", off, err)
	}
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("chunk decode at offset %d: %w", off, err)
	}
	return m, nil
}
