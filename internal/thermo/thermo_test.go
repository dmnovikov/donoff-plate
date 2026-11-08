package thermo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

func TestDecideHysteresis(t *testing.T) {
	c := DefaultConfig() // 26 / 2
	fan := false
	// rising: off up to 26 inclusive, on above it
	for _, s := range []struct {
		temp *float64
		want bool
	}{
		{f(23), false}, {f(25), false}, {f(26), false}, {f(26.06), true},
		// falling: stays on inside the band, off at 24
		{f(25.5), true}, {f(24.06), true}, {f(24), false},
		// rising inside the band again: stays off
		{f(25), false}, {f(26), false},
		// sensor error: off at once, even when it was on
		{f(27), true}, {nil, false},
		// sensor back inside the band: stays off
		{f(25), false}, {f(26.5), true},
	} {
		fan, _ = Decide(c, s.temp, fan)
		if fan != s.want {
			t.Fatalf("T=%v: fan=%v, want %v", fmtTemp(s.temp), fan, s.want)
		}
	}
}

func TestDecideHeat(t *testing.T) {
	c := Config{Setpoint: 20, Hysteresis: 1, Mode: Auto, Action: Heat}
	fan := false
	for _, s := range []struct {
		temp *float64
		want bool
	}{
		// falling: off down to 20 inclusive, on below it
		{f(22), false}, {f(20), false}, {f(19.9), true},
		// rising: stays on inside the band, off at 21
		{f(20.5), true}, {f(20.94), true}, {f(21), false},
		// falling inside the band again: stays off
		{f(20.5), false},
		// sensor error: a heater goes off at once, even when it was on
		{f(19), true}, {nil, false},
	} {
		fan, _ = Decide(c, s.temp, fan)
		if fan != s.want {
			t.Fatalf("heat T=%v: on=%v, want %v", fmtTemp(s.temp), fan, s.want)
		}
	}
	if c.OffEdge() != 21 {
		t.Fatal("heat off edge = setpoint + hysteresis")
	}
}

func TestOldStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte(`{"setpoint": 26, "hysteresis": 1, "mode": "auto"}`), 0o644)
	got := New(&fakeSensor{}, &fakeRelay{}, path, time.Second, time.Second, time.Hour).Status().Config
	if got != (Config{Setpoint: 26, Hysteresis: 1, Mode: Auto, Action: Cool}) {
		t.Fatalf("a state file without action must load as cool: %+v", got)
	}
}

func TestDecideManual(t *testing.T) {
	c := DefaultConfig()
	c.Mode = Off
	if fan, _ := Decide(c, nil, true); fan {
		t.Fatal("manual off must win over a sensor error")
	}
	c.Mode = On
	if fan, _ := Decide(c, f(10), false); !fan {
		t.Fatal("manual on")
	}
}

type fakeSensor struct {
	t   float64
	err error
}

func (s *fakeSensor) Read() (float64, string, error) { return s.t, "28-test", s.err }

type fakeRelay struct{ on bool }

func (r *fakeRelay) Set(on bool) error { r.on = on; return nil }

func TestThermostatStateAndCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, r := &fakeSensor{t: 27}, &fakeRelay{}
	th := New(s, r, path, time.Second, time.Nanosecond, time.Hour)
	th.step()
	if !r.on || !th.Status().Fan {
		t.Fatal("27 > 26: the fan must be on")
	}
	s.err = errors.New("CRC error")
	th.step()
	if st := th.Status(); r.on || st.Temp != nil || st.SensorErr == "" {
		t.Fatalf("sensor error: %+v", st)
	}
	s.err, s.t = nil, 23
	th.step()
	if st := th.Status(); r.on || st.Switches != 2 {
		t.Fatalf("23: off, 2 switches (on at 27, off on the error); got %+v", st)
	}
	if len(th.History()) != 3 {
		t.Fatalf("history: %d", len(th.History()))
	}

	c := Config{Setpoint: 30, Hysteresis: 1.5, Mode: Auto, Action: Heat}
	if err := th.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	if got := New(s, r, path, time.Second, time.Second, time.Hour).Status().Config; got != c {
		t.Fatalf("saved config: %+v", got)
	}
	if th.SetConfig(Config{Setpoint: 26, Hysteresis: 0, Mode: Auto, Action: Cool}) == nil {
		t.Fatal("hysteresis 0 must be refused")
	}
}
