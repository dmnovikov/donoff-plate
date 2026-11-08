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
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	Params    map[string]Value `json:"params"` // the last known answers: hotter, cooler, tls, time, lschm1, lschm2, cschm1, cschm2
	Info      []Msg            `json:"info"`   // /out/info answers and the commands sent, newest last
	Log       []Msg            `json:"log"`    // /out/log
	lastCmd   time.Time
	autoUntil time.Time // answers before it belong to the periodic poll
	lastAsk   string    // the last "sh ..." sent: "sh cschm1" answers only with the bits
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
		d = &Device{ID: id, Topics: map[string]Value{}, Params: map[string]Value{}}
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
		key := paramKey(payload, d.lastAsk)
		if key != "" {
			d.Params[key] = Value{V: strings.TrimSpace(payload), At: now}
		}
		if key == "" || !now.Before(d.autoUntil) { // the answers to the poll stay out of the feed
			d.Info = push(d.Info, Msg{At: now, Text: payload})
		}
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

// the periodic poll: the clock of every device; the thermostat mode, the levels and the
// schedules (lschm1/2 with aofs1/2 — seconds on at the start of every hour, for watering — and the custom ones cschm1/2 when a relay uses them) of the devices with a relay
var (
	relayTopic = regexp.MustCompile(`^(relays/r\d+|b\d+)$`)
	answers    = []struct {
		re  *regexp.Regexp
		key string
	}{
		{regexp.MustCompile(`^hotter=\d+$`), "hotter"},
		{regexp.MustCompile(`^cooler=\d+$`), "cooler"},
		{regexp.MustCompile(`^C=-?\d+.*dT=-?\d+$`), "tls"},
		{regexp.MustCompile(`^I:S=\d+,T=`), "time"},
		{regexp.MustCompile(`^(lschm1?|schm1)=\d+$`), "lschm1"},
		{regexp.MustCompile(`^(lschm2|l2schm|schm2)=\d+$`), "lschm2"},
		{regexp.MustCompile(`^aofs1?=\d+$`), "aofs1"},
		{regexp.MustCompile(`^(aofs2|a2ofs)=\d+$`), "aofs2"},
	}
	bitsAnswer = regexp.MustCompile(`^[01]{4}( [01]{4}){5}$`)
)

// paramKey: which known parameter the /out/info message answers ("" = none)
func paramKey(text, lastAsk string) string {
	text = strings.TrimSpace(text)
	for _, a := range answers {
		if a.re.MatchString(text) {
			return a.key
		}
	}
	if bitsAnswer.MatchString(text) && (lastAsk == "sh cschm1" || lastAsk == "sh cschm2") {
		return lastAsk[3:]
	}
	return ""
}

// pollCmds is what the poll asks a device now
func (h *Hub) pollCmds(id string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.dev[id]
	if d == nil {
		return nil
	}
	cmds := []string{"sh time"}
	nrel := 0
	for k := range d.Topics {
		if relayTopic.MatchString(k) {
			nrel++
		}
	}
	if nrel == 0 {
		return cmds
	}
	cmds = append(cmds, "sh hotter", "sh cooler", "sh tls", "sh lschm", "sh aofs") // "lschm1" is not known to the old firmware
	if nrel > 1 {
		cmds = append(cmds, "sh lschm2", "sh aofs2")
	}
	used := map[string]bool{}
	for _, k := range []string{"lschm1", "lschm2"} {
		if v, ok := d.Params[k]; ok {
			_, n, _ := strings.Cut(v.V, "=")
			used[n] = true
		}
	}
	if used["99"] {
		cmds = append(cmds, "sh cschm1")
	}
	if used["100"] {
		cmds = append(cmds, "sh cschm2")
	}
	return cmds
}

const (
	pollGap   = 1500 * time.Millisecond
	pollFresh = 5 * time.Minute // a device silent for longer is not asked
)

// Poll asks the devices every period until stop is closed; period 0 = never.
func (h *Hub) Poll(period time.Duration, stop <-chan struct{}) {
	if period <= 0 {
		return
	}
	log.Printf("poll: sh time (every device), sh hotter/cooler/tls/lschm/aofs/lschm2/aofs2 [cschm1/2] (devices with a relay) every %v", period)
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
				for i, c := range h.pollCmds(id) {
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
		if !d.LastSeen.IsZero() && time.Since(d.LastSeen) <= pollFresh {
			ids = append(ids, id)
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
	d.lastAsk = cmd
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
	Me      *Who              `json:"me,omitempty"`
	Users   bool              `json:"users"` // the panel has users (the admin page works)
	Now     time.Time         `json:"now"`
}

// GroupOf is the group of the device ("" = none).
func (h *Hub) GroupOf(dev string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.groups[dev]
}

// AllGroups: the names of the groups in use, sorted.
func (h *Hub) AllGroups() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, g := range h.groups {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

func (h *Hub) snapshot() stateView { return h.snapshotFor(nil) }

// snapshotFor: only the devices the one may see (nil = all)
func (h *Hub) snapshotFor(who *Who) stateView {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := stateView{Broker: h.st, Now: time.Now(), Devices: []Device{}, Groups: map[string]string{}, History: h.influx != nil, Me: who}
	see := func(id string) bool { return who == nil || who.CanSee(h.groups[id]) }
	for k, g := range h.groups {
		if see(k) {
			v.Groups[k] = g
		}
	}
	if who != nil && !who.Admin {
		v.Broker.User, v.Broker.Error = "", "" // the broker login is the admin's business
	}
	for _, d := range h.dev {
		if d.LastSeen.IsZero() || !see(d.ID) {
			continue // only a command was sent to it (the device itself never spoke), or not for this user
		}
		c := *d
		c.Topics = make(map[string]Value, len(d.Topics))
		for k, x := range d.Topics {
			c.Topics[k] = x
		}
		c.Params = make(map[string]Value, len(d.Params))
		for k, x := range d.Params {
			c.Params[k] = x
		}
		c.Info = append([]Msg{}, d.Info...) // [] and not null in the JSON
		c.Log = append([]Msg{}, d.Log...)
		v.Devices = append(v.Devices, c)
	}
	sort.Slice(v.Devices, func(i, j int) bool { return v.Devices[i].ID < v.Devices[j].ID })
	return v
}

// Handler: the page and the API for the trusted networks (and loopback): everyone there is the admin.
// u (may be nil) gives the admin page of the users.
func (h *Hub) Handler(trusted []netip.Prefix, u *Users) http.Handler {
	lan := &Who{Name: "локальная сеть", Admin: true, Control: true}
	m := h.routes(u)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed(r.RemoteAddr, trusted) {
			reply(w, 403, map[string]string{"error": "this address is not in the trusted networks"})
			return
		}
		m.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), whoKey{}, lan)))
	})
}

// UsersHandler: the same page and API for anyone with a login (for a tunnel or the internet):
// a user sees only the devices of their groups and may send commands only with "control".
func (h *Hub) UsersHandler(u *Users) http.Handler {
	m := h.routes(u)
	open := map[string]bool{"/login": true, "/invite": true, "/api/v1/invite": true, "/logout": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := u.FromRequest(r)
		if who == nil && !open[r.URL.Path] {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				reply(w, 401, map[string]string{"error": "log in first"})
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		m.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), whoKey{}, who)))
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

// ParseNets reads "192.168.0.0/16,10.0.0.0/8" (the --trusted flag).
func ParseNets(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("--trusted %q: %w", f, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// LoadDevices reads the devices kept by SaveDevices, so that a device silent since the restart
// is still shown (gray, "last seen ..."); a missing file is no devices.
func (h *Hub) LoadDevices(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []Device
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range list {
		if !devRe.MatchString(d.ID) || d.LastSeen.IsZero() {
			continue
		}
		c := d
		if c.Topics == nil {
			c.Topics = map[string]Value{}
		}
		if c.Params == nil {
			c.Params = map[string]Value{}
		}
		h.dev[c.ID] = &c
	}
	return nil
}

// SaveDevices writes the last state of every device (atomically).
func (h *Hub) SaveDevices(path string) error {
	b, err := json.Marshal(h.snapshot().Devices)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// KeepDevices saves the devices every period until stop is closed, and once more at the end.
func (h *Hub) KeepDevices(path string, period time.Duration, stop <-chan struct{}) {
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-stop:
			if err := h.SaveDevices(path); err != nil {
				log.Printf("devices: %v", err)
			}
			return
		}
		if err := h.SaveDevices(path); err != nil {
			log.Printf("devices: %v", err)
		}
	}
}
