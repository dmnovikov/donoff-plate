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
//	POST /api/v1/group/rename {"from": "Теплицы", "to": "Теплицы 2"}: every device of the group moves, the order keeps its place
//	POST /api/v1/group/delete {"name": "Теплицы"}: its devices are left without a group
//	PUT  /api/v1/group/order  {"order": ["Ташкент", "Доможирка"]}: the order of the group tabs; kept next to the groups file (-order.json)
//	PUT  /api/v1/comment   {"dev": "dmzk10", "comment": "теплица у забора"} ("" = none); kept in the comments file
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
	"slices"
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
	keepMsgs      = 30                      // answers and log lines kept per device
	cmdGap        = 1100 * time.Millisecond // donoff-lib drops commands closer than 1 s
	sentKeep      = 30 * time.Second        // the broker echoes our commands much sooner than this
	maxJournalCmd = 200                     // characters of a command kept in the journal
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
	By   string    `json:"by,omitempty"`   // the command journal: byPanel or byOther
}

type Device struct {
	ID        string           `json:"id"`
	LastSeen  time.Time        `json:"last_seen"`
	Topics    map[string]Value `json:"topics"` // after "/out/": "sensors/temp_in", "relays/r1", "time_up", ...
	Params    map[string]Value `json:"params"` // the last known answers: hotter, cooler, tls, time, lschm1, lschm2, cschm1, cschm2
	Info      []Msg            `json:"info"`   // /out/info answers and the commands sent, newest last
	Log       []Msg            `json:"log"`    // /out/log
	Cmds      []Msg            `json:"cmds"`   // the command journal: everything but the panel's own "sh ..."
	lastCmd   time.Time
	autoUntil time.Time       // answers before it belong to the periodic poll
	lastAsk   string          // the last "sh ..." sent: "sh cschm1" answers only with the bits
	pollNext  int             // the poll sends pollCmds()[pollNext % len] next
	unknown   map[string]bool // poll commands the device answered with "E:..." (napi-termostat has no lschm/aofs): not asked again
	sent      []sentCmd       // what the panel published and the broker has not echoed yet
}

// sentCmd lets onCommand tell the broker's echo of our own command from a command of another client.
type sentCmd struct {
	text string
	at   time.Time
}

// who sent a command; MQTT does not say, so "other" is whatever the panel did not publish itself
const (
	byPanel = "panel"
	byOther = "other"
)

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

	mu           sync.Mutex
	dev          map[string]*Device
	st           BrokerState
	groups       map[string]string // dev_id -> group
	groupsPath   string
	groupOrder   []string          // the group tabs in this order; the others go after them by name
	comments     map[string]string // dev_id -> what it is, where it is
	commentsPath string
	cmdsPath     string  // the command journal, one JSON object per line ("" = page only)
	influx       *Influx // nil = no history
}

func New(c Creds) *Hub {
	return &Hub{creds: c, dev: map[string]*Device{}, groups: map[string]string{}, comments: map[string]string{},
		st: BrokerState{Server: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), User: c.User, Since: time.Now()}}
}

// LoadGroups reads the groups file ({"dmzk10": "Теплицы", ...}); a missing file is no groups.
func (h *Hub) LoadGroups(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.groupsPath = path
	if err := loadMap(path, &h.groups); err != nil {
		return err
	}
	b, err := os.ReadFile(orderPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &h.groupOrder); err != nil {
		return fmt.Errorf("%s: %w", orderPath(path), err)
	}
	return nil
}

func orderPath(groups string) string { return strings.TrimSuffix(groups, ".json") + "-order.json" }

func checkGroupName(g string) error {
	if len([]rune(g)) > 40 || strings.ContainsAny(g, "\r\n") {
		return errors.New("a group name is one line, up to 40 characters")
	}
	return nil
}

// RenameGroup moves every device of a group into another name (an existing group too: they merge).
func (h *Hub) RenameGroup(from, to string) error {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return errors.New("both names are needed")
	}
	if err := checkGroupName(to); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for d, g := range h.groups {
		if g == from {
			h.groups[d] = to
		}
	}
	if err := writeJSON(h.groupsPath, h.groups); err != nil {
		return err
	}
	ord := make([]string, 0, len(h.groupOrder))
	for _, g := range h.groupOrder {
		if g == from {
			g = to
		}
		if !slices.Contains(ord, g) {
			ord = append(ord, g)
		}
	}
	h.groupOrder = ord
	return h.saveOrder()
}

// DeleteGroup takes every device out of a group and the group out of the order.
func (h *Hub) DeleteGroup(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a name is needed")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for d, g := range h.groups {
		if g == name {
			delete(h.groups, d)
		}
	}
	if err := writeJSON(h.groupsPath, h.groups); err != nil {
		return err
	}
	h.groupOrder = slices.DeleteFunc(h.groupOrder, func(g string) bool { return g == name })
	return h.saveOrder()
}

// SetGroupOrder keeps the order of the group tabs.
func (h *Hub) SetGroupOrder(order []string) error {
	if len(order) > 200 {
		return errors.New("too many groups")
	}
	ord := []string{}
	for _, g := range order {
		g = strings.TrimSpace(g)
		if err := checkGroupName(g); err != nil {
			return err
		}
		if g != "" && !slices.Contains(ord, g) {
			ord = append(ord, g)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.groupOrder = ord
	return h.saveOrder()
}

func (h *Hub) saveOrder() error {
	if h.groupsPath == "" {
		return nil
	}
	return writeJSON(orderPath(h.groupsPath), h.groupOrder)
}

// LoadComments reads the comments file ({"dmzk10": "теплица у забора", ...}); a missing file is no comments.
func (h *Hub) LoadComments(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commentsPath = path
	return loadMap(path, &h.comments)
}

// SetGroup puts a device into a group ("" = out of any group) and saves the file.
func (h *Hub) SetGroup(dev, group string) error {
	group = strings.TrimSpace(group)
	switch {
	case !devRe.MatchString(dev):
		return errors.New("bad device id")
	case checkGroupName(group) != nil:
		return checkGroupName(group)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return setIn(h.groups, h.groupsPath, dev, group)
}

// SetComment keeps a free text about a device ("" = none) and saves the file.
func (h *Hub) SetComment(dev, text string) error {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	switch {
	case !devRe.MatchString(dev):
		return errors.New("bad device id")
	case len([]rune(text)) > 300 || strings.Count(text, "\n") > 5 || strings.ContainsRune(text, '\r'):
		return errors.New("a comment is up to 300 characters and 6 lines")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return setIn(h.comments, h.commentsPath, dev, text)
}

func loadMap(path string, to *map[string]string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	*to = m
	return nil
}

// setIn sets or deletes ("" ) one key and writes the whole map to path ("" = memory only).
func setIn(m map[string]string, path, key, v string) error {
	if v == "" {
		delete(m, key)
	} else {
		m[key] = v
	}
	return writeJSON(path, m)
}

// writeJSON writes v to path through a temporary file ("" = memory only).
func writeJSON(path string, v any) error {
	if path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
			cl.Subscribe(prefix+"+/in/#", 0, func(_ paho.Client, m paho.Message) {
				if !m.Retained() { // a retained message is an old one, repeated on every subscribe
					h.onCommand(prefix, m.Topic(), string(m.Payload()))
				}
			})
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
		if key == "" && now.Before(d.autoUntil) && strings.HasPrefix(strings.TrimSpace(payload), "E:") && strings.HasPrefix(d.lastAsk, "sh ") {
			if d.unknown == nil {
				d.unknown = map[string]bool{}
			}
			if !d.unknown[d.lastAsk] {
				log.Printf("poll: %s does not know %q (%s): not asked any more", id, d.lastAsk, strings.TrimSpace(payload))
			}
			d.unknown[d.lastAsk] = true
			return
		}
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

// onCommand: a message on /<user>/<dev>/in/... — our own echo, or a command of another client.
func (h *Hub) onCommand(prefix, topic, payload string) {
	rest, ok := strings.CutPrefix(topic, prefix)
	if !ok {
		return
	}
	id, sub, ok := strings.Cut(rest, "/in/")
	if !ok || !devRe.MatchString(id) {
		return
	}
	cmd := strings.TrimSpace(payload)
	if sub != "params" {
		cmd = "in/" + sub + " " + cmd
	}
	if r := []rune(cmd); len(r) > maxJournalCmd {
		cmd = string(r[:maxJournalCmd])
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.device(id)
	if sub == "params" && d.takeSent(cmd, now) {
		return
	}
	h.journal(d, id, cmd, byOther, now)
}

// takeSent drops what is too old for an echo and reports whether cmd is one of ours (call with h.mu held).
func (d *Device) takeSent(cmd string, now time.Time) bool {
	found := false
	kept := d.sent[:0]
	for _, s := range d.sent {
		switch {
		case now.Sub(s.at) > sentKeep:
		case !found && s.text == cmd:
			found = true
		default:
			kept = append(kept, s)
		}
	}
	d.sent = kept
	return found
}

func isSh(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) > 0 && f[0] == "sh"
}

// journal records a command in the card and the file (call with h.mu held). The poll never gets here.
func (h *Hub) journal(d *Device, dev, cmd, by string, at time.Time) {
	if by != byPanel || !isSh(cmd) {
		d.Cmds = push(d.Cmds, Msg{At: at, Text: cmd, By: by})
	}
	if h.cmdsPath == "" {
		return
	}
	line, _ := json.Marshal(struct {
		At  time.Time `json:"at"`
		Dev string    `json:"dev"`
		Cmd string    `json:"cmd"`
		By  string    `json:"by"`
	}{at, dev, cmd, by})
	f, err := os.OpenFile(h.cmdsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("journal: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		log.Printf("journal: %v", err)
	}
}

// SetCmdLog makes the command journal go to a file too (call before Start).
func (h *Hub) SetCmdLog(path string) { h.cmdsPath = path }

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
	out := cmds[:0]
	for _, c := range cmds {
		if !d.unknown[c] {
			out = append(out, c)
		}
	}
	return out
}

const (
	pollFresh = 5 * time.Minute // a device silent for longer is not asked
)

// Poll sends every fresh device one poll command each period, going round its list
// (a device with a relay: 7–9 commands, so the full round takes 7–9 periods); period 0 = never.
// The values change slowly, and the devices should not be loaded: one command per period is enough.
func (h *Hub) Poll(period time.Duration, stop <-chan struct{}) {
	if period <= 0 {
		return
	}
	log.Printf("poll: one command per device every %v, in turn: sh time; sh hotter/cooler/tls/lschm/aofs/lschm2/aofs2 [cschm1/2] (devices with a relay)", period)
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		for _, id := range h.pollTargets() {
			if c := h.nextPoll(id); c != "" {
				go h.send(id, c, true) // "wait" (a command from the page just went): this one is skipped, the next tick goes on
			}
		}
	}
}

// nextPoll is the next poll command for the device, in turn
func (h *Hub) nextPoll(id string) string {
	cmds := h.pollCmds(id)
	if len(cmds) == 0 {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.dev[id]
	if d == nil {
		return ""
	}
	c := cmds[d.pollNext%len(cmds)]
	d.pollNext++
	return c
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
	d.takeSent("", d.lastCmd) // only the cleanup: forget what was never echoed
	d.sent = append(d.sent, sentCmd{cmd, d.lastCmd})
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
		h.mu.Lock()
		h.journal(h.device(dev), dev, cmd, byPanel, time.Now())
		h.mu.Unlock()
	}
	return nil
}

type stateView struct {
	Broker   BrokerState       `json:"broker"`
	Devices  []Device          `json:"devices"`
	Groups   map[string]string `json:"groups"`
	Comments map[string]string `json:"comments"`
	Order    []string          `json:"group_order"`
	History  bool              `json:"history"` // /api/v1/history works
	Now      time.Time         `json:"now"`
}

func (h *Hub) snapshot() stateView {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := stateView{Broker: h.st, Now: time.Now(), Devices: []Device{}, Groups: map[string]string{}, Comments: map[string]string{}, Order: slices.Clone(h.groupOrder), History: h.influx != nil}
	if v.Order == nil {
		v.Order = []string{}
	}
	for k, g := range h.groups {
		v.Groups[k] = g
	}
	for k, c := range h.comments {
		v.Comments[k] = c
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
		c.Params = make(map[string]Value, len(d.Params))
		for k, x := range d.Params {
			c.Params[k] = x
		}
		c.Info = append([]Msg{}, d.Info...) // [] and not null in the JSON
		c.Log = append([]Msg{}, d.Log...)
		c.Cmds = append([]Msg{}, d.Cmds...)
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
	m.HandleFunc("POST /api/v1/group/rename", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ From, To string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
			reply(w, 400, map[string]string{"error": "bad JSON"})
			return
		}
		if err := h.RenameGroup(in.From, in.To); err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, map[string]string{"ok": "saved"})
	})
	m.HandleFunc("POST /api/v1/group/delete", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
			reply(w, 400, map[string]string{"error": "bad JSON"})
			return
		}
		if err := h.DeleteGroup(in.Name); err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, map[string]string{"ok": "saved"})
	})
	m.HandleFunc("PUT /api/v1/group/order", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Order []string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&in); err != nil {
			reply(w, 400, map[string]string{"error": "bad JSON"})
			return
		}
		if err := h.SetGroupOrder(in.Order); err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, map[string]string{"ok": "saved"})
	})
	m.HandleFunc("PUT /api/v1/comment", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Dev, Comment string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			reply(w, 400, map[string]string{"error": "bad JSON"})
			return
		}
		if err := h.SetComment(in.Dev, in.Comment); err != nil {
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
