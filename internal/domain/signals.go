package domain

import (
	"slices"
	"time"
)

type TargetID string
type CampaignID string

// Stamp orders facts within one core epoch. A fact from another epoch, or from a
// runtime generation that no longer holds its server, is rejected, not merged.
type Stamp struct {
	Epoch      CoreEpoch
	Generation uint64
	Sequence   uint64
	At         time.Time
}

type SignalKind uint8

const (
	TCPInfo SignalKind = iota + 1
	OutboundCounters
	CoreWarning
	ActiveProbe
	WANBaseline
)

type Verdict uint8

const (
	Unknown Verdict = iota
	Succeeded
	Failed
)

type ProbePurpose uint8

const (
	Confirmation ProbePurpose = iota + 1
	ManualProbe
	Heartbeat // A serving server's liveness probe. Analytics counts it among the server's probes.
	UDPCheck  // One DNS question over UDP. It says nothing of TCP health or the WAN, and metrics.db never sees it.
	Check     // A full probe of one server on the ranking site that the probe plan scheduled, such as an introduction.
	Search    // A liveness probe of a server a closed or leaving slot could use.
)

// Signal is a normalized fact, not a persisted health flag. A missing Ping stays
// unknown. Target and purpose say what a result can be compared with.
// OutboundCounters carry cumulative Up/Down for the epoch; deltas are derived by
// whoever owns a watermark.
type Signal struct {
	Stamp                   Stamp
	Heard                   uint64 // Its place in the order the controller heard results. Stamp.At is when a probe began, and one can land long after.
	Server                  ServerRef
	Kind                    SignalKind
	Verdict                 Verdict
	Target                  TargetID
	Purpose                 ProbePurpose
	Campaign                CampaignID
	Round                   uint8
	NewConn                 *time.Duration // First request on a new connection, handshakes included.
	Ping                    *time.Duration // Median of the reused requests. Nil on a heartbeat and when reuse didn't happen.
	PingSpread              *time.Duration // Slowest reused request minus the fastest. Nil with Ping.
	NoReuse                 bool           // Full probe: the follow-up requests didn't reuse the connection.
	SRTT, RTTVar            *time.Duration
	Retransmits, BytesAcked *uint64
	DeliveryRate            *uint64
	Up, Down                *uint64
	Covered                 time.Duration
	AttributionCertain      bool
	WANExcluded             bool // Failed while the WAN baseline also failed: no one's fault.
}

// Summary keeps what a switch event needs from s.
func (s Signal) Summary() EvidenceSummary {
	return EvidenceSummary{
		Server: s.Server, Kind: s.Kind, Verdict: s.Verdict, Target: s.Target,
		Round: s.Round, At: s.Stamp.At, NewConn: s.NewConn, Ping: s.Ping,
	}
}

// EvidenceSummary is the part of a signal a switch event stores in state.db.
type EvidenceSummary struct {
	Server  ServerRef
	Kind    SignalKind
	Verdict Verdict
	Target  TargetID
	Round   uint8
	At      time.Time
	NewConn *time.Duration
	Ping    *time.Duration
}

type WANState uint8

const (
	WANUnknown WANState = iota
	WANUp
	WANDown
)

// Median of ds, the mean of the middle two when their number is even. Zero for none.
func Median[T ~int64 | ~uint64](ds []T) T {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(ds))
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return sorted[mid-1] + (sorted[mid]-sorted[mid-1])/2
}

// P95 is the nearest-rank 95th percentile of ds, zero for none.
func P95[T ~int64 | ~uint64](ds []T) T {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(ds))
	return sorted[(len(sorted)*95+99)/100-1]
}

// MedianDeviation is the median absolute deviation of ds from their median, which one
// wild sample can't move the way it moves a standard deviation.
func MedianDeviation(ds []time.Duration) time.Duration {
	center := Median(ds)
	deviations := make([]time.Duration, len(ds))
	for i, d := range ds {
		deviations[i] = (d - center).Abs()
	}
	return Median(deviations)
}
