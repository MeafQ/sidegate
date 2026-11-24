package daemon

import (
	"encoding/json"
	"net/http"

	"sidegate/internal/domain"
)

// published is what the controller last made readable to handlers outside it. Its values
// are never changed once stored, so a preview can't wait on a commit.
type published struct {
	world   world
	sources map[domain.SourceID]domain.Source
	slots   map[domain.SlotID]slotRow
	udp     map[domain.LineageID]udpState
	records map[domain.LineageID]record
	site    site
}

// testRef is the revision of a lineage a Test probes: a present one the core hasn't refused, or one a
// slot still serves.
func (p *published) testRef(id domain.LineageID) (domain.ServerRef, bool) {
	l, ok := p.world.catalogue.Lookup(id)
	if !ok || p.world.refused[l.Ref] {
		return domain.ServerRef{}, false
	}
	if l.Presence == domain.Present {
		return l.Ref, true
	}
	for _, row := range p.slots {
		if server, serving := row.Selection.Server(); serving && server == l.Ref {
			return l.Ref, true
		}
	}
	return domain.ServerRef{}, false
}

// unkeyed handles a route whose answer is never replayed, so it takes the CSRF check and no key: a preview
// stores nothing, and an update's state is read and not answered.
func (w *web) unkeyed(status int, answer func(*http.Request, []byte) (any, *Answer)) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !w.csrf(rw, r) {
			return
		}
		body, ok := readBody(rw, r, maxBody)
		if !ok {
			return
		}
		out, refused := answer(r, body)
		if refused != nil {
			writeAnswer(rw, refused)
			return
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			writeAnswer(rw, unavailable())
			return
		}
		writeAnswer(rw, &Answer{Status: status, Body: encoded})
	})
}

// GroupPreview is what a rule's conditions but its box match now. The page applies the box, from the
// ticks it shows.
type GroupPreview struct {
	Matched []domain.LineageID `json:"matched"`
}

func (w *web) previewGroup(_ *http.Request, body []byte) (any, *Answer) {
	var dto struct {
		Members MembersDTO `json:"members"`
	}
	if refused := decode(body, &dto); refused != nil {
		return nil, refused
	}
	members, err := dto.Members.admit()
	if err != nil {
		return nil, admission(err)
	}
	world := w.app.published.Load().world
	out := GroupPreview{Matched: []domain.LineageID{}}
	for _, l := range world.catalogue.Matched(members) {
		out.Matched = append(out.Matched, l.ID)
	}
	return out, nil
}
