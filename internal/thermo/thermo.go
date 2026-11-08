// Package thermo is a two-point thermostat: one relay (fan or heater) with a hysteresis.
//
//	cool: T > setpoint                 -> on
//	      T <= setpoint - hysteresis   -> off
//	      sensor error                 -> off at once (as in donoff-lib)
//	heat: T < setpoint                 -> on
//	      T >= setpoint + hysteresis   -> off
//	      sensor error                 -> off at once
//	between the edges: no change.
//	mode on / off: the relay is forced, the sensor is still read and shown.
package thermo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Mode string

const (
	Auto Mode = "auto"
	On   Mode = "on"
	Off  Mode = "off"
)

// Action is what the relay drives.
type Action string

const (
	Cool Action = "cool"
	Heat Action = "heat"
)

// Config is what the page and the API change; it is kept in the state file.
type Config struct {
	Setpoint   float64 `json:"setpoint"`   // °C, the relay goes on beyond it (above for cool, below for heat)
	Hysteresis float64 `json:"hysteresis"` // °C, the relay goes off at setpoint -/+ hysteresis
	Mode       Mode    `json:"mode"`
	Action     Action  `json:"action"`
}

func DefaultConfig() Config { return Config{Setpoint: 26, Hysteresis: 2, Mode: Auto, Action: Cool} }

// OffEdge is where the relay goes off: setpoint - hysteresis for cool, + for heat.
func (c Config) OffEdge() float64 {
	if c.Action == Heat {
		return c.Setpoint + c.Hysteresis
	}
	return c.Setpoint - c.Hysteresis
}

func (c Config) Validate() error {
	switch {
	case math.IsNaN(c.Setpoint) || c.Setpoint < -40 || c.Setpoint > 100:
		return errors.New("setpoint: -40..100 °C")
	case math.IsNaN(c.Hysteresis) || c.Hysteresis < 0.1 || c.Hysteresis > 20:
		return errors.New("hysteresis: 0.1..20 °C")
	case c.Mode != Auto && c.Mode != On && c.Mode != Off:
		return errors.New(`mode: "auto", "on" or "off"`)
	case c.Action != Cool && c.Action != Heat:
		return errors.New(`action: "cool" or "heat"`)
	}
	return nil
}

type Sensor interface {
	Read() (celsius float64, id string, err error)
}

type Relay interface {
	Set(on bool) error
}

// Status is the live state for the page.
type Status struct {
	Config
	Temp      *float64  `json:"temp"` // null when the sensor fails
	SensorID  string    `json:"sensor_id"`
	SensorErr string    `json:"sensor_error,omitempty"`
	ReadAt    time.Time `json:"read_at"`
	Fan       bool      `json:"fan"`
	FanSince  time.Time `json:"fan_since"`
	Reason    string    `json:"reason"` // why the fan is as it is
	RelayErr  string    `json:"relay_error,omitempty"`
	OffAt     float64   `json:"off_at"` // setpoint -/+ hysteresis
	Switches  int       `json:"switches"`
	OnSeconds float64   `json:"on_seconds"` // fan time since the start
	Started   time.Time `json:"started"`
}

// Point is one history sample.
type Point struct {
	T   int64    `json:"t"` // unix seconds
	C   *float64 `json:"c"`
	Fan bool     `json:"fan"`
}

type Thermostat struct {
	sensor    Sensor
	relay     Relay
	statePath string
	poll      time.Duration

	mu       sync.Mutex
	st       Status
	onTotal  time.Duration
	hist     []Point
	histMax  int
	histStep time.Duration
	lastHist time.Time
	kick     chan struct{}
}

// New loads the config from statePath (if it is there) and starts with the fan off.
func New(s Sensor, r Relay, statePath string, poll, histStep time.Duration, histKeep time.Duration) *Thermostat {
	t := &Thermostat{sensor: s, relay: r, statePath: statePath, poll: poll,
		histStep: histStep, histMax: int(histKeep / histStep), kick: make(chan struct{}, 1)}
	cfg := DefaultConfig()
	if b, err := os.ReadFile(statePath); err == nil {
		c := Config{Action: Cool} // state files of the first version have no action
		if err := json.Unmarshal(b, &c); err == nil && c.Validate() == nil {
			cfg = c
		} else {
			log.Printf("state %s ignored: %v", statePath, err)
		}
	}
	now := time.Now()
	t.st = Status{Config: cfg, Started: now, FanSince: now, Reason: "start"}
	return t
}

// Run reads the sensor and drives the relay until ctx is done; then the fan is switched off.
func (t *Thermostat) Run(ctx context.Context) {
	t.apply(false, "start")
	tick := time.NewTicker(t.poll)
	defer tick.Stop()
	for {
		t.step()
		select {
		case <-ctx.Done():
			t.apply(false, "service stopped")
			return
		case <-tick.C:
		case <-t.kick:
		}
	}
}

func (t *Thermostat) step() {
	c, id, err := t.sensor.Read()
	now := time.Now()

	t.mu.Lock()
	st := &t.st
	st.ReadAt, st.SensorID = now, id
	if err != nil {
		if st.SensorErr != err.Error() {
			log.Printf("sensor: %v", err)
		}
		st.Temp, st.SensorErr = nil, err.Error()
	} else {
		if st.SensorErr != "" {
			log.Printf("sensor %s is back: %.3f °C", id, c)
		}
		st.Temp, st.SensorErr = &c, ""
	}
	fan, reason := Decide(st.Config, st.Temp, st.Fan)
	if now.Sub(t.lastHist) >= t.histStep {
		t.lastHist = now
		t.hist = append(t.hist, Point{T: now.Unix(), C: st.Temp, Fan: fan})
		if len(t.hist) > t.histMax {
			t.hist = append(t.hist[:0:0], t.hist[len(t.hist)-t.histMax:]...)
		}
	}
	t.mu.Unlock()

	t.apply(fan, reason)
}

// Decide is the whole control law: the new fan state from the config, the temperature
// (nil = sensor error) and the fan state now.
func Decide(c Config, temp *float64, fan bool) (bool, string) {
	switch {
	case c.Mode == On:
		return true, "manual: on"
	case c.Mode == Off:
		return false, "manual: off"
	case c.Action == Heat:
		switch {
		case temp == nil:
			return false, "sensor error: relay off"
		case *temp < c.Setpoint:
			return true, "below the setpoint"
		case *temp >= c.OffEdge():
			return false, "at or above the high edge"
		case fan:
			return true, "heating up to the high edge"
		default:
			return false, "inside the band"
		}
	case temp == nil:
		return false, "sensor error: relay off"
	case *temp > c.Setpoint:
		return true, "above the setpoint"
	case *temp <= c.OffEdge():
		return false, "at or below the low edge"
	case fan:
		return true, "cooling down to the low edge"
	default:
		return false, "inside the band"
	}
}

// apply drives the relay and keeps the counters; the relay is written every time (cheap, self-healing).
func (t *Thermostat) apply(fan bool, reason string) {
	err := t.relay.Set(fan)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	st := &t.st
	if fan != st.Fan {
		if st.Fan {
			t.onTotal += now.Sub(st.FanSince)
		}
		st.FanSince = now
		st.Switches++
		log.Printf("relay %s (%s: %s), T=%s", onOff(fan), st.Action, reason, fmtTemp(st.Temp))
	}
	st.Fan, st.Reason = fan, reason
	st.RelayErr = ""
	if err != nil {
		st.RelayErr = err.Error()
		log.Printf("relay: %v", err)
	}
}

func pick[T any](c bool, a, b T) T {
	if c {
		return a
	}
	return b
}

// Status is a copy of the live state.
func (t *Thermostat) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.st
	s.OffAt = s.OffEdge()
	on := t.onTotal
	if s.Fan {
		on += time.Since(s.FanSince)
	}
	s.OnSeconds = on.Seconds()
	return s
}

func (t *Thermostat) History() []Point {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Point(nil), t.hist...)
}

// SetConfig validates, saves and applies at once.
func (t *Thermostat) SetConfig(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := save(t.statePath, c); err != nil {
		return fmt.Errorf("save %s: %w", t.statePath, err)
	}
	t.mu.Lock()
	old := t.st.Config
	t.st.Config = c
	t.mu.Unlock()
	if old != c {
		log.Printf("config: %s, setpoint %.1f, hysteresis %.1f, mode %s", c.Action, c.Setpoint, c.Hysteresis, c.Mode)
	}
	select {
	case t.kick <- struct{}{}:
	default:
	}
	return nil
}

func save(path string, c Config) error {
	if path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func onOff(b bool) string { return pick(b, "ON", "OFF") }

func fmtTemp(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3f °C", *p)
}
