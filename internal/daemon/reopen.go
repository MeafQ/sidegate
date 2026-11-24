package daemon

import "time"

// reopenBackoff is how long a closed owner keeps its last server out, and since when a closure proved
// its group failing. A closure soon after a reopening doubles the wait, so a server that flaps slowly
// costs fewer and fewer switches. ProvenFailing keeps any older pass from reopening the owner; the
// record keeps passes across a restart, so it persists too, until the owner serves again.
type reopenBackoff struct {
	Closures      int
	ServingSince  time.Time
	NotBefore     time.Time
	ProvenFailing time.Time
}

// closed is the backoff once a ConfirmedDead closure lands. Serving StableServing forgives the
// closures before it.
func (b reopenBackoff) closed(now time.Time, t timings) reopenBackoff {
	if now.Sub(b.ServingSince) >= t.StableServing {
		b.Closures = 0
	}
	b.Closures++
	b.NotBefore = now.Add(min(t.RecoverStable<<min(b.Closures-1, 8), t.RecoverBackoffMax))
	return b
}

// serving is the backoff once a server lands on an owner that served none.
func (b reopenBackoff) serving(now time.Time) reopenBackoff {
	b.ServingSince, b.ProvenFailing = now, time.Time{}
	return b
}

// proven is the backoff of a ConfirmedDead decision, whose campaign found the group failing from at.
func (b reopenBackoff) proven(at time.Time) reopenBackoff {
	b.ProvenFailing = at
	return b
}

// waived is the backoff once nothing should wait for it: a press, or an owner that had no Exit.
func (b reopenBackoff) waived() reopenBackoff {
	b.NotBefore = time.Time{}
	return b
}

// clamped is the backoff a restart loads, which never waits longer than a closure could have asked.
func (b reopenBackoff) clamped(now time.Time, t timings) reopenBackoff {
	if reopen := now.Add(t.RecoverBackoffMax); b.NotBefore.After(reopen) {
		b.NotBefore = reopen
	}
	return b
}

// holdsOut says the owner's last server still waits.
func (b reopenBackoff) holdsOut(now time.Time) bool { return now.Before(b.NotBefore) }

func (b reopenBackoff) same(o reopenBackoff) bool {
	return b.Closures == o.Closures && b.ServingSince.Equal(o.ServingSince) && b.NotBefore.Equal(o.NotBefore) && b.ProvenFailing.Equal(o.ProvenFailing)
}
