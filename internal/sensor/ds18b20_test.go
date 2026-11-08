package sensor

import "testing"

func TestParse(t *testing.T) {
	for _, c := range []struct {
		text string
		want float64
		ok   bool
	}{
		{"69 01 4b 46 7f ff 0c 10 7d : crc=7d YES\n69 01 4b 46 7f ff 0c 10 7d t=22562\n", 22.562, true},
		{"f8 ff 4b 46 7f ff 08 10 ef : crc=ef YES\nf8 ff 4b 46 7f ff 08 10 ef t=-500\n", -0.5, true},
		{"50 05 4b 46 7f ff 0c 10 1c : crc=1c YES\n50 05 4b 46 7f ff 0c 10 1c t=85000\n", 0, false},
		{"00 00 00 00 00 00 00 00 00 : crc=00 YES\n00 00 00 00 00 00 00 00 00 t=0\n", 0, false},
		{"ff ff ff ff ff ff ff ff ff : crc=c9 NO\nff ff ff ff ff ff ff ff ff t=-62\n", 0, false},
		{"", 0, false},
	} {
		got, err := parse(c.text)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%q: %v, %v", c.text, got, err)
		}
	}
}
