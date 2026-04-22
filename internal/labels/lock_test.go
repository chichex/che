package labels

import "testing"

func TestParseHasLabel(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		label string
		want  bool
	}{
		{"present", `{"labels":[{"name":"che:locked"},{"name":"ct:plan"}]}`, "che:locked", true},
		{"absent", `{"labels":[{"name":"ct:plan"}]}`, "che:locked", false},
		{"empty labels", `{"labels":[]}`, "che:locked", false},
		{"missing key", `{}`, "che:locked", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseHasLabel([]byte(c.body), c.label)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseHasLabel_InvalidJSON(t *testing.T) {
	if _, err := parseHasLabel([]byte("not json"), "x"); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

func TestParseListLocked(t *testing.T) {
	body := `[
		{"number":42,"title":"Fix foo","url":"https://github.com/acme/demo/issues/42","isPullRequest":false},
		{"number":7,"title":"feat(x)","url":"https://github.com/acme/demo/pull/7","isPullRequest":true}
	]`
	got, err := parseListLocked([]byte(body), "acme/demo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got))
	}
	if got[0].Number != 42 || got[0].IsPR || got[0].Repo != "acme/demo" {
		t.Errorf("item 0 mismatch: %+v", got[0])
	}
	if got[1].Number != 7 || !got[1].IsPR {
		t.Errorf("item 1 mismatch: %+v", got[1])
	}
}

func TestRefNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"42", 42, true},
		{"#42", 42, true},
		{"  7  ", 7, true},
		{"acme/demo#115", 115, true},
		{"https://github.com/acme/demo/pull/99", 99, true},
		{"https://github.com/acme/demo/issues/3", 3, true},
		{"https://github.com/acme/demo/pull/99/files", 99, true},
		{"not-a-ref", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := refNumber(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("refNumber(%q) unexpected error: %v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("refNumber(%q) = %d, want %d", c.in, got, c.want)
			}
		} else if err == nil {
			t.Errorf("refNumber(%q) expected error, got %d", c.in, got)
		}
	}
}
