package panel

import (
	"encoding/json"
	"os"
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

func TestComments(t *testing.T) {
	path := t.TempDir() + "/comments.json"
	h := New(Creds{})
	if err := h.LoadComments(path); err != nil {
		t.Fatal(err)
	}
	if err := h.SetComment("dmzk10", "  теплица у забора\r\nреле 1 — насос  "); err != nil {
		t.Fatal(err)
	}
	h.SetComment("h2", "x")
	h.SetComment("h2", " ") // removed
	h2 := New(Creds{})
	if err := h2.LoadComments(path); err != nil {
		t.Fatal(err)
	}
	if c := h2.snapshot().Comments; len(c) != 1 || c["dmzk10"] != "теплица у забора\nреле 1 — насос" {
		t.Fatalf("comments = %q", c)
	}
	if h.SetComment("a/b", "x") == nil || h.SetComment("x", strings.Repeat("я", 301)) == nil || h.SetComment("x", "1\n2\n3\n4\n5\n6\n7") == nil {
		t.Fatal("a bad id, a long or a tall comment was taken")
	}
}

func TestRenameAndOrder(t *testing.T) {
	path := t.TempDir() + "/groups.json"
	h := New(Creds{})
	h.LoadGroups(path)
	h.SetGroup("a", "Теплицы")
	h.SetGroup("b", "Теплицы")
	h.SetGroup("c", "Дом")
	if err := h.SetGroupOrder([]string{"Дом", "Теплицы", "Дом", " "}); err != nil {
		t.Fatal(err)
	}
	if err := h.RenameGroup("Теплицы", "Сад"); err != nil {
		t.Fatal(err)
	}
	h2 := New(Creds{})
	if err := h2.LoadGroups(path); err != nil {
		t.Fatal(err)
	}
	v := h2.snapshot()
	if v.Groups["a"] != "Сад" || v.Groups["b"] != "Сад" || v.Groups["c"] != "Дом" || strings.Join(v.Order, ",") != "Дом,Сад" {
		t.Fatalf("groups = %q, order = %q", v.Groups, v.Order)
	}
	h2.RenameGroup("Сад", "Дом") // merge
	if v := h2.snapshot(); v.Groups["a"] != "Дом" || strings.Join(v.Order, ",") != "Дом" {
		t.Fatalf("after merge groups = %q, order = %q", v.Groups, v.Order)
	}
	h2.SetGroup("x", "Сарай")
	h2.SetGroupOrder([]string{"Сарай", "Дом"})
	if err := h2.DeleteGroup("Дом"); err != nil {
		t.Fatal(err)
	}
	if v := h2.snapshot(); len(v.Groups) != 1 || v.Groups["x"] != "Сарай" || strings.Join(v.Order, ",") != "Сарай" {
		t.Fatalf("after delete groups = %q, order = %q", v.Groups, v.Order)
	}
	if h.RenameGroup("Дом", "") == nil || h.RenameGroup("Дом", strings.Repeat("я", 41)) == nil {
		t.Fatal("an empty or long name was taken")
	}
}

func TestPollAndParams(t *testing.T) {
	h := New(Creds{User: "donoff"})
	p := "/donoff/"
	h.onMessage(p, "/donoff/dmzk10/out/relays/r1", "0")
	h.onMessage(p, "/donoff/h2/out/relays/r1", "0")
	h.onMessage(p, "/donoff/h2/out/relays/r2", "0")
	h.onMessage(p, "/donoff/t14/out/sensors/pzem_power", "680")
	if got := strings.Join(h.pollTargets(), ","); got != "dmzk10,h2,t14" {
		t.Fatalf("targets: %s", got)
	}
	for id, want := range map[string]string{
		"t14":    "sh time",
		"dmzk10": "sh time,sh hotter,sh cooler,sh tls,sh lschm,sh aofs",
		"h2":     "sh time,sh hotter,sh cooler,sh tls,sh lschm,sh aofs,sh lschm2,sh aofs2",
	} {
		if got := strings.Join(h.pollCmds(id), ","); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
	// the poll answers go to the params and stay out of the feed; other messages go to the feed
	h.mu.Lock()
	h.dev["h2"].autoUntil = time.Now().Add(time.Second)
	h.mu.Unlock()
	for _, m := range []string{"hotter=0", "lschm1=99", "lschm2=100", "aofs=30", "a2ofs=0", "I:S=1,T=17:5:2026", "something else"} {
		h.onMessage(p, "/donoff/h2/out/info", m)
	}
	h.mu.Lock()
	h.dev["h2"].lastAsk = "sh cschm1"
	h.mu.Unlock()
	h.onMessage(p, "/donoff/h2/out/info", "0000 0000 0011 1111 1111 1100")
	h.mu.Lock()
	h.dev["h2"].lastAsk = "sh tls"
	h.mu.Unlock()
	h.onMessage(p, "/donoff/h2/out/info", "1111 1111 1111 1111 1111 1111") // not after sh cschmN: not a scheme
	var d Device
	for _, x := range h.snapshot().Devices {
		if x.ID == "h2" {
			d = x
		}
	}
	for k, want := range map[string]string{"hotter": "hotter=0", "lschm1": "lschm1=99", "lschm2": "lschm2=100", "aofs1": "aofs=30", "aofs2": "a2ofs=0", "time": "I:S=1,T=17:5:2026", "cschm1": "0000 0000 0011 1111 1111 1100"} {
		if d.Params[k].V != want {
			t.Errorf("param %s = %q, want %q", k, d.Params[k].V, want)
		}
	}
	if len(d.Info) != 2 || d.Info[0].Text != "something else" {
		t.Fatalf("feed: %+v", d.Info)
	}
	if got := strings.Join(h.pollCmds("h2"), ","); !strings.HasSuffix(got, "sh lschm2,sh aofs2,sh cschm1,sh cschm2") {
		t.Fatalf("with the custom schemes in use: %s", got)
	}
}

func TestSaveLoadDevices(t *testing.T) {
	h := New(Creds{User: "donoff"})
	h.onMessage("/donoff/", "/donoff/t14/out/sensors/pzem_power", "680.70")
	h.onMessage("/donoff/", "/donoff/h2/out/info", "lschm2=1")
	f := t.TempDir() + "/devices.json"
	if err := h.SaveDevices(f); err != nil {
		t.Fatal(err)
	}
	g := New(Creds{User: "donoff"})
	if err := g.LoadDevices(f); err != nil {
		t.Fatal(err)
	}
	v := g.snapshot()
	if len(v.Devices) != 2 || v.Devices[1].Topics["sensors/pzem_power"].V != "680.70" || v.Devices[0].Params["lschm2"].V != "lschm2=1" || v.Devices[1].LastSeen.IsZero() {
		t.Fatalf("%+v", v.Devices)
	}
	g.onMessage("/donoff/", "/donoff/t14/out/sensors/pzem_power", "1") // a loaded device takes new messages
	if g.snapshot().Devices[1].Topics["sensors/pzem_power"].V != "1" {
		t.Fatal("no update after load")
	}
	if err := New(Creds{}).LoadDevices(t.TempDir() + "/none.json"); err != nil {
		t.Fatal("a missing file is no devices:", err)
	}
}

func TestCommandJournal(t *testing.T) {
	h := New(Creds{Host: "h", Port: 1883, User: "donoff"})
	path := t.TempDir() + "/cmds.jsonl"
	h.SetCmdLog(path)
	p := "/donoff/"
	h.onMessage(p, "/donoff/d1/out/time_up", "1")
	now := time.Now()
	h.mu.Lock()
	d := h.device("d1")
	d.sent = []sentCmd{{"sh hotter", now}, {"r11", now}, {"r10", now.Add(-time.Minute)}} // the poll, a button, a lost one
	h.mu.Unlock()
	h.onCommand(p, "/donoff/d1/in/params", "sh hotter") // our echoes: nothing new
	h.onCommand(p, "/donoff/d1/in/params", "r11")
	h.onCommand(p, "/donoff/d1/in/params", "r11")      // the same text again: this one is not ours
	h.onCommand(p, "/donoff/d1/in/params", "r10")      // our r10 is too old to be its echo
	h.onCommand(p, "/donoff/d1/in/params", "sh lschm") // somebody else asks
	h.onCommand(p, "/donoff/d1/in/b1", "0")            // straight to a relay topic
	h.onCommand(p, "/donoff/a/b/in/params", "x")       // not a device
	h.onCommand(p, "/other/d1/in/params", "x")         // other user
	h.mu.Lock()
	h.journal(h.device("d1"), "d1", "sh time", byPanel, now) // by hand from the page: file only
	h.journal(h.device("d1"), "d1", "ctempl=2600", byPanel, now)
	h.mu.Unlock()
	var got []string
	for _, m := range h.snapshot().Devices[0].Cmds {
		got = append(got, m.By+":"+m.Text)
	}
	want := "other:r11 other:r10 other:sh lschm other:in/b1 0 panel:ctempl=2600"
	if strings.Join(got, " ") != want {
		t.Fatalf("card: %q", got)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.Count(string(b), "\n") != 6 || !strings.Contains(string(b), `"cmd":"sh time","by":"panel"`) {
		t.Fatalf("file: %v\n%s", err, b)
	}
}
