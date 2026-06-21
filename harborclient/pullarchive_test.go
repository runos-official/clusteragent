package harborclient

import (
	"bytes"
	"strings"
	"testing"
)

// TestCopyBoundedLayer pins the PullArchive size guard: a layer within its
// advertised size streams fully; a layer that streams MORE than advertised is
// rejected (a lying descriptor can't overrun the destination); an advertised
// size over the hard ceiling is rejected up front; an unknown (<=0) advertised
// size is bounded to the ceiling. A regression here lets a compromised/corrupt
// registry layer fill disk or memory unbounded.
func TestCopyBoundedLayer(t *testing.T) {
	const ceiling = 1024

	t.Run("within advertised size streams fully", func(t *testing.T) {
		src := bytes.Repeat([]byte("a"), 500)
		var w bytes.Buffer
		n, err := copyBoundedLayer(&w, bytes.NewReader(src), 500, ceiling)
		if err != nil || n != 500 || w.Len() != 500 {
			t.Fatalf("n=%d err=%v len=%d, want 500/nil/500", n, err, w.Len())
		}
	})

	t.Run("streams more than advertised is rejected", func(t *testing.T) {
		src := bytes.Repeat([]byte("a"), 600)
		var w bytes.Buffer
		_, err := copyBoundedLayer(&w, bytes.NewReader(src), 500, ceiling)
		if err == nil || !strings.Contains(err.Error(), "more than its advertised") {
			t.Fatalf("err=%v, want 'more than its advertised'", err)
		}
	})

	t.Run("advertised over ceiling is rejected before reading", func(t *testing.T) {
		src := bytes.Repeat([]byte("a"), 10)
		var w bytes.Buffer
		n, err := copyBoundedLayer(&w, bytes.NewReader(src), ceiling+1, ceiling)
		if err == nil || !strings.Contains(err.Error(), "over the") || n != 0 || w.Len() != 0 {
			t.Fatalf("n=%d err=%v len=%d, want 0/over-the-limit/0", n, err, w.Len())
		}
	})

	t.Run("unknown advertised size is bounded to ceiling", func(t *testing.T) {
		var w bytes.Buffer
		// within the ceiling: passes
		n, err := copyBoundedLayer(&w, bytes.NewReader(bytes.Repeat([]byte("a"), 900)), 0, ceiling)
		if err != nil || n != 900 {
			t.Fatalf("within-ceiling: n=%d err=%v, want 900/nil", n, err)
		}
		// exceeding the ceiling: rejected
		w.Reset()
		if _, err := copyBoundedLayer(&w, bytes.NewReader(bytes.Repeat([]byte("a"), ceiling+50)), 0, ceiling); err == nil {
			t.Fatal("over-ceiling with unknown size: err=nil, want rejection")
		}
	})
}
