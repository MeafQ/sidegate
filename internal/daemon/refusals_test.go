package daemon

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"sidegate/internal/domain"
)

// TestEveryRefusalNamesItsReason reads the daemon's source: every error body and every refusal is
// built with a reason, and every reason is one some refusal gives.
func TestEveryRefusalNamesItsReason(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	given := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				if ident, ok := n.Type.(*ast.Ident); ok && ident.Name == "Reason" {
					for _, name := range n.Names {
						declared[name.Name] = true
					}
					return false
				}
			case *ast.Ident:
				if strings.HasPrefix(n.Name, "Refused") {
					given[n.Name] = true
				}
			case *ast.CompositeLit:
				if ident, ok := n.Type.(*ast.Ident); ok && (ident.Name == "errorBody" || ident.Name == "CommandError") {
					named := false
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok && kv.Key.(*ast.Ident).Name == "Reason" {
							named = true
						}
					}
					if !named {
						t.Errorf("%s builds a %s with no reason", file, ident.Name)
					}
				}
			}
			return true
		})
	}
	if len(declared) == 0 {
		t.Fatal("no reasons declared")
	}
	for name := range declared {
		if !given[name] {
			t.Errorf("%s is a reason no refusal gives", name)
		}
	}
}

func TestARefusedPatternSaysWhyInGosFewestWords(t *testing.T) {
	_, server := newPageDaemon(t)
	for pattern, want := range map[string]string{
		"(?=x)": "lookahead not supported", "(?<!x)": "lookbehind not supported", "[a": "missing closing ]", "(a": "missing closing )",
		"a**": "invalid repeat", "a{2,1}": "invalid repeat count", "*a": "nothing to repeat", `\q`: "invalid escape", `a\`: "trailing backslash",
	} {
		include, _ := json.Marshal(pattern)
		body := `{"members":{"sources":{"not":false,"values":[]},"countries":{"not":false,"values":[]},"protocols":{"not":false,"values":[]},"include":` + string(include) + `,"exclude":"","box":null}}`
		got := mutate(t, server.URL, http.MethodPost, "api/groups/preview", "p1", nil, body)
		var refusal errorBody
		json.Unmarshal([]byte(got.body), &refusal)
		if got.status != http.StatusUnprocessableEntity || refusal.Reason != RefusedPattern || refusal.Detail != want {
			t.Errorf("%s answered %d %+v, want %s", pattern, got.status, refusal, want)
		}
	}
}

func TestASlotWithNoNameIsRefusedForThat(t *testing.T) {
	_, server := newPageDaemon(t)
	got := mutate(t, server.URL, http.MethodPut, "api/slots/s", "k1", map[string]string{"If-None-Match": "*"}, `{"name":" ","exit":{"kind":"direct"}}`)
	var refusal errorBody
	json.Unmarshal([]byte(got.body), &refusal)
	if got.status != http.StatusUnprocessableEntity || refusal.Reason != RefusedNoName {
		t.Errorf("a blank name answered %d %+v", got.status, refusal)
	}
	if _, err := domain.NewIntent(" ", domain.DirectExit()); err != domain.ErrNoName {
		t.Errorf("a blank name is %v", err)
	}
}
