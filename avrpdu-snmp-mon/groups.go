package main

// The groups of the devices: the tabs of the main page (as on the donoff panel). A device is in one
// group or in none; "Архив" is a group too (the page keeps it apart). Kept in a JSON file
// ({"ccu1": "Шкаф 1", ...}) and the order of the tabs next to it (-order.json); no file: memory only.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
)

type groups struct {
	mu    sync.Mutex
	path  string
	of    map[string]string // device id -> group
	order []string          // the tabs in this order; the others go after them by name
}

func orderPath(p string) string { return strings.TrimSuffix(p, ".json") + "-order.json" }

// loadGroups reads the files; missing files are no groups.
func loadGroups(path string) (*groups, error) {
	g := &groups{path: path, of: map[string]string{}}
	if path == "" {
		return g, nil
	}
	for p, v := range map[string]any{path: &g.of, orderPath(path): &g.order} {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, v); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if g.of == nil {
		g.of = map[string]string{}
	}
	return g, nil
}

func checkGroup(name string) error {
	if len([]rune(name)) > 40 || strings.ContainsAny(name, "\r\n") {
		return errors.New("название группы — одна строка до 40 символов")
	}
	return nil
}

// view: a copy for the page.
func (g *groups) view() (map[string]string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := make(map[string]string, len(g.of))
	for k, v := range g.of {
		m[k] = v
	}
	return m, append([]string{}, g.order...)
}

// set puts a device into a group ("" = out of any).
func (g *groups) set(dev, name string) error {
	name = strings.TrimSpace(name)
	if err := checkGroup(name); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if name == "" {
		delete(g.of, dev)
	} else {
		g.of[dev] = name
	}
	return g.save()
}

// rename moves every device of a group into another name (an existing group too: they merge).
func (g *groups) rename(from, to string) error {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return errors.New("нужны оба названия")
	}
	if err := checkGroup(to); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for d, n := range g.of {
		if n == from {
			g.of[d] = to
		}
	}
	ord := []string{}
	for _, n := range g.order {
		if n == from {
			n = to
		}
		if !slices.Contains(ord, n) {
			ord = append(ord, n)
		}
	}
	g.order = ord
	return g.save()
}

// remove takes every device out of a group and the group out of the order.
func (g *groups) remove(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("нужно название")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for d, n := range g.of {
		if n == name {
			delete(g.of, d)
		}
	}
	g.order = slices.DeleteFunc(g.order, func(n string) bool { return n == name })
	return g.save()
}

// setOrder keeps the order of the tabs.
func (g *groups) setOrder(order []string) error {
	if len(order) > 200 {
		return errors.New("слишком много групп")
	}
	ord := []string{}
	for _, n := range order {
		n = strings.TrimSpace(n)
		if err := checkGroup(n); err != nil {
			return err
		}
		if n != "" && !slices.Contains(ord, n) {
			ord = append(ord, n)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.order = ord
	return g.save()
}

func (g *groups) save() error {
	if g.path == "" {
		return nil
	}
	if err := writeJSON(g.path, g.of); err != nil {
		return err
	}
	return writeJSON(orderPath(g.path), g.order)
}

// writeJSON writes v to path through a temporary file.
func writeJSON(path string, v any) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
