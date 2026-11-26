//go:build live

package bot

// Replays stored incidents through the tier policy:
//   REPLAY_DB=/path/copy.db go test -tags live -run TestReplayTiers -v ./internal/bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/rules"
	"github.com/IWNOC/noc-bot/internal/store"
)

func TestReplayTiers(t *testing.T) {
	path := os.Getenv("REPLAY_DB")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := sql.Open("sqlite", path)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, _ := New(&config.Config{}, log, st, nil, nil, &fakeSlack{cards: map[string]string{}})
	rows, err := db.Query(`SELECT body FROM incidents ORDER BY opened`)
	if err != nil {
		t.Fatal(err)
	}
	var all []incident.Incident
	for rows.Next() {
		var raw string
		rows.Scan(&raw)
		var in incident.Incident
		json.Unmarshal([]byte(raw), &in)
		all = append(all, in)
	}
	// config changes as noc-bot would have seen them
	for _, in := range all {
		if configChangeRe.MatchString(in.Template) {
			b.noteConfigChange(in.Host, in.Template, in.Opened)
		}
	}
	attended := map[string]bool{"1126-0065": true, "1126-0070": true, "1126-0081": true}
	perHour := map[string]map[string]int{}
	byTier := map[string]int{}
	ctx := context.Background()
	for _, in := range all {
		tier, why, _ := b.decideTier(ctx, in, rules.Signal{Kind: in.Kind, TplID: in.TplID})
		byTier[tier]++
		h := in.Opened.In(jst).Format("01/02 15")
		if perHour[h] == nil {
			perHour[h] = map[string]int{}
		}
		perHour[h][tier]++
		mark := ""
		if attended[in.ID] {
			mark = "  <== people reacted"
		}
		if tier == TierNow || attended[in.ID] {
			t.Logf("%s %-6s %-9s %-16s %s | %s%s", in.ID, tier, in.Kind, in.Host, clipText(in.Template, 60), why, mark)
		}
	}
	var hours []string
	for h := range perHour {
		hours = append(hours, h)
	}
	sort.Strings(hours)
	for _, h := range hours {
		t.Logf("%s JST  now=%d digest=%d silent=%d", h, perHour[h][TierNow], perHour[h][TierDigest], perHour[h][TierSilent])
	}
	t.Logf("total %d: %v", len(all), byTier)
	_ = fmt.Sprint
	_ = time.Now
}
