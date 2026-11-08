// Package mqtt connects the thermostat to an MQTT broker with the topics of donoff-lib
// (github.com/dmnovikov/donoff-lib, branch newlife32), so the same server side works with it.
//
// Every topic is "/<user>/<dev_id><channel>":
//
//	/out/sensors/temp_out   temperature, "23.44" (°C, 2 decimals)
//	/out/relays/r1         relay, "1" / "0"
//	/out/time_up           uptime, "HH:MM:SS"
//	/out/time_comm         relay on for, "HH:MM:SS" ("OFF" when off, as the donoff-lib devices send)
//	/out/time_down         relay off for, "HH:MM:SS" ("OFF" when on)
//	/out/info              answers to commands: "ctempl=2600", "no command", ...
//	/out/log               events: relay switches, sensor errors
//	/in/params             commands (subscribed):
//	    r11 / r10            relay on / off (mode on / off)
//	    cooler=1 / cooler=0  cooling, auto / off      hotter=1 / hotter=0  heating, auto / off
//	    ctempl=2600          setpoint in 1/100 °C (a value below 100 is °C, as in donoff-lib)
//	    tdelta=100           hysteresis in 1/100 °C (a value below 50 is 1/10 °C, as in donoff-lib)
//	    mode=auto            our own: auto / on / off
//	    sh <name>            show: hotter, cooler, ctempl, tdelta, r1, tlevels, mode, time, ip
//	    save                 accepted (the settings are always saved); a trailing ";" too
package mqtt

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/lab240/napi-donoff/internal/thermo"
)

const (
	chTemp     = "/out/sensors/temp_out"
	chRelay    = "/out/relays/r1"
	chUptime   = "/out/time_up"
	chOntime   = "/out/time_comm"
	chDowntime = "/out/time_down"
	chInfo     = "/out/info"
	chLog      = "/out/log"
	chParams   = "/in/params"

	antiFlood = time.Second // donoff-lib DDOS_MS
)

// Channels is the topic list for the page.
var Channels = []struct{ Topic, Dir, Text string }{
	{chTemp, "out", "температура, °C"},
	{chRelay, "out", "реле: 1 / 0"},
	{chUptime, "out", "время работы службы"},
	{chOntime, "out", "реле включено уже"},
	{chDowntime, "out", "реле выключено уже"},
	{chInfo, "out", "ответы на команды"},
	{chLog, "out", "события"},
	{chParams, "in", "команды"},
}

// Config is kept in its own file (0600: it holds the password).
type Config struct {
	Enabled  bool   `json:"enabled"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DevID    string `json:"dev_id"`
	Interval int    `json:"interval"` // s between the periodic publishes
}

func DefaultConfig() Config { return Config{Port: 1883, Interval: 10} }

func (c Config) Validate() error {
	switch {
	case c.Port < 1 || c.Port > 65535:
		return errors.New("port: 1..65535")
	case c.Interval < 1 || c.Interval > 3600:
		return errors.New("interval: 1..3600 s")
	case strings.ContainsAny(c.User+c.DevID, "/+# "):
		return errors.New(`user and dev_id go into the topics: no "/", "+", "#" or spaces`)
	case c.Enabled && (c.Server == "" || c.User == "" || c.DevID == ""):
		return errors.New("server, user and dev_id are needed to connect")
	}
	return nil
}

// Prefix is "/<user>/<dev_id>".
func (c Config) Prefix() string { return "/" + c.User + "/" + c.DevID }

// Status is for the page.
type Status struct {
	Connected  bool      `json:"connected"`
	State      string    `json:"state"` // off, connecting, connected, error
	Error      string    `json:"error,omitempty"`
	Since      time.Time `json:"since"`
	Published  int       `json:"published"`
	Received   int       `json:"received"`
	LastCmd    string    `json:"last_cmd,omitempty"`
	LastCmdAt  time.Time `json:"last_cmd_at"`
	LastAnswer string    `json:"last_answer,omitempty"`
}

type Client struct {
	th   *thermo.Thermostat
	path string

	mu      sync.Mutex
	cfg     Config
	st      Status
	cli     paho.Client
	stop    chan struct{}
	done    chan struct{}
	lastIn  time.Time
	lastFan *bool
	lastErr string
}

// New loads the config from path; Start connects when it is enabled.
func New(th *thermo.Thermostat, path string) *Client {
	c := &Client{th: th, path: path, cfg: DefaultConfig(), st: Status{State: "off", Since: time.Now()}}
	if b, err := os.ReadFile(path); err == nil {
		cfg := DefaultConfig()
		if err := json.Unmarshal(b, &cfg); err == nil && cfg.Validate() == nil {
			c.cfg = cfg
		} else {
			log.Printf("mqtt: %s ignored: %v", path, err)
		}
	}
	return c
}

func (c *Client) Config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

// SetConfig validates, saves and reconnects.
func (c *Client) SetConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := save(c.path, cfg); err != nil {
		return fmt.Errorf("save %s: %w", c.path, err)
	}
	c.Stop()
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
	log.Printf("mqtt: config: enabled=%v %s:%d user %q dev_id %q", cfg.Enabled, cfg.Server, cfg.Port, cfg.User, cfg.DevID)
	c.Start()
	return nil
}

func (c *Client) setState(state, err string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.State != state || c.st.Error != err {
		c.st.Since = time.Now()
	}
	c.st.State, c.st.Error, c.st.Connected = state, err, state == "connected"
	if err != "" && err != c.lastErr {
		log.Printf("mqtt: %s", err)
	}
	c.lastErr = err
}

// Start connects in the background (auto reconnect) when the config is enabled.
func (c *Client) Start() {
	cfg := c.Config()
	if !cfg.Enabled {
		c.setState("off", "")
		return
	}
	id := make([]byte, 2)
	rand.Read(id)
	o := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s", net.JoinHostPort(cfg.Server, strconv.Itoa(cfg.Port)))).
		SetClientID("donoff-" + cfg.DevID + hex.EncodeToString(id)).
		SetUsername(cfg.User).SetPassword(cfg.Password).
		SetCleanSession(true).
		SetKeepAlive(30 * time.Second).
		SetConnectTimeout(10 * time.Second).
		SetAutoReconnect(true).SetMaxReconnectInterval(30 * time.Second).
		SetOnConnectHandler(func(cl paho.Client) {
			c.setState("connected", "")
			log.Printf("mqtt: connected to %s:%d, topics %s/...", cfg.Server, cfg.Port, cfg.Prefix())
			cl.Subscribe(cfg.Prefix()+chParams, 0, func(_ paho.Client, m paho.Message) { c.onCommand(string(m.Payload())) })
			c.publishAll(true)
		}).
		SetConnectionLostHandler(func(_ paho.Client, err error) { c.setState("connecting", "connection lost: "+err.Error()) }).
		SetReconnectingHandler(func(paho.Client, *paho.ClientOptions) {
			c.mu.Lock()
			e := c.st.Error
			c.mu.Unlock()
			c.setState("connecting", e)
		})
	cl := paho.NewClient(o)
	stop, done := make(chan struct{}), make(chan struct{})
	c.mu.Lock()
	c.cli, c.stop, c.done, c.lastFan = cl, stop, done, nil
	c.mu.Unlock()
	c.setState("connecting", "")

	go func() {
		defer close(done)
		// the first connection: our own retry, so the page shows why it fails (wrong password, ...);
		// after that paho reconnects by itself
		for {
			t := cl.Connect()
			t.Wait()
			if t.Error() == nil {
				break
			}
			c.setState("error", t.Error().Error()+"; retry in 10 s")
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Second):
			}
		}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			n++
			if !cl.IsConnectionOpen() {
				continue
			}
			c.publishAll(n%cfg.Interval == 0)
		}
	}()
}

// Stop disconnects (a no-op when not running).
func (c *Client) Stop() {
	c.mu.Lock()
	cl, stop, done := c.cli, c.stop, c.done
	c.cli, c.stop, c.done = nil, nil, nil
	c.mu.Unlock()
	if cl == nil {
		return
	}
	close(stop)
	if cl.IsConnectionOpen() {
		c.publish(chLog, "donoff stopped")
	}
	cl.Disconnect(500)
	<-done
	c.setState("off", "")
}

func (c *Client) publish(ch, payload string) {
	c.mu.Lock()
	cl, prefix := c.cli, c.cfg.Prefix()
	c.mu.Unlock()
	if cl == nil || !cl.IsConnectionOpen() {
		return
	}
	cl.Publish(prefix+ch, 0, false, payload)
	c.mu.Lock()
	c.st.Published++
	c.mu.Unlock()
}

// publishAll sends the relay at once when it changed, and everything when periodic.
func (c *Client) publishAll(periodic bool) {
	s := c.th.Status()
	c.mu.Lock()
	changed := c.lastFan == nil || *c.lastFan != s.Fan
	first := c.lastFan == nil
	fan := s.Fan
	c.lastFan = &fan
	c.mu.Unlock()
	if changed {
		c.publish(chRelay, b01(s.Fan))
		if !first {
			c.publish(chLog, fmt.Sprintf("R1 %s: %s, T=%s", map[bool]string{true: "ON", false: "OFF"}[s.Fan], s.Reason, tempStr(s.Temp)))
		}
	}
	if !periodic {
		return
	}
	if s.Temp != nil {
		c.publish(chTemp, fmt.Sprintf("%.2f", *s.Temp))
	} else if s.SensorErr != "" {
		c.publish(chLog, "sensor error: "+s.SensorErr)
	}
	if !changed {
		c.publish(chRelay, b01(s.Fan))
	}
	now := time.Now()
	c.publish(chUptime, hms(now.Sub(s.Started)))
	on, off := "OFF", "OFF"
	if s.Fan {
		on = hms(now.Sub(s.FanSince))
	} else {
		off = hms(now.Sub(s.FanSince))
	}
	c.publish(chOntime, on)
	c.publish(chDowntime, off)
}

func (c *Client) answer(s string) {
	c.mu.Lock()
	c.st.LastAnswer = s
	c.mu.Unlock()
	c.publish(chInfo, s)
}

func (c *Client) onCommand(in string) {
	now := time.Now()
	c.mu.Lock()
	c.st.Received++
	c.st.LastCmd, c.st.LastCmdAt = in, now
	flood := now.Sub(c.lastIn) < antiFlood
	c.lastIn = now
	c.mu.Unlock()
	if flood {
		c.answer("DDOS CHECK")
		return
	}
	log.Printf("mqtt: command %q", in)
	c.answer(Execute(c.th, strings.TrimSpace(in)))
}

// Execute runs one donoff-lib style command and returns the answer for /out/info ("" = none).
func Execute(th *thermo.Thermostat, in string) string {
	in = strings.TrimSuffix(in, ";") // donoff-lib: ";" = autosave; ours always saves
	cfg := th.Status().Config
	set := func(c thermo.Config, ok string) string {
		if err := th.SetConfig(c); err != nil {
			return "E:" + err.Error()
		}
		return ok
	}
	if name, ok := strings.CutPrefix(in, "sh "); ok {
		return show(th, strings.TrimSpace(name))
	}
	cmd, val, isSet := strings.Cut(in, "=")
	if !isSet {
		switch in {
		case "r11":
			cfg.Mode = thermo.On
			return set(cfg, "r1=1")
		case "r10":
			cfg.Mode = thermo.Off
			return set(cfg, "r1=0")
		case "save":
			return "saved"
		}
		return "no command"
	}
	num := func() (float64, bool) {
		v, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	bad := "E:Varable error" // the donoff-lib text
	switch cmd {
	case "cooler", "hotter", "h":
		v, ok := num()
		if !ok {
			return bad
		}
		action := thermo.Cool
		if cmd != "cooler" {
			action, cmd = thermo.Heat, "hotter"
		}
		if v != 0 {
			cfg.Action, cfg.Mode = action, thermo.Auto
		} else if cfg.Action == action {
			cfg.Mode = thermo.Off
		}
		return set(cfg, fmt.Sprintf("%s=%d", cmd, b2i(v != 0)))
	case "ctempl":
		v, ok := num()
		if !ok {
			return bad
		}
		if math.Abs(v) >= 100 {
			v /= 100
		}
		cfg.Setpoint = v
		return set(cfg, fmt.Sprintf("ctempl=%d", hundredths(cfg.Setpoint)))
	case "tdelta":
		v, ok := num()
		if !ok || v < 0 {
			return bad
		}
		if v < 50 {
			v *= 10
		}
		cfg.Hysteresis = v / 100
		return set(cfg, fmt.Sprintf("tdelta=%d", hundredths(cfg.Hysteresis)))
	case "mode":
		cfg.Mode = thermo.Mode(strings.TrimSpace(val))
		return set(cfg, "mode="+string(cfg.Mode))
	}
	return bad
}

func show(th *thermo.Thermostat, name string) string {
	s := th.Status()
	switch name {
	case "hotter", "h":
		return fmt.Sprintf("hotter=%d", b2i(s.Action == thermo.Heat && s.Mode == thermo.Auto))
	case "cooler":
		return fmt.Sprintf("cooler=%d", b2i(s.Action == thermo.Cool && s.Mode == thermo.Auto))
	case "ctempl":
		return fmt.Sprintf("ctempl=%d", hundredths(s.Setpoint))
	case "tdelta":
		return fmt.Sprintf("tdelta=%d", hundredths(s.Hysteresis))
	case "r1":
		return "r1=" + b01(s.Fan)
	case "mode":
		return "mode=" + string(s.Mode)
	case "tlevels", "tls":
		l := hundredths(s.Setpoint)
		return fmt.Sprintf("C=%d,D=%d,N=%d dT=%d", l, l, l, hundredths(s.Hysteresis))
	case "time":
		t := time.Now()
		return fmt.Sprintf("I:S=1,T=%d:%d:%d", t.Hour(), t.Minute(), t.Year())
	case "ip":
		return "ip=" + localIP()
	}
	return "E:no such parameter: " + name
}

func localIP() string {
	conn, err := net.Dial("udp", "192.0.2.1:9") // no packet is sent; it only picks the route
	if err != nil {
		return "?"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func save(path string, c Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func hms(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s%3600/60, s%60)
}

func hundredths(v float64) int { return int(math.Round(v * 100)) }

func b01(b bool) string { return map[bool]string{true: "1", false: "0"}[b] }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func tempStr(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", *p)
}
