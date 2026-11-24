package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sidegate/internal/domain"
)

func TestAPickStepsAsItsTableSays(t *testing.T) {
	now := t0.Add(time.Hour)
	said := t0.Add(-time.Minute)
	a, b := ref("a"), ref("b")
	aAgain := domain.ServerRef{Lineage: a.Lineage, Digest: domain.Digest{9}}
	live := boost{At: t0, Size: 5}
	ended := boost{At: now.Add(-preferFor), Size: 5}
	ahead := boost{At: now.Add(time.Hour), Size: 5}
	queued := func(move domain.ServerRef, b boost) handPick {
		return handPick{at: queuedPick{move: move, boost: b}, said: said}
	}
	held := func(server domain.LineageID, b boost) handPick {
		return handPick{at: heldPick{server: server, boost: b}, said: said}
	}
	none := handPick{said: said}
	gapOf := func(size float64) func() float64 { return func() float64 { return size } }
	unread := func() float64 { t.Error("a press that keeps its boost read a gap"); return 0 }
	began := now.Add(-time.Second)
	for _, c := range []struct {
		name     string
		from     handPick
		ev       pickEvent
		want     handPick
		wantSaid time.Time
	}{
		{"a press on the server whose boost holds keeps its deadline", held("a", live), pressed{a, began, unread}, queued(a, live), began},
		{"a press while queued on the same server keeps the boost", queued(a, live), pressed{aAgain, began, unread}, queued(aAgain, live), began},
		{"a press on another server starts its gap", held("a", live), pressed{b, began, gapOf(3)}, queued(b, boost{At: now, Size: 3}), began},
		{"a press past the boost's day starts its gap", held("a", ended), pressed{a, began, gapOf(3)}, queued(a, boost{At: now, Size: 3}), began},
		{"a press with no gap queues no boost", none, pressed{a, began, gapOf(0)}, queued(a, boost{}), began},
		{"a Clear of the held boost ends it", held("a", live), cleared{"a", live.endsAt()}, none, now},
		{"a Clear of a queued boost keeps the move", queued(a, live), cleared{"a", live.endsAt()}, queued(a, boost{}), now},
		{"a Clear of another server is no word", held("a", live), cleared{"b", live.endsAt()}, held("a", live), said},
		{"a Clear of another deadline is no word", held("a", live), cleared{"a", live.endsAt().Add(time.Second)}, held("a", live), said},
		{"a Clear past the boost's day is no word", held("a", ended), cleared{"a", ended.endsAt()}, held("a", ended), said},
		{"a Clear of nothing is no word", none, cleared{"a", live.endsAt()}, none, said},
		{"an evaluate on the move holds its boost", queued(a, live), settledOn{domain.Serving(a)}, held("a", live), said},
		{"an evaluate on another revision of the move holds its boost", queued(a, live), settledOn{domain.Serving(aAgain)}, held("a", live), said},
		{"an evaluate ends a move with no boost", queued(a, boost{}), settledOn{domain.Serving(a)}, none, said},
		{"an evaluate ends a move whose boost's day is over", queued(a, ended), settledOn{domain.Serving(a)}, none, said},
		{"an evaluate off the move ends it", queued(a, live), settledOn{domain.Serving(b)}, none, said},
		{"an evaluate on a closed owner ends a move", queued(a, live), settledOn{domain.Closed(domain.ConfirmedDead)}, none, said},
		{"an evaluate on the held server keeps it", held("a", live), settledOn{domain.Serving(aAgain)}, held("a", live), said},
		{"an evaluate off the held server ends it", held("a", live), settledOn{domain.Serving(b)}, none, said},
		{"an evaluate on a closed owner ends a held boost", held("a", live), settledOn{domain.Closed(domain.ConfirmedDead)}, none, said},
		{"an evaluate past the day ends a held boost", held("a", ended), settledOn{domain.Serving(a)}, none, said},
		{"an evaluate leaves none as none", none, settledOn{domain.Serving(a)}, none, said},
		{"a new Exit forgets a held boost and the last word", held("a", live), choseAnew{}, handPick{}, time.Time{}},
		{"a new Exit forgets a move", queued(a, live), choseAnew{}, handPick{}, time.Time{}},
		{"a restart forgets the last word", held("a", live), restarted{}, held("a", live), time.Time{}},
		{"a restart begins a held boost from the future now", held("a", ahead), restarted{}, held("a", boost{At: now, Size: 5}), time.Time{}},
		{"a restart begins a queued boost from the future now", queued(a, ahead), restarted{}, queued(a, boost{At: now, Size: 5}), time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.from.step(now, c.ev)
			if !got.sameStored(c.want) || !got.said.Equal(c.wantSaid) {
				t.Errorf("%+v steps to %+v, want %+v said at %s", c.from, got, c.want, c.wantSaid)
			}
		})
	}
}

func TestOnlyPickGoAndCodecGoBuildAPick(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "pick.go" || f == "codec.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, built := range []string{"queuedPick{", "heldPick{"} {
			if strings.Contains(string(src), built) {
				t.Errorf("%s builds a %s}: only step and the codec make a pick", f, built)
			}
		}
	}
}
