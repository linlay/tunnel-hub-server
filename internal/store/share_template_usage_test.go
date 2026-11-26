package store

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShareTemplateUsageWindowsAndDistinctShares(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	current, historical, unseen := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	started := now.AddDate(0, 0, -40)
	if err := db.StartShareTemplateObservation(t.Context(), []string{current, historical, unseen}, started); err != nil {
		t.Fatal(err)
	}
	one, two := genericTestShare(t), genericTestShare(t)
	one.Title = "<script>not HTML</script>"
	if _, err := db.CreateShareWithResources(t.Context(), one, nil); err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		set, id string
		at      time.Time
	}{
		{current, one.ID, now.AddDate(0, 0, -31)},
		{current, one.ID, now.AddDate(0, 0, -10)},
		{current, one.ID, now.AddDate(0, 0, -1)},
		{current, one.ID, now}, {current, two.ID, now},
		{current, "", now}, {historical, "", now},
	} {
		if err := db.RecordShareTemplateUsage(t.Context(), event.set, event.id, event.at); err != nil {
			t.Fatal(err)
		}
	}
	// Concurrent requests increment the same bucket atomically; a second startup
	// must neither erase counters nor change the original observation time.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := db.RecordShareTemplateUsage(t.Context(), current, one.ID, now); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := db.StartShareTemplateObservation(t.Context(), []string{current, historical, unseen}, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, window := range []struct {
		start   string
		renders int64
	}{{"2026-11-12", 11}, {"2026-10-20", 12}} {
		usage, err := db.ShareTemplateUsage(t.Context(), window.start, "2026-11-18")
		if err != nil {
			t.Fatal(err)
		}
		got := usage[current]
		if got.RenderCount != window.renders || got.ShareCount != 2 || got.ResourceRequestCount != 1 || got.ObservedSince == nil || !got.ObservedSince.Equal(started) || got.LastRenderedAt == nil || !got.LastRenderedAt.Equal(now) {
			t.Fatalf("usage = %+v", got)
		}
		if usage[historical].RenderCount != 0 || usage[historical].ResourceRequestCount != 1 || usage[unseen].ObservedSince == nil || usage[unseen].LastRenderedAt != nil {
			t.Fatalf("historical/idle = %+v", usage)
		}
		if _, ok := usage[strings.Repeat("d", 64)]; ok {
			t.Fatal("invented coverage for unknown version")
		}
	}
	items, err := db.ListShareTemplateShares(t.Context(), current, "2026-10-20", "2026-11-18", 100)
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	for _, item := range items {
		if item.ID == one.ID && (item.Title != one.Title || item.RenderCount != 11) {
			t.Fatalf("detail=%+v", item)
		}
	}
}

func TestShareTemplateObservationValidationAndNoForeignBinding(t *testing.T) {
	db := openTestDB(t)
	assetSet := strings.Repeat("a", 64)
	if err := db.StartShareTemplateObservation(t.Context(), []string{assetSet, "bad"}, time.Now()); err == nil {
		t.Fatal("invalid set accepted")
	}
	usage, err := db.ShareTemplateUsage(t.Context(), "2026-01-01", "2026-12-31")
	if err != nil || len(usage) != 0 {
		t.Fatal("partial observation transaction", usage, err)
	}
	if err := db.RecordShareTemplateUsage(t.Context(), assetSet, "arbitrary-url-value", time.Now()); err == nil {
		t.Fatal("invalid share ID accepted")
	}
	if _, err := db.ListShareTemplateShares(t.Context(), assetSet, "2026-01-01", "2026-12-31", 101); err == nil {
		t.Fatal("unbounded detail accepted")
	}
}
