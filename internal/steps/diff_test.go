package steps

import (
	"strings"
	"testing"
)

func TestRenderDiff(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want []string
	}{
		{"new file",
			renderDiff("/etc/new.conf", false, nil, 0, []byte("a\nb\n"), 0o600),
			[]string{"--- /dev/null", "+++ /etc/new.conf (new, mode 0600)", "+a", "+b"}},
		{"changed middle line",
			renderDiff("/etc/f", true, []byte("keep\nold\nkeep2\n"), 0o644, []byte("keep\nnew\nkeep2\n"), 0o644),
			[]string{"-old", "+new"}},
		{"mode only",
			renderDiff("/etc/f", true, []byte("same\n"), 0o644, []byte("same\n"), 0o600),
			[]string{"mode 0644 -> 0600"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, w := range tt.want {
				if !strings.Contains(tt.diff, w) {
					t.Fatalf("diff missing %q:\n%s", w, tt.diff)
				}
			}
		})
	}
	if d := renderDiff("/etc/f", true, []byte("keep\nold\nkeep2\n"), 0o644, []byte("keep\nnew\nkeep2\n"), 0o644); strings.Contains(d, "-keep") || strings.Contains(d, "+keep") {
		t.Fatalf("common lines must be trimmed:\n%s", d)
	}
}
