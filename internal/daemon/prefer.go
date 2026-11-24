package daemon

import (
	"fmt"
	"net/http"
	"time"

	"sidegate/internal/domain"
)

// PreferServer is a press on one server of a group or source whose Test passed: the owner's slots move
// to it, and it keeps a boost that fades over a day. Signal is that Test's pass.
type PreferServer struct {
	Request domain.RequestID
	Owner   domain.Owner
	Signal  domain.Signal
}

func (cmd PreferServer) request() domain.RequestID { return cmd.Request }
func (cmd PreferServer) digest() string {
	return fmt.Sprintf("preferred %s %s %d", cmd.Owner, cmd.Signal.Server.Lineage, cmd.Signal.Stamp.At.UnixNano())
}

func (cmd PreferServer) answer(*controller, *stage) (int, any) {
	return http.StatusOK, probeResultOf(cmd.Signal)
}

// ClearPreference drops the preference the page showed: Server's, ending at EndsAt. A preference since
// replaced, ended or renewed stays as it is, and the answer's revision brings the page what stands. It
// never moves a slot. One that cleared the preference is the owner's last word, so a press whose Test
// began before it moves nothing; one that cleared nothing changed no choice and is no word.
type ClearPreference struct {
	Request domain.RequestID
	Owner   domain.Owner
	Server  domain.LineageID
	EndsAt  time.Time
}

func (cmd ClearPreference) request() domain.RequestID { return cmd.Request }
func (cmd ClearPreference) digest() string {
	return fmt.Sprintf("clear %s %s %d", cmd.Owner, cmd.Server, cmd.EndsAt.UnixNano())
}
func (cmd ClearPreference) answer(*controller, *stage) (int, any) { return answered() }

func (cmd ClearPreference) perform(c *controller, s *stage) error {
	if _, err := c.exitOf(cmd.Owner); err != nil {
		return err
	}
	m, used := c.state.owners[cmd.Owner]
	if !used {
		return nil
	}
	m.Hand = m.Hand.step(s.now, cleared{server: cmd.Server, endsAt: cmd.EndsAt})
	c.stageOwner(s, cmd.Owner, m)
	return nil
}

// exitOf is the Exit a group or source owner is, or why there is none.
func (c *controller) exitOf(o domain.Owner) (domain.Exit, error) {
	if id, ok := o.Group(); ok {
		if _, known := c.world.groups[id]; !known {
			return domain.Exit{}, refuse(KindNotFound, RefusedGroupGone, "That group is gone.")
		}
		return domain.GroupExit(id), nil
	}
	id, _ := o.Source()
	if _, known := c.state.sources[id]; !known {
		return domain.Exit{}, refuse(KindNotFound, RefusedSourceGone, "That source is gone.")
	}
	return domain.SourceExit(id), nil
}

// perform stages the pick for the next evaluate, which moves every slot of the owner. A press whose Test
// began before the owner's last word landed is stale: the later press or Clear stands. The Test's pass
// is in the picked record the receipt commits, and the book hears it once the commit lands, so it counts
// once whether this or the signal loop's frame of it comes first. A press on the server already
// preferred keeps its deadline, and one on another replaces the preference with its own gap.
func (cmd PreferServer) perform(c *controller, s *stage) error {
	exit, err := c.exitOf(cmd.Owner)
	if err != nil {
		return err
	}
	m, used := c.state.owners[cmd.Owner]
	if !used {
		return refuse(KindValidation, RefusedOwnerIdle, "No slot uses it.")
	}
	sig := cmd.Signal
	if m.Hand.stale(sig.Stamp.At) {
		return stale(uint64(c.state.revision))
	}
	id := sig.Server.Lineage
	pool := c.world.Pool(exit)
	l, member := pool.member(id)
	if !member {
		if known, ok := c.world.catalogue.Lookup(id); !ok || known.Presence != domain.Present {
			return refuse(KindValidation, RefusedServerGone, "That server is gone.")
		}
		return refuse(KindValidation, RefusedNotMember, "That server isn't one it uses.")
	}
	if l.Ref != sig.Server || sig.Stamp.Epoch == "" || !c.evidence.admits(sig) {
		return refuse(KindValidation, RefusedServerChanged, "That server changed during its test. Try again.")
	}
	if r := c.evidence.book.records[id]; r.Failures > 0 && r.Last.After(sig.Stamp.At) {
		return refuse(KindValidation, RefusedFailedSince, "That server failed after its test began.")
	}
	wanDownAt := c.evidence.wan.LastDown
	picked := c.evidence.book.heardBy(sig, wanDownAt)
	gap := func() float64 {
		e := evaluation{now: s.now, who: cmd.Owner, pool: pool, memory: m, ev: c.evidence.withRecord(id, picked), t: c.timings}
		return e.gap(id)
	}
	m.Hand, m.Backoff = m.Hand.step(s.now, pressed{move: l.Ref, began: sig.Stamp.At, gap: gap}), m.Backoff.waived()
	c.stageOwner(s, cmd.Owner, m)
	records := map[domain.LineageID]record{id: picked}
	s.tx.Records = records
	s.then(func() {
		c.evidence.book.hear([]domain.Signal{sig}, wanDownAt)
		c.evidence.book.written(records)
	})
	return nil
}
