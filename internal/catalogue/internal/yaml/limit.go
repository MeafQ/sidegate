package yaml

import "errors"

// This package is gopkg.in/yaml.v3 v3.0.1, formatted with gofmt, plus a budget
// that the parser charges where it allocates for what it reads: each node it
// builds, each comment it keeps, each document, and each %TAG directive. A
// decoder with a budget stops at the first charge past it, before allocating,
// so what one stream may cost is bounded by the parser that builds it rather
// than by a separate reading of the input. The duplicate-key check in decode.go also
// differs from upstream. Every other line is upstream's.

// ErrBudget reports a stream that would cost more than its decoder's budget.
var ErrBudget = errors.New("yaml: document exceeds the budget")

// Costs in nodes, from yaml.v3's measured allocation for each. A node costs
// about 240 bytes. A comment is appended to a list the parser never trims, which
// costs about 1 KB per comment as the list grows. A document also builds a
// decoder and copies the default tag directives. Each %TAG directive is checked
// against every earlier one, so it is priced to keep that quadratic scan short.
const (
	nodeCost      = 1
	commentCost   = 4
	documentCost  = 4
	directiveCost = 16
)

type budget struct {
	left    int
	limited bool
}

func (b *budget) charge(cost int) {
	if !b.limited {
		return
	}
	if b.left -= cost; b.left < 0 {
		fail(ErrBudget)
	}
}

// Budget limits what the rest of the stream may cost, in nodes: Decode fails
// with ErrBudget at the first node, comment, document, or directive past n.
func (dec *Decoder) Budget(n int) {
	dec.parser.parser.budget = budget{left: n, limited: true}
}
