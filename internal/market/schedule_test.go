package market

import (
	"testing"
	"time"
)

func stamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
func session(open, close string) Session { return Session{Open: stamp(open), Close: stamp(close)} }

func TestNextAcrossSessionsBreaksAndMissedCloses(t *testing.T) {
	lunchStart, lunchEnd := stamp("2026-11-18T04:00:00Z"), stamp("2026-11-18T05:00:00Z")
	p := &Schedule{ValidUntil: stamp("2026-12-10T00:00:00Z"), Markets: []Exchange{
		{MIC: "XETR", Sessions: []Session{session("2026-11-17T08:00:00Z", "2026-11-17T16:30:00Z"), session("2026-11-18T08:00:00Z", "2026-11-18T16:30:00Z"), session("2026-11-19T08:00:00Z", "2026-11-19T16:30:00Z")}},
	}}
	for _, tc := range []struct{ name, last, now, want, reason string }{
		{"overnight already fresh", "2026-11-17T16:50:01Z", "2026-11-18T03:00:00Z", "2026-11-18T08:00:00Z", "Market-hours refresh"},
		{"missed close after restart", "2026-11-17T16:20:00Z", "2026-11-18T03:00:00Z", "2026-11-18T03:00:00Z", "After market close"},
		{"open but recent", "2026-11-18T10:02:00Z", "2026-11-18T10:03:00Z", "2026-11-18T10:17:00Z", "Market-hours refresh"},
		{"open stale", "2026-11-18T10:02:00Z", "2026-11-18T10:25:00Z", "2026-11-18T10:25:00Z", "Market-hours refresh"},
		{"wait for delayed closing prices", "2026-11-18T16:25:00Z", "2026-11-18T16:31:00Z", "2026-11-18T16:50:00Z", "After market close"},
		{"closing sync completed", "2026-11-18T16:50:00Z", "2026-11-18T17:00:00Z", "2026-11-19T08:00:00Z", "Market-hours refresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Next(stamp(tc.last), stamp(tc.now), 15*time.Minute)
			if !got.At.Equal(stamp(tc.want)) || got.Reason != tc.reason {
				t.Fatalf("got %+v, want %s %s", got, tc.want, tc.reason)
			}
		})
	}
	// A second held venue keeps refreshes running after the first venue closes.
	p.Markets = append(p.Markets, Exchange{MIC: "XNYS", Sessions: []Session{session("2026-11-18T14:30:00Z", "2026-11-18T21:00:00Z")}})
	if got := p.Next(stamp("2026-11-18T16:50:00Z"), stamp("2026-11-18T17:00:00Z"), 15*time.Minute); !got.At.Equal(stamp("2026-11-18T17:05:00Z")) {
		t.Fatal(got)
	}
	p.Markets = []Exchange{{MIC: "XHKG", Sessions: []Session{{Open: stamp("2026-11-18T01:30:00Z"), Close: stamp("2026-11-18T08:00:00Z"), BreakStart: &lunchStart, BreakEnd: &lunchEnd}}}}
	if got := p.Next(stamp("2026-11-18T03:55:00Z"), stamp("2026-11-18T04:05:00Z"), 15*time.Minute); !got.At.Equal(lunchEnd) {
		t.Fatal(got)
	}
	if p.Markets[0].IsOpen(stamp("2026-11-18T04:05:00Z")) || !p.Markets[0].IsOpen(lunchEnd) {
		t.Fatal("incorrect lunch-break state")
	}
	if got := p.Next(stamp("2026-11-18T03:55:00Z"), stamp("2026-12-11T04:05:00Z"), 15*time.Minute); !got.At.IsZero() {
		t.Fatal("expired calendar used", got)
	}
}

func TestCalendarValidation(t *testing.T) {
	now := stamp("2026-11-18T10:00:00Z")
	p := Schedule{GeneratedAt: now, ValidUntil: now.AddDate(0, 0, 28), HoldingCount: 1, Markets: []Exchange{{MIC: "XETR", Timezone: "Europe/Berlin", Sessions: []Session{session("2026-11-18T08:00:00Z", "2026-11-18T16:30:00Z")}}}}
	if err := p.Validate(now, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(now, 2); err == nil {
		t.Fatal("accepted a plan for a different portfolio")
	}
	p.Markets[0].Sessions[0].Close = p.Markets[0].Sessions[0].Open
	if err := p.Validate(now, 1); err == nil {
		t.Fatal("accepted invalid session")
	}
}
