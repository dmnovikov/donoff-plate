package panel

import (
	"os"
	"testing"
	"time"
)

func TestPoint(t *testing.T) {
	at := time.UnixMilli(1700000000123)
	for _, c := range []struct{ dev, sub, pay, want string }{
		{"dmzk10", "sensors/temp_in", "12.50", "sensor,dev=dmzk10,name=temp_in value=12.5 1700000000123"},
		{"dmzk13", "temp_out", "7.00", "sensor,dev=dmzk13,name=temp_out value=7 1700000000123"},
		{"dmzk16", "sensors/sct013_1", "0.39", "sensor,dev=dmzk16,name=sct013_1 value=0.39 1700000000123"},
		{"dmzk10", "relays/r1", "1", "relay,dev=dmzk10,name=r1 state=1 1700000000123"},
		{"dmzk13", "b2", "0", "relay,dev=dmzk13,name=b2 state=0 1700000000123"},
		{"dmzk15", "sensors/temp_out_json", `{"val":"775"}`, ""},
		{"t14", "sensors/pzem004", `{"current":"3.16"}`, ""},
		{"dmzk10", "time_up", "127:47:51", ""},
		{"dmzk10", "relays/r1", "ON", ""},
		{"dmzk10", "sensors/temp_in", "nan", ""},
		{"dmzk10", "sensors/temp_in", "", ""},
	} {
		if got := point(c.dev, c.sub, c.pay, at); got != c.want {
			t.Errorf("%s %s %q: %q, want %q", c.dev, c.sub, c.pay, got, c.want)
		}
	}
}

func TestParseInflux(t *testing.T) {
	x, err := ParseInflux("url=http://127.0.0.1:8086/\norg=lab240\nbucket=donoff\ntoken=abc=\n# web ui admin / x\n")
	if err != nil || x.URL != "http://127.0.0.1:8086" || x.Org != "lab240" || x.Bucket != "donoff" || x.Token != "abc=" {
		t.Fatalf("%+v %v", x, err)
	}
	if _, err := ParseInflux("url=x\norg=y\n"); err == nil {
		t.Fatal("no bucket and token: want an error")
	}
}

// DONOFF_INFLUX_TEST=influx.creds: writes test points of the device "panel-test" and reads them back
func TestHistoryLive(t *testing.T) {
	f := os.Getenv("DONOFF_INFLUX_TEST")
	if f == "" {
		t.Skip("DONOFF_INFLUX_TEST=<influx.creds> not set")
	}
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	x, err := ParseInflux(string(b))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Minute).Add(-time.Hour)
	var lines []string
	for i := 0; i < 60; i++ { // an hour: 30 min on, 30 min off, a point every 30 s
		at := now.Add(time.Duration(i) * time.Minute)
		st := "0"
		if i < 30 {
			st = "1"
		}
		for _, s := range []time.Duration{0, 30 * time.Second} {
			lines = append(lines, point("panel-test", "relays/r1", st, at.Add(s)), point("panel-test", "sensors/temp_in", "20.5", at.Add(s)), point("panel-test", "sensors/sct013_1", "0.39", at.Add(s)))
		}
	}
	if err := x.write(joinLines(lines)); err != nil {
		t.Fatal(err)
	}
	defer x.do("/api/v2/delete", map[string][]string{"bucket": {x.Bucket}}, "application/json", "",
		`{"start":"2000-01-01T00:00:00Z","stop":"2100-01-01T00:00:00Z","predicate":"dev=\"panel-test\""}`)
	h, err := x.History("panel-test", "6h")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Relays) != 1 || len(h.Temps) != 1 || h.Temps[0].Name != "temp_in" || len(h.Others) != 1 || h.Others[0].Name != "sct013_1" {
		t.Fatalf("%+v", h)
	}
	r := h.Relays[0]
	if r.OnS < 29*60 || r.OnS > 31*60 || r.CoveredS < 59*60 || r.CoveredS > 61*60 {
		t.Fatalf("on %v s of %v s, want 1800 of 3600", r.OnS, r.CoveredS)
	}
	if len(h.Temps[0].Pts) == 0 || h.Temps[0].Pts[0][1] != 20.5 {
		t.Fatalf("temps %+v", h.Temps[0].Pts)
	}
}

func joinLines(l []string) string {
	s := ""
	for i, x := range l {
		if i > 0 {
			s += "\n"
		}
		s += x
	}
	return s
}
