package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	gateway "sidegate/internal/runtime"
)

// carriedNames are directory names an earlier release's units could serve from, and some they couldn't.
var carriedNames = []string{"backup(1)", "a b", "$HOME", "x${HOME}y", "100%", "%n %%", "a;b&c|d<e>f", "`id`", "*?[x]{y}", "#~!@:-", "ключ"}

// unitValues are the values of every line of unit that sets key.
func unitValues(unit []byte, key string) []string {
	var values []string
	for _, line := range strings.Split(string(unit), "\n") {
		if value, found := strings.CutPrefix(line, key+"="); found {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
}

var unitVariable = regexp.MustCompile(`\$\{\w+\}`)

// unitCommand is what systemd runs for the unit's one line that sets key, as its 245 and 257 read it: words
// end at spaces outside quotes, %% is one %, and ${NAME} is expanded unless the executable has the prefix
// ":". A lone % or a backslash would mean more, and no line here may have one.
func unitCommand(t *testing.T, unit []byte, key string) []string {
	t.Helper()
	values := unitValues(unit, key)
	if len(values) != 1 {
		t.Fatalf("the unit sets %s %d times:\n%s", key, len(values), unit)
	}
	var words []string
	var word strings.Builder
	quote, began := rune(0), false
	for _, r := range values[0] + " " {
		switch {
		case r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote, began = r, true
		case quote == 0 && r == ' ':
			if began {
				words = append(words, word.String())
			}
			word.Reset()
			began = false
		default:
			word.WriteRune(r)
			began = true
		}
	}
	if quote != 0 || len(words) == 0 {
		t.Fatalf("%s=%s has an unbalanced quote or no word", key, values[0])
	}
	executable := strings.TrimLeft(words[0], "-:")
	literal := strings.Contains(words[0][:len(words[0])-len(executable)], ":")
	words[0] = executable
	for i, w := range words {
		plain := strings.ReplaceAll(w, "%%", "")
		if strings.ContainsAny(plain, `%\`) {
			t.Fatalf("%s=%s has %q, which systemd reads as something else", key, values[0], w)
		}
		if i > 0 && !literal {
			w = unitVariable.ReplaceAllString(w, "")
		}
		words[i] = strings.ReplaceAll(w, "%%", "%")
	}
	return words
}

// The core unit names each path whole, whatever characters it has.
func TestTheCoreUnitCarriesItsPaths(t *testing.T) {
	for _, name := range append([]string{"data"}, carriedNames...) {
		data, run := "/data/"+name+"/sidegate", "/run/"+name+"/sidegate"
		unit := gateway.CoreUnit(gateway.Options{DataDir: data, RunDir: run})
		if got, want := unitCommand(t, unit, "ExecStart"), []string{data + "/current/xray", "run", "-c", run + "/xray.json"}; !slices.Equal(got, want) {
			t.Errorf("%s: the core starts as %q, want %q", name, got, want)
		}
		if got, want := unitCommand(t, unit, "ExecStopPost"), []string{data + "/current/sidegated", "close-gates", "--run-dir", run}; !slices.Equal(got, want) {
			t.Errorf("%s: the core stops with %q, want %q", name, got, want)
		}
		if got, want := unitValues(unit, "StandardOutput"), []string{"append:" + strings.ReplaceAll(gateway.CoreLogPath(run), "%", "%%")}; !slices.Equal(got, want) {
			t.Errorf("%s: the core logs to %q, want %q", name, got, want)
		}
	}
}

// A path no unit can carry is refused before anything is written, by an install and an upgrade, as the
// data dir and as the run dir bootstrap.json names.
func TestAPathNoUnitCarriesIsRefused(t *testing.T) {
	r := newInstallRig(t)
	refused := func(request LifecycleRequest, why string) {
		t.Helper()
		before := r.tree()
		r.clearCalls()
		if err := RunLifecycle(context.Background(), request); err == nil || outcomeOf(err) != outcomeRefused || !strings.Contains(err.Error(), "can't be in a systemd unit") {
			t.Errorf("%s with %s: %v", request.Action, why, err)
		}
		if change := r.changed(before); change != "" {
			t.Errorf("the refused %s with %s changed the host: %s", request.Action, why, change)
		}
	}
	names := []string{"it's", `a"b`, `a\b`, "a\nb", "a\tb", "a\x7fb", "a\xffb", "a￾b", "a﷐b"}
	for _, name := range names {
		install := r.request(Install, r.release("one"))
		install.DataDir = filepath.Join(r.root, name, "sidegate")
		refused(install, fmt.Sprintf("the data dir %q", install.DataDir))
		if _, err := os.Lstat(filepath.Join(r.root, name)); err == nil {
			t.Errorf("the refused install made %q", name)
		}
	}
	r.mustRun(r.request(Install, r.release("one")))
	bootstrap, _ := os.ReadFile(bootstrapPath(r.data))
	for _, name := range names {
		fields := map[string]any{}
		json.Unmarshal(bootstrap, &fields)
		fields["runDir"] = filepath.Join(r.root, name, "sidegate")
		raw, _ := json.Marshal(fields)
		os.WriteFile(bootstrapPath(r.data), raw, 0o600)
		if b, _ := LoadBootstrap(r.data); b.RunDir != fields["runDir"] {
			continue // JSON can't hold it either.
		}
		refused(r.request(Install, r.release("one")), fmt.Sprintf("the run dir %q", fields["runDir"]))
		refused(r.request(Upgrade, r.release("two")), fmt.Sprintf("the run dir %q", fields["runDir"]))
	}
}
