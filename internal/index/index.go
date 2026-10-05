// Package index keeps one SQLite cache per sphere. Everything in it can be
// rebuilt from the store (`artefact reindex`).
package index

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS artefacts (
  id        TEXT PRIMARY KEY,
  digest    TEXT NOT NULL,
  title     TEXT NOT NULL,
  filename  TEXT NOT NULL,
  mime      TEXT NOT NULL,
  size      INTEGER NOT NULL,
  date      TEXT NOT NULL,
  version   INTEGER NOT NULL,
  sealed    INTEGER NOT NULL DEFAULT 0,
  deposited TEXT NOT NULL,
  updated   TEXT NOT NULL,
  path      TEXT NOT NULL,
  keyed     TEXT NOT NULL DEFAULT '',
  ext       TEXT NOT NULL DEFAULT '',
  archived  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS artefacts_digest ON artefacts(digest);
CREATE TABLE IF NOT EXISTS holders (
  id  TEXT NOT NULL,
  by  TEXT NOT NULL,
  ref TEXT NOT NULL,
  at  TEXT NOT NULL,
  PRIMARY KEY (id, by, ref)
);
CREATE INDEX IF NOT EXISTS holders_by ON holders(by, ref);
CREATE TABLE IF NOT EXISTS remote_files (
  backend   TEXT NOT NULL,
  path      TEXT NOT NULL,   -- local path, relative to the sphere's storage root
  remote    TEXT NOT NULL,   -- path on the backend (".age" added when encrypted)
  sha256    TEXT NOT NULL,   -- of the local file
  rsha256   TEXT NOT NULL,   -- of what was sent
  size      INTEGER NOT NULL,-- of what was sent
  synced    TEXT NOT NULL,
  verified  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (backend, path)
);
CREATE TABLE IF NOT EXISTS local_files (
  path   TEXT PRIMARY KEY,
  size   INTEGER NOT NULL,
  mtime  INTEGER NOT NULL,
  sha256 TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS classes (
  id         TEXT NOT NULL,
  plan       TEXT NOT NULL,
  node       TEXT NOT NULL,
  by         TEXT NOT NULL,
  confidence REAL NOT NULL,
  pending    INTEGER NOT NULL,
  PRIMARY KEY (id, plan)
);
CREATE TABLE IF NOT EXISTS keys (
  key    TEXT PRIMARY KEY,
  id     TEXT NOT NULL,
  digest TEXT NOT NULL
);
`

type Row struct {
	ID        string `json:"id"`
	Digest    string `json:"digest"`
	Title     string `json:"title"`
	Filename  string `json:"filename"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Date      string `json:"date"`
	Version   int    `json:"version"`
	Sealed    bool   `json:"sealed"`
	Deposited string `json:"deposited"`
	Updated   string `json:"updated"`
	Path      string `json:"-"`                  // object root, relative to the sphere's storage root
	Keyed     string `json:"-"`                  // keyed digest of a sealed payload's plaintext
	Ext       string `json:"ext"`                // payload extension, fixed at deposit
	Archived  string `json:"archived,omitempty"` // when the artefact was archived
}

type Holder struct {
	By  string `json:"by" yaml:"by"`
	Ref string `json:"ref,omitempty" yaml:"ref,omitempty"`
	At  string `json:"at" yaml:"at"`
}

type Index struct{ db *sql.DB }

var ErrNotFound = errors.New("index: not found")

func Open(path string) (*Index, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	// Indexes made by earlier versions lack the later columns.
	for _, col := range []string{"keyed", "ext", "archived"} {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('artefacts') WHERE name = ?`, col).Scan(&n)
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE artefacts ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				db.Close()
				return nil, fmt.Errorf("index %s: %w", path, err)
			}
		}
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS search USING fts5(id UNINDEXED, title, filename, meta, body, tokenize='unicode61 remove_diacritics 2')`); err != nil {
		db.Close()
		return nil, fmt.Errorf("index %s: full-text search: %w", path, err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS artefacts_keyed ON artefacts(keyed)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	return &Index{db: db}, nil
}

func (x *Index) Close() error { return x.db.Close() }

// Reset empties every table, before a rebuild.
func (x *Index) Reset() error {
	_, err := x.db.Exec(`DELETE FROM artefacts; DELETE FROM holders; DELETE FROM keys; DELETE FROM classes; DELETE FROM search;`)
	return err
}

// Put writes the row and replaces the artefact's holders.
func (x *Index) Put(r Row, holders []Holder) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO artefacts (id, digest, title, filename, mime, size, date, version, sealed, deposited, updated, path, keyed, ext, archived)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET digest=excluded.digest, title=excluded.title, filename=excluded.filename,
		mime=excluded.mime, size=excluded.size, date=excluded.date, version=excluded.version, sealed=excluded.sealed,
		deposited=excluded.deposited, updated=excluded.updated, path=excluded.path, keyed=excluded.keyed, ext=excluded.ext, archived=excluded.archived`,
		r.ID, r.Digest, r.Title, r.Filename, r.Mime, r.Size, r.Date, r.Version, r.Sealed, r.Deposited, r.Updated, r.Path, r.Keyed, r.Ext, r.Archived)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM holders WHERE id = ?`, r.ID); err != nil {
		return err
	}
	for _, h := range holders {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO holders (id, by, ref, at) VALUES (?,?,?,?)`, r.ID, h.By, h.Ref, h.At); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Delete removes an artefact, its holders and its idempotency keys.
func (x *Index) Delete(id string) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM artefacts WHERE id = ?`, `DELETE FROM holders WHERE id = ?`, `DELETE FROM keys WHERE id = ?`, `DELETE FROM classes WHERE id = ?`, `DELETE FROM search WHERE id = ?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (x *Index) PutKey(key, id, digest string) error {
	_, err := x.db.Exec(`INSERT INTO keys (key, id, digest) VALUES (?,?,?) ON CONFLICT(key) DO NOTHING`, key, id, digest)
	return err
}

// Key returns the artefact and digest a key was first used with.
func (x *Index) Key(key string) (id, digest string, err error) {
	err = x.db.QueryRow(`SELECT id, digest FROM keys WHERE key = ?`, key).Scan(&id, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

const cols = `id, digest, title, filename, mime, size, date, version, sealed, deposited, updated, path, keyed, ext, archived`

func scan(s interface{ Scan(...any) error }) (Row, error) {
	var r Row
	err := s.Scan(&r.ID, &r.Digest, &r.Title, &r.Filename, &r.Mime, &r.Size, &r.Date, &r.Version, &r.Sealed, &r.Deposited, &r.Updated, &r.Path, &r.Keyed, &r.Ext, &r.Archived)
	return r, err
}

func (x *Index) Get(id string) (Row, error) {
	r, err := scan(x.db.QueryRow(`SELECT `+cols+` FROM artefacts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

// ByDigest returns the unsealed artefact whose current payload has digest.
func (x *Index) ByDigest(digest string) (Row, error) {
	r, err := scan(x.db.QueryRow(`SELECT `+cols+` FROM artefacts WHERE digest = ? AND sealed = 0 ORDER BY deposited LIMIT 1`, digest))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

// ByKeyed returns the sealed artefact whose plaintext has this keyed digest.
func (x *Index) ByKeyed(keyed string) (Row, error) {
	r, err := scan(x.db.QueryRow(`SELECT `+cols+` FROM artefacts WHERE keyed = ? AND sealed = 1 ORDER BY deposited LIMIT 1`, keyed))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

func (x *Index) Holders(id string) ([]Holder, error) {
	rows, err := x.db.Query(`SELECT by, ref, at FROM holders WHERE id = ? ORDER BY at, by, ref`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Holder{}
	for rows.Next() {
		var h Holder
		if err := rows.Scan(&h.By, &h.Ref, &h.At); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

type Filter struct {
	HeldBy   string // tool, or tool:ref
	Class    string // plan, or plan:notation (descendants included)
	Pending  bool   // classified but waiting for a human (in Class's plan when given)
	State    string // "" every artefact, "active" not archived, "archived" archived only
	Orphaned bool
	Since    string // deposited on or after this date
	Limit    int
}

func (x *Index) List(f Filter) ([]Row, error) {
	var where []string
	var args []any
	if f.HeldBy != "" {
		by, ref, hasRef := strings.Cut(f.HeldBy, ":")
		if hasRef {
			where = append(where, `id IN (SELECT id FROM holders WHERE by = ? AND ref = ?)`)
			args = append(args, by, ref)
		} else {
			where = append(where, `id IN (SELECT id FROM holders WHERE by = ?)`)
			args = append(args, by)
		}
	}
	if f.Orphaned {
		where = append(where, `id NOT IN (SELECT id FROM holders)`)
	}
	plan, node, _ := strings.Cut(f.Class, ":")
	switch {
	case node != "":
		where = append(where, `id IN (SELECT id FROM classes WHERE plan = ? AND (node = ? OR node LIKE ? ESCAPE '\'))`)
		args = append(args, plan, node, likePrefix(node+"."))
	case plan != "":
		where = append(where, `id IN (SELECT id FROM classes WHERE plan = ?)`)
		args = append(args, plan)
	}
	if f.Pending {
		if plan != "" {
			where = append(where, `id IN (SELECT id FROM classes WHERE plan = ? AND pending = 1)`)
			args = append(args, plan)
		} else {
			where = append(where, `id IN (SELECT id FROM classes WHERE pending = 1)`)
		}
	}
	switch f.State {
	case "active":
		where = append(where, `archived = ''`)
	case "archived":
		where = append(where, `archived != ''`)
	}
	if f.Since != "" {
		where = append(where, `deposited >= ?`)
		args = append(args, f.Since)
	}
	q := `SELECT ` + cols + ` FROM artefacts`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY deposited DESC, id DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := x.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Usage struct {
	Artefacts int   `json:"artefacts"`
	Bytes     int64 `json:"payload_bytes"`
	Orphaned  int   `json:"orphaned"`
	Holders   int   `json:"holders"`
}

func (x *Index) Usage() (Usage, error) {
	var u Usage
	if err := x.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM artefacts`).Scan(&u.Artefacts, &u.Bytes); err != nil {
		return u, err
	}
	if err := x.db.QueryRow(`SELECT COUNT(*) FROM artefacts WHERE id NOT IN (SELECT id FROM holders)`).Scan(&u.Orphaned); err != nil {
		return u, err
	}
	err := x.db.QueryRow(`SELECT COUNT(*) FROM holders`).Scan(&u.Holders)
	return u, err
}

// RemoteFile is one file copied to a backend.
type RemoteFile struct {
	Backend, Path, Remote, SHA256, RSHA256 string
	Size                                   int64
	Synced, Verified                       string
}

func (x *Index) PutRemote(f RemoteFile) error {
	_, err := x.db.Exec(`INSERT INTO remote_files (backend, path, remote, sha256, rsha256, size, synced, verified) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(backend, path) DO UPDATE SET remote=excluded.remote, sha256=excluded.sha256, rsha256=excluded.rsha256,
		size=excluded.size, synced=excluded.synced, verified=excluded.verified`,
		f.Backend, f.Path, f.Remote, f.SHA256, f.RSHA256, f.Size, f.Synced, f.Verified)
	return err
}

func (x *Index) DeleteRemote(backend, path string) error {
	_, err := x.db.Exec(`DELETE FROM remote_files WHERE backend = ? AND path = ?`, backend, path)
	return err
}

func (x *Index) SetVerified(backend, path, at string) error {
	_, err := x.db.Exec(`UPDATE remote_files SET verified = ? WHERE backend = ? AND path = ?`, at, backend, path)
	return err
}

// Remotes lists the copied files of backend under prefix ("" for all).
func (x *Index) Remotes(backend, prefix string) ([]RemoteFile, error) {
	rows, err := x.db.Query(`SELECT backend, path, remote, sha256, rsha256, size, synced, verified FROM remote_files
		WHERE backend = ? AND path LIKE ? ESCAPE '\' ORDER BY path`, backend, likePrefix(prefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteFile
	for rows.Next() {
		var f RemoteFile
		if err := rows.Scan(&f.Backend, &f.Path, &f.Remote, &f.SHA256, &f.RSHA256, &f.Size, &f.Synced, &f.Verified); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RemoteBackends lists the backends that hold files under prefix.
func (x *Index) RemoteBackends(prefix string) ([]string, error) {
	rows, err := x.db.Query(`SELECT DISTINCT backend FROM remote_files WHERE path LIKE ? ESCAPE '\' ORDER BY backend`, likePrefix(prefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p) + "%"
}

// LocalHash returns the cached SHA-256 of a local file, if size and mtime match.
func (x *Index) LocalHash(path string, size, mtime int64) (string, bool) {
	var sha string
	err := x.db.QueryRow(`SELECT sha256 FROM local_files WHERE path = ? AND size = ? AND mtime = ?`, path, size, mtime).Scan(&sha)
	return sha, err == nil
}

func (x *Index) PutLocalHash(path string, size, mtime int64, sha string) error {
	_, err := x.db.Exec(`INSERT INTO local_files (path, size, mtime, sha256) VALUES (?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, sha256=excluded.sha256`, path, size, mtime, sha)
	return err
}

// ClassRow is the place of an artefact in one plan.
type ClassRow struct {
	Plan, Node, By string
	Confidence     float64
	Pending        bool
}

// PutClasses replaces the classes of an artefact.
func (x *Index) PutClasses(id string, cs []ClassRow) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM classes WHERE id = ?`, id); err != nil {
		return err
	}
	for _, c := range cs {
		if _, err := tx.Exec(`INSERT INTO classes (id, plan, node, by, confidence, pending) VALUES (?,?,?,?,?,?)`, id, c.Plan, c.Node, c.By, c.Confidence, c.Pending); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Classified lists the artefacts placed in plan, with their node.
func (x *Index) Classified(plan string) (map[string]ClassRow, error) {
	rows, err := x.db.Query(`SELECT id, node, by, confidence, pending FROM classes WHERE plan = ?`, plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ClassRow{}
	for rows.Next() {
		var id string
		c := ClassRow{Plan: plan}
		if err := rows.Scan(&id, &c.Node, &c.By, &c.Confidence, &c.Pending); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// PutSearch replaces the searchable text of an artefact.
func (x *Index) PutSearch(id, title, filename, meta, body string) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM search WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO search (id, title, filename, meta, body) VALUES (?,?,?,?,?)`, id, title, filename, meta, body); err != nil {
		return err
	}
	return tx.Commit()
}

// Hit is one search result.
type Hit struct {
	ID      string
	Score   float64 // bm25: lower is better
	Snippet string
}

// Search finds artefacts whose words start with every word of q, accents
// and case ignored; title and file name weigh more than the content.
func (x *Index) Search(q string, limit int) ([]Hit, error) {
	match := ftsQuery(q)
	if match == "" {
		return []Hit{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := x.db.Query(`SELECT id, bm25(search, 0, 10, 5, 2, 1) AS score,
		snippet(search, -1, '«', '»', '…', 12) FROM search WHERE search MATCH ? ORDER BY score LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Hit{}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.Score, &h.Snippet); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ftsQuery turns free words into an FTS5 query of quoted prefixes.
func ftsQuery(q string) string {
	var parts []string
	for _, w := range strings.FieldsFunc(q, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '@' || unicode.IsLetter(r) || unicode.IsDigit(r))
	}) {
		w = strings.Trim(w, "-_.@")
		if w == "" {
			continue
		}
		parts = append(parts, `"`+strings.ReplaceAll(w, `"`, `""`)+`"*`)
	}
	return strings.Join(parts, " AND ")
}
