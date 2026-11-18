package main

// The SNMP side: GET only, by the profile of the model. The ATS and the inputs every 2 s, the
// outlets every 5 s, the names, the limits and the system every minute. SET only in switchOutlet,
// for the outlets the guard allows.

import (
	"errors"
	"fmt"
	"log"
	"net"
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
	N        int     `json:"n"`
	Name     string  `json:"name"`
	Present  int     `json:"present"` // the voltage/frequency is there
	Hz       float64 `json:"hz"`
	V        float64 `json:"v"`
	MA       float64 `json:"ma"`
	W        float64 `json:"w"`
	KWh      float64 `json:"kwh"`
	Inverted int     `json:"inverted"`
	VMin     float64 `json:"vmin"`
	VMax     float64 `json:"vmax"`
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

type State struct {
	Host    string    `json:"host"`
	Main    int       `json:"main"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`     // the last good answer
	OutAt   time.Time `json:"out_at"` // the last good outlets
	Profile *Profile  `json:"profile"`
	System  System    `json:"system"`
	ATS     ATS       `json:"ats"`
	Inputs  []Input   `json:"inputs"`
	Outlets []Outlet  `json:"outlets"`
	Version string    `json:"version"`
}

type poller struct {
	snmp  *gosnmp.GoSNMP
	smu   sync.Mutex // one SNMP request at a time
	mu    sync.Mutex
	st    State
	prof  *Profile // nil until found
	all   map[string]*Profile
	guard *guard
}

func newPoller(host, community string, mainIn int, prof *Profile, all map[string]*Profile) *poller {
	h, port := host, uint16(161)
	if hh, pp, err := net.SplitHostPort(host); err == nil {
		h = hh
		if n, err := strconv.Atoi(pp); err == nil {
			port = uint16(n)
		}
	}
	g := &gosnmp.GoSNMP{Target: h, Port: port, Community: community, Version: gosnmp.Version2c,
		Timeout: 2 * time.Second, Retries: 1, MaxOids: 10}
	p := &poller{snmp: g, all: all, st: State{Host: host, Main: mainIn, Version: version, Inputs: []Input{}, Outlets: []Outlet{}}}
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
	p.st.Inputs = make([]Input, prof.ATS.Inputs)
	for i := range p.st.Inputs {
		p.st.Inputs[i].N = prof.ATS.First + i
	}
	if prof.Outlets.Count > 0 {
		p.setOutlets(prof.Outlets.Count)
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
	s.Inputs = append([]Input(nil), p.st.Inputs...)
	s.Outlets = append([]Outlet(nil), p.st.Outlets...)
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
	q.add(f, n, func(v gosnmp.SnmpPDU) { *dst = b2i(int(value(f, v)) == f.yes()) })
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
	log.Printf("profile %s by sysObjectID %s", prof.ID, id)
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
				log.Printf("device: %s", msg)
			} else {
				log.Printf("device: answering again")
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
	p.mu.Lock()
	ins := append([]Input(nil), p.st.Inputs...)
	p.mu.Unlock()
	for i := range ins {
		x, n := &ins[i], ins[i].N
		x.Present = 1 // without the field: as if there
		q.yes(in.Present, n, &x.Present)
		q.num(in.Hz, n, &x.Hz)
		q.num(in.V, n, &x.V)
		q.num(in.MA, n, &x.MA)
		q.num(in.W, n, &x.W)
		q.num(in.KWh, n, &x.KWh)
		q.yes(in.Inverted, n, &x.Inverted)
	}
	return p.run1(q, func() {
		p.st.ATS = st
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
		q.int(o.On, n, &x.On)
		q.num(o.MA, n, &x.MA)
		q.num(o.W, n, &x.W)
		q.num(o.VA, n, &x.VA)
		q.num(o.Var, n, &x.Var)
		q.num(o.KWh, n, &x.KWh)
		alarms[i] = make([]bool, len(o.Alarms))
		for k, f := range o.Alarms {
			dst := &alarms[i][k]
			q.add(f, n, func(v gosnmp.SnmpPDU) { *dst = int(value(f, v)) == f.yes() })
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
	var up, fc float64
	q.num(pr.System.Uptime, 0, &up)
	q.num(pr.Outlets.CountOID, 0, &fc)
	if err := p.run1(q, func() {
		s.ObjectID = p.st.System.ObjectID
		s.UptimeS = int64(up)
		p.st.System = s
		cnt := pr.Outlets.Count
		if pr.Outlets.CountOID.ok() {
			cnt = min(max(int(fc), 0), 64)
		}
		p.setOutlets(cnt)
	}); err != nil {
		return err
	}
	// the names and the limits
	p.mu.Lock()
	ins := append([]Input(nil), p.st.Inputs...)
	outs := append([]Outlet(nil), p.st.Outlets...)
	p.mu.Unlock()
	q = &req{}
	for i := range ins {
		q.text(pr.Input.Name, ins[i].N, &ins[i].Name)
		q.num(pr.Input.VMin, ins[i].N, &ins[i].VMin)
		q.num(pr.Input.VMax, ins[i].N, &ins[i].VMax)
	}
	for i := range outs {
		q.text(pr.Outlets.Name, outs[i].N, &outs[i].Name)
	}
	return p.run1(q, func() {
		for i := range ins {
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
		log.Printf("outlet %d %s by %s: %v", n, state, who, err)
		return err
	}
	log.Printf("outlet %d %s by %s", n, state, who)
	time.Sleep(time.Second)
	p.outlets()
	return nil
}
