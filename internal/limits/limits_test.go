package limits

import (
	"strconv"
	"testing"
	"time"
)

func TestBucketStampAligned(t *testing.T) {
	now := time.Unix(1234567, 0)
	b := BucketStamp(now)
	sz := int64(BucketSize.Seconds())
	if b%sz != 0 {
		t.Errorf("bucket %d not aligned to %d", b, sz)
	}
	if b > now.Unix() || now.Unix()-b >= sz {
		t.Errorf("bucket %d not within the current window of %d", b, now.Unix())
	}
}

func TestSumWindows(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	stamp := func(ago time.Duration) string {
		return strconv.FormatInt(BucketStamp(now.Add(-ago)), 10)
	}
	fields := map[string]string{
		stamp(1 * time.Hour):      "100", // within all windows
		stamp(6 * time.Hour):      "50",  // within 24h and 7d, outside 5h
		stamp(8 * 24 * time.Hour): "999", // outside everything
		stamp(2 * 24 * time.Hour): "7",   // within 7d only
		"not-a-number":            "5",   // ignored
		stamp(3 * time.Hour):      "bad", // value ignored
	}
	sums := SumWindows(now, fields)
	if sums["5h"] != 100 {
		t.Errorf("5h = %d, want 100", sums["5h"])
	}
	if sums["24h"] != 150 {
		t.Errorf("24h = %d, want 150", sums["24h"])
	}
	if sums["7d"] != 157 {
		t.Errorf("7d = %d, want 157", sums["7d"])
	}
}

func TestExpiredFields(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	cutoff := now.Add(-maxWindow() - BucketSize).Unix()
	recent := strconv.FormatInt(BucketStamp(now), 10)
	old := strconv.FormatInt(BucketStamp(now.Add(-9*24*time.Hour)), 10)
	exp := expiredFields(cutoff, map[string]string{recent: "1", old: "1", "junk": "1"})
	// old and junk are expired/invalid; recent is not.
	if len(exp) != 2 {
		t.Errorf("expired = %v, want 2 entries", exp)
	}
	for _, f := range exp {
		if f == recent {
			t.Errorf("recent bucket %s wrongly marked expired", recent)
		}
	}
}
