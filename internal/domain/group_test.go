package domain

import (
	"slices"
	"testing"
)

func catalogueFor(t *testing.T) Catalogue {
	t.Helper()
	seen := []Occurrence{{Config: "k", Tag: "t", Position: 1}}
	tagged := []Occurrence{{Config: "k", Tag: "Berlin", Position: 1}}
	return NewCatalogue(3, []Lineage{
		{ID: "b", Source: "s1", Label: "DE b", Country: Country{Code: "DE"}, Presence: Present, Seen: tagged},
		{ID: "a", Source: "s1", Label: "DE a", Country: Country{Code: "DE"}, Presence: Present, Seen: seen},
		{ID: "c", Source: "s2", Label: "NL c", Country: Country{Code: "NL"}, Presence: Present, Seen: seen},
		{ID: "d", Source: "s2", Label: "DE d", Country: Country{Code: "DE"}, Presence: Missing, Seen: seen},
		{ID: "e", Source: "s3", Label: "SE e", Country: Country{Code: "SE"}, Presence: Present, Seen: seen},
	})
}

func ids(lineages []Lineage) []LineageID {
	out := []LineageID{}
	for _, l := range lineages {
		out = append(out, l.ID)
	}
	return out
}

func is[T comparable](values ...T) Among[T]  { return Among[T]{Values: values} }
func not[T comparable](values ...T) Among[T] { return Among[T]{Not: true, Values: values} }

func TestMembers(t *testing.T) {
	c := catalogueFor(t)
	admit := func(m Members) Members {
		t.Helper()
		out, err := AdmitMembers(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	named := func(include string) Filter {
		t.Helper()
		f, err := NewFilter(include, "")
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	ticked := []LineageID{"a", "c", "d", "gone"}
	for name, tc := range map[string]struct {
		members Members
		want    []LineageID
	}{
		"a country, lower case":                       {admit(Members{Countries: is("de")}), []LineageID{"a", "b"}},
		"none of a country":                           {admit(Members{Countries: not("de")}), []LineageID{"c", "e"}},
		"every present server of a source is offered": {admit(Members{Sources: is[SourceID]("s3")}), []LineageID{"e"}},
		"a source":                                 {admit(Members{Sources: is[SourceID]("s2")}), []LineageID{"c"}},
		"a name, whatever its case":                {admit(Members{Names: named("^de A$")}), []LineageID{"a"}},
		"a name read from a tag":                   {admit(Members{Names: named("berlin")}), []LineageID{"b"}},
		"nothing set matches nothing, ticks aside": {admit(Members{}), []LineageID{}},
		"both conditions narrow":                   {admit(Members{Sources: is[SourceID]("s1"), Countries: is("NL")}), []LineageID{}},
		"an unset box leaves ticks aside":          {admit(Members{Countries: is("DE")}), []LineageID{"a", "b"}},
		"ticked alone, present ones":               {admit(Members{Box: BoxTicked}), []LineageID{"a", "c"}},
		"not ticked alone":                         {admit(Members{Box: BoxUnticked}), []LineageID{"b", "e"}},
		"ticked and a country":                     {admit(Members{Countries: is("DE"), Box: BoxTicked}), []LineageID{"a"}},
		"not ticked and a country":                 {admit(Members{Countries: is("DE"), Box: BoxUnticked}), []LineageID{"b"}},
	} {
		if got := ids(c.Members(tc.members, ticked)); !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", name, got, tc.want)
		}
	}
	if got := ids(c.Matched(admit(Members{Countries: is("NL"), Box: BoxTicked}))); !slices.Equal(got, []LineageID{"c"}) {
		t.Errorf("matched = %v, want the conditions but the box", got)
	}
	if got := ids(c.Matched(admit(Members{Box: BoxUnticked}))); !slices.Equal(got, []LineageID{"a", "b", "c", "e"}) {
		t.Errorf("matched = %v, want every present server under the box alone", got)
	}
	if _, err := AdmitMembers(Members{Countries: is("DEU")}); err == nil {
		t.Error("a three-letter country was admitted")
	}
	if _, err := AdmitMembers(Members{Box: BoxUnticked + 1}); err == nil {
		t.Error("a fourth box condition was admitted")
	}
	if _, err := AdmitTicked([]LineageID{"a", "a"}); err == nil {
		t.Error("a repeated tick was admitted")
	}
	if got, _ := AdmitTicked([]LineageID{"c", "a"}); !slices.Equal(got, []LineageID{"a", "c"}) {
		t.Errorf("ticks = %v, want them sorted", got)
	}
}

func TestAConditionHasNoLimit(t *testing.T) {
	lineages := make([]Lineage, 0, 2000)
	for i := range 2000 {
		lineages = append(lineages, Lineage{ID: NewLineageID("s", 1, i), Source: "s", Presence: Present, Seen: []Occurrence{{Config: "k", Tag: "t", Position: 1}}})
	}
	c := NewCatalogue(1, lineages)
	if got := len(c.Members(Members{Sources: is[SourceID]("s")}, nil)); got != 2000 {
		t.Fatalf("members = %d", got)
	}
}

// withoutS1 is catalogueFor once source s1 is deleted: its servers are tombstones.
func withoutS1(t *testing.T) Catalogue {
	var lineages []Lineage
	for _, l := range catalogueFor(t).All() {
		if l.Source == "s1" {
			l.Presence = Tombstone
		}
		lineages = append(lineages, l)
	}
	return NewCatalogue(4, lineages)
}

func TestWithoutSource(t *testing.T) {
	c := catalogueFor(t)
	group := func(m Members) Group { return Group{ID: "g", Members: m} }
	t.Run("a condition's last source removes the group rather than widen to any", func(t *testing.T) {
		if _, changed, removed := group(Members{Sources: is[SourceID]("s1")}).WithoutSource("s1", c); !changed || !removed {
			t.Fatalf("changed %v removed %v", changed, removed)
		}
	})
	t.Run("a condition drops one of several", func(t *testing.T) {
		next, changed, removed := group(Members{Sources: is[SourceID]("s1", "s2")}).WithoutSource("s1", c)
		if !changed || removed || !slices.Equal(next.Members.Sources.Values, []SourceID{"s2"}) {
			t.Fatalf("changed %v removed %v sources %v", changed, removed, next.Members.Sources)
		}
	})
	t.Run("none of its last source stays, since it narrows nothing more", func(t *testing.T) {
		next, changed, removed := group(Members{Sources: not[SourceID]("s1"), Countries: is("NL")}).WithoutSource("s1", c)
		if got := ids(withoutS1(t).Members(next.Members, nil)); !changed || removed || !slices.Equal(got, []LineageID{"c"}) {
			t.Fatalf("changed %v removed %v members %v", changed, removed, got)
		}
	})
	t.Run("none of its last source, alone, still matches every other source", func(t *testing.T) {
		next, changed, removed := group(Members{Sources: not[SourceID]("s1")}).WithoutSource("s1", c)
		if got := ids(withoutS1(t).Members(next.Members, nil)); !changed || removed || !slices.Equal(got, []LineageID{"c", "e"}) {
			t.Fatalf("changed %v removed %v members %v", changed, removed, got)
		}
	})
	t.Run("a group naming others is untouched", func(t *testing.T) {
		if _, changed, removed := group(Members{Sources: is[SourceID]("s2")}).WithoutSource("s1", c); changed || removed {
			t.Fatalf("changed %v removed %v", changed, removed)
		}
	})
	ticks := func(m Members, ticked ...LineageID) Group { return Group{ID: "g", Members: m, Ticked: ticked} }
	t.Run("a group that is ticked alone and loses every tick is removed", func(t *testing.T) {
		if _, changed, removed := ticks(Members{Box: BoxTicked}, "a", "b").WithoutSource("s1", c); !changed || !removed {
			t.Fatalf("changed %v removed %v", changed, removed)
		}
	})
	t.Run("ticks drop that source's servers", func(t *testing.T) {
		next, changed, removed := ticks(Members{Box: BoxTicked}, "a", "c").WithoutSource("s1", c)
		if !changed || removed || !slices.Equal(next.Ticked, []LineageID{"c"}) {
			t.Fatalf("changed %v removed %v ticks %v", changed, removed, next.Ticked)
		}
	})
	t.Run("a rule beside the ticks keeps the group when they all go", func(t *testing.T) {
		next, changed, removed := ticks(Members{Countries: is("NL"), Box: BoxUnticked}, "a").WithoutSource("s1", c)
		if !changed || removed || len(next.Ticked) != 0 {
			t.Fatalf("changed %v removed %v ticks %v", changed, removed, next.Ticked)
		}
	})
}
