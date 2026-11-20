package main

// The SNMP side: GET only, by the profile of the model. The ATS and the inputs every 2 s, the
// outlets every 5 s, the names, the limits and the system every minute. SET only in switchOutlet,
// for the outlets the guard allows.

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

type System struct {
	ObjectID string `json:"object_id"`
	Model    string `json:"model"`
	Serial   string `json:"serial"`
	Firmware string `json:"firmware"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Temp     int    `json:"temp"`
	UptimeS  int64  `json:"uptime_s"`
}

type ATS struct {
	Active   int `json:"active"`
	Priority int `json:"priority"`
	Failback int `json:"failback"`
	Ground   int `json:"ground"` // 1 good
}

type Input struct {
	N        int      `json:"n"` // 1, 2, ... on the page
	Idx      int      `json:"-"` // the SNMP index
	Name     string   `json:"name"`
	Present  int      `json:"present"` // the voltage/frequency is there
	Hz       float64  `json:"hz"`
	V        float64  `json:"v"`
	MA       float64  `json:"ma"`
	W        float64  `json:"w"`
	Var      float64  `json:"var"`
	KWh      float64  `json:"kwh"`
	Inverted int      `json:"inverted"`
	VMin     float64  `json:"vmin"`
	VMax     float64  `json:"vmax"`
	Alarms   []string `json:"alarms"`
}

type Outlet struct {
	N      int      `json:"n"`
	Name   string   `json:"name"`
	Admin  int      `json:"admin"`
	On     int      `json:"on"`
	MA     float64  `json:"ma"`
	W      float64  `json:"w"`
	VA     float64  `json:"va"`
	Var    float64  `json:"var"`
	KWh    float64  `json:"kwh"`
	Alarms []string `json:"alarms"`
	Ctl    bool     `json:"ctl"`              // this page may switch it
	NextS  float64  `json:"next_s,omitempty"` // seconds until the next switch is allowed
}

// Sensor: one value of a sensor (a temperature, a humidity, a dry contact).
type Sensor struct {
	G      int     `json:"g"` // the group in profile.sensors
	N      int     `json:"n"` // the sensor's SNMP index
	V      int     `json:"v"` // the value's index
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Text   string  `json:"text,omitempty"` // the value by the map (a state)
	Unit   string  `json:"unit"`
	Status string  `json:"status,omitempty"` // what is wrong, if anything
	Valid  bool    `json:"valid"`            // the sensor answers and the value is there
	Alarm  bool    `json:"alarm"`            // out of the norm
	Off    bool    `json:"-"`                // not there (avail): not shown, not kept
}

type State struct {
	ID      string    `json:"id"`
	Host    string    `json:"host"`
	Main    int       `json:"main"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`     // the last good answer
	OutAt   time.Time `json:"out_at"` // the last good outlets
	Profile *Profile  `json:"profile"`
	Kind    string    `json:"kind"` // the kind of the equipment: of the config, else of the profile
	System  System    `json:"system"`
	ATS     ATS       `json:"ats"`
	Inputs  []Input   `json:"inputs"`
	Outlets []Outlet  `json:"outlets"`
	Sensors []Sensor  `json:"sensors"`
	SensAt  time.Time `json:"sens_at"`
	Alarms  []string  `json:"alarms"` // of the whole device
	Version string    `json:"version"`
}

type poller struct {
	id    string
	kind  string // of the config
	snmp  *gosnmp.GoSNMP
	smu   sync.Mutex // one SNMP request at a time
	mu    sync.Mutex
	st    State
	prof  *Profile // nil until found
	all   map[string]*Profile
	guard *guard
	scnt  []int // sensors of each group
}

func newPoller(id, host, community string, mainIn int, prof *Profile, all map[string]*Profile) *poller {
	h, port := host, uint16(161)
	if hh, pp, err := net.SplitHostPort(host); err == nil {
		h = hh
		if n, err := strconv.Atoi(pp); err == nil {
			port = uint16(n)
		}
	}
	g := &gosnmp.GoSNMP{Target: h, Port: port, Community: community, Version: gosnmp.Version2c,
		Timeout: 2 * time.Second, Retries: 1, MaxOids: 10}
	p := &poller{id: id, snmp: g, all: all, st: State{ID: id, Host: host, Main: mainIn, Version: version, Inputs: []Input{}, Outlets: []Outlet{}, Sensors: []Sensor{}, Alarms: []string{}}}
	if prof != nil {
		p.use(prof)
	}
	return p
}

func (p *poller) use(prof *Profile) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prof = prof
	p.st.Profile = prof
	p.setInputs(prof.ATS.Inputs)
	if prof.Outlets.Count > 0 {
		p.setOutlets(prof.Outlets.Count)
	}
	cnt := make([]int, len(prof.Sensors))
	for g, sg := range prof.Sensors {
		cnt[g] = sg.Count
	}
	p.setSensors(cnt)
}

// setInputs, setSensors, setOutlets: under p.mu
func (p *poller) setInputs(cnt int) {
	if len(p.st.Inputs) == cnt && cnt > 0 {
		return
	}
	p.st.Inputs = make([]Input, cnt)
	for i := range p.st.Inputs {
		p.st.Inputs[i].N, p.st.Inputs[i].Idx, p.st.Inputs[i].Alarms = i+1, p.prof.first()+i, []string{}
	}
}

func (p *poller) setSensors(cnt []int) {
	if p.scnt != nil && fmt.Sprint(cnt) == fmt.Sprint(p.scnt) {
		return
	}
	p.scnt = cnt
	p.st.Sensors = []Sensor{}
	for g, sg := range p.prof.Sensors {
		for i := 0; i < cnt[g]; i++ {
			for v := 0; v < sg.Values; v++ {
				n := sg.First + i
				if i < len(sg.List) {
					n = sg.List[i]
				}
				p.st.Sensors = append(p.st.Sensors, Sensor{G: g, N: n, V: sg.VFirst + v})
			}
		}
	}
}

func (p *poller) setOutlets(cnt int) {
	if len(p.st.Outlets) == cnt {
		return
	}
	p.st.Outlets = make([]Outlet, cnt)
	for i := range p.st.Outlets {
		p.st.Outlets[i].N = p.prof.Outlets.First + i
		p.st.Outlets[i].Alarms = []string{}
	}
}

func (p *poller) snapshot() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.st
	s.Kind = p.kind
	if s.Kind == "" && p.prof != nil {
		s.Kind = p.prof.Kind
	}
	s.Inputs = append([]Input{}, p.st.Inputs...) // [] not null: a device without inputs
	s.Outlets = append([]Outlet{}, p.st.Outlets...)
	s.Alarms = append([]string{}, p.st.Alarms...)
	s.Sensors = make([]Sensor, 0, len(p.st.Sensors))
	for _, x := range p.st.Sensors {
		if !x.Off {
			s.Sensors = append(s.Sensors, x)
		}
	}
	for i := range s.Inputs {
		s.Inputs[i].Alarms = append([]string{}, s.Inputs[i].Alarms...)
	}
	for i := range s.Outlets {
		s.Outlets[i].Alarms = append([]string{}, s.Outlets[i].Alarms...)
		s.Outlets[i].Ctl = p.guard.allowed(s.Outlets[i].N) && p.prof != nil && p.prof.Has["switch"]
		if s.Outlets[i].Ctl {
			s.Outlets[i].NextS = p.guard.wait(s.Outlets[i].N).Seconds()
		}
	}
	return s
}

// get reads the OIDs, 10 per request; the values come back by OID.
func (p *poller) get(oids []string) (map[string]gosnmp.SnmpPDU, error) {
	p.smu.Lock()
	defer p.smu.Unlock()
	out := make(map[string]gosnmp.SnmpPDU, len(oids))
	for i := 0; i < len(oids); i += 10 {
		r, err := p.snmp.Get(oids[i:min(i+10, len(oids))])
		if err != nil {
			return nil, err
		}
		if r.Error != gosnmp.NoError {
			return nil, fmt.Errorf("snmp: %v", r.Error)
		}
		for _, v := range r.Variables {
			out[v.Name] = v
		}
	}
	return out, nil
}

// req: the OIDs to read and what to do with each value
type req struct {
	oids []string
	put  []func(gosnmp.SnmpPDU)
}

func (q *req) add(f *Field, n int, put func(gosnmp.SnmpPDU)) {
	if f.ok() {
		q.oids = append(q.oids, f.oid(n))
		q.put = append(q.put, put)
	}
}
func (q *req) num(f *Field, n int, dst *float64) {
	q.add(f, n, func(v gosnmp.SnmpPDU) { *dst = value(f, v) })
}
func (q *req) int(f *Field, n int, dst *int) {
	q.add(f, n, func(v gosnmp.SnmpPDU) { *dst = int(value(f, v)) })
}
func (q *req) yes(f *Field, n int, dst *int) {
	q.add(f, n, func(v gosnmp.SnmpPDU) {
		if f.Min != nil {
			*dst = b2i(!missing(v) && value(f, v) >= *f.Min)
		} else {
			*dst = b2i(int(value(f, v)) == f.yes())
		}
	})
}
func (q *req) text(f *Field, n int, dst *string) {
	q.add(f, n, func(v gosnmp.SnmpPDU) { *dst = text(v) })
}

// run1 reads the OIDs, then puts the values and calls after, both under the state lock.
func (p *poller) run1(q *req, after func()) error {
	var r map[string]gosnmp.SnmpPDU
	if len(q.oids) > 0 {
		var err error
		if r, err = p.get(q.oids); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, o := range q.oids {
		if v, ok := r[o]; ok {
			q.put[i](v)
		}
	}
	if after != nil {
		after()
	}
	return nil
}

func value(f *Field, v gosnmp.SnmpPDU) float64 {
	var x float64
	switch v.Type {
	case gosnmp.OctetString:
		x, _ = strconv.ParseFloat(strings.TrimSpace(string(v.Value.([]byte))), 64)
	case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.Null, gosnmp.EndOfMibView:
		return 0
	default:
		x = float64(gosnmp.ToBigInt(v.Value).Int64())
	}
	if f.Scale != 0 {
		x *= f.Scale
	}
	return x
}

// missing: the device has no such object (an alarm of it is no alarm).
func missing(v gosnmp.SnmpPDU) bool {
	switch v.Type {
	case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.Null, gosnmp.EndOfMibView:
		return true
	}
	return false
}

func text(v gosnmp.SnmpPDU) string {
	if b, ok := v.Value.([]byte); ok {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// detect finds the profile by sysObjectID.
func (p *poller) detect() error {
	r, err := p.get([]string{".1.3.6.1.2.1.1.2.0"})
	if err != nil {
		return err
	}
	id, _ := r[".1.3.6.1.2.1.1.2.0"].Value.(string)
	p.mu.Lock()
	p.st.System.ObjectID = id
	p.mu.Unlock()
	prof := matchProfile(p.all, id)
	if prof == nil {
		return fmt.Errorf("нет профиля для sysObjectID %s (есть: %s; задайте -profile)", id, profileIDs(p.all))
	}
	log.Printf("%s: profile %s by sysObjectID %s", p.id, prof.ID, id)
	p.use(prof)
	return nil
}

func (p *poller) run() {
	if err := p.snmp.Connect(); err != nil {
		log.Fatal(err)
	}
	var lastSlow, lastOut time.Time
	failing := ""
	for {
		now := time.Now()
		var err error
		if p.prof == nil {
			err = p.detect()
		}
		if err == nil {
			err = p.fast()
		}
		if err == nil && now.Sub(lastSlow) >= time.Minute {
			if err = p.slow(); err == nil {
				lastSlow = now
			}
		}
		if err == nil && now.Sub(lastOut) >= 5*time.Second {
			if err = p.outlets(); err == nil {
				err = p.sensors()
			}
			if err == nil {
				lastOut = now
			}
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		p.mu.Lock()
		p.st.OK, p.st.Error = err == nil, msg
		p.mu.Unlock()
		if msg != failing {
			if msg != "" {
				log.Printf("%s: %s", p.id, msg)
			} else {
				log.Printf("%s: answering", p.id)
			}
		}
		failing = msg
		time.Sleep(time.Until(now.Add(2 * time.Second)))
	}
}

func (p *poller) fast() error {
	pr, q := p.prof, &req{}
	a, in := &pr.ATS, &pr.Input
	var st ATS
	q.int(a.Active, 0, &st.Active)
	q.int(a.Priority, 0, &st.Priority)
	q.int(a.Failback, 0, &st.Failback)
	q.yes(a.Ground, 0, &st.Ground)
	// alarms: the device's and each input's, as flags in the order of the profile
	alarm := func(l []*Field, n int) []bool {
		out := make([]bool, len(l))
		for k, f := range l {
			dst := &out[k]
			q.add(f, n, func(v gosnmp.SnmpPDU) { *dst = !missing(v) && f.alarm(int(value(f, v))) })
		}
		return out
	}
	labels := func(l []*Field, on []bool) []string {
		out := []string{}
		for k, b := range on {
			if b {
				out = append(out, l[k].Label)
			}
		}
		return out
	}
	devAl := alarm(pr.Alarms, 0)
	p.mu.Lock()
	ins := append([]Input(nil), p.st.Inputs...)
	p.mu.Unlock()
	act := make([]int, len(ins))
	inAl := make([][]bool, len(ins))
	for i := range ins {
		x, n := &ins[i], ins[i].Idx
		q.yes(in.Active, n, &act[i])
		inAl[i] = alarm(in.Alarms, n)
		x.Present = 1 // without the field: as if there
		q.yes(in.Present, n, &x.Present)
		q.num(in.Hz, n, &x.Hz)
		q.num(in.V, n, &x.V)
		q.num(in.MA, n, &x.MA)
		q.num(in.W, n, &x.W)
		q.num(in.Var, n, &x.Var)
		q.num(in.KWh, n, &x.KWh)
		q.yes(in.Inverted, n, &x.Inverted)
	}
	return p.run1(q, func() {
		if !a.Active.ok() {
			for i := range ins {
				if act[i] == 1 {
					st.Active = ins[i].N
				}
			}
		}
		p.st.ATS = st
		p.st.Alarms = labels(pr.Alarms, devAl)
		for i := range ins {
			ins[i].Alarms = labels(in.Alarms, inAl[i])
		}
		for i := range ins { // the names and the limits come with slow()
			if i < len(p.st.Inputs) {
				ins[i].Name, ins[i].VMin, ins[i].VMax = p.st.Inputs[i].Name, p.st.Inputs[i].VMin, p.st.Inputs[i].VMax
			}
		}
		p.st.Inputs = ins
		p.st.At = time.Now()
	})
}

func (p *poller) outlets() error {
	o := &p.prof.Outlets
	p.mu.Lock()
	outs := append([]Outlet(nil), p.st.Outlets...)
	p.mu.Unlock()
	if len(outs) == 0 {
		return nil
	}
	q := &req{}
	alarms := make([][]bool, len(outs))
	for i := range outs {
		x, n := &outs[i], outs[i].N
		q.int(o.Admin, n, &x.Admin)
		if o.On.ok() && (o.On.Yes != nil || o.On.Min != nil) {
			q.yes(o.On, n, &x.On) // a state code (Raritan: 7 on): 1 or 0
		} else {
			q.int(o.On, n, &x.On)
		}
		q.num(o.MA, n, &x.MA)
		q.num(o.W, n, &x.W)
		q.num(o.VA, n, &x.VA)
		q.num(o.Var, n, &x.Var)
		q.num(o.KWh, n, &x.KWh)
		alarms[i] = make([]bool, len(o.Alarms))
		for k, f := range o.Alarms {
			dst := &alarms[i][k]
			q.add(f, n, func(v gosnmp.SnmpPDU) { *dst = !missing(v) && f.alarm(int(value(f, v))) })
		}
	}
	return p.run1(q, func() {
		if len(outs) != len(p.st.Outlets) {
			return // the count changed meanwhile
		}
		for i := range outs {
			outs[i].Alarms = []string{}
			for k, on := range alarms[i] {
				if on {
					outs[i].Alarms = append(outs[i].Alarms, o.Alarms[k].Label)
				}
			}
			if !o.Admin.ok() {
				outs[i].Admin = outs[i].On
			}
			outs[i].Name = p.st.Outlets[i].Name
		}
		p.st.Outlets = outs
		p.st.OutAt = time.Now()
	})
}

func (p *poller) slow() error {
	pr := p.prof
	s, q := System{}, &req{}
	q.text(pr.System.Model, 0, &s.Model)
	q.text(pr.System.Serial, 0, &s.Serial)
	q.text(pr.System.Firmware, 0, &s.Firmware)
	q.text(pr.System.Name, 0, &s.Name)
	q.text(pr.System.Location, 0, &s.Location)
	q.int(pr.System.Temp, 0, &s.Temp)
	var up, fc, ic float64
	q.num(pr.System.Uptime, 0, &up)
	q.num(pr.Outlets.CountOID, 0, &fc)
	q.num(pr.ATS.InputsOID, 0, &ic)
	sc := make([]float64, len(pr.Sensors))
	for g, sg := range pr.Sensors {
		q.num(sg.CountOID, 0, &sc[g])
	}
	if err := p.run1(q, func() {
		s.ObjectID = p.st.System.ObjectID
		s.UptimeS = int64(up)
		p.st.System = s
		cnt := pr.Outlets.Count
		if pr.Outlets.CountOID.ok() {
			cnt = min(max(int(fc), 0), 64)
		}
		p.setOutlets(cnt)
		if pr.ATS.InputsOID.ok() {
			p.setInputs(min(max(int(ic), 1), 4))
		}
		scnt := make([]int, len(pr.Sensors))
		for g, sg := range pr.Sensors {
			scnt[g] = sg.Count
			if sg.CountOID.ok() {
				scnt[g] = min(max(int(sc[g]), 0), 32)
			}
		}
		p.setSensors(scnt)
	}); err != nil {
		return err
	}
	// the names and the limits
	p.mu.Lock()
	ins := append([]Input(nil), p.st.Inputs...)
	outs := append([]Outlet(nil), p.st.Outlets...)
	p.mu.Unlock()
	q = &req{}
	var vnom, vdev float64
	q.num(pr.ATS.VNom, 0, &vnom)
	q.num(pr.ATS.VDev, 0, &vdev)
	for i := range ins {
		q.text(pr.Input.Name, ins[i].Idx, &ins[i].Name)
		q.num(pr.Input.VMin, ins[i].Idx, &ins[i].VMin)
		q.num(pr.Input.VMax, ins[i].Idx, &ins[i].VMax)
	}
	for i := range outs {
		q.text(pr.Outlets.Name, outs[i].N, &outs[i].Name)
	}
	return p.run1(q, func() {
		for i := range ins {
			if vnom > 0 && !pr.Input.VMin.ok() {
				ins[i].VMin = vnom - vdev
			}
			if vnom > 0 && !pr.Input.VMax.ok() {
				ins[i].VMax = vnom + vdev
			}
			if i < len(p.st.Inputs) {
				p.st.Inputs[i].Name, p.st.Inputs[i].VMin, p.st.Inputs[i].VMax = ins[i].Name, ins[i].VMin, ins[i].VMax
			}
		}
		if len(outs) == len(p.st.Outlets) {
			for i := range outs {
				p.st.Outlets[i].Name = outs[i].Name
			}
		}
	})
}

// sensors reads every value of every sensor: the value, the unit, the name and the states.
func (p *poller) sensors() error {
	pr := p.prof
	p.mu.Lock()
	ss := append([]Sensor(nil), p.st.Sensors...)
	p.mu.Unlock()
	if len(ss) == 0 {
		return nil
	}
	type raw struct {
		unit, st, vst, dig, av, div int
		utext                       string
	}
	rs := make([]raw, len(ss))
	q := &req{}
	for i := range ss {
		x, r, g := &ss[i], &rs[i], pr.Sensors[ss[i].G]
		add := func(f *Field, put func(gosnmp.SnmpPDU)) {
			if f.ok() {
				oid := f.oid2(x.N, x.V)
				if k := x.V - g.VFirst; k >= 0 && k < len(g.VSub) {
					oid = strings.ReplaceAll(oid, "{w}", fmt.Sprint(g.VSub[k]))
				}
				q.oids = append(q.oids, oid)
				q.put = append(q.put, put)
			}
		}
		add(g.Name, func(v gosnmp.SnmpPDU) { x.Name = text(v) })
		add(g.Value, func(v gosnmp.SnmpPDU) { x.Value = value(g.Value, v) })
		add(g.Unit, func(v gosnmp.SnmpPDU) {
			if g.Unit.Type == "text" {
				r.utext = text(v)
			} else {
				r.unit = int(value(g.Unit, v))
			}
		})
		add(g.Div, func(v gosnmp.SnmpPDU) { r.div = int(value(g.Div, v)) })
		add(g.Status, func(v gosnmp.SnmpPDU) { r.st = int(value(g.Status, v)) })
		add(g.VStatus, func(v gosnmp.SnmpPDU) { r.vst = int(value(g.VStatus, v)) })
		add(g.Digits, func(v gosnmp.SnmpPDU) { r.dig = int(value(g.Digits, v)) })
		add(g.Avail, func(v gosnmp.SnmpPDU) { r.av = int(value(g.Avail, v)) })
	}
	return p.run1(q, func() {
		if len(ss) != len(p.st.Sensors) {
			return // the count changed meanwhile
		}
		for i := range ss {
			x, r, g := &ss[i], rs[i], pr.Sensors[ss[i].G]
			x.Unit = g.UnitText
			if k := x.V - g.VFirst; k >= 0 && k < len(g.Units) {
				x.Unit = g.Units[k]
			}
			if g.Unit.ok() {
				x.Unit = g.Unit.Map[r.unit]
				if g.Unit.Type == "text" {
					x.Unit = r.utext
				}
			}
			if g.Value.Map != nil {
				x.Text = g.Value.Map[int(x.Value)]
			}
			x.Valid, x.Alarm, x.Status, x.Off = true, false, "", false
			if g.Avail.ok() && (len(g.Avail.In) > 0 && !slices.Contains(g.Avail.In, r.av) || len(g.Avail.In) == 0 && r.av != g.Avail.yes()) {
				x.Valid, x.Off = false, true
				continue
			}
			if r.dig > 0 && r.dig < 10 {
				x.Value /= math.Pow10(r.dig)
			}
			if r.div > 1 {
				x.Value /= float64(r.div)
			}
			if g.Status.ok() && slices.Contains(g.NA, r.st) {
				x.Valid, x.Status = false, g.Status.Map[r.st]
				continue
			}
			if g.Status.ok() && g.Status.alarm(r.st) {
				x.Status = g.Status.Map[r.st]
				if g.Value.Map == nil { // a sensor that does not answer has no value
					x.Valid = false
				} else {
					x.Alarm = true // a dry contact in alarm
				}
			}
			// the value's own state: "invalid" (0 when ok is 1) means no data, the others are alarms
			if x.Valid && g.VStatus.ok() && g.VStatus.alarm(r.vst) {
				x.Status = g.VStatus.Map[r.vst]
				if r.vst <= 0 {
					x.Valid = false
				} else {
					x.Alarm = true
				}
			}
		}
		p.st.Sensors = ss
		p.st.SensAt = time.Now()
	})
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// switchOutlet sets the outlet by the profile's switch OID after the guard lets it, and reads the
// outlets back at once so that the page shows the new state.
func (p *poller) switchOutlet(n, v int, who string) error {
	if p.prof == nil || !p.prof.Has["switch"] {
		return errors.New("эта модель не умеет переключать розетки")
	}
	if err := p.guard.take(n); err != nil {
		return err
	}
	sw := p.prof.Outlets.Switch
	val := sw.Off
	if v == 1 {
		val = sw.On
	}
	p.smu.Lock()
	r, err := p.snmp.Set([]gosnmp.SnmpPDU{{Name: strings.ReplaceAll(sw.OID, "{n}", strconv.Itoa(n)), Type: gosnmp.Integer, Value: val}})
	p.smu.Unlock()
	if err == nil && r.Error != gosnmp.NoError {
		err = fmt.Errorf("snmp set: %v", r.Error)
	}
	state := map[int]string{0: "off", 1: "on"}[v]
	if err != nil {
		log.Printf("%s: outlet %d %s by %s: %v", p.id, n, state, who, err)
		return err
	}
	log.Printf("%s: outlet %d %s by %s", p.id, n, state, who)
	time.Sleep(time.Second)
	p.outlets()
	return nil
}
