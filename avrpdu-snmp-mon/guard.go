package main

// The guard of the switching: only the outlets given by -control, not more often than one switch
// in -min-gap for an outlet and -max-hour switches an hour.

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type guard struct {
	mu      sync.Mutex
	ok      map[int]bool
	gap     time.Duration
	perHour int
	last    map[int][]time.Time
}

func newGuard(list string, gap time.Duration, perHour int) (*guard, error) {
	g := &guard{ok: map[int]bool{}, gap: gap, perHour: perHour, last: map[int][]time.Time{}}
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || n > 31 {
			return nil, fmt.Errorf("-control: bad outlet %q", f)
		}
		g.ok[n] = true
	}
	return g, nil
}

func (g *guard) allowed(n int) bool {
	if g == nil {
		return false
	}
	return g.ok[n]
}

// wait: how long until the outlet may be switched again (0: now).
func (g *guard) wait(n int) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waitLocked(n, time.Now())
}

func (g *guard) waitLocked(n int, now time.Time) time.Duration {
	ts := g.last[n]
	k := 0
	for _, t := range ts { // keep the last hour only
		if now.Sub(t) < time.Hour {
			ts[k] = t
			k++
		}
	}
	ts = ts[:k]
	g.last[n] = ts
	var w time.Duration
	if len(ts) > 0 {
		w = g.gap - now.Sub(ts[len(ts)-1])
	}
	if len(ts) >= g.perHour {
		w = max(w, time.Hour-now.Sub(ts[len(ts)-g.perHour]))
	}
	return max(w, 0).Round(time.Second)
}

// take books a switch of outlet n now, or says why not.
func (g *guard) take(n int) error {
	if !g.allowed(n) {
		return fmt.Errorf("розеткой %d управлять отсюда нельзя", n)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if w := g.waitLocked(n, now); w > 0 {
		return fmt.Errorf("слишком часто: следующее переключение через %d с", int(w.Seconds()))
	}
	g.last[n] = append(g.last[n], now)
	return nil
}
