package mqtt

import (
	"testing"
	"time"

	"github.com/lab240/napi-donoff/internal/thermo"
)

type sens struct{}

func (sens) Read() (float64, string, error) { return 23.5, "28-test", nil }

type rel struct{}

func (rel) Set(bool) error { return nil }

func TestExecute(t *testing.T) {
	th := thermo.New(sens{}, rel{}, "", time.Second, time.Second, time.Hour)
	for _, c := range []struct{ in, want string }{
		{"ctempl=2650", "ctempl=2650"},
		{"sh ctempl", "ctempl=2650"},
		{"ctempl=25", "ctempl=2500"}, // below 100 = °C, as in donoff-lib
		{"tdelta=150", "tdelta=150"},
		{"tdelta=5", "tdelta=50"}, // below 50 = 1/10 °C
		{"sh tlevels", "C=2500,D=2500,N=2500 dT=50"},
		{"hotter=1", "hotter=1"},
		{"sh hotter", "hotter=1"},
		{"sh cooler", "cooler=0"},
		{"cooler=1;", "cooler=1"},
		{"sh hotter", "hotter=0"},
		{"r11", "r1=1"},
		{"sh mode", "mode=on"},
		{"mode=auto", "mode=auto"},
		{"cooler=0", "cooler=0"},
		{"sh mode", "mode=off"},
		{"tdelta=x", "E:Varable error"},
		{"ctempl=15000", "E:setpoint: -40..100 °C"},
		{"blah", "no command"},
		{"save", "saved"},
	} {
		if got := Execute(th, c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
	if c := th.Status().Config; c.Action != thermo.Cool || c.Setpoint != 25 || c.Hysteresis != 0.5 {
		t.Fatalf("config after the commands: %+v", c)
	}
}

func TestValidate(t *testing.T) {
	c := DefaultConfig()
	if c.Validate() != nil {
		t.Fatal("default (disabled) must be valid")
	}
	c.Enabled = true
	if c.Validate() == nil {
		t.Fatal("enabled without a server must be refused")
	}
	c.Server, c.User, c.DevID = "broker", "u/x", "d1"
	if c.Validate() == nil {
		t.Fatal(`"/" in the user must be refused`)
	}
	c.User = "u"
	if c.Validate() != nil || c.Prefix() != "/u/d1" {
		t.Fatal("prefix")
	}
}
