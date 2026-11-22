package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProfiles(t *testing.T) {
	all, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	p := matchProfile(all, ".1.3.6.1.4.1.46235.2")
	if p == nil || p.ID != "rcntec-rpcm" {
		t.Fatalf("RPCM not matched: %v", p)
	}
	if matchProfile(all, ".1.3.6.1.4.1.46235.20") != nil || matchProfile(all, ".1.3.6.1.4.1.58833.1.3.1") != nil {
		t.Fatal("a foreign sysObjectID matched")
	}
	for _, k := range []string{"ats", "input.v", "input.hz", "outlet.va", "switch"} {
		if !p.Has[k] {
			t.Errorf("RPCM has no %s", k)
		}
	}
	if got := p.Outlets.Switch.OID; got != ".1.3.6.1.4.1.46235.2.4.2.1.4.{n}" {
		t.Errorf("switch oid %s", got)
	}
	if p.Input.Hz.oid(2) != ".1.3.6.1.4.1.46235.2.3.2.1.5.2" || p.Input.Hz.Scale != 0.01 {
		t.Errorf("hz %s %v", p.Input.Hz.oid(2), p.Input.Hz.Scale)
	}
}

func TestElemy(t *testing.T) {
	all, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	p := matchProfile(all, ".1.3.6.1.4.1.58833.1.2.2")
	if p == nil || p.ID != "elemy-ats1204" {
		t.Fatalf("Elemy not matched: %v", p)
	}
	if p.first() != 0 || all["rcntec-rpcm"].first() != 1 {
		t.Errorf("first input: elemy %d, rpcm %d", p.first(), all["rcntec-rpcm"].first())
	}
	for _, k := range []string{"ats", "ats.active", "input.v", "input.hz", "input.vmin", "outlets", "outlet.ma"} {
		if !p.Has[k] {
			t.Errorf("Elemy has no %s", k)
		}
	}
	if p.Has["switch"] || p.Has["input.w"] {
		t.Error("Elemy: switch or input.w")
	}
	if a := p.Alarms[0]; !a.alarm(2) || a.alarm(0) {
		t.Error("ok: 0 alarm")
	}
	if a := p.Outlets.Alarms[1]; !a.alarm(2) || a.alarm(1) {
		t.Error("yes: 2 alarm")
	}
}

func TestElemyIPDU(t *testing.T) {
	all, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	p := matchProfile(all, ".1.3.6.1.4.1.58833.1.4.1")
	if p == nil || p.ID != "elemy-ipdu" {
		t.Fatalf("IPDU not matched: %v", p)
	}
	if p.ATS.Inputs != 1 || !p.Has["ats"] || p.Has["ats.active"] || !p.Has["input.var"] || !p.Has["sensors"] {
		t.Errorf("IPDU has: %v", p.Has)
	}
	if len(p.Sensors) != 2 || p.Sensors[0].Values != 2 || p.Sensors[1].Values != 1 {
		t.Fatalf("sensors %+v", p.Sensors)
	}
	g := p.Sensors[0]
	if g.Value.oid2(1, 0) != ".1.3.6.1.4.1.58833.1.4.1.6.2.1.6.1.0" || g.Unit.Map[15] != "°C" || !g.Status.alarm(2) || g.Status.alarm(1) {
		t.Errorf("sensor oid %s unit %q", g.Value.oid2(1, 0), g.Unit.Map[15])
	}
	if p.OutletItem != "группа" || p.ATSTitle != "Ввод питания" || all["rcntec-rpcm"].ATSTitle != "АВР" {
		t.Errorf("titles %q %q", p.OutletItem, p.ATSTitle)
	}
}

func TestRaritan(t *testing.T) {
	all, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	p := matchProfile(all, ".1.3.6.1.4.1.13742.6")
	if p == nil || p.ID != "raritan-px" {
		t.Fatalf("Raritan not matched: %v", p)
	}
	if !p.Has["ats"] || !p.Has["switch"] || !p.Has["outlet.va"] || !p.Has["sensors"] || p.Outlets.On.yes() != 7 {
		t.Errorf("Raritan has: %v", p.Has)
	}
	g := p.Sensors[0]
	if !g.Avail.ok() || !g.Digits.ok() || g.VStatus.alarm(4) || !g.VStatus.alarm(5) || g.Unit.Map[22] != "г/м³" {
		t.Errorf("sensors %+v", g)
	}
}

func TestExagate(t *testing.T) {
	all, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	p := matchProfile(all, ".1.3.1.4.1.35483.1.1.1.1.4.0")
	if p == nil || p.ID != "exagate-sysguard" {
		t.Fatalf("Exagate not matched: %v", p)
	}
	if p.Has["ats"] || !p.Has["sensors"] || p.Has["switch"] || len(p.Sensors) != 4 {
		t.Errorf("Exagate has: %v", p.Has)
	}
	g := p.Sensors[0]
	if g.Count != 6 || g.List[4] != 48 || g.Unit.Type != "text" || !g.Div.ok() || len(g.NA) != 1 {
		t.Errorf("sensors %+v", g)
	}
}

func TestElemyCCU(t *testing.T) {
	all, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	p := matchProfile(all, ".1.3.6.1.4.1.58833.1.1.1")
	if p == nil || p.ID != "elemy-ccu" {
		t.Fatalf("CCU not matched: %v", p)
	}
	if !p.Has["ats"] || !p.Has["sensors"] || p.Has["switch"] || len(p.Sensors) != 2 {
		t.Errorf("CCU has: %v", p.Has)
	}
	g := p.Sensors[0]
	if g.Values != 2 || g.Value.oid2(1, 5) != ".1.3.6.1.4.1.58833.1.1.1.4.2.1.5.1" || g.Units[1] != "%" || g.VSub[0] != 10 {
		t.Errorf("sensors %+v", g)
	}
	if !g.VStatus.alarm(7) || g.VStatus.alarm(3) || g.VStatus.alarm(0) {
		t.Error("alarm by in")
	}
}

func TestGroups(t *testing.T) {
	path := t.TempDir() + "/groups.json"
	g, _ := loadGroups(path)
	g.set("a", " Шкаф 1 ")
	g.set("b", "Шкаф 1")
	g.set("c", "Стойка")
	g.setOrder([]string{"Стойка", "Шкаф 1", "Стойка", " "})
	if err := g.rename("Шкаф 1", "Шкаф"); err != nil {
		t.Fatal(err)
	}
	g2, err := loadGroups(path)
	if err != nil {
		t.Fatal(err)
	}
	m, ord := g2.view()
	if m["a"] != "Шкаф" || m["b"] != "Шкаф" || m["c"] != "Стойка" || strings.Join(ord, ",") != "Стойка,Шкаф" {
		t.Fatalf("groups %q, order %q", m, ord)
	}
	g2.remove("Стойка")
	if m, ord := g2.view(); len(m) != 2 || strings.Join(ord, ",") != "Шкаф" {
		t.Fatalf("after remove: %q %q", m, ord)
	}
	if g2.set("x", "a\nb") == nil || g2.rename("Шкаф", strings.Repeat("я", 41)) == nil {
		t.Fatal("bad names accepted")
	}
}

func TestViewOnly(t *testing.T) {
	h := viewOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for _, c := range []struct {
		method, role string
		code         int
	}{{"GET", "view", 200}, {"PUT", "view", 403}, {"POST", "view", 403}, {"POST", "admin", 200}, {"PUT", "", 200}} {
		r := httptest.NewRequest(c.method, "/api/group", nil)
		if c.role != "" {
			r.Header.Set("X-Pdu-Role", c.role)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.code {
			t.Errorf("%s as %q: %d, want %d", c.method, c.role, w.Code, c.code)
		}
	}
}
