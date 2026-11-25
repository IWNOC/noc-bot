// Package drain groups log messages into templates ("patterns"), in the spirit of the Drain
// algorithm: variable parts (addresses, numbers, ids) are masked, messages are grouped by program,
// token count and first token, and within a group the most similar template absorbs the message,
// turning the positions that differ into <*>.
//
// A template keeps the ID it was born with even as it generalises, so labels and statistics keyed
// on the ID stay attached to it.
package drain

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// Wildcard marks a position that varies between messages of one template.
const Wildcard = "<*>"

var masks = []struct {
	re  *regexp.Regexp
	tok string
}{
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`), "<TS>"},
	{regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b`), "<MAC>"},
	// IPv6: compressed (contains "::") or full (at least five groups), so clock times like 10:33:23 stay
	{regexp.MustCompile(`(?i)(?:[0-9a-f]{1,4}:)*[0-9a-f]{0,4}::(?:[0-9a-f]{1,4}:)*[0-9a-f]{0,4}(?:/\d{1,3})?(?:#\d+)?`), "<IP6>"},
	{regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){4,7}[0-9a-f]{1,4}\b(?:/\d{1,3})?(?:#\d+)?`), "<IP6>"},
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?:/\d{1,2})?(?:[#:]\d+)?\b`), "<IP>"},
	{regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b|\b[0-9a-f]{12,}\b`), "<HEX>"},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?\b`), "<NUM>"},
}

// Mask replaces variable parts of a message with placeholders.
func Mask(msg string) string {
	for _, m := range masks {
		msg = m.re.ReplaceAllString(msg, m.tok)
	}
	return msg
}

// Template is one learned pattern.
type Template struct {
	ID      string   `json:"id"`
	Program string   `json:"program"`
	Tokens  []string `json:"tokens"`
	Count   uint64   `json:"count"`
}

// Text is the template as a string.
func (t *Template) Text() string { return strings.Join(t.Tokens, " ") }

// Match is the result of adding one message.
type Match struct {
	ID       string
	Template string
	New      bool // the message created a new template
}

// Miner learns templates. It is safe for concurrent use.
type Miner struct {
	mu     sync.Mutex
	sim    float64
	max    int // templates per group before the least used one is evicted
	groups map[string][]*Template
}

// New returns a miner. sim is the share of non-wildcard tokens that must match (0.4–0.6 works well).
func New(sim float64) *Miner {
	return &Miner{sim: sim, max: 200, groups: map[string][]*Template{}}
}

func groupKey(program string, toks []string) string {
	first := ""
	if len(toks) > 0 {
		first = toks[0]
		if strings.HasPrefix(first, "<") {
			first = "<v>"
		}
	}
	return program + "\x00" + itoa(len(toks)) + "\x00" + first
}

// Add learns from one message and returns its template.
func (m *Miner) Add(program, msg string) Match {
	toks := strings.Fields(Mask(msg))
	key := groupKey(program, toks)
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Template
	bestScore := -1.0
	for _, t := range m.groups[key] {
		if s := similarity(t.Tokens, toks); s >= m.sim && s > bestScore {
			best, bestScore = t, s
		}
	}
	if best == nil {
		t := &Template{ID: makeID(program, toks), Program: program, Tokens: toks, Count: 1}
		g := append(m.groups[key], t)
		if len(g) > m.max {
			g = evict(g)
		}
		m.groups[key] = g
		return Match{ID: t.ID, Template: t.Text(), New: true}
	}
	for i := range best.Tokens {
		if best.Tokens[i] != toks[i] {
			best.Tokens[i] = Wildcard
		}
	}
	best.Count++
	return Match{ID: best.ID, Template: best.Text()}
}

func similarity(tpl, toks []string) float64 {
	same, n := 0, 0
	for i := range tpl {
		if tpl[i] == Wildcard {
			continue
		}
		n++
		if tpl[i] == toks[i] {
			same++
		}
	}
	if n == 0 {
		return 1
	}
	return float64(same) / float64(n)
}

func evict(g []*Template) []*Template {
	low := 0
	for i, t := range g {
		if t.Count < g[low].Count {
			low = i
		}
	}
	return append(g[:low], g[low+1:]...)
}

func makeID(program string, toks []string) string {
	h := sha1.Sum([]byte(program + "\x00" + strings.Join(toks, " ")))
	return hex.EncodeToString(h[:6])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Snapshot serialises the miner so templates (and their IDs) survive restarts.
func (m *Miner) Snapshot() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []*Template
	for _, g := range m.groups {
		all = append(all, g...)
	}
	return json.Marshal(all)
}

// Restore loads a snapshot.
func (m *Miner) Restore(b []byte) error {
	var all []*Template
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.groups = map[string][]*Template{}
	for _, t := range all {
		k := groupKey(t.Program, t.Tokens)
		m.groups[k] = append(m.groups[k], t)
	}
	return nil
}

// Len is the number of templates.
func (m *Miner) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, g := range m.groups {
		n += len(g)
	}
	return n
}
