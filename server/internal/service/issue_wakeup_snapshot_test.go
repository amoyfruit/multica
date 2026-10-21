package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestWakeupSnapshotKeepsHistoricalSchedules(t *testing.T) {
	w := db.IssueWakeup{Kind: "at", Mode: "once", EventTypes: []string{}, Timezone: "UTC"}
	var persisted [][]byte
	for _, hour := range []int{9, 10, 11} {
		w.NextFireAt = pgtype.Timestamptz{Time: time.Date(2099, 1, 1, hour, 0, 0, 0, time.UTC), Valid: true}
		raw, err := json.Marshal(wakeupPreview(w))
		if err != nil {
			t.Fatal(err)
		}
		persisted = append(persisted, raw)
	}
	w.NextFireAt = pgtype.Timestamptz{}
	for i, raw := range persisted {
		var snapshot struct {
			Kind       string  `json:"kind"`
			NextFireAt string  `json:"next_fire_at"`
			Interval   *int64  `json:"interval_seconds"`
			Cron       *string `json:"cron_expression"`
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		want := time.Date(2099, 1, 1, 9+i, 0, 0, 0, time.UTC).Format(time.RFC3339)
		if snapshot.NextFireAt != want || snapshot.Kind != "at" || snapshot.Interval != nil || snapshot.Cron != nil {
			t.Fatalf("historical snapshot changed: %s", raw)
		}
	}
}
