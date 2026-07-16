package domain

import (
	"encoding/json"
	"testing"
)

func TestStoryCompassMigratesLegacyRootShape(t *testing.T) {
	var got StoryCompass
	err := json.Unmarshal([]byte(`{"ending_direction":"归乡","open_threads":["旧债"],"estimated_scale":"5卷","last_updated":9}`), &got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Long.EndingDirection != "归乡" || len(got.Long.OpenThreads) != 1 || got.Long.EstimatedScale != "5卷" || got.Long.LastUpdated != 9 || got.Current != nil {
		t.Fatalf("legacy migration failed: %+v", got)
	}
}

func TestStoryCompassCurrentShapeRoundTrip(t *testing.T) {
	want := StoryCompass{
		Long: LongCompass{
			EndingDirection: "终局",
			OpenThreads:     []string{"长线"},
			LastUpdated:     2,
			Reference:       json.RawMessage(`{"schema":"book-long.v1","rooms":[1,2]}`),
		},
		Current: &Compass{Direction: "近期", OpenThreads: []string{"短线"}, LastUpdated: 5},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got StoryCompass
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Current == nil || got.Current.Direction != "近期" || got.LatestUpdated() != 5 {
		t.Fatalf("round trip failed: %+v", got)
	}
}
