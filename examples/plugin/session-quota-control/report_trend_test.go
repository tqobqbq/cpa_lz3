package main

import (
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReportServerDateAdvancesBeyondAppliedDateFilter(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { time.Local = previous })
	store := newReportStore()
	store.configure(true, filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(store.close)
	q := url.Values{"start": {"2026-10-31"}, "end": {"2026-11-06"}}
	for _, test := range []struct {
		now  time.Time
		date string
	}{
		{time.Date(2026, 11, 6, 14, 59, 59, 0, time.UTC), "2026-11-06"},
		{time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC), "2026-11-07"},
	} {
		result, status, err := store.query(q, test.now)
		if err != nil || status != 200 {
			t.Fatalf("query failed: %d, %v", status, err)
		}
		if result.ServerDate != test.date || result.Timezone != "JST" {
			t.Fatalf("server calendar = %s %s, want %s JST", result.ServerDate, result.Timezone, test.date)
		}
	}
}

func TestReportTimelinePreservesCalendarBoundaries(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = location
	t.Cleanup(func() { time.Local = previous })
	for _, test := range []struct {
		name, start, end, bucket string
		count                    int
	}{
		{"spring-forward", "2026-03-08", "2026-03-09", "hour", 23},
		{"fall-back", "2026-11-01", "2026-11-02", "hour", 25},
		{"calendar-days", "2026-03-07", "2026-03-10", "day", 3},
		{"partial-months", "2024-01-31", "2024-03-02", "month", 3},
		{"leap-day", "2024-02-28", "2024-03-02", "day", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			start, _ := time.ParseInLocation("2006-01-02", test.start, location)
			end, _ := time.ParseInLocation("2006-01-02", test.end, location)
			bins, err := reportTimeline(start, end, test.bucket)
			if err != nil || len(bins) != test.count {
				t.Fatalf("interval count = %d, want %d, error %v", len(bins), test.count, err)
			}
			labels := make(map[string]bool)
			for i, bin := range bins {
				if labels[bin.Label] || !reportBucketStart(bin.StartedAt.Add(time.Minute), test.bucket).Equal(bin.StartedAt) {
					t.Fatalf("ambiguous interval: %+v", bin)
				}
				labels[bin.Label] = true
				if i > 0 && !bin.StartedAt.After(bins[i-1].StartedAt) {
					t.Fatalf("interval order is not chronological: %+v", bins)
				}
			}
		})
	}
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, location)
	if _, err := reportTimeline(start, start.AddDate(1, 0, 0), "hour"); err == nil {
		t.Fatal("unbounded hourly timeline accepted")
	}
}

func TestReportTrendFillsIdleIntervalsAndMatchesFilteredSummary(t *testing.T) {
	installTestPrices(t)
	store := newReportStore()
	store.configure(true, filepath.Join(t.TempDir(), "usage.sqlite"))
	t.Cleanup(store.close)
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.Local)
	for i, offset := range []int{0, 2, 2} {
		r := trafficFixture()
		r.ID = []string{"first", "second", "excluded"}[i]
		r.StartedAt, r.CompletedAt = start.AddDate(0, 0, offset).Add(time.Hour), start.AddDate(0, 0, offset).Add(2*time.Hour)
		if i == 2 {
			r.APIKey = "sk-other-client"
		}
		store.change(r.ID, r.StartedAt, func(record *reportRequest) { *record = r })
	}
	now := start.AddDate(0, 0, 3).Add(4 * time.Hour)
	for _, bucket := range []string{"hour", "day", "month"} {
		q := url.Values{"start": {"2026-11-02"}, "end": {"2026-11-05"}, "bucket": {bucket}, "q": {"sk-client-alice"}, "page_size": {"1"}}
		result, status, err := store.query(q, now)
		if err != nil || status != 200 || result.Total != 2 || len(result.Events) != 1 {
			t.Fatalf("query failed: %+v, %d, %v", result, status, err)
		}
		var total reportMetrics
		for _, bin := range result.Buckets {
			total.add(bin.reportMetrics)
			if bin.StartedAt.After(now) {
				t.Fatalf("future interval presented as zero usage: %+v", bin)
			}
		}
		if !reflect.DeepEqual(total, result.Summary) {
			t.Fatalf("trend differs from filtered, unpaginated summary: %+v != %+v", total, result.Summary)
		}
		if bucket == "day" && (len(result.Buckets) != 4 || result.Buckets[1].Requests != 0 || result.Buckets[1].Total != 0 || result.Buckets[3].Requests != 0) {
			t.Fatalf("missing empty intervals: %+v", result.Buckets)
		}
		if bucket == "hour" && len(result.Buckets) != 77 {
			t.Fatalf("current hour missing or future hours included: %d", len(result.Buckets))
		}
	}
	result, status, err := store.query(url.Values{"start": {"2026-11-06"}, "end": {"2026-11-07"}}, now)
	if err != nil || status != 200 || len(result.Buckets) != 0 || result.Total != 0 {
		t.Fatalf("future date range: %+v, %d, %v", result, status, err)
	}
	current := trafficFixture()
	current.ID, current.StartedAt = "current-millisecond", now
	store.change(current.ID, current.StartedAt, func(record *reportRequest) { *record = current })
	result, status, err = store.query(url.Values{"start": {"2026-11-05"}, "end": {"2026-11-05"}, "q": {current.ID}, "bucket": {"hour"}}, now)
	if err != nil || status != 200 || result.Total != 1 || result.Buckets[len(result.Buckets)-1].Requests != 1 {
		t.Fatalf("request in current millisecond omitted: %+v, %d, %v", result, status, err)
	}
}
