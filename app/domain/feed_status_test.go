package domain

import (
	"testing"
	"time"
)

// A subscription that stopped working was only visible in the server log, so a
// feed could 404 for months while the UI showed it as a normal row. The status
// fields are what lets the UI say "this one is broken".
func TestFeedFailingStatus(t *testing.T) {
	tests := []struct {
		name          string
		feed          Feed
		wantFailing   bool
		wantUnchecked bool
	}{
		{
			name:        "healthy",
			feed:        Feed{OKAt: time.Now(), LastItems: 5},
			wantFailing: false, wantUnchecked: false,
		},
		{
			name:        "publisher went away",
			feed:        Feed{LastError: "rss: got status 404", LastErrorAt: time.Now(), Failures: 3, OKAt: time.Now().Add(-24 * time.Hour)},
			wantFailing: true, wantUnchecked: false,
		},
		{
			// A blank message is not a failure, and with no timestamps either
			// this is a feed that has simply never been attempted.
			name:        "error text is only whitespace",
			feed:        Feed{LastError: "   "},
			wantFailing: false, wantUnchecked: true,
		},
		{
			name:        "never fetched",
			feed:        Feed{Name: "just added"},
			wantFailing: false, wantUnchecked: true,
		},
		{
			name:        "fetched, no items",
			feed:        Feed{OKAt: time.Now(), LastItems: 0},
			wantFailing: false, wantUnchecked: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.feed.Failing(); got != tc.wantFailing {
				t.Errorf("Failing() = %v, want %v", got, tc.wantFailing)
			}
			if got := tc.feed.NeverChecked(); got != tc.wantUnchecked {
				t.Errorf("NeverChecked() = %v, want %v", got, tc.wantUnchecked)
			}
		})
	}
}

// The badge in the UI distinguishes "broken since Tuesday" from "never polled
// yet", which needs the two timestamps.
func TestFeedStatusAge(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	feed := Feed{LastErrorAt: now.Add(-3 * time.Hour), Failures: 7}
	if got := feed.StatusAge(now); got != 3*time.Hour {
		t.Errorf("StatusAge = %v, want 3h", got)
	}
	// A feed with no attempt at all has no age; the caller must not get a
	// nonsense duration out of the zero time.
	if got := (Feed{}).StatusAge(now); got != 0 {
		t.Errorf("StatusAge of an unchecked feed = %v, want 0", got)
	}
	if got := (Feed{LastError: "boom"}).StatusAge(now); got != 0 {
		t.Errorf("StatusAge without a timestamp = %v, want 0", got)
	}
}

// A publisher asking for a refresh interval of a week is polite to say and ruinous
// for a news reader, so the fetcher can cap what it honours. The UI has to show the
// same number the fetcher uses, or a row that looks stuck is not explainable.
func TestFeedEffectiveTTL(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		feed    Feed
		maxTTL  time.Duration
		wantDue time.Duration // 0 means due now
	}{
		{name: "never fetched", feed: Feed{}, wantDue: 0},
		{name: "no ttl published", feed: Feed{FetchedAt: now.Add(-time.Minute)}, wantDue: 0},
		{
			name:    "ttl honoured exactly when there is no cap",
			feed:    Feed{FetchedAt: now.Add(-5 * time.Minute), TTL: time.Hour},
			wantDue: 55 * time.Minute,
		},
		{
			name:    "ttl capped",
			feed:    Feed{FetchedAt: now.Add(-5 * time.Minute), TTL: time.Hour},
			maxTTL:  15 * time.Minute,
			wantDue: 10 * time.Minute,
		},
		{
			name:    "cap shorter than what already passed",
			feed:    Feed{FetchedAt: now.Add(-20 * time.Minute), TTL: time.Hour},
			maxTTL:  15 * time.Minute,
			wantDue: 0,
		},
		{
			// The fetch was recorded ahead of the clock we now believe (an NTP
			// correction after startup). Held back for one ttl, not for the skew
			// plus the ttl.
			name:    "a future fetchedAt (clock skew) postpones at most one ttl",
			feed:    Feed{FetchedAt: now.Add(time.Hour), TTL: time.Minute},
			wantDue: time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.feed.DueIn(now, tc.maxTTL); got != tc.wantDue {
				t.Errorf("DueIn(%v) = %v, want %v", tc.maxTTL, got, tc.wantDue)
			}
		})
	}
}
