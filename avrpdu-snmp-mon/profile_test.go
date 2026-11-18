package main

import "testing"

func TestProfiles(t *testing.T) {
	all, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	p := matchProfile(all, ".1.3.6.1.4.1.46235.2")
	if p == nil || p.ID != "rcntec-rpcm" {
		t.Fatalf("RPCM not matched: %v", p)
	}
	if matchProfile(all, ".1.3.6.1.4.1.46235.20") != nil || matchProfile(all, ".1.3.6.1.4.1.58833.1.1.1") != nil {
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
