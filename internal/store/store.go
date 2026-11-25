// Package store keeps what noc-bot must not forget: every decision laya made (with the exact text
// it read), every label people or outcomes gave, incidents, and the template miner's state. These
// rows are the training and evaluation data the event produces; keep them.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
