// Package store keeps what noc-bot must not forget: every decision laya made (with the exact text
// it read), every label people or outcomes gave, incidents, and the template miner's state. These
// rows are the training and evaluation data the event produces; keep them.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps a SQLite database.
type Store struct{ db *sql.DB }

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS decisions (
  id INTEGER PRIMARY KEY, at TEXT NOT NULL, desk TEXT NOT NULL, version TEXT NOT NULL,
  subject TEXT NOT NULL, text TEXT NOT NULL, answers TEXT NOT NULL, teacher_called INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS decisions_subject ON decisions(desk, subject);
CREATE TABLE IF NOT EXISTS labels (
  id INTEGER PRIMARY KEY, at TEXT NOT NULL, desk TEXT NOT NULL, text TEXT NOT NULL,
  answers TEXT NOT NULL, origin TEXT NOT NULL, who TEXT NOT NULL, correction INTEGER NOT NULL, note TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS outcomes (
  decision_id INTEGER PRIMARY KEY, due TEXT NOT NULL, desk TEXT NOT NULL, subject TEXT NOT NULL,
  text TEXT NOT NULL, predicted TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS incidents (
  id TEXT PRIMARY KEY, key TEXT NOT NULL, opened TEXT NOT NULL, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS notes (
  id INTEGER PRIMARY KEY, at TEXT NOT NULL, kind TEXT NOT NULL, subject TEXT NOT NULL, text TEXT NOT NULL, who TEXT NOT NULL);
`

// Open opens (and migrates) the database.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Decision records what a desk answered for a text. subject is what it was about (template id,
// incident id...). It returns the row id.
func (s *Store) Decision(ctx context.Context, at time.Time, desk, version, subject, text string, answers any, teacher bool) (int64, error) {
	a, _ := json.Marshal(answers)
	r, err := s.db.ExecContext(ctx, `INSERT INTO decisions(at,desk,version,subject,text,answers,teacher_called) VALUES(?,?,?,?,?,?,?)`,
		ts(at), desk, version, subject, text, string(a), teacher)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// Label records a right answer.
func (s *Store) Label(ctx context.Context, at time.Time, desk, text string, answers map[string]any, origin, who string, correction bool, note string) error {
	a, _ := json.Marshal(answers)
	_, err := s.db.ExecContext(ctx, `INSERT INTO labels(at,desk,text,answers,origin,who,correction,note) VALUES(?,?,?,?,?,?,?,?)`,
		ts(at), desk, text, string(a), origin, who, correction, note)
	return err
}

// ExpectOutcome schedules a later check of a decision against what actually happened.
func (s *Store) ExpectOutcome(ctx context.Context, decisionID int64, due time.Time, desk, subject, text string, predicted any) error {
	p, _ := json.Marshal(predicted)
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO outcomes(decision_id,due,desk,subject,text,predicted) VALUES(?,?,?,?,?,?)`,
		decisionID, ts(due), desk, subject, text, string(p))
	return err
}

// Outcome is a decision waiting to be checked.
type Outcome struct {
	DecisionID int64
	Desk       string
	Subject    string
	Text       string
	Predicted  string
}

// DueOutcomes lists outcome checks that are due.
func (s *Store) DueOutcomes(ctx context.Context, now time.Time) ([]Outcome, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT decision_id,desk,subject,text,predicted FROM outcomes WHERE done=0 AND due<=? LIMIT 100`, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outcome
	for rows.Next() {
		var o Outcome
		if err := rows.Scan(&o.DecisionID, &o.Desk, &o.Subject, &o.Text, &o.Predicted); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OutcomeDone marks an outcome checked.
func (s *Store) OutcomeDone(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outcomes SET done=1 WHERE decision_id=?`, id)
	return err
}

// Incident saves an incident snapshot.
func (s *Store) Incident(ctx context.Context, id, key string, opened time.Time, body any) error {
	b, _ := json.Marshal(body)
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO incidents(id,key,opened,body) VALUES(?,?,?,?)`, id, key, ts(opened), string(b))
	return err
}

// Put stores a blob.
func (s *Store) Put(ctx context.Context, k string, v []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO kv(k,v) VALUES(?,?)`, k, v)
	return err
}

// Get reads a blob (nil if missing).
func (s *Store) Get(ctx context.Context, k string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return v, err
}

// Subjects lists the subjects a desk has already decided (e.g. template ids for the line desk).
func (s *Store) Subjects(ctx context.Context, desk string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT subject FROM decisions WHERE desk=?`, desk)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// LastDecision returns the text a desk last read about a subject, and whether there was one.
func (s *Store) LastDecision(ctx context.Context, desk, subject string) (string, bool, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT text FROM decisions WHERE desk=? AND subject=? ORDER BY id DESC LIMIT 1`, desk, subject).Scan(&text)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return text, err == nil, err
}

// Note records something people said: a comment, a confirmed fact, a resolution.
func (s *Store) Note(ctx context.Context, at time.Time, kind, subject, text, who string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO notes(at,kind,subject,text,who) VALUES(?,?,?,?,?)`, ts(at), kind, subject, text, who)
	return err
}

// Notes lists the latest notes of the given kinds, newest first.
func (s *Store) Notes(ctx context.Context, limit int, kinds ...string) ([]string, error) {
	q := `SELECT at, kind, subject, text FROM notes`
	args := []any{}
	if len(kinds) > 0 {
		q += ` WHERE kind IN (?` + strings.Repeat(`,?`, len(kinds)-1) + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var at, kind, subject, text string
		if err := rows.Scan(&at, &kind, &subject, &text); err != nil {
			return nil, err
		}
		out = append(out, at[:16]+" ["+kind+"] "+subject+": "+text)
	}
	return out, rows.Err()
}

// PutJSON / GetJSON store small structured values (known issues and teams added from Slack).
func (s *Store) PutJSON(ctx context.Context, k string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Put(ctx, k, b)
}

func (s *Store) GetJSON(ctx context.Context, k string, v any) (bool, error) {
	b, err := s.Get(ctx, k)
	if err != nil || b == nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}
