// Package ingest receives log lines that rsyslog on the syslog server forwards to noc-bot.
//
// rsyslog sends each line in the same format it writes to /var/log/remote (template RemoteFmt):
//
//	<received RFC3339> <from-ip> <hostname> devtime=<device RFC3339> <syslogtag><message>
package ingest

import (
	"errors"
	"strings"
	"time"
)

// Line is one parsed log line.
type Line struct {
	Recv    time.Time // when the syslog server received it (the clock we trust)
	From    string    // sender IP
	Host    string    // hostname the sender reported
	Dev     time.Time // time the sender wrote, zero if missing or unparsable
	Program string    // syslog tag without pid and colon
	Msg     string    // message text
	Raw     string
}

// ErrFormat is returned for lines that do not follow RemoteFmt.
var ErrFormat = errors.New("ingest: line does not match RemoteFmt")

// Parse parses one RemoteFmt line.
func Parse(s string) (Line, error) {
	s = strings.TrimRight(s, "\r\n")
	l := Line{Raw: s}
	f := strings.SplitN(s, " ", 5)
	if len(f) < 5 || !strings.HasPrefix(f[3], "devtime=") {
		return l, ErrFormat
	}
	t, err := time.Parse(time.RFC3339Nano, f[0])
	if err != nil {
		return l, ErrFormat
	}
	l.Recv, l.From, l.Host = t, f[1], f[2]
	if d, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(f[3], "devtime=")); err == nil {
		l.Dev = d
	}
	rest := f[4]
	tag, msg, _ := strings.Cut(rest, " ")
	l.Program = programOf(tag)
	l.Msg = strings.TrimLeft(strings.TrimPrefix(msg, "\ufeff"), " ")
	return l, nil
}

// programOf turns "kea-dhcp4[12]:" or "Bgp:" into "kea-dhcp4" / "Bgp".
func programOf(tag string) string {
	tag = strings.TrimSuffix(tag, ":")
	if i := strings.IndexByte(tag, '['); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

// Skew is how far the sender's clock is from the syslog server's (Dev - Recv); zero if unknown.
func (l Line) Skew() time.Duration {
	if l.Dev.IsZero() {
		return 0
	}
	return l.Dev.Sub(l.Recv)
}
