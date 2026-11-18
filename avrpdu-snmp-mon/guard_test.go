package main

import (
	"testing"
	"time"
)

func TestGuard(t *testing.T) {
	g, err := newGuard("1", 10*time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	if g.take(5) == nil {
		t.Fatal("outlet 5 must be refused")
	}
	if err := g.take(1); err != nil {
		t.Fatal(err)
	}
	if g.take(1) == nil {
		t.Fatal("a second switch at once must be refused")
	}
	if w := g.wait(1); w < 9*time.Second || w > 10*time.Second {
		t.Fatalf("wait %v", w)
	}
	// 3 switches in the last hour, 20 s apart: the 4th waits for the first to age out
	now := time.Now()
	g.last[1] = []time.Time{now.Add(-50 * time.Minute), now.Add(-40 * time.Minute), now.Add(-30 * time.Second)}
	if w := g.wait(1); w < 9*time.Minute || w > 11*time.Minute {
		t.Fatalf("hour limit: wait %v", w)
	}
	g.last[1] = []time.Time{now.Add(-70 * time.Minute), now.Add(-40 * time.Minute), now.Add(-30 * time.Second)}
	if err := g.take(1); err != nil {
		t.Fatalf("old switches must not count: %v", err)
	}
}
