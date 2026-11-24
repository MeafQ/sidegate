package daemon

import (
	"time"

	"sidegate/internal/domain"
)

// wanBelief is what the signal loop believes of the WAN. The loop alone moves it, and each frame hands
// the controller a copy, so the two read one belief. It is unknown only until the first baseline or
// proxied success, and never again after.
type wanBelief struct {
	State     domain.WANState
	At        time.Time // When it was last read.
	LastDown  time.Time // When it last read down.
	DownUntil time.Time // When it last stopped reading down.
	UpSince   time.Time // When it last began reading up.
	lastProxy time.Time // When a proxied success last proved it.
	missed    bool      // The last baseline failed with it reading up, which one more failure confirms.
}

// read sets the state read at at, as of now, and says whether it changed.
func (w *wanBelief) read(state domain.WANState, at, now time.Time) bool {
	if w.State == domain.WANDown && state != domain.WANDown {
		w.DownUntil = now
	}
	if w.State != domain.WANUp && state == domain.WANUp {
		w.UpSince = now
	}
	if state == domain.WANDown {
		w.LastDown = at
	}
	changed := state != w.State
	w.State, w.At = state, at
	return changed
}

// judged folds in a finished baseline. A proxied success this recent proves the WAN even if every
// baseline target is blocked, so a censored baseline can't freeze switching forever. One failure with
// the WAN reading up waits for the retry, so a blip starts nothing new to wait for it.
func (w *wanBelief) judged(ok bool, now time.Time) bool {
	proven := ok || now.Sub(w.lastProxy) < proofOfWAN
	state := domain.WANDown
	if proven || w.State == domain.WANUp && !w.missed {
		state = domain.WANUp
	}
	w.missed = !proven && state == domain.WANUp
	return w.read(state, now, now)
}

// proxied takes a probe that succeeded at at for proof the WAN is up.
func (w *wanBelief) proxied(at, now time.Time) {
	w.lastProxy = at
	w.read(domain.WANUp, at, now)
}

// up says the WAN is known up. Unknown is not up: nothing it would excuse can be judged yet.
func (w wanBelief) up() bool { return w.State == domain.WANUp }

// upWithin says it read up no longer than ttl before now.
func (w wanBelief) upWithin(now time.Time, ttl time.Duration) bool {
	return w.up() && now.Sub(w.At) <= ttl
}

// upThroughout says it read up at every moment since start.
func (w wanBelief) upThroughout(start time.Time) bool {
	return w.up() && !w.UpSince.After(start)
}

// downSince says it read down at some moment since start.
func (w wanBelief) downSince(start time.Time) bool {
	return w.State == domain.WANDown || w.DownUntil.After(start)
}
