package domain

import (
	"fmt"
	"regexp"
	"slices"
	"time"
)

// Remote is a list of servers fetched from a URL on a schedule.
type Remote struct {
	Location     SecretRef // Token-bearing URL; never appears in public DTOs.
	Host         string    // The URL's host alone, which carries no token: a source's name when nothing better is known.
	UserAgent    string    // Providers pick a format by User-Agent. Empty is the default.
	HWID         SecretRef // Empty sends no device headers. Never derived from a serial.
	RefreshEvery time.Duration
	Paused       bool
}

// Manual is a list of servers the owner pasted. The text is write-only like a URL, since
// it carries credentials.
type Manual struct{ Text SecretRef }

// Origin is exactly one of Remote or Manual, by constructor.
type Origin struct {
	remote *Remote
	manual *Manual
}

func RemoteOrigin(r Remote) Origin { return Origin{remote: &r} }
func ManualOrigin(m Manual) Origin { return Origin{manual: &m} }

func (o Origin) Remote() (Remote, bool) {
	if o.remote == nil {
		return Remote{}, false
	}
	return *o.remote, true
}

func (o Origin) Manual() (Manual, bool) {
	if o.manual == nil {
		return Manual{}, false
	}
	return *o.manual, true
}

// Source is owner intent for one list of servers. Every present server it lists is offered.
// Fetch progress (validators, failures, Retry-After, provider metadata) is not here: it is
// catalogue.FetchState.
type Source struct {
	ID       SourceID
	Revision SourceRevision // Moves only on an owner write. The page's If-Match.
	Name     string
	Origin   Origin
	Feed     *Feed     // What the latest document listed. Nil until a document is read.
	Added    time.Time // When it was created. Zero for a source stored before state.db kept it.
}

// Feed is a document's configs in its order, with what each became: how many of its outbounds
// became servers, and the ones that didn't with why.
type Feed struct {
	Configs []FeedConfig
}

type FeedConfig struct {
	Key     LineageID // As Occurrence.Config. Empty for a config with no server.
	Place   int       // As Occurrence.ConfigPlace.
	Name    string    // Its remarks, as sent.
	Servers int       // Its proxy roots that became a server.
	Skipped []Skip    // In outbound order.
}

type SkipReason uint8

const (
	SkipHelper  SkipReason = iota + 1 // A utility such as freedom or blackhole, never a server.
	SkipHop                           // Another outbound dials through it, so it is part of that server.
	SkipRefused                       // A proxy root the core can't run.
)

// Skip is one outbound of a config that became no server of its own. Protocol is a helper's,
// one of the parser's fixed names. Position, Code and Field are a refused root's: its place among
// the roots and the parser's diagnostic.
type Skip struct {
	Index       int // Its place in the config's outbound list from 1.
	Tag         string
	Reason      SkipReason
	Protocol    string
	Position    int
	Code, Field string
}

// Occurrence is one place a server was seen, stored on the lineage.
type Occurrence struct {
	Config      LineageID // The config's key, which stays with it across refreshes.
	ConfigName  string    // Its remarks, which can name the server.
	ConfigPlace int       // The config's place in the document from 1, in provider order.
	Tag         string    // The outbound's tag in that config.
	Position    int       // Its place among the config's proxy roots from 1, in provider order, valid or not.
}

// Filter holds two case-insensitive RE2 patterns, compiled at admission. A plain word is
// a valid pattern, and the zero Filter passes everything.
type Filter struct {
	include, exclude string
	in, ex           *regexp.Regexp
}

func NewFilter(include, exclude string) (Filter, error) {
	f := Filter{include: include, exclude: exclude}
	var err error
	if include != "" {
		if f.in, err = regexp.Compile("(?i)" + include); err != nil {
			return Filter{}, fmt.Errorf("include pattern: %w", err)
		}
	}
	if exclude != "" {
		if f.ex, err = regexp.Compile("(?i)" + exclude); err != nil {
			return Filter{}, fmt.Errorf("exclude pattern: %w", err)
		}
	}
	return f, nil
}

// Patterns returns what the filter was admitted from.
func (f Filter) Patterns() (include, exclude string) { return f.include, f.exclude }

// Pass is true when include is empty or matches a name, and exclude matches none.
func (f Filter) Pass(names ...string) bool {
	included := f.in == nil
	for _, name := range names {
		if f.ex != nil && f.ex.MatchString(name) {
			return false
		}
		included = included || f.in.MatchString(name)
	}
	return included
}

// Among is a condition on one field: is any of Values, or with Not none of them. Is any of nothing
// isn't set. Is none of nothing is set and holds for anything, so a group whose only condition loses
// the last value it excluded still matches everything else.
type Among[T comparable] struct {
	Not    bool
	Values []T
}

func (a Among[T]) Set() bool { return len(a.Values) > 0 || a.Not }

func (a Among[T]) Holds(v T) bool { return !a.Set() || slices.Contains(a.Values, v) != a.Not }

func (a Among[T]) clone() Among[T] {
	return Among[T]{Not: a.Not, Values: append([]T{}, a.Values...)}
}
