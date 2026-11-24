package daemon

import (
	"encoding/json"
	"fmt"
	"time"
)

// Margins and counts stay constants, because a lab that changes a margin proves
// nothing about production. Only durations can move, through timingOverrides.
const (
	watchClosed = 4 // Top members of a closed or leaving slot that get close heartbeats.

	failureRounds, failureRivals = 2, 3
	campaignRounds               = 5 // Rivals taking turns to pass never give one two wins.
	udpBrokenAfter               = 3 // Failed UDP checks in a row that mark a server broken.

	gainFraction, gainAbsolute       = 0.20, 20 * time.Millisecond // A rival's Ping times its factor beats the current server's by both in a comparison.
	severePing                       = 200 * time.Millisecond      // On the failure path, Ping loses by this or by the rival's whole Ping.
	severeSpread                     = 150 * time.Millisecond      // Or its reused requests differ by this much more than the rival's. A lost packet costs one of them about 200 ms, and a server that is only far away shows no spread.
	severeConnRatio, severeConnFloor = 3, time.Second              // Or new connection loses by both.
	beatRatio, beatFloor             = 2, 200 * time.Millisecond   // A heartbeat this far above the server's own median starts slow rounds. They decide, so this only has to catch a retransmit.
	beatMinSamples                   = 3                           // Earlier heartbeats a median needs, since one or two are noise.
)

// timings holds selection's durations. defaultTimings is production.
type timings struct {
	FailureSpacing    time.Duration // Between a failure round's end and the next's start.
	FailureExpiry     time.Duration // A failure round with no result by then ends its campaign.
	CompareEvery      time.Duration // Between a group's comparison rounds.
	Handover          time.Duration // How long the core may stay on a server a slot is leaving.
	WANTTL            time.Duration // How long a baseline vouches for the WAN.
	QuietAfter        time.Duration // After a campaign a slow heartbeat started that moved nothing: no further slow trigger.
	PassiveQuiet      time.Duration // After a failure campaign that moved nothing: no campaign from a passive suspicion.
	SettleFor         time.Duration // A closed slot waits this long for the other routed servers' first results.
	RecoverStable     time.Duration // The first wait for a slot's last server after it closed the slot.
	RecoverBackoffMax time.Duration // The longest a slot's last server waits after closing it again.
	StableServing     time.Duration // Serving this long forgives earlier closures.
	UDPCheckEvery     time.Duration // No current server's UDP check comes sooner after its last.
}

func defaultTimings() timings {
	return timings{
		FailureSpacing: time.Second, FailureExpiry: 30 * time.Second, CompareEvery: compareEvery, Handover: 30 * time.Second,
		WANTTL: 3 * baselineEvery, QuietAfter: 30 * time.Second, PassiveQuiet: 2 * time.Minute, SettleFor: 8 * time.Second,
		RecoverStable: 30 * time.Second, RecoverBackoffMax: 5 * time.Minute, StableServing: 10 * time.Minute, UDPCheckEvery: 5 * time.Minute,
	}
}

// rule is how a path confirms.
func (t timings) rule(p path) pathRule {
	if p == improvePath {
		return pathRule{Rounds: compareWins, Rivals: compareRivals, Expiry: compareExpiry}
	}
	return pathRule{Rounds: failureRounds, Rivals: failureRivals, Spacing: t.FailureSpacing, Expiry: t.FailureExpiry}
}

func (t *timings) byName() map[string]*time.Duration {
	return map[string]*time.Duration{
		"failureSpacing": &t.FailureSpacing, "failureExpiry": &t.FailureExpiry, "compareEvery": &t.CompareEvery, "handover": &t.Handover,
		"wanTTL": &t.WANTTL, "quietAfter": &t.QuietAfter, "passiveQuiet": &t.PassiveQuiet, "settleFor": &t.SettleFor,
		"recoverStable": &t.RecoverStable, "recoverBackoffMax": &t.RecoverBackoffMax, "stableServing": &t.StableServing, "udpCheckEvery": &t.UDPCheckEvery,
	}
}

// timingOverrides is bootstrap.json's timings object, such as {"handover": "60s"}. The
// lab shortens durations this way, and the page has no route that reaches them.
type timingOverrides map[string]time.Duration

func (o *timingOverrides) UnmarshalJSON(data []byte) error {
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	known := (&timings{}).byName()
	*o = make(timingOverrides, len(raw))
	for name, text := range raw {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("timings: unknown field %q", name)
		}
		d, err := time.ParseDuration(text)
		if err != nil {
			return fmt.Errorf("timings: %s: %w", name, err)
		}
		if d <= 0 {
			return fmt.Errorf("timings: %s is %s, and must be positive", name, d)
		}
		(*o)[name] = d
	}
	return nil
}

func (o timingOverrides) apply() timings {
	t := defaultTimings()
	fields := t.byName()
	for name, d := range o {
		*fields[name] = d
	}
	return t
}
