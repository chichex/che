package runner

import "testing"

// TestRingBufferAppendUnderCap cubre el path basico: appendear menos
// elementos que la capacidad → todos quedan, en orden de insercion.
func TestRingBufferAppendUnderCap(t *testing.T) {
	rb := NewRingBuffer(3)
	rb.Append(LogLineStdout, "a")
	rb.Append(LogLineStdout, "b")
	snap := rb.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snap))
	}
	if snap[0].Text != "a" || snap[1].Text != "b" {
		t.Errorf("unexpected order: %v", snap)
	}
}

// TestRingBufferWrap cubre la rama de overflow: con cap=3 y 5 appends, el
// snapshot tiene que ser solo los ultimos 3.
func TestRingBufferWrap(t *testing.T) {
	rb := NewRingBuffer(3)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		rb.Append(LogLineStdout, s)
	}
	snap := rb.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 entries (cap), got %d", len(snap))
	}
	want := []string{"c", "d", "e"}
	for i, w := range want {
		if snap[i].Text != w {
			t.Errorf("snap[%d]: expected %q, got %q", i, w, snap[i].Text)
		}
	}
}

// TestRingBufferClear cubre ctrl+l (vacia el viewport sin tocar disco).
func TestRingBufferClear(t *testing.T) {
	rb := NewRingBuffer(5)
	rb.Append(LogLineStderr, "boom")
	rb.Clear()
	if rb.Len() != 0 {
		t.Fatalf("expected empty after Clear, got len=%d", rb.Len())
	}
	// nextSeq no se resetea — un append posterior sigue contando hacia
	// adelante (importante para no confundir consumidores que correlacionen
	// el seq con events.jsonl).
	seq := rb.Append(LogLineStdout, "post")
	if seq <= 1 {
		t.Errorf("expected seq to keep growing post-Clear, got %d", seq)
	}
}

// TestRingBufferSeqMonotonic cubre que la Seq devuelta crece monotonicamente
// independientemente del kind.
func TestRingBufferSeqMonotonic(t *testing.T) {
	rb := NewRingBuffer(10)
	s1 := rb.Append(LogLineStdout, "x")
	s2 := rb.Append(LogLineStderr, "y")
	s3 := rb.Append(LogLineStdout, "z")
	if s1 != 1 || s2 != 2 || s3 != 3 {
		t.Errorf("expected seq 1,2,3 — got %d,%d,%d", s1, s2, s3)
	}
}
