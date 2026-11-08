package panel

// The history: every sensor value and relay state that comes from the broker goes to InfluxDB 2
// ("sensor,dev=dmzk10,name=temp_in value=12.5" and "relay,dev=dmzk10,name=r1 state=1"), and
// GET /api/v1/history?dev=dmzk10&range=24h reads it back for the charts and the relay on-time.

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Influx is the InfluxDB 2 server; the file has the lines url=, org=, bucket=, token=.
type Influx struct {
	URL, Org, Bucket, Token string

	cli  *http.Client
	ch   chan string
	once sync.Once
}

func ParseInflux(text string) (*Influx, error) {
	x := &Influx{}
	for _, s := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(s), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		switch strings.TrimSpace(k) {
		case "url":
			x.URL = strings.TrimRight(strings.TrimSpace(v), "/")
		case "org":
			x.Org = strings.TrimSpace(v)
		case "bucket":
			x.Bucket = strings.TrimSpace(v)
		case "token":
			x.Token = strings.TrimSpace(v)
		}
	}
	if x.URL == "" || x.Org == "" || x.Bucket == "" || x.Token == "" {
		return nil, errors.New("want the lines url=, org=, bucket=, token=")
	}
	return x, nil
}

func (x *Influx) init() {
	x.once.Do(func() {
		x.cli = &http.Client{Timeout: 20 * time.Second}
		x.ch = make(chan string, 20000)
	})
}

// add queues one line of the line protocol; a full queue drops it (the server is down for long).
func (x *Influx) add(line string) {
	x.init()
	select {
	case x.ch <- line:
	default:
	}
}

// Run writes the queue every 5 s until stop is closed; a failed batch is kept and tried again.
func (x *Influx) Run(stop <-chan struct{}) {
	x.init()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var buf []string
	failing := false
	for {
		done := false
		select {
		case l := <-x.ch:
			buf = append(buf, l)
			if len(buf) < 5000 {
				continue
			}
		case <-tick.C:
		case <-stop:
			done = true
		}
		if len(buf) > 0 {
			err := x.write(strings.Join(buf, "\n"))
			switch {
			case err == nil:
				if failing {
					log.Printf("influx: writing again")
				}
				failing, buf = false, buf[:0]
			case len(buf) > 50000:
				buf = buf[len(buf)-50000:]
				fallthrough
			default:
				if !failing {
					log.Printf("influx: %v (keep the points, try again)", err)
				}
				failing = true
			}
		}
		if done {
			return
		}
	}
}

func (x *Influx) do(path string, q url.Values, ctype, accept, body string) ([]byte, error) {
	x.init()
	q.Set("org", x.Org)
	req, err := http.NewRequest("POST", x.URL+path+"?"+q.Encode(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+x.Token)
	req.Header.Set("Content-Type", ctype)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	r, err := x.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if r.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: %s %s", path, r.Status, bytes.TrimSpace(b))
	}
	return b, nil
}

func (x *Influx) write(lines string) error {
	_, err := x.do("/api/v2/write", url.Values{"bucket": {x.Bucket}, "precision": {"ms"}}, "text/plain; charset=utf-8", "", lines)
	return err
}

// query runs Flux and gives the rows as maps column -> value (every table of every result).
func (x *Influx) query(flux string) ([]map[string]string, error) {
	b, err := x.do("/api/v2/query", url.Values{}, "application/vnd.flux", "application/csv", flux)
	if err != nil {
		return nil, err
	}
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	var head []string
	var rows []map[string]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		if len(rec) > 1 && rec[1] == "result" {
			head = rec
			continue
		}
		if head == nil || len(rec) != len(head) {
			continue
		}
		m := make(map[string]string, len(rec))
		for i, k := range head {
			m[k] = rec[i]
		}
		rows = append(rows, m)
	}
}

var tagEsc = strings.NewReplacer(",", `\,`, " ", `\ `, "=", `\=`)

// point is the line for one topic of a device, or "" when the topic is not a sensor or a relay:
// "sensors/temp_in" (or the old bare "temp_in") 12.50 -> a sensor, "relays/r1" (or "b1") 1 -> a relay.
func point(dev, sub, payload string, at time.Time) string {
	payload = strings.TrimSpace(payload)
	ts := strconv.FormatInt(at.UnixMilli(), 10)
	if relayTopic.MatchString(sub) {
		v, err := strconv.Atoi(payload)
		if err != nil || (v != 0 && v != 1) {
			return ""
		}
		return fmt.Sprintf("relay,dev=%s,name=%s state=%d %s", tagEsc.Replace(dev), strings.TrimPrefix(sub, "relays/"), v, ts)
	}
	name, ok := strings.CutPrefix(sub, "sensors/")
	if !ok && !strings.HasPrefix(sub, "temp_") {
		return ""
	}
	if name == "" || strings.Contains(name, "/") || strings.HasSuffix(name, "_json") {
		return ""
	}
	v, err := strconv.ParseFloat(payload, 64)
	if err != nil || v != v || v > 1e15 || v < -1e15 {
		return ""
	}
	return fmt.Sprintf("sensor,dev=%s,name=%s value=%s %s", tagEsc.Replace(dev), tagEsc.Replace(name), strconv.FormatFloat(v, 'f', -1, 64), ts)
}

// the ranges of the page and the step of their charts
var ranges = map[string]struct{ d, step time.Duration }{
	"15m": {15 * time.Minute, 10 * time.Second},
	"1h":  {time.Hour, 30 * time.Second},
	"6h":  {6 * time.Hour, 2 * time.Minute},
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
	"30d": {30 * 24 * time.Hour, 2 * time.Hour},
}

// onStep: the relay on-time is the sum of the minute means of the state (a minute without data is
// not counted, neither as on nor as off)
const onStep = time.Minute

type Series struct {
	Name string       `json:"name"`
	Pts  [][2]float64 `json:"pts"` // [unix seconds (the end of the step), mean]
}

type RelayHist struct {
	Series
	OnS      float64 `json:"on_s"`      // seconds on
	CoveredS float64 `json:"covered_s"` // seconds with data
}

type History struct {
	Dev    string      `json:"dev"`
	Range  string      `json:"range"`
	From   time.Time   `json:"from"`
	To     time.Time   `json:"to"`
	Step   float64     `json:"step"` // seconds
	Temps  []Series    `json:"temps"`  // temp_*
	Others []Series    `json:"others"` // the other sensors: sct013_1, current, pzem_power, ...
	Relays []RelayHist `json:"relays"`
}

func dur(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/time.Second)) + "s"
}

func (x *Influx) History(dev, rng string) (*History, error) {
	r, ok := ranges[rng]
	if !ok {
		return nil, errors.New("range: 15m, 1h, 6h, 24h, 7d or 30d")
	}
	if !devRe.MatchString(dev) {
		return nil, errors.New("bad device id")
	}
	to := time.Now().UTC()
	h := &History{Dev: dev, Range: rng, From: to.Add(-r.d), To: to, Step: r.step.Seconds(), Temps: []Series{}, Others: []Series{}, Relays: []RelayHist{}}
	base := fmt.Sprintf(`from(bucket: %q) |> range(start: %s, stop: %s) |> filter(fn: (r) => r.dev == %q)`,
		x.Bucket, h.From.Format(time.RFC3339), to.Format(time.RFC3339), dev)
	flux := base + ` |> filter(fn: (r) => r._measurement == "sensor" or r._measurement == "relay")
  |> aggregateWindow(every: ` + dur(r.step) + `, fn: mean, createEmpty: false)
  |> keep(columns: ["_time", "_value", "_measurement", "name"]) |> yield(name: "chart")
on = ` + base + ` |> filter(fn: (r) => r._measurement == "relay")
  |> aggregateWindow(every: ` + dur(onStep) + `, fn: mean, createEmpty: false) |> keep(columns: ["_value", "name"])
on |> sum() |> yield(name: "on")
on |> count() |> yield(name: "count")`
	rows, err := x.query(flux)
	if err != nil {
		return nil, err
	}
	temps, relays := map[string]*Series{}, map[string]*RelayHist{}
	relay := func(n string) *RelayHist {
		if relays[n] == nil {
			relays[n] = &RelayHist{Series: Series{Name: n, Pts: [][2]float64{}}}
		}
		return relays[n]
	}
	for _, m := range rows {
		v, err := strconv.ParseFloat(m["_value"], 64)
		if err != nil {
			continue
		}
		n := m["name"]
		switch m["result"] {
		case "chart":
			t, err := time.Parse(time.RFC3339Nano, m["_time"])
			if err != nil {
				continue
			}
			p := [2]float64{float64(t.Unix()), v}
			if m["_measurement"] == "relay" {
				relay(n).Pts = append(relay(n).Pts, p)
			} else {
				if temps[n] == nil {
					temps[n] = &Series{Name: n, Pts: [][2]float64{}}
				}
				temps[n].Pts = append(temps[n].Pts, p)
			}
		case "on":
			relay(n).OnS = v * onStep.Seconds()
		case "count":
			relay(n).CoveredS = v * onStep.Seconds()
		}
	}
	for _, s := range temps {
		sort.Slice(s.Pts, func(i, j int) bool { return s.Pts[i][0] < s.Pts[j][0] })
		if strings.HasPrefix(s.Name, "temp") {
			h.Temps = append(h.Temps, *s)
		} else {
			h.Others = append(h.Others, *s)
		}
	}
	sort.Slice(h.Others, func(i, j int) bool { return h.Others[i].Name < h.Others[j].Name })
	for _, s := range relays {
		// the first and the last minute are cut by the range but counted whole: not more than the range
		s.CoveredS = min(s.CoveredS, r.d.Seconds())
		s.OnS = min(s.OnS, s.CoveredS)
		sort.Slice(s.Pts, func(i, j int) bool { return s.Pts[i][0] < s.Pts[j][0] })
		h.Relays = append(h.Relays, *s)
	}
	sort.Slice(h.Temps, func(i, j int) bool { return tempOrder(h.Temps[i].Name) < tempOrder(h.Temps[j].Name) })
	sort.Slice(h.Relays, func(i, j int) bool { return h.Relays[i].Name < h.Relays[j].Name })
	return h, nil
}

// temp_in first, then temp_out, then the rest by name (the page gives the colors in this order)
func tempOrder(n string) string {
	switch n {
	case "temp_in":
		return "0"
	case "temp_out":
		return "1"
	}
	return "2" + n
}

// Seed adds the devices that are in InfluxDB for the last 30 days but unknown to the hub, with
// their last values, so that a device silent since the start of the panel is still on the page.
func (h *Hub) Seed(x *Influx) error {
	rows, err := x.query(fmt.Sprintf(`from(bucket: %q) |> range(start: -30d) |> filter(fn: (r) => r._measurement == "sensor" or r._measurement == "relay")
  |> last() |> keep(columns: ["_time", "_value", "_measurement", "dev", "name"])`, x.Bucket))
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	known := map[string]bool{}
	for id := range h.dev {
		known[id] = true
	}
	for _, m := range rows {
		id, n := m["dev"], m["name"]
		if known[id] || !devRe.MatchString(id) || n == "" || id == "panel-test" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, m["_time"])
		if err != nil {
			continue
		}
		v := m["_value"]
		topic := "sensors/" + n
		if m["_measurement"] == "relay" {
			topic = "relays/" + n
			if !strings.HasPrefix(n, "r") { // the old bare bN
				topic = n
			}
		} else if f, err := strconv.ParseFloat(v, 64); err == nil {
			v = strconv.FormatFloat(f, 'f', 2, 64)
		}
		d := h.device(id)
		d.Topics[topic] = Value{V: v, At: t}
		if t.After(d.LastSeen) {
			d.LastSeen = t
		}
	}
	return nil
}
