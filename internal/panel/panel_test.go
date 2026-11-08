package panel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseCreds(t *testing.T) {
	c, err := ParseCreds("mqtt.example.org\nuser\npass\n")
	if err != nil || c.Host != "mqtt.example.org" || c.Port != 1883 || c.User != "user" || c.Password != "pass" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, _ := ParseCreds("h:1884\nu\np"); c.Port != 1884 || c.Host != "h" {
		t.Fatalf("host:port %+v", c)
	}
	if c, _ := ParseCreds("h\nu\np\n8883"); c.Port != 8883 {
		t.Fatalf("4th line port %+v", c)
	}
	if _, err := ParseCreds("h\nu"); err == nil {
		t.Fatal("2 lines must be refused")
	}
}

func TestOnMessage(t *testing.T) {
	h := New(Creds{Host: "h", Port: 1883, User: "donoff"})
	p := "/donoff/"
	h.onMessage(p, "/donoff/dmzk10/out/sensors/temp_in", "12.50")
	h.onMessage(p, "/donoff/dmzk10/out/relays/r1", "0")
	h.onMessage(p, "/donoff/dmzk10/out/info", "ctempl=2600")
	h.onMessage(p, "/donoff/dmzk13/out/temp_in", "12.81")
	h.onMessage(p, "/donoff/r1/in/b1", "0")     // not /out/: ignored
	h.onMessage(p, "/other/x/out/time_up", "1") // other user: ignored
	s := h.snapshot()
	if len(s.Devices) != 2 || s.Devices[0].ID != "dmzk10" || s.Devices[1].ID != "dmzk13" {
		t.Fatalf("devices: %+v", s.Devices)
	}
	d := s.Devices[0]
	if d.Topics["sensors/temp_in"].V != "12.50" || d.Topics["relays/r1"].V != "0" || len(d.Info) != 1 {
		t.Fatalf("dmzk10: %+v", d)
	}
}

func TestSendChecks(t *testing.T) {
	h := New(Creds{Host: "h", Port: 1883, User: "donoff"})
	for _, c := range []struct{ dev, cmd string }{{"a/b", "r11"}, {"x", ""}, {"x", "a\nb"}, {"x", "r11"}} {
		if h.Send(c.dev, c.cmd) == nil {
			t.Errorf("%+v must be refused (bad input or no broker)", c)
		}
	}
}

func TestUnknownDeviceHidden(t *testing.T) {
	h := New(Creds{Host: "h", Port: 1883, User: "donoff"})
	h.mu.Lock()
	h.device("panel-test") // what Send does for a device that never published
	h.mu.Unlock()
	if n := len(h.snapshot().Devices); n != 0 {
		t.Fatalf("a device that never spoke is listed: %d", n)
	}
}

func TestSnapshotJSONHasNoNulls(t *testing.T) {
	h := New(Creds{Host: "h", Port: 1883, User: "donoff"})
	h.onMessage("/donoff/", "/donoff/d1/out/time_up", "1")
	b, _ := json.Marshal(h.snapshot())
	if strings.Contains(string(b), "null") {
		t.Fatalf("null in %s", b)
	}
}

func TestGroups(t *testing.T) {
	path := t.TempDir() + "/g.json"
	h := New(Creds{User: "donoff"})
	if err := h.LoadGroups(path); err != nil {
		t.Fatal(err)
	}
	if err := h.SetGroup("dmzk10", " Теплицы "); err != nil {
		t.Fatal(err)
	}
	h.SetGroup("h2", "Дом")
	h.SetGroup("h2", "") // out of the group
	h2 := New(Creds{User: "donoff"})
	if err := h2.LoadGroups(path); err != nil {
		t.Fatal(err)
	}
	if g := h2.snapshot().Groups; len(g) != 1 || g["dmzk10"] != "Теплицы" {
		t.Fatalf("groups: %v", g)
	}
	if h.SetGroup("a/b", "x") == nil || h.SetGroup("x", "a\nb") == nil {
		t.Fatal("bad input must be refused")
	}
}

func TestPollTargetsAndAutoAnswers(t *testing.T) {
	h := New(Creds{User: "donoff"})
	p := "/donoff/"
	h.onMessage(p, "/donoff/dmzk10/out/relays/r1", "0")
	h.onMessage(p, "/donoff/dmzk13/out/b1", "0")
	h.onMessage(p, "/donoff/t14/out/sensors/pzem_power", "680") // no relay: not asked
	if got := strings.Join(h.pollTargets(), ","); got != "dmzk10,dmzk13" {
		t.Fatalf("targets: %s", got)
	}
	h.mu.Lock()
	h.dev["dmzk10"].autoUntil = time.Now().Add(time.Second)
	h.mu.Unlock()
	h.onMessage(p, "/donoff/dmzk10/out/info", "hotter=1")
	h.onMessage(p, "/donoff/dmzk10/out/info", "something else")
	info := h.snapshot().Devices[0].Info
	if !info[0].Auto || info[1].Auto {
		t.Fatalf("auto flags: %+v", info)
	}
}
