// Package index keeps one SQLite cache per sphere. Everything in it can be
// rebuilt from the store (`artefact reindex`).
package index

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
  path      TEXT NOT NULL
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
	Path      string `json:"-"` // object root, relative to the sphere's storage root
}

type Holder struct {
	By  string `json:"by" yaml:"by"`
	Ref string `json:"ref,omitempty" yaml:"ref,omitempty"`
	At  string `json:"at" yaml:"at"`
}

type Index struct{ db *sql.DB }

var ErrNotFound = errors.New("index: not found")

func Open(path string) (*Index, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	return &Index{db: db}, nil
}

func (x *Index) Close() error { return x.db.Close() }

// Reset empties every table, before a rebuild.
func (x *Index) Reset() error {
	_, err := x.db.Exec(`DELETE FROM artefacts; DELETE FROM holders; DELETE FROM keys;`)
	return err
}

// Put writes the row and replaces the artefact's holders.
func (x *Index) Put(r Row, holders []Holder) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO artefacts (id, digest, title, filename, mime, size, date, version, sealed, deposited, updated, path)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET digest=excluded.digest, title=excluded.title, filename=excluded.filename,
		mime=excluded.mime, size=excluded.size, date=excluded.date, version=excluded.version, sealed=excluded.sealed,
		deposited=excluded.deposited, updated=excluded.updated, path=excluded.path`,
		r.ID, r.Digest, r.Title, r.Filename, r.Mime, r.Size, r.Date, r.Version, r.Sealed, r.Deposited, r.Updated, r.Path)
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

const cols = `id, digest, title, filename, mime, size, date, version, sealed, deposited, updated, path`

func scan(s interface{ Scan(...any) error }) (Row, error) {
	var r Row
	err := s.Scan(&r.ID, &r.Digest, &r.Title, &r.Filename, &r.Mime, &r.Size, &r.Date, &r.Version, &r.Sealed, &r.Deposited, &r.Updated, &r.Path)
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
	r, err := scan(x.db.QueryRow(`SELECT `+cols+` FROM artefacts WHERE digest = ? ORDER BY deposited LIMIT 1`, digest))
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
