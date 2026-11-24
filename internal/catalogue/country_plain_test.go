package catalogue

import "testing"

func TestPlainLabelDropsFlagsAndKeepsAName(t *testing.T) {
	for in, want := range map[string]string{
		"Bulgaria 🇧🇬":    "Bulgaria",
		"🇩🇪 Frankfurt 2": "Frankfurt 2",
		"🇩🇪  Berlin  🇩🇪": "Berlin",
		"Paris":          "Paris",
		"🇫🇷":             "🇫🇷",
	} {
		if got := PlainLabel(in); got != want {
			t.Errorf("PlainLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
