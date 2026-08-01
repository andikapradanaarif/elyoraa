package job

import "testing"

func TestIsBot(t *testing.T) {
	cases := []struct {
		author string
		want   bool
	}{
		{"dependabot[bot]", true},
		{"github-actions[bot]", true},
		{"github-actions", true},
		{"dependabot", true},
		{"andikaarif", false},
		{"", false},
	}

	for _, c := range cases {
		j := Review{Author: c.author}
		if got := j.IsBot(); got != c.want {
			t.Errorf("IsBot(%q) = %v, want %v", c.author, got, c.want)
		}
	}
}
