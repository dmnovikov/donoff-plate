// Package gpio drives one output line through the Linux GPIO character device (uapi v2),
// without libgpiod and without cgo.
package gpio

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// linux/gpio.h, checked on the NanoPi: sizeof(gpio_v2_line_request) = 592, config at 288, fd at 588.
const (
	flagActiveLow = 1 << 1
	flagOutput    = 1 << 3

	attrOutputValues = 2

	ioctlGetLine   = 0xc250b407 // GPIO_V2_GET_LINE_IOCTL
	ioctlGetValues = 0xc010b40e // GPIO_V2_LINE_GET_VALUES_IOCTL
	ioctlSetValues = 0xc010b40f // GPIO_V2_LINE_SET_VALUES_IOCTL
)

type lineAttr struct {
	ID    uint32
	_     uint32
	Value uint64 // flags / values / debounce_period_us
}

type lineConfigAttr struct {
	Attr lineAttr
	Mask uint64
}

type lineConfig struct {
	Flags    uint64
	NumAttrs uint32
	_        [5]uint32
	Attrs    [10]lineConfigAttr
}

type lineRequest struct {
	Offsets         [64]uint32
	Consumer        [32]byte
	Config          lineConfig
	NumLines        uint32
	EventBufferSize uint32
	_               [5]uint32
	Fd              int32
}

type lineValues struct {
	Bits uint64
	Mask uint64
}

func init() {
	if unsafe.Sizeof(lineRequest{}) != 592 || unsafe.Offsetof(lineRequest{}.Fd) != 588 {
		panic("gpio: uapi struct layout mismatch")
	}
}

// Output is one requested output line. Set(true) = active (relay on).
type Output struct {
	mu   sync.Mutex
	fd   int
	chip string
	line int
}

// OpenOutput requests line on chip (e.g. "/dev/gpiochip2", 14) as an output, inactive at first.
func OpenOutput(chip string, line int, activeLow bool, consumer string) (*Output, error) {
	f, err := os.OpenFile(chip, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r lineRequest
	r.Offsets[0] = uint32(line)
	r.NumLines = 1
	copy(r.Consumer[:len(r.Consumer)-1], consumer)
	r.Config.Flags = flagOutput
	if activeLow {
		r.Config.Flags |= flagActiveLow
	}
	r.Config.NumAttrs = 1
	r.Config.Attrs[0] = lineConfigAttr{Attr: lineAttr{ID: attrOutputValues, Value: 0}, Mask: 1}
	if err := ioctl(f.Fd(), ioctlGetLine, unsafe.Pointer(&r)); err != nil {
		return nil, fmt.Errorf("request %s line %d: %w (busy? see: gpioinfo -c %s)", chip, line, err, chip)
	}
	return &Output{fd: int(r.Fd), chip: chip, line: line}, nil
}

func (o *Output) Set(on bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	v := lineValues{Mask: 1}
	if on {
		v.Bits = 1
	}
	return ioctl(uintptr(o.fd), ioctlSetValues, unsafe.Pointer(&v))
}

// Get reads the line back (logical value: true = active).
func (o *Output) Get() (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v := lineValues{Mask: 1}
	if err := ioctl(uintptr(o.fd), ioctlGetValues, unsafe.Pointer(&v)); err != nil {
		return false, err
	}
	return v.Bits&1 == 1, nil
}

func (o *Output) String() string { return fmt.Sprintf("%s line %d", o.chip, o.line) }

func (o *Output) Close() error { return syscall.Close(o.fd) }

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}
