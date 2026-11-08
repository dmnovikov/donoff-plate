package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lab240/napi-donoff/internal/thermo"
)

type sens struct{}

func (sens) Read() (float64, string, error) { return 25, "28-test", nil }

type rel struct{}

func (rel) Set(bool) error { return nil }

func TestAccess(t *testing.T) {
	nets, err := ParseNets("192.168.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	th := thermo.New(sens{}, rel{}, "", time.Second, time.Second, time.Hour)
	for _, c := range []struct {
		token, remote, auth string
		want                int
	}{
		{"", "127.0.0.1:5000", "", 200},
		{"", "192.168.30.56:5000", "", 200},
		{"", "10.0.0.5:5000", "", 403},
		{"s3cret", "10.0.0.5:5000", "", 401},
		{"s3cret", "10.0.0.5:5000", "Bearer s3cret", 200},
		{"s3cret", "[::ffff:192.168.1.2]:5000", "", 200},
	} {
		h := New(th, nil, c.token, nets, "test").Handler()
		r := httptest.NewRequest("GET", "/api/v1/status", nil)
		r.RemoteAddr = c.remote
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%+v: got %d", c, w.Code)
		}
	}
}

func TestPutConfig(t *testing.T) {
	th := thermo.New(sens{}, rel{}, "", time.Second, time.Second, time.Hour)
	h := New(th, nil, "", nil, "test").Handler()
	put := func(body string) int {
		r := httptest.NewRequest("PUT", "/api/v1/config", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if put(`{"setpoint": 28.5}`) != http.StatusOK || th.Status().Setpoint != 28.5 || th.Status().Hysteresis != 2 {
		t.Fatal("partial setpoint change")
	}
	if put(`{"mode": "turbo"}`) != http.StatusBadRequest || put(`{"hysteresis": -1}`) != http.StatusBadRequest {
		t.Fatal("bad values must be refused")
	}
}
