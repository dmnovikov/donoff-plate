package main

// Profiles: where a model keeps its values. One YAML file per model (profiles/*.yaml built in,
// more in -profiles DIR); the program picks it by sysObjectID or by -profile ID. See README.md.

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed profiles/*.yaml
var builtinProfiles embed.FS

// Field is one value: its OID ({n} is the input or the outlet number), how to read it
// (int, string: a number in a text, text), a scale, and for a state the value that means "yes".
type Field struct {
	OID   string         `yaml:"oid" json:"-"`
	Type  string         `yaml:"type" json:"-"`  // int (default), string (a number in text), text
	Scale float64        `yaml:"scale" json:"-"` // multiplied by (0: 1)
	Yes   *int           `yaml:"yes" json:"-"`   // for a state: the value that means yes/ok (default 1)
	OK    *int           `yaml:"ok" json:"-"`    // for an alarm: the value that means "no alarm" (any other is one)
	Label string         `yaml:"label" json:"label,omitempty"`
	Map   map[int]string `yaml:"map" json:"-"` // texts of the values: units, states
	In    []int          `yaml:"in" json:"-"`  // for an alarm: the values that are one (Elemy CCU: the active states)
}

func (f *Field) ok() bool { return f != nil && f.OID != "" }
func (f *Field) oid(n int) string {
	return strings.ReplaceAll(f.OID, "{n}", fmt.Sprint(n))
}

// oid2: {n} the sensor, {v} its value.
func (f *Field) oid2(n, v int) string {
	return strings.ReplaceAll(f.oid(n), "{v}", fmt.Sprint(v))
}

// alarm: is the value an alarm? With ok: any other value; else the value equals yes.
func (f *Field) alarm(v int) bool {
	if len(f.In) > 0 {
		return slices.Contains(f.In, v)
	}
	if f.OK != nil {
		return v != *f.OK
	}
	return v == f.yes()
}

func (f *Field) yes() int {
	if f.Yes != nil {
		return *f.Yes
	}
	return 1
}

type Profile struct {
	ID          string         `yaml:"id" json:"id"`
	Vendor      string         `yaml:"vendor" json:"vendor"`
	Model       string         `yaml:"model" json:"model"`
	Title       string         `yaml:"title" json:"title"`
	Kind        string         `yaml:"kind" json:"kind"` // the kind of the equipment: ПДУ, АВР, шкаф управления, ...
	Description string         `yaml:"description" json:"description"`
	Links       []string       `yaml:"links" json:"links,omitempty"`
	Match       []string       `yaml:"match" json:"-"`         // sysObjectID prefixes
	Alarms      []*Field       `yaml:"alarms" json:"-"`        // of the whole device, each with a label
	Sensors     []*SensorGroup `yaml:"sensors" json:"sensors"` // temperature, humidity, dry contacts, ...

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
		Title     string `yaml:"title"`      // the heading of the inputs' card ("АВР" by default)
		Inputs    int    `yaml:"inputs"`     // 0: no ATS
		InputsOID *Field `yaml:"inputs_oid"` // or read from the device (up to 4)
		First     *int   `yaml:"first"`      // the SNMP index of the first input (1 by default); the page counts from 1
		Active    *Field `yaml:"active"`     // the number of the active input (or input.active: a flag of each)
		Priority  *Field `yaml:"priority"`
		Failback  *Field `yaml:"failback"`
		Ground    *Field `yaml:"ground"`
		VNom      *Field `yaml:"vnom"` // the nominal voltage and the allowed deviation: the limits of all the
		VDev      *Field `yaml:"vdev"` // inputs, when the inputs have no vmin/vmax of their own
	} `yaml:"ats" json:"-"`

	Input struct {
		Name     *Field   `yaml:"name"`
		Present  *Field   `yaml:"present"`
		Active   *Field   `yaml:"active"` // yes: this input feeds the load
		Hz       *Field   `yaml:"hz"`
		V        *Field   `yaml:"v"`
		MA       *Field   `yaml:"ma"`
		W        *Field   `yaml:"w"`
		Var      *Field   `yaml:"var"`
		KWh      *Field   `yaml:"kwh"`
		Inverted *Field   `yaml:"inverted"`
		VMin     *Field   `yaml:"vmin"`
		VMax     *Field   `yaml:"vmax"`
		Alarms   []*Field `yaml:"alarms"`
	} `yaml:"input" json:"-"`

	Outlets struct {
		Title    string   `yaml:"title" json:"-"` // the heading on the page ("Розетки" by default)
		Item     string   `yaml:"item" json:"-"`  // one of them without a name ("розетка" by default)
		Count    int      `yaml:"count"`          // a fixed count, or
		CountOID *Field   `yaml:"count_oid"`      // read from the device
		First    int      `yaml:"first"`          // the index of the first outlet (0 or 1)
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

	Has         map[string]bool `yaml:"-" json:"has"` // what the page may show: "input.hz", "outlet.va", "switch", ...
	OutletTitle string          `yaml:"-" json:"outlet_title"`
	OutletItem  string          `yaml:"-" json:"outlet_item"`
	ATSTitle    string          `yaml:"-" json:"ats_title"`
}

// SensorGroup: a table of sensors, each with one or more values ({n} the sensor, {v} the value).
type SensorGroup struct {
	Title    string   `yaml:"title" json:"title"`
	Count    int      `yaml:"count" json:"-"`
	CountOID *Field   `yaml:"count_oid" json:"-"`
	First    int      `yaml:"first" json:"-"`  // the index of the first sensor
	Values   int      `yaml:"values" json:"-"` // values of a sensor (1 by default), {v} from vfirst
	VFirst   int      `yaml:"vfirst" json:"-"`
	Name     *Field   `yaml:"name" json:"-"`
	Value    *Field   `yaml:"value" json:"-"` // a number, or a text by its map
	Unit     *Field   `yaml:"unit" json:"-"`  // a code with a map of the units
	UnitText string   `yaml:"unit_text" json:"-"`
	Status   *Field   `yaml:"status" json:"-"`  // the sensor: an alarm by ok/yes, the text by the map
	VStatus  *Field   `yaml:"vstatus" json:"-"` // the value: the same
	Digits   *Field   `yaml:"digits" json:"-"`  // the decimal digits of each value (Raritan): value / 10^digits
	Avail    *Field   `yaml:"avail" json:"-"`   // the sensor is there (equals yes): the others are not shown
	Div      *Field   `yaml:"div" json:"-"`     // the divisor of each value (Exagate: 1, 10, 100)
	List     []int    `yaml:"list" json:"-"`    // the SNMP indexes of the sensors (instead of first + count)
	NA       []int    `yaml:"na" json:"-"`      // status codes that mean "no data" (not an alarm)
	Units    []string `yaml:"units" json:"-"`   // the unit of each value (from vfirst)
	VSub     []int    `yaml:"vsub" json:"-"`    // {w} in the OIDs: this number for each value (a column by the value)
}

// first: the SNMP index of the first input.
func (p *Profile) first() int {
	if p.ATS.First != nil {
		return *p.ATS.First
	}
	return 1
}

func (p *Profile) check() error {
	if p.ID == "" {
		return errors.New("no id")
	}
	if p.ATS.Inputs < 0 || p.ATS.Inputs > 4 {
		return errors.New("ats.inputs: 0..4")
	}
	if p.Outlets.Count < 0 || p.Outlets.Count > 64 {
		return errors.New("outlets.count: 0..64")
	}
	if p.ATS.Inputs == 0 && p.ATS.InputsOID.ok() {
		p.ATS.Inputs = 1 // until the device tells
	}
	if p.ATS.Inputs == 0 && p.Outlets.Count == 0 && !p.Outlets.CountOID.ok() {
		return errors.New("neither ats nor outlets")
	}
	for k, l := range map[string][]*Field{"alarms": p.Alarms, "input.alarms": p.Input.Alarms, "outlets.alarms": p.Outlets.Alarms} {
		for _, a := range l {
			if !a.ok() || a.Label == "" {
				return errors.New(k + ": oid and label")
			}
		}
	}
	p.OutletTitle, p.OutletItem, p.ATSTitle = p.Outlets.Title, p.Outlets.Item, p.ATS.Title
	if p.OutletTitle == "" {
		p.OutletTitle = "Розетки"
	}
	if p.OutletItem == "" {
		p.OutletItem = "розетка"
	}
	if p.ATSTitle == "" {
		p.ATSTitle = "АВР"
	}
	for _, g := range p.Sensors {
		if g.Title == "" || !g.Value.ok() || g.Count <= 0 && !g.CountOID.ok() && len(g.List) == 0 {
			return errors.New("sensors: title, value and count or count_oid")
		}
		if g.Values <= 0 {
			g.Values = 1
		}
		if len(g.List) > 0 {
			g.Count, g.CountOID = len(g.List), nil
		}
	}
	h := map[string]bool{}
	set := func(k string, f *Field) { h[k] = f.ok() }
	h["ats"] = p.ATS.Inputs > 0
	h["ats.active"] = p.ATS.Active.ok() || p.Input.Active.ok()
	set("ats.priority", p.ATS.Priority)
	set("ats.failback", p.ATS.Failback)
	set("ats.ground", p.ATS.Ground)
	i := &p.Input
	for k, f := range map[string]*Field{"name": i.Name, "present": i.Present, "hz": i.Hz, "v": i.V, "ma": i.MA, "w": i.W, "var": i.Var,
		"kwh": i.KWh, "inverted": i.Inverted, "vmin": i.VMin, "vmax": i.VMax} {
		set("input."+k, f)
	}
	h["input.vmin"] = h["input.vmin"] || p.ATS.VNom.ok() && p.ATS.VDev.ok()
	h["input.vmax"] = h["input.vmax"] || p.ATS.VNom.ok() && p.ATS.VDev.ok()
	o := &p.Outlets
	h["outlets"] = o.Count > 0 || o.CountOID.ok()
	for k, f := range map[string]*Field{"name": o.Name, "admin": o.Admin, "on": o.On, "ma": o.MA, "w": o.W, "va": o.VA, "var": o.Var, "kwh": o.KWh} {
		set("outlet."+k, f)
	}
	h["switch"] = o.Switch != nil && o.Switch.OID != ""
	set("system.temp", p.System.Temp)
	h["sensors"] = len(p.Sensors) > 0
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
