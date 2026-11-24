package yaml

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	upstream "gopkg.in/yaml.v3"
)

// tree renders a yaml.v3 node tree of either package, so the fork's trees can be
// compared with upstream's field by field.
func tree(b *strings.Builder, n reflect.Value) {
	if n.IsNil() {
		b.WriteString("nil")
		return
	}
	n = n.Elem()
	for _, f := range []string{"Kind", "Style", "Tag", "Value", "Anchor", "HeadComment", "LineComment", "FootComment", "Line", "Column"} {
		fmt.Fprintf(b, "%s=%q ", f, fmt.Sprint(n.FieldByName(f).Interface()))
	}
	if alias := n.FieldByName("Alias"); !alias.IsNil() {
		fmt.Fprintf(b, "alias=%q ", alias.Elem().FieldByName("Anchor").String())
	}
	content := n.FieldByName("Content")
	b.WriteString("[")
	for i := range content.Len() {
		tree(b, content.Index(i))
		b.WriteString(" ")
	}
	b.WriteString("]")
}

type stream struct {
	trees       string
	nodes, docs int
	err         error
}

// load decodes every document of data, the way the catalogue reads a feed.
func load[N any](data []byte, decode func(*N) error) stream {
	var s stream
	var b strings.Builder
	for {
		var doc N
		err := decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			s.err = err
			break
		}
		tree(&b, reflect.ValueOf(&doc))
		s.docs++
		s.nodes += strings.Count(b.String()[len(s.trees):], "Kind=")
		s.trees = b.String()
	}
	return s
}

func loadUpstream(data []byte) stream {
	dec := upstream.NewDecoder(bytes.NewReader(data))
	return load(data, func(n *upstream.Node) error { return dec.Decode(n) })
}

func loadFork(data []byte, budget int) stream {
	dec := NewDecoder(bytes.NewReader(data))
	if budget >= 0 {
		dec.Budget(budget)
	}
	return load(data, func(n *Node) error { return dec.Decode(n) })
}

// The reviewer's differential: random streams from YAML's indicators. Upstream
// and the fork without a budget must agree on every stream, and a budget one
// short of the nodes upstream builds must stop the fork.
func TestBudgetCountsWhatTheParserBuilds(t *testing.T) {
	toks := []string{"a", "b", ":", ": ", "- ", "-", "? ", "[", "]", "{", "}", ",", ", ", " ", "  ", "\n", "\n  ", "\n    ",
		"#", " #x", "'", "\"", "\\", "|", ">", "|-\n", "---", "...", "\t", "%", "\r", "\r\n", "\u0085", " ", " ",
		"!t ", "!!str ", "&x ", "*x", ":!t", "%TAG !t! x\n", "%YAML 1.1\n"}
	iterations := 300_000
	if testing.Short() {
		iterations = 20_000
	}
	r := rand.New(rand.NewSource(1))
	accepted := 0
	for range iterations {
		var b bytes.Buffer
		for k := r.Intn(14) + 1; k > 0; k-- {
			b.WriteString(toks[r.Intn(len(toks))])
		}
		data := b.Bytes()
		want := loadUpstream(data)
		got := loadFork(data, -1)
		if (want.err == nil) != (got.err == nil) || want.trees != got.trees {
			t.Fatalf("%q: fork = %v %s, upstream = %v %s", data, got.err, got.trees, want.err, want.trees)
		}
		if want.err != nil {
			continue
		}
		accepted++
		if short := loadFork(data, want.nodes-1); want.nodes > 0 && !errors.Is(short.err, ErrBudget) {
			t.Fatalf("%q: budget %d for %d nodes: err = %v, want ErrBudget", data, want.nodes-1, want.nodes, short.err)
		}
		enough := want.nodes + (documentCost-nodeCost)*want.docs +
			commentCost*bytes.Count(data, []byte("#")) + directiveCost*bytes.Count(data, []byte("%"))
		if full := loadFork(data, enough); full.err != nil || full.trees != want.trees {
			t.Fatalf("%q: budget %d for %d nodes: err = %v", data, enough, want.nodes, full.err)
		}
	}
	if accepted < iterations/10 {
		t.Fatalf("only %d of %d streams parsed; the corpus tests little", accepted, iterations)
	}
}

func TestBudgetCharges(t *testing.T) {
	for _, c := range []struct {
		name  string
		data  string
		nodes int
		cost  int
	}{
		{"mapping", "a: b\n", 4, 3 + documentCost},
		{"line comment", "a: b # c\n", 4, 3 + documentCost + commentCost},
		{"head comment", "# c\na: b\n", 4, 3 + documentCost + commentCost},
		{"documents", "a\n--- b\n", 4, 2 + 2*documentCost},
		{"directive", "%TAG !t! tag:x,2000:\n--- a\n", 2, 1 + documentCost + directiveCost},
	} {
		t.Run(c.name, func(t *testing.T) {
			if s := loadFork([]byte(c.data), c.cost); s.err != nil || s.nodes != c.nodes {
				t.Fatalf("budget %d: err = %v, nodes = %d, want %d", c.cost, s.err, s.nodes, c.nodes)
			}
			if s := loadFork([]byte(c.data), c.cost-1); !errors.Is(s.err, ErrBudget) {
				t.Fatalf("budget %d: err = %v, want ErrBudget", c.cost-1, s.err)
			}
		})
	}
}
