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
CREATE TABLE IF NOT EXISTS threads (channel TEXT NOT NULL, ts TEXT NOT NULL, at TEXT NOT NULL, PRIMARY KEY(channel, ts));
CREATE TABLE IF NOT EXISTS verdicts (desk TEXT NOT NULL, subject TEXT NOT NULL, at TEXT NOT NULL, answers TEXT NOT NULL, PRIMARY KEY(desk, subject));
CREATE TABLE IF NOT EXISTS lines (at TEXT NOT NULL, host TEXT NOT NULL, program TEXT NOT NULL, msg TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS lines_at ON lines(at);
CREATE INDEX IF NOT EXISTS lines_host ON lines(host, at);
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
	// added later: labels people take back are kept but marked, and left out when laya is rebuilt
	var n int
	db.QueryRow(`SELECT count(*) FROM pragma_table_info('labels') WHERE name='retracted_at'`).Scan(&n)
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE labels ADD COLUMN retracted_at TEXT`); err != nil {
			db.Close()
			return nil, err
		}
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

// Subjects lists the subjects a desk has already decided with a given wording version (e.g.
// template ids for the line desk). A new wording means everything is judged once more.
func (s *Store) Subjects(ctx context.Context, desk, version string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT subject FROM decisions WHERE desk=? AND version=?`, desk, version)
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

// Thread remembers a thread noc-bot posted in.
func (s *Store) Thread(ctx context.Context, channel, threadTS string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO threads(channel,ts,at) VALUES(?,?,?)`, channel, threadTS, ts(at))
	return err
}

// IsThread reports whether noc-bot posted in a thread.
func (s *Store) IsThread(ctx context.Context, channel, threadTS string) bool {
	var n int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM threads WHERE channel=? AND ts=?`, channel, threadTS).Scan(&n)
	return n > 0
}

// DeleteNote removes a note (a memory people asked to forget).
func (s *Store) DeleteNote(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notes WHERE id=?`, id)
	return err
}

// NotesWithID lists notes of one kind with their ids, oldest first.
func (s *Store) NotesWithID(ctx context.Context, kind string, limit int) ([]NoteRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, subject, text, who FROM notes WHERE kind=? ORDER BY id DESC LIMIT ?`, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteRow
	for rows.Next() {
		var n NoteRow
		if err := rows.Scan(&n.ID, &n.At, &n.Subject, &n.Text, &n.Who); err != nil {
			return nil, err
		}
		out = append([]NoteRow{n}, out...)
	}
	return out, rows.Err()
}

// NoteRow is one note.
type NoteRow struct {
	ID                     int64
	At, Subject, Text, Who string
}

// LastAnswers returns the answers a desk last gave about a subject, per question (raw JSON).
func (s *Store) LastAnswers(ctx context.Context, desk, subject string) (map[string]json.RawMessage, bool) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT answers FROM decisions WHERE desk=? AND subject=? ORDER BY id DESC LIMIT 1`, desk, subject).Scan(&raw); err != nil {
		return nil, false
	}
	m := map[string]json.RawMessage{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil, false
	}
	return m, true
}

// NotesAbout returns the latest notes of a kind about one subject (newest first).
func (s *Store) NotesAbout(ctx context.Context, kind, subject string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT text FROM notes WHERE kind=? AND subject=? ORDER BY id DESC LIMIT ?`, kind, subject, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// NotesSince lists the texts of notes of a kind written after t (oldest first, at most 30).
func (s *Store) NotesSince(ctx context.Context, kind string, t time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT text FROM notes WHERE kind=? AND at>? ORDER BY id LIMIT 30`, kind, ts(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// RetractLabels marks the human labels given on an incident as taken back. It returns how many.
func (s *Store) RetractLabels(ctx context.Context, incidentID string, at time.Time) (int64, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE labels SET retracted_at=? WHERE origin='human' AND note=? AND retracted_at IS NULL`, ts(at), "incident "+incidentID)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// Verdict returns what the teacher last said about a subject (kept for 3 days).
func (s *Store) Verdict(ctx context.Context, desk, subject string) (map[string]any, bool) {
	var at, raw string
	if err := s.db.QueryRowContext(ctx, `SELECT at, answers FROM verdicts WHERE desk=? AND subject=?`, desk, subject).Scan(&at, &raw); err != nil {
		return nil, false
	}
	if t, err := time.Parse(time.RFC3339Nano, at); err != nil || time.Since(t) > 72*time.Hour {
		return nil, false
	}
	m := map[string]any{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil, false
	}
	return m, true
}

// SetVerdict merges the teacher's answers for a subject.
func (s *Store) SetVerdict(ctx context.Context, desk, subject string, answers map[string]any) {
	m, _ := s.Verdict(ctx, desk, subject)
	if m == nil {
		m = map[string]any{}
	}
	for k, v := range answers {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	s.db.ExecContext(ctx, `INSERT OR REPLACE INTO verdicts(desk,subject,at,answers) VALUES(?,?,?,?)`, desk, subject, ts(time.Now()), string(b))
}

// DropVerdict forgets the teacher's verdict for a subject (a person corrected it).
func (s *Store) DropVerdict(ctx context.Context, desk, subject string) {
	s.db.ExecContext(ctx, `DELETE FROM verdicts WHERE desk=? AND subject=?`, desk, subject)
}

// LineDecision is a stored line-desk decision.
type LineDecision struct{ Subject, Text, Answers string }

// LineDecisionsOf lists the line desk's decisions about one host's patterns in a time range.
func (s *Store) LineDecisionsOf(ctx context.Context, host string, from, to time.Time) ([]LineDecision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, text, answers FROM decisions WHERE desk='line' AND subject LIKE ? AND at>=? AND at<=? ORDER BY id LIMIT 50`,
		"%@"+host, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LineDecision
	for rows.Next() {
		var d LineDecision
		if err := rows.Scan(&d.Subject, &d.Text, &d.Answers); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Line is one received log line kept for later questions.
type Line struct {
	At                 time.Time
	Host, Program, Msg string
}

// AddLines stores received lines in one transaction.
func (s *Store) AddLines(ctx context.Context, ls []Line) error {
	if len(ls) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	st, err := tx.PrepareContext(ctx, `INSERT INTO lines(at,host,program,msg) VALUES(?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, l := range ls {
		if _, err := st.ExecContext(ctx, ts(l.At), l.Host, l.Program, l.Msg); err != nil {
			tx.Rollback()
			return err
		}
	}
	st.Close()
	return tx.Commit()
}

// PruneLines deletes lines older than keep.
func (s *Store) PruneLines(ctx context.Context, keep time.Duration) {
	s.db.ExecContext(ctx, `DELETE FROM lines WHERE at<?`, ts(time.Now().Add(-keep)))
}

// SearchLines finds stored lines: by host (exact, optional), text contained in the message or
// program (optional), in a time range, newest last. It also returns how many matched in total.
func (s *Store) SearchLines(ctx context.Context, host, contains string, from, to time.Time, limit int) ([]Line, int, error) {
	where, args := `at>=? AND at<=?`, []any{ts(from), ts(to)}
	if host != "" {
		where += ` AND host=?`
		args = append(args, host)
	}
	if contains != "" {
		where += " AND (msg LIKE ? ESCAPE '!' OR program LIKE ? ESCAPE '!')"
		pat := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(contains) + "%"
		args = append(args, pat, pat)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM lines WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT at,host,program,msg FROM (SELECT rowid r,at,host,program,msg FROM lines WHERE `+where+` ORDER BY rowid DESC LIMIT ?) ORDER BY r`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Line
	for rows.Next() {
		var l Line
		var at string
		if err := rows.Scan(&at, &l.Host, &l.Program, &l.Msg); err != nil {
			return nil, 0, err
		}
		l.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// IncidentsBetween returns stored incident bodies (JSON) opened in a time range, oldest first.
func (s *Store) IncidentsBetween(ctx context.Context, from, to time.Time, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM incidents WHERE opened>=? AND opened<=? ORDER BY opened LIMIT ?`, ts(from), ts(to), limit)
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

// SearchNotes finds notebook and memory entries containing a word, newest first.
func (s *Store) SearchNotes(ctx context.Context, word string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, kind, subject, text FROM notes WHERE kind IN ('memory','meaning','entity','observation','hypothesis','handover') AND (text LIKE ? OR subject LIKE ?) ORDER BY id DESC LIMIT ?`,
		"%"+word+"%", "%"+word+"%", limit)
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
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			at = t.In(time.FixedZone("JST", 9*3600)).Format("01/02 15:04")
		}
		out = append(out, at+" ["+kind+"] "+subject+": "+text)
	}
	return out, rows.Err()
}

// NotesBetween lists notes of the given kinds in a time range, oldest first.
func (s *Store) NotesBetween(ctx context.Context, from, to time.Time, limit int, kinds ...string) ([]string, error) {
	q := `SELECT kind, subject, text FROM notes WHERE at>=? AND at<=? AND kind IN (?` + strings.Repeat(`,?`, len(kinds)-1) + `) ORDER BY id LIMIT ?`
	args := []any{ts(from), ts(to)}
	for _, k := range kinds {
		args = append(args, k)
	}
	rows, err := s.db.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, subject, text string
		if err := rows.Scan(&kind, &subject, &text); err != nil {
			return nil, err
		}
		out = append(out, "["+kind+"] "+subject+": "+text)
	}
	return out, rows.Err()
}

// NotesBetweenRaw lists notes of one kind in a time range with their subjects, newest first.
func (s *Store) NotesBetweenRaw(ctx context.Context, from, to time.Time, kind string) ([]NoteRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, subject, text, who FROM notes WHERE kind=? AND at>=? AND at<=? ORDER BY id DESC LIMIT 100`, kind, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteRow
	for rows.Next() {
		var n NoteRow
		if err := rows.Scan(&n.ID, &n.At, &n.Subject, &n.Text, &n.Who); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
