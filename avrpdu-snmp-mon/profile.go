package main

// Profiles: where a model keeps its values. One YAML file per model (profiles/*.yaml built in,
// more in -profiles DIR); the program picks it by sysObjectID or by -profile ID. See README.md.

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed profiles/*.yaml
var builtinProfiles embed.FS

// Field is one value: its OID ({n} is the input or the outlet number), how to read it
// (int, string: a number in a text, text), a scale, and for a state the value that means "yes".
type Field struct {
	OID   string  `yaml:"oid" json:"-"`
	Type  string  `yaml:"type" json:"-"`  // int (default), string (a number in text), text
	Scale float64 `yaml:"scale" json:"-"` // multiplied by (0: 1)
	Yes   *int    `yaml:"yes" json:"-"`   // for a state: the value that means yes/ok (default 1)
	Label string  `yaml:"label" json:"label,omitempty"`
}

func (f *Field) ok() bool { return f != nil && f.OID != "" }
func (f *Field) oid(n int) string {
	return strings.ReplaceAll(f.OID, "{n}", fmt.Sprint(n))
}
func (f *Field) yes() int {
	if f.Yes != nil {
		return *f.Yes
	}
	return 1
}

type Profile struct {
	ID          string   `yaml:"id" json:"id"`
	Vendor      string   `yaml:"vendor" json:"vendor"`
	Model       string   `yaml:"model" json:"model"`
	Title       string   `yaml:"title" json:"title"`
	Description string   `yaml:"description" json:"description"`
	Links       []string `yaml:"links" json:"links,omitempty"`
	Match       []string `yaml:"match" json:"-"` // sysObjectID prefixes

	System struct {
		Model    *Field `yaml:"model"`
		Serial   *Field `yaml:"serial"`
		Firmware *Field `yaml:"firmware"`
		Name     *Field `yaml:"name"`
		Location *Field `yaml:"location"`
		Temp     *Field `yaml:"temp"`
		Uptime   *Field `yaml:"uptime"` // in seconds after the scale
	} `yaml:"system" json:"-"`

	ATS struct {
		Inputs   int    `yaml:"inputs"` // 0: no ATS
		First    int    `yaml:"first"`  // the number of the first input (1 by default)
		Active   *Field `yaml:"active"`
		Priority *Field `yaml:"priority"`
		Failback *Field `yaml:"failback"`
		Ground   *Field `yaml:"ground"`
	} `yaml:"ats" json:"-"`

	Input struct {
		Name     *Field `yaml:"name"`
		Present  *Field `yaml:"present"`
		Hz       *Field `yaml:"hz"`
		V        *Field `yaml:"v"`
		MA       *Field `yaml:"ma"`
		W        *Field `yaml:"w"`
		KWh      *Field `yaml:"kwh"`
		Inverted *Field `yaml:"inverted"`
		VMin     *Field `yaml:"vmin"`
		VMax     *Field `yaml:"vmax"`
	} `yaml:"input" json:"-"`

	Outlets struct {
		Count    int      `yaml:"count"`     // a fixed count, or
		CountOID *Field   `yaml:"count_oid"` // read from the device
		First    int      `yaml:"first"`     // the index of the first outlet (0 or 1)
		Name     *Field   `yaml:"name"`
		Admin    *Field   `yaml:"admin"` // what is set
		On       *Field   `yaml:"on"`    // what is
		MA       *Field   `yaml:"ma"`
		W        *Field   `yaml:"w"`
		VA       *Field   `yaml:"va"`
		Var      *Field   `yaml:"var"`
		KWh      *Field   `yaml:"kwh"`
		Alarms   []*Field `yaml:"alarms"` // each with a label: active when the value is "yes"
		Switch   *struct {
			OID string `yaml:"oid"`
			On  int    `yaml:"on"`
			Off int    `yaml:"off"`
		} `yaml:"switch"`
	} `yaml:"outlets" json:"-"`

	Has map[string]bool `yaml:"-" json:"has"` // what the page may show: "input.hz", "outlet.va", "switch", ...
}

func (p *Profile) check() error {
	if p.ID == "" {
		return errors.New("no id")
	}
	if p.ATS.Inputs < 0 || p.ATS.Inputs > 4 {
		return errors.New("ats.inputs: 0..4")
	}
	if p.ATS.First == 0 {
		p.ATS.First = 1
	}
	if p.Outlets.Count < 0 || p.Outlets.Count > 64 {
		return errors.New("outlets.count: 0..64")
	}
	if p.ATS.Inputs == 0 && p.Outlets.Count == 0 && !p.Outlets.CountOID.ok() {
		return errors.New("neither ats nor outlets")
	}
	for _, a := range p.Outlets.Alarms {
		if !a.ok() || a.Label == "" {
			return errors.New("outlets.alarms: oid and label")
		}
	}
	h := map[string]bool{}
	set := func(k string, f *Field) { h[k] = f.ok() }
	h["ats"] = p.ATS.Inputs > 0
	set("ats.active", p.ATS.Active)
	set("ats.priority", p.ATS.Priority)
	set("ats.failback", p.ATS.Failback)
	set("ats.ground", p.ATS.Ground)
	i := &p.Input
	for k, f := range map[string]*Field{"name": i.Name, "present": i.Present, "hz": i.Hz, "v": i.V, "ma": i.MA, "w": i.W,
		"kwh": i.KWh, "inverted": i.Inverted, "vmin": i.VMin, "vmax": i.VMax} {
		set("input."+k, f)
	}
	o := &p.Outlets
	h["outlets"] = o.Count > 0 || o.CountOID.ok()
	for k, f := range map[string]*Field{"name": o.Name, "admin": o.Admin, "on": o.On, "ma": o.MA, "w": o.W, "va": o.VA, "var": o.Var, "kwh": o.KWh} {
		set("outlet."+k, f)
	}
	h["switch"] = o.Switch != nil && o.Switch.OID != ""
	set("system.temp", p.System.Temp)
	p.Has = h
	return nil
}

// loadProfiles: the built-in ones, then the files of dir (the same id replaces a built-in one).
func loadProfiles(dir string) (map[string]*Profile, error) {
	out := map[string]*Profile{}
	add := func(name string, b []byte) error {
		var p Profile
		if err := yaml.Unmarshal(b, &p); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		if err := p.check(); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		out[p.ID] = &p
		return nil
	}
	ents, _ := builtinProfiles.ReadDir("profiles")
	for _, e := range ents {
		b, _ := builtinProfiles.ReadFile("profiles/" + e.Name())
		if err := add(e.Name(), b); err != nil {
			return nil, err
		}
	}
	if dir != "" {
		files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			if err := add(f, b); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// matchProfile: the profile with the longest sysObjectID prefix.
func matchProfile(all map[string]*Profile, objectID string) *Profile {
	objectID = "." + strings.TrimPrefix(objectID, ".")
	var best *Profile
	bl := 0
	for _, p := range all {
		for _, m := range p.Match {
			m = "." + strings.TrimPrefix(m, ".")
			if (objectID == m || strings.HasPrefix(objectID, m+".")) && len(m) > bl {
				best, bl = p, len(m)
			}
		}
	}
	return best
}

func profileIDs(all map[string]*Profile) string {
	var ids []string
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}
