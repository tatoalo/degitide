// Package market describes cached regular exchange sessions, supplied by the
// worker's exchange_calendars tables. It makes no quote-freshness guarantees.
package market

import (
	"errors"
	"regexp"
	"time"
)

const CloseDelay = 20 * time.Minute

type Session struct {
	Open       time.Time  `json:"open"`
	Close      time.Time  `json:"close"`
	BreakStart *time.Time `json:"break_start,omitempty"`
	BreakEnd   *time.Time `json:"break_end,omitempty"`
}

type Exchange struct {
	MIC      string    `json:"mic"`
	Name     string    `json:"name"`
	Timezone string    `json:"timezone"`
	Sessions []Session `json:"sessions"`
}

type Schedule struct {
	GeneratedAt     time.Time  `json:"generated_at"`
	ValidUntil      time.Time  `json:"valid_until"`
	HoldingCount    int        `json:"holding_count"`
	UnknownHoldings int        `json:"unknown_holdings"`
	Markets         []Exchange `json:"markets"`
}

var micPattern = regexp.MustCompile(`^[A-Z0-9]{4}$`)

func (p *Schedule) Validate(now time.Time, holdings int) error {
	bad := errors.New("invalid market calendar coverage")
	if p == nil || p.HoldingCount != holdings || p.UnknownHoldings < 0 || p.UnknownHoldings > holdings ||
		p.GeneratedAt.Before(now.Add(-24*time.Hour)) || p.GeneratedAt.After(now.Add(5*time.Minute)) ||
		!p.ValidUntil.After(now) || p.ValidUntil.After(p.GeneratedAt.Add(31*24*time.Hour)) || len(p.Markets) > holdings {
		return bad
	}
	if holdings > p.UnknownHoldings && len(p.Markets) == 0 {
		return bad
	}
	seen := map[string]bool{}
	for _, m := range p.Markets {
		if !micPattern.MatchString(m.MIC) || seen[m.MIC] || len(m.Name) > 320 || len(m.Sessions) == 0 || len(m.Sessions) > 50 {
			return bad
		}
		seen[m.MIC] = true
		if _, err := time.LoadLocation(m.Timezone); err != nil {
			return bad
		}
		var previous time.Time
		for _, session := range m.Sessions {
			if !session.Open.Before(session.Close) || session.Close.Sub(session.Open) > 24*time.Hour ||
				session.Open.Before(p.GeneratedAt.Add(-16*24*time.Hour)) || session.Close.After(p.GeneratedAt.Add(32*24*time.Hour)) ||
				(!previous.IsZero() && !session.Open.After(previous)) {
				return bad
			}
			if (session.BreakStart == nil) != (session.BreakEnd == nil) {
				return bad
			}
			if session.BreakStart != nil && (!session.BreakStart.After(session.Open) || !session.BreakEnd.After(*session.BreakStart) || !session.BreakEnd.Before(session.Close)) {
				return bad
			}
			previous = session.Close
		}
	}
	return nil
}

type Decision struct {
	At     time.Time
	Reason string
}

// Next returns the earliest useful market refresh after the saved snapshot.
// Past closing targets collapse into one catch-up, even after a long outage.
func (p *Schedule) Next(last, now time.Time, interval time.Duration) Decision {
	return p.NextWithCloseDelay(last, now, interval, CloseDelay)
}

func (p *Schedule) NextWithCloseDelay(last, now time.Time, interval, closeDelay time.Duration) Decision {
	var next Decision
	if p == nil || !now.Before(p.ValidUntil) || interval <= 0 {
		return next
	}
	consider := func(at time.Time, reason string) {
		if at.Before(now) {
			at = now
		}
		if next.At.IsZero() || at.Before(next.At) {
			next = Decision{at, reason}
		}
	}
	for _, m := range p.Markets {
		for _, session := range m.Sessions {
			closeTarget := session.Close.Add(closeDelay)
			if closeTarget.After(last) {
				consider(closeTarget, "After market close")
			}
			intervals := [][2]time.Time{{session.Open, session.Close}}
			if session.BreakStart != nil {
				intervals = [][2]time.Time{{session.Open, *session.BreakStart}, {*session.BreakEnd, session.Close}}
			}
			for _, span := range intervals {
				if !now.Before(span[1]) {
					continue
				}
				at := last.Add(interval)
				if at.Before(span[0]) {
					at = span[0]
				}
				if at.Before(now) {
					at = now
				}
				if at.Before(span[1]) {
					consider(at, "Market-hours refresh")
				}
			}
		}
	}
	return next
}

func (m Exchange) IsOpen(now time.Time) bool {
	for _, session := range m.Sessions {
		if !now.Before(session.Open) && now.Before(session.Close) &&
			(session.BreakStart == nil || now.Before(*session.BreakStart) || !now.Before(*session.BreakEnd)) {
			return true
		}
	}
	return false
}
