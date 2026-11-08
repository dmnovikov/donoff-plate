// Package panel is a web panel for the donoff devices on one MQTT broker (donoff-lib topics):
// it listens to "/<user>/+/out/#", keeps the last value of every topic per device, and sends
// commands to "/<user>/<dev_id>/in/params".
//
//	GET  /                 the page
//	GET  /api/v1/state     broker state and every device with its topics, answers (/out/info) and log
//	POST /api/v1/cmd       {"dev": "dmzk10", "cmd": "r11"} -> /<user>/dmzk10/in/params
//	GET  /history          ?dev=dmzk10: the page with the charts
//	GET  /api/v1/history   ?dev=dmzk10&range=15m|1h|6h|24h|7d|30d: temperatures and relays from InfluxDB (with --influx)
//	PUT  /api/v1/group     {"dev": "dmzk10", "group": "Теплицы"} ("" = no group); kept in the groups file
package panel

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

//go:embed web/index.html
var page []byte

//go:embed web/history.html
var historyPage []byte

const (
	keepMsgs = 30                      // answers and log lines kept per device
	cmdGap   = 1100 * time.Millisecond // donoff-lib drops commands closer than 1 s
)

// Creds is the broker: the file has the lines host, user, password and an optional port.
type Creds struct {
	Host     string
	Port     int
	User     string
	Password string
}

func ParseCreds(text string) (Creds, error) {
	var l []string
	for _, s := range strings.Split(text, "\n") {
		if s = strings.TrimSpace(s); s != "" {
			l = append(l, s)
		}
	}
	if len(l) < 3 {
		return Creds{}, errors.New("want the lines: host, user, password [, port]")
	}
	c := Creds{Host: l[0], User: l[1], Password: l[2], Port: 1883}
	if h, p, err := net.SplitHostPort(l[0]); err == nil {
		c.Host = h
		c.Port, _ = strconv.Atoi(p)
	}
	if len(l) > 3 {
		p, err := strconv.Atoi(l[3])
		if err != nil {
			return Creds{}, fmt.Errorf("port %q: %w", l[3], err)
		}
		c.Port = p
	}
	return c, nil
}

type Value struct {
	V  string    `json:"v"`
	At time.Time `json:"at"`
}

type Msg struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	Out  bool      `json:"out,omitempty"`  // a command we sent
	Auto bool      `json:"auto,omitempty"` // an answer to the periodic poll (the page hides it in the feed)
}

type Device struct {
	ID        string           `json:"id"`
	LastSeen  time.Time        `json:"last_seen"`
	Topics    map[string]Value `json:"topics"` // after "/out/": "sensors/temp_in", "relays/r1", "time_up", ...
	Info      []Msg            `json:"info"`   // /out/info answers and the commands sent, newest last
	Log       []Msg            `json:"log"`    // /out/log
	lastCmd   time.Time
	autoUntil time.Time // answers before it belong to the periodic poll
}

type BrokerState struct {
	Server    string    `json:"server"`
	User      string    `json:"user"`
	Connected bool      `json:"connected"`
	Error     string    `json:"error,omitempty"`
	Since     time.Time `json:"since"`
}

type Hub struct {
	creds Creds
	cli   paho.Client

	mu         sync.Mutex
	dev        map[string]*Device
	st         BrokerState
	groups     map[string]string // dev_id -> group
	groupsPath string
	influx     *Influx // nil = no history
}

func New(c Creds) *Hub {
	return &Hub{creds: c, dev: map[string]*Device{}, groups: map[string]string{},
		st: BrokerState{Server: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), User: c.User, Since: time.Now()}}
}

// LoadGroups reads the groups file ({"dmzk10": "Теплицы", ...}); a missing file is no groups.
func (h *Hub) LoadGroups(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.groupsPath = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	g := map[string]string{}
	if err := json.Unmarshal(b, &g); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	h.groups = g
	return nil
}

// SetGroup puts a device into a group ("" = out of any group) and saves the file.
func (h *Hub) SetGroup(dev, group string) error {
	group = strings.TrimSpace(group)
	switch {
	case !devRe.MatchString(dev):
		return errors.New("bad device id")
	case len([]rune(group)) > 40 || strings.ContainsAny(group, "\r\n"):
		return errors.New("a group name is one line, up to 40 characters")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if group == "" {
		delete(h.groups, dev)
	} else {
		h.groups[dev] = group
	}
	if h.groupsPath == "" {
		return nil
	}
	b, _ := json.MarshalIndent(h.groups, "", "  ")
	tmp := h.groupsPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, h.groupsPath)
}

// SetInflux makes every sensor value and relay state go to InfluxDB too (call before Start).
func (h *Hub) SetInflux(x *Influx) { h.influx = x }

func (h *Hub) state(connected bool, err string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.st.Connected != connected || h.st.Error != err {
		h.st.Since = time.Now()
		if err != "" {
			log.Printf("mqtt: %s", err)
		}
	}
	h.st.Connected, h.st.Error = connected, err
}

// Start connects (and reconnects) in the background.
func (h *Hub) Start() {
	id := make([]byte, 3)
	rand.Read(id)
	prefix := "/" + h.creds.User + "/"
	o := paho.NewClientOptions().
		AddBroker("tcp://" + net.JoinHostPort(h.creds.Host, strconv.Itoa(h.creds.Port))).
		SetClientID("donoff-panel-" + hex.EncodeToString(id)).
		SetUsername(h.creds.User).SetPassword(h.creds.Password).
		SetCleanSession(true).SetKeepAlive(30 * time.Second).SetConnectTimeout(10 * time.Second).
		SetAutoReconnect(true).SetMaxReconnectInterval(30 * time.Second).
		SetOnConnectHandler(func(cl paho.Client) {
			h.state(true, "")
			log.Printf("mqtt: connected to %s as %s, listening %s+/out/#", h.st.Server, h.creds.User, prefix)
			cl.Subscribe(prefix+"+/out/#", 0, func(_ paho.Client, m paho.Message) { h.onMessage(prefix, m.Topic(), string(m.Payload())) })
		}).
		SetConnectionLostHandler(func(_ paho.Client, err error) { h.state(false, "connection lost: "+err.Error()) })
	h.cli = paho.NewClient(o)
	go func() {
		for {
			t := h.cli.Connect()
			t.Wait()
			if t.Error() == nil {
				return
			}
			h.state(false, t.Error().Error()+"; retry in 10 s")
			time.Sleep(10 * time.Second)
		}
	}()
}

func (h *Hub) Stop() {
	if h.cli != nil {
		h.cli.Disconnect(300)
	}
}

func (h *Hub) device(id string) *Device {
	d := h.dev[id]
	if d == nil {
		d = &Device{ID: id, Topics: map[string]Value{}}
		h.dev[id] = d
	}
	return d
}

func push(l []Msg, m Msg) []Msg {
	l = append(l, m)
	if len(l) > keepMsgs {
		l = append(l[:0:0], l[len(l)-keepMsgs:]...)
	}
	return l
}

func (h *Hub) onMessage(prefix, topic, payload string) {
	rest, ok := strings.CutPrefix(topic, prefix)
	if !ok {
		return
	}
	id, sub, ok := strings.Cut(rest, "/out/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.device(id)
	d.LastSeen = now
	switch sub {
	case "info":
		d.Info = push(d.Info, Msg{At: now, Text: payload, Auto: now.Before(d.autoUntil) && pollAnswer.MatchString(payload)})
	case "log":
		d.Log = push(d.Log, Msg{At: now, Text: payload})
	default:
		d.Topics[sub] = Value{V: payload, At: now}
		if h.influx != nil {
			if l := point(id, sub, payload, now); l != "" {
				h.influx.add(l)
			}
		}
	}
}

var devRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// Send publishes one command to the device.
func (h *Hub) Send(dev, cmd string) error { return h.send(dev, cmd, false) }

// the periodic poll: the relay mode and the levels of every device with a relay
var (
	pollCmds   = []string{"sh hotter", "sh cooler", "sh tls"}
	pollAnswer = regexp.MustCompile(`^(hotter=\d+|cooler=\d+|C=-?\d+.*dT=-?\d+)$`)
	relayTopic = regexp.MustCompile(`^(relays/r\d+|b\d+)$`)
)

const (
	pollGap   = 1500 * time.Millisecond
	pollFresh = 5 * time.Minute // a device silent for longer is not asked
)

// Poll asks the devices every period until stop is closed; period 0 = never.
func (h *Hub) Poll(period time.Duration, stop <-chan struct{}) {
	if period <= 0 {
		return
	}
	log.Printf("poll: %v every %v, devices with a relay", pollCmds, period)
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		for _, id := range h.pollTargets() {
			go func(id string) {
				for i, c := range pollCmds {
					if i > 0 {
						time.Sleep(pollGap)
					}
					if err := h.send(id, c, true); err != nil && strings.Contains(err.Error(), "wait") {
						time.Sleep(pollGap) // a command from the page just went: try once more
						h.send(id, c, true)
					}
				}
			}(id)
		}
	}
}

func (h *Hub) pollTargets() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for id, d := range h.dev {
		if d.LastSeen.IsZero() || time.Since(d.LastSeen) > pollFresh {
			continue
		}
		for k := range d.Topics {
			if relayTopic.MatchString(k) {
				ids = append(ids, id)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func (h *Hub) send(dev, cmd string, auto bool) error {
	cmd = strings.TrimSpace(cmd)
	switch {
	case !devRe.MatchString(dev):
		return errors.New("bad device id")
	case cmd == "" || len(cmd) > 100 || strings.ContainsAny(cmd, "\r\n"):
		return errors.New("a command is one line, 1..100 characters")
	case h.cli == nil || !h.cli.IsConnectionOpen():
		return errors.New("no connection to the broker")
	}
	h.mu.Lock()
	d := h.device(dev)
	if wait := cmdGap - time.Since(d.lastCmd); wait > 0 {
		h.mu.Unlock()
		return fmt.Errorf("the device takes one command a second: wait %.1f s", wait.Seconds())
	}
	d.lastCmd = time.Now()
	if auto {
		d.autoUntil = d.lastCmd.Add(3 * time.Second)
	} else {
		d.Info = push(d.Info, Msg{At: d.lastCmd, Text: cmd, Out: true})
	}
	h.mu.Unlock()
	topic := "/" + h.creds.User + "/" + dev + "/in/params"
	t := h.cli.Publish(topic, 0, false, cmd)
	if !t.WaitTimeout(5*time.Second) || t.Error() != nil {
		return fmt.Errorf("publish: %v", t.Error())
	}
	if !auto {
		log.Printf("cmd %s <- %q", topic, cmd)
	}
	return nil
}

type stateView struct {
	Broker  BrokerState       `json:"broker"`
	Devices []Device          `json:"devices"`
	Groups  map[string]string `json:"groups"`
	History bool              `json:"history"` // /api/v1/history works
	Now     time.Time         `json:"now"`
}

func (h *Hub) snapshot() stateView {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := stateView{Broker: h.st, Now: time.Now(), Devices: []Device{}, Groups: map[string]string{}, History: h.influx != nil}
	for k, g := range h.groups {
		v.Groups[k] = g
	}
	for _, d := range h.dev {
		if d.LastSeen.IsZero() {
			continue // only a command was sent to it; the device itself never spoke
		}
		c := *d
		c.Topics = make(map[string]Value, len(d.Topics))
		for k, x := range d.Topics {
			c.Topics[k] = x
		}
		c.Info = append([]Msg{}, d.Info...) // [] and not null in the JSON
		c.Log = append([]Msg{}, d.Log...)
		v.Devices = append(v.Devices, c)
	}
	sort.Slice(v.Devices, func(i, j int) bool { return v.Devices[i].ID < v.Devices[j].ID })
	return v
}

// Handler: the page and the API; only the trusted networks (and loopback) get in.
func (h *Hub) Handler(trusted []netip.Prefix) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(page)
	})
	m.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(historyPage)
	})
	m.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, h.snapshot()) })
	m.HandleFunc("GET /api/v1/history", func(w http.ResponseWriter, r *http.Request) {
		if h.influx == nil {
			reply(w, 404, map[string]string{"error": "the panel runs without --influx"})
			return
		}
		x, err := h.influx.History(r.URL.Query().Get("dev"), r.URL.Query().Get("range"))
		if err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, x)
	})
	m.HandleFunc("PUT /api/v1/group", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Dev, Group string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
			reply(w, 400, map[string]string{"error": "bad JSON"})
			return
		}
		if err := h.SetGroup(in.Dev, in.Group); err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, map[string]string{"ok": "saved"})
	})
	m.HandleFunc("POST /api/v1/cmd", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Dev, Cmd string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
			reply(w, 400, map[string]string{"error": "bad JSON"})
			return
		}
		if err := h.Send(in.Dev, in.Cmd); err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("cmd from %s", r.RemoteAddr)
		reply(w, 200, map[string]string{"ok": "sent"})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed(r.RemoteAddr, trusted) {
			reply(w, 403, map[string]string{"error": "this address is not in the trusted networks"})
			return
		}
		m.ServeHTTP(w, r)
	})
}

func allowed(remote string, trusted []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	if a.IsLoopback() {
		return true
	}
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
