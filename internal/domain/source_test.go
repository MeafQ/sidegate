package domain

import "testing"

func filter(t *testing.T, include, exclude string) Filter {
	t.Helper()
	f, err := NewFilter(include, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFilter(t *testing.T) {
	cases := []struct {
		include, exclude string
		names            []string
		want             bool
	}{
		{"", "", []string{"anything"}, true},
		{"", "", nil, true},
		{"germany", "", []string{"GERMANY 1"}, true},
		{"germany", "", []string{"Sweden"}, false},
		{"germany", "", []string{"proxy", "Germany"}, true},
		{"", "edge", []string{"EDGE-Sweden"}, false},
		{"", "edge", []string{"proxy", "EDGE-Sweden"}, false},
		{"åland", "", []string{"ÅLAND 1"}, true},
		{"åland", "edge", []string{"EDGE-ÅLAND"}, false},
		{"de|nl", "", []string{"xNLx"}, true},
	}
	for _, c := range cases {
		if got := filter(t, c.include, c.exclude).Pass(c.names...); got != c.want {
			t.Errorf("include %q exclude %q names %q = %v", c.include, c.exclude, c.names, got)
		}
	}
	for _, bad := range [][2]string{{"(", ""}, {"", "("}} {
		if _, err := NewFilter(bad[0], bad[1]); err == nil {
			t.Errorf("%q compiled", bad)
		}
	}
	if include, exclude := filter(t, "a", "b").Patterns(); include != "a" || exclude != "b" {
		t.Errorf("patterns = %q, %q", include, exclude)
	}
}

func TestOriginIsExactlyOneArm(t *testing.T) {
	if _, ok := RemoteOrigin(Remote{Location: "l"}).Manual(); ok {
		t.Error("a remote origin reads as manual")
	}
	if r, ok := RemoteOrigin(Remote{Location: "l"}).Remote(); !ok || r.Location != "l" {
		t.Errorf("remote = %+v, %v", r, ok)
	}
	if _, ok := ManualOrigin(Manual{Text: "t"}).Remote(); ok {
		t.Error("a manual origin reads as remote")
	}
	if m, ok := ManualOrigin(Manual{Text: "t"}).Manual(); !ok || m.Text != "t" {
		t.Errorf("manual = %+v, %v", m, ok)
	}
}
