// Package sensor reads a DS18B20 through the kernel w1_therm driver (sysfs).
package sensor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const w1Devices = "/sys/bus/w1/devices"

// DS18B20 is one sensor; ID "" = the first 28-* found on the bus.
type DS18B20 struct {
	ID string
}

// Find returns the IDs of all DS18B20 (family 28) on the 1-Wire buses.
func Find() []string {
	m, _ := filepath.Glob(filepath.Join(w1Devices, "28-*"))
	ids := make([]string, 0, len(m))
	for _, p := range m {
		ids = append(ids, filepath.Base(p))
	}
	return ids
}

func (s *DS18B20) id() (string, error) {
	if s.ID != "" {
		return s.ID, nil
	}
	ids := Find()
	if len(ids) == 0 {
		return "", errors.New("no DS18B20 on the 1-Wire bus (" + w1Devices + "/28-*)")
	}
	return ids[0], nil
}

// Read does one conversion (~750 ms at 12 bit) and returns °C.
// A bad CRC, a missing sensor and the power-on value 85.000 are errors.
// It tries up to 3 times before giving up, so one noisy read is not a fault.
func (s *DS18B20) Read() (float64, string, error) {
	id, err := s.id()
	if err != nil {
		return 0, "", err
	}
	var last error
	for range 3 {
		t, err := readOnce(filepath.Join(w1Devices, id, "w1_slave"))
		if err == nil {
			return t, id, nil
		}
		last = err
	}
	return 0, id, fmt.Errorf("%s: %w", id, last)
}

func readOnce(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, errors.New("sensor is gone")
	}
	if err != nil {
		return 0, err
	}
	return parse(string(b))
}

// parse is the w1_slave text:
//
//	50 05 4b 46 7f ff 0c 10 1c : crc=1c YES
//	50 05 4b 46 7f ff 0c 10 1c t=85000
func parse(text string) (float64, error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) < 2 || !strings.HasSuffix(lines[0], "YES") {
		return 0, errors.New("CRC error")
	}
	// A bus glitch reads 9 zero bytes, and their CRC is 0 too, so the driver says YES and t=0.
	// Byte 4 is the config register: bits 0-4 always read 1 (0x1f, 0x3f, 0x5f, 0x7f).
	if f := strings.Fields(lines[0]); len(f) < 9 || !configOK(f[4]) {
		return 0, errors.New("bad scratchpad (bus glitch): " + lines[0])
	}
	i := strings.LastIndex(lines[1], "t=")
	if i < 0 {
		return 0, errors.New("no t=")
	}
	milli, err := strconv.Atoi(lines[1][i+2:])
	if err != nil {
		return 0, err
	}
	switch {
	case milli == 85000:
		return 0, errors.New("85.000 °C: power-on value, no conversion")
	case milli < -55000 || milli > 125000:
		return 0, fmt.Errorf("out of range: %d", milli)
	}
	return float64(milli) / 1000, nil
}

func configOK(hex string) bool {
	v, err := strconv.ParseUint(hex, 16, 8)
	return err == nil && v&0x9f == 0x1f
}
