package main

// The history in InfluxDB 2: one point of every input, the ATS and every outlet each 10 s
// (rpcm_input, rpcm_ats, rpcm_outlet; tags dev and n). The charts read it back as means over a
// step that keeps a line under 400 points; one query at a time, the answers kept 15 s.

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

type Influx struct {
	url, org, bucket, token, dev string
	cli                          *http.Client

	cmu   sync.Mutex
	cache map[string]cached
}

// one query at a time for all the devices: the board is small
var influxQ sync.Mutex

type cached struct {
	at time.Time
	h  *History
}

func newInflux(u, org, bucket, token, dev string) *Influx {
	return &Influx{url: strings.TrimRight(u, "/"), org: org, bucket: bucket, token: token, dev: dev,
		cli: &http.Client{Timeout: 20 * time.Second}, cache: map[string]cached{}}
}

func (x *Influx) do(path string, q url.Values, ctype, accept, body string) ([]byte, error) {
	q.Set("org", x.org)
	req, err := http.NewRequest("POST", x.url+path+"?"+q.Encode(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+x.token)
	req.Header.Set("Content-Type", ctype)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	r, err := x.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if r.StatusCode/100 != 2 {
		return nil, fmt.Errorf("influx %s: %s %s", path, r.Status, bytes.TrimSpace(b))
	}
	return b, nil
}

func f(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// run writes the points every 10 s while the RPCM answers; a failed write keeps up to 30 min of them.
func (x *Influx) run(p *poller) {
	var buf []string
	failing := false
	for range time.Tick(10 * time.Second) {
		s := p.snapshot()
		if s.OK && time.Since(s.At) < 10*time.Second {
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
			h := map[string]bool{}
			if s.Profile != nil {
				h = s.Profile.Has
			}
			// only what the model has: a missing value is not written as 0
			fields := func(kv ...any) string {
				var b []string
				for i := 0; i+2 < len(kv)+1; i += 3 {
					if h[kv[i].(string)] {
						b = append(b, kv[i+1].(string)+"="+f(kv[i+2].(float64)))
					}
				}
				return strings.Join(b, ",")
			}
			add := func(meas, tags, fs string) {
				if fs != "" {
					buf = append(buf, meas+",dev="+x.dev+tags+" "+fs+" "+ts)
				}
			}
			a := s.ATS
			add("rpcm_ats", "", fields("ats.active", "active", float64(a.Active), "ats.priority", "priority", float64(a.Priority), "ats.ground", "ground", float64(a.Ground)))
			for _, in := range s.Inputs {
				add("rpcm_input", fmt.Sprintf(",n=%d", in.N), fields("input.v", "v", in.V, "input.hz", "hz", in.Hz, "input.ma", "ma", in.MA,
					"input.w", "w", in.W, "input.var", "var", in.Var, "input.kwh", "kwh", in.KWh, "input.present", "present", float64(in.Present)))
			}
			if time.Since(s.OutAt) < 15*time.Second {
				for _, o := range s.Outlets {
					add("rpcm_outlet", fmt.Sprintf(",n=%d", o.N), fields("outlet.on", "on", float64(o.On), "outlet.ma", "ma", o.MA, "outlet.w", "w", o.W,
						"outlet.va", "va", o.VA, "outlet.var", "var", o.Var, "outlet.kwh", "kwh", o.KWh))
				}
			}
			// the sensors with a number (not a state) and a value
			if time.Since(s.SensAt) < 15*time.Second && s.Profile != nil {
				for _, x := range s.Sensors {
					if x.Valid && s.Profile.Sensors[x.G].Value.Map == nil {
						add("rpcm_sensor", fmt.Sprintf(",g=%d,n=%d,v=%d", x.G, x.N, x.V), "value="+f(x.Value))
					}
				}
			}
		}
		if len(buf) == 0 {
			continue
		}
		_, err := x.do("/api/v2/write", url.Values{"bucket": {x.bucket}, "precision": {"ms"}}, "text/plain; charset=utf-8", "", strings.Join(buf, "\n"))
		if err == nil {
			if failing {
				log.Printf("influx: writing again")
			}
			failing, buf = false, buf[:0]
			continue
		}
		if !failing {
			log.Printf("influx: %v (keeping the points)", err)
		}
		failing = true
		if len(buf) > 2500 {
			buf = append([]string(nil), buf[len(buf)-2500:]...)
		}
	}
}

var ranges = map[string]struct{ d, step time.Duration }{
	"15m": {15 * time.Minute, 10 * time.Second},
	"30m": {30 * time.Minute, 10 * time.Second},
	"1h":  {time.Hour, 30 * time.Second},
	"6h":  {6 * time.Hour, 2 * time.Minute},
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
}

// the small charts on the tiles: 24 h in 15 min steps (96 points), all of them in one query
var sparkRange = struct{ d, step time.Duration }{24 * time.Hour, 15 * time.Minute}

type Series struct {
	Key string       `json:"key"` // "in1.v", "ats.active", "out7.w"
	Pts [][2]float64 `json:"pts"` // [unix seconds (the end of the step), mean]
}

type History struct {
	Range  string   `json:"range"`
	From   int64    `json:"from"`
	To     int64    `json:"to"`
	Step   float64  `json:"step"`
	Series []Series `json:"series"`
}

func dur(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/time.Second)) + "s"
}

// history: what=ats (the inputs' voltage and power, the active input), what=outlet&n=7, or
// what=spark (the inputs' voltage and the outlets' current for the tiles, 24 h).
func (x *Influx) history(what, n, rng string) (*History, error) {
	r, ok := ranges[rng]
	keep := 15 * time.Second
	if what == "spark" {
		r, ok, rng, n, keep = sparkRange, true, "24h", "", time.Minute
	}
	if !ok {
		return nil, errors.New("range: 15m, 30m, 1h, 6h, 24h or 7d")
	}
	var filter string
	switch what {
	case "spark":
		filter = `(r._measurement == "rpcm_input" and r._field == "v") or (r._measurement == "rpcm_outlet" and r._field == "ma") or r._measurement == "rpcm_sensor"`
	case "ats":
		filter = `(r._measurement == "rpcm_input" and (r._field == "v" or r._field == "hz" or r._field == "w")) or (r._measurement == "rpcm_ats" and r._field == "active")`
	case "outlet":
		k, err := strconv.Atoi(n)
		if err != nil || k < 0 || k > 31 {
			return nil, errors.New("bad outlet number")
		}
		// the outlet, and the inputs' voltage and frequency with the active input: the outlet gets
		// what the active input gives (the RPCM measures no voltage on the outlets)
		filter = fmt.Sprintf(`(r._measurement == "rpcm_outlet" and r.n == "%d" and (r._field == "w" or r._field == "ma" or r._field == "va" or r._field == "var"))
  or (r._measurement == "rpcm_input" and (r._field == "v" or r._field == "hz")) or (r._measurement == "rpcm_ats" and r._field == "active")`, k)
	default:
		return nil, errors.New("what: ats or outlet")
	}
	key := what + "/" + n + "/" + rng
	x.cmu.Lock()
	if c, ok := x.cache[key]; ok && time.Since(c.at) < keep {
		x.cmu.Unlock()
		return c.h, nil
	}
	x.cmu.Unlock()

	influxQ.Lock()
	defer influxQ.Unlock()
	to := time.Now().UTC()
	from := to.Add(-r.d)
	flux := fmt.Sprintf(`from(bucket: %q) |> range(start: %s, stop: %s) |> filter(fn: (r) => r.dev == %q and (%s))
  |> aggregateWindow(every: %s, fn: mean, createEmpty: false) |> keep(columns: ["_time", "_value", "_measurement", "_field", "n", "g", "v"])`,
		x.bucket, from.Format(time.RFC3339), to.Format(time.RFC3339), x.dev, filter, dur(r.step))
	b, err := x.do("/api/v2/query", url.Values{}, "application/vnd.flux", "application/csv", flux)
	if err != nil {
		return nil, err
	}
	cr := csv.NewReader(bytes.NewReader(b))
	cr.FieldsPerRecord = -1
	var head []string
	idx := map[string]int{}
	ser := map[string]*Series{}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) > 1 && rec[1] == "result" {
			head = rec
			idx = map[string]int{} // a table with other columns has its own header
			for i, k := range head {
				idx[k] = i
			}
			continue
		}
		if head == nil || len(rec) != len(head) {
			continue
		}
		v, err := strconv.ParseFloat(rec[idx["_value"]], 64)
		if err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, rec[idx["_time"]])
		if err != nil {
			continue
		}
		k := ""
		switch rec[idx["_measurement"]] {
		case "rpcm_input":
			k = "in" + rec[idx["n"]] + "." + rec[idx["_field"]]
		case "rpcm_ats":
			k = "ats." + rec[idx["_field"]]
		case "rpcm_outlet":
			k = "out" + rec[idx["n"]] + "." + rec[idx["_field"]]
		case "rpcm_sensor":
			if gi, ok := idx["g"]; ok && idx["v"] > 0 {
				k = "s" + rec[gi] + "." + rec[idx["n"]] + "." + rec[idx["v"]]
			}
		}
		if k == "" {
			continue
		}
		if ser[k] == nil {
			ser[k] = &Series{Key: k}
		}
		ser[k].Pts = append(ser[k].Pts, [2]float64{float64(t.Unix()), v})
	}
	h := &History{Range: rng, From: from.Unix(), To: to.Unix(), Step: r.step.Seconds(), Series: []Series{}}
	for _, s := range ser {
		sort.Slice(s.Pts, func(i, j int) bool { return s.Pts[i][0] < s.Pts[j][0] })
		h.Series = append(h.Series, *s)
	}
	sort.Slice(h.Series, func(i, j int) bool { return h.Series[i].Key < h.Series[j].Key })
	x.cmu.Lock()
	x.cache[key] = cached{at: time.Now(), h: h}
	for k, c := range x.cache { // drop the old answers
		if time.Since(c.at) > 2*time.Minute {
			delete(x.cache, k)
		}
	}
	x.cmu.Unlock()
	return h, nil
}
