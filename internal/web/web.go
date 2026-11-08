// Package web is the page and the JSON API of the thermostat.
//
// Clients from the trusted networks (default 127.0.0.0/8, ::1, 192.168.0.0/16) need no token;
// any other address needs "Authorization: Bearer <token>", and is refused when no token is set.
//
//	GET  /                          the page
//	GET  /api/v1/status             temperature, fan, config, counters
//	GET  /api/v1/history[?since=T]  samples newer than unix time T
//	PUT  /api/v1/config             change the fields given: {"setpoint": 26, "hysteresis": 2, "mode": "auto"|"on"|"off",
//	                                "action": "cool"|"heat"}
//	GET  /mqtt                      the MQTT settings page
//	GET  /api/v1/mqtt               MQTT settings (no password, only "password_set"), state, topics
//	PUT  /api/v1/mqtt               change the fields given: {"enabled": true, "server": "...", "port": 1883,
//	                                "user": "...", "password": "...", "dev_id": "...", "interval": 10}
package web

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/lab240/napi-donoff/internal/mqtt"
	"github.com/lab240/napi-donoff/internal/thermo"
)

//go:embed web/index.html
var page []byte

//go:embed web/mqtt.html
var mqttPage []byte

type Server struct {
	th      *thermo.Thermostat
	mq      *mqtt.Client
	token   string
	trusted []netip.Prefix
	version string
}

// ParseNets parses "127.0.0.0/8,192.168.0.0/16".
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

func New(th *thermo.Thermostat, mq *mqtt.Client, token string, trusted []netip.Prefix, version string) *Server {
	return &Server{th: th, mq: mq, token: token, trusted: trusted, version: version}
}

func html(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	}
}

type mqttView struct {
	Enabled     bool        `json:"enabled"`
	Server      string      `json:"server"`
	Port        int         `json:"port"`
	User        string      `json:"user"`
	PasswordSet bool        `json:"password_set"`
	DevID       string      `json:"dev_id"`
	Interval    int         `json:"interval"`
	Prefix      string      `json:"prefix"`
	Status      mqtt.Status `json:"status"`
	Channels    any         `json:"channels"`
}

func (s *Server) mqttView() mqttView {
	c := s.mq.Config()
	return mqttView{c.Enabled, c.Server, c.Port, c.User, c.Password != "", c.DevID, c.Interval,
		c.Prefix(), s.mq.Status(), mqtt.Channels}
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", html(page))
	m.HandleFunc("GET /mqtt", html(mqttPage))
	m.HandleFunc("GET /api/v1/mqtt", func(w http.ResponseWriter, r *http.Request) {
		if s.mq == nil {
			fail(w, http.StatusNotFound, "mqtt is not built in")
			return
		}
		reply(w, http.StatusOK, s.mqttView())
	})
	m.HandleFunc("PUT /api/v1/mqtt", func(w http.ResponseWriter, r *http.Request) {
		if s.mq == nil {
			fail(w, http.StatusNotFound, "mqtt is not built in")
			return
		}
		var in struct {
			Enabled  *bool   `json:"enabled"`
			Server   *string `json:"server"`
			Port     *int    `json:"port"`
			User     *string `json:"user"`
			Password *string `json:"password"`
			DevID    *string `json:"dev_id"`
			Interval *int    `json:"interval"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "bad JSON: "+err.Error())
			return
		}
		c := s.mq.Config()
		str := func(p *string, dst *string) {
			if p != nil {
				*dst = strings.TrimSpace(*p)
			}
		}
		if in.Enabled != nil {
			c.Enabled = *in.Enabled
		}
		str(in.Server, &c.Server)
		str(in.User, &c.User)
		str(in.DevID, &c.DevID)
		if in.Password != nil {
			c.Password = *in.Password
		}
		if in.Port != nil {
			c.Port = *in.Port
		}
		if in.Interval != nil {
			c.Interval = *in.Interval
		}
		if err := s.mq.SetConfig(c); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("mqtt config changed from %s", r.RemoteAddr)
		reply(w, http.StatusOK, s.mqttView())
	})
	m.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		st := s.th.Status()
		reply(w, http.StatusOK, struct {
			thermo.Status
			Version string    `json:"version"`
			Now     time.Time `json:"now"`
		}{st, s.version, time.Now()})
	})
	m.HandleFunc("GET /api/v1/history", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		h := s.th.History()
		i := 0
		for i < len(h) && h[i].T <= since {
			i++
		}
		reply(w, http.StatusOK, h[i:])
	})
	m.HandleFunc("PUT /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Setpoint   *float64       `json:"setpoint"`
			Hysteresis *float64       `json:"hysteresis"`
			Mode       *thermo.Mode   `json:"mode"`
			Action     *thermo.Action `json:"action"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "bad JSON: "+err.Error())
			return
		}
		c := s.th.Status().Config
		if in.Setpoint != nil {
			c.Setpoint = *in.Setpoint
		}
		if in.Hysteresis != nil {
			c.Hysteresis = *in.Hysteresis
		}
		if in.Mode != nil {
			c.Mode = *in.Mode
		}
		if in.Action != nil {
			c.Action = *in.Action
		}
		if err := s.th.SetConfig(c); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("config changed from %s", r.RemoteAddr)
		reply(w, http.StatusOK, c)
	})
	return s.auth(m)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isTrusted(r.RemoteAddr) || r.URL.Path == "/" || r.URL.Path == "/mqtt" {
			next.ServeHTTP(w, r)
			return
		}
		if s.token == "" {
			fail(w, http.StatusForbidden, "this address is not in the trusted networks and no token is set")
			return
		}
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			fail(w, http.StatusUnauthorized, "missing or wrong token (Authorization: Bearer ...)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) isTrusted(remote string) bool {
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
	for _, p := range s.trusted {
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

func fail(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}
