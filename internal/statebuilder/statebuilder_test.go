package statebuilder

import (
	"strings"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/stats"
)

func TestLineStateWords(t *testing.T) {
	now := time.Date(2026, 11, 25, 17, 35, 0, 0, time.UTC) // 02:35 JST
	s := LineState(Line{
		Template: "%BGP-3-NOTIFICATION: received from neighbor <*> (VRF default AS <NUM>) <NUM>/<NUM> (Hold Timer Expired Error/None) <NUM> bytes",
		Example:  "%BGP-3-NOTIFICATION: received from neighbor 163.220.192.86 (VRF default AS 45679) 4/0 (Hold Timer Expired Error/None) 0 bytes",
		Program:  "Bgp",
		Sender:   config.Sender{Name: "eps-rt-01", Role: "border router", Vendor: "arista"},
		Stats:    stats.Info{Level: stats.LevelFarAbove, LastHour: 19, Baseline: 0.2, FirstSeen: now.Add(-17 * time.Hour), LastSeen: now},
		Now:      now,
	})
	for _, want := range []string{"border router eps-rt-01 (arista)", "FAR ABOVE NORMAL (19 in the last hour, normally under 1 per hour)", "Known issue: none.", "night"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}
