package audio

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := []float32{0, 0.5, -0.5, 1, -1, 2, -2}
	b := EncodeMono16(in, 16000)
	if len(b) != 44+2*len(in) {
		t.Fatalf("len=%d", len(b))
	}
	out, info, err := DecodeMono16(b, 16000, 20)
	if err != nil {
		t.Fatal(err)
	}
	if info.Samples != len(in) || info.SampleRate != 16000 {
		t.Fatalf("info=%+v", info)
	}
	want := []float32{0, 0.5, -0.5, 1, -1, 1, -1} // clipped
	for i := range want {
		if math.Abs(float64(out[i]-want[i])) > 1e-3 {
			t.Fatalf("sample %d: got %v want %v", i, out[i], want[i])
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	good := EncodeMono16(make([]float32, 16000), 16000)
	cases := map[string]struct {
		b    []byte
		rate int
		max  float64
		want error
	}{
		"short":      {b: []byte("RIFF"), rate: 16000, want: ErrBadWAV},
		"not wave":   {b: append([]byte("RIFF\x00\x00\x00\x00JUNK"), make([]byte, 40)...), rate: 16000, want: ErrBadWAV},
		"wrong rate": {b: EncodeMono16(make([]float32, 100), 44100), rate: 16000, want: ErrBadWAV},
		"too long":   {b: good, rate: 16000, max: 0.5, want: ErrTooLong},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := DecodeMono16(c.b, c.rate, c.max)
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v want %v", err, c.want)
			}
		})
	}
}

func TestStereoRejected(t *testing.T) {
	b := EncodeMono16(make([]float32, 100), 16000)
	binary.LittleEndian.PutUint16(b[22:], 2)
	if _, _, err := DecodeMono16(b, 16000, 0); !errors.Is(err, ErrBadWAV) {
		t.Fatalf("err=%v", err)
	}
}

func TestBogusDataSizeClamped(t *testing.T) {
	b := EncodeMono16(make([]float32, 100), 16000)
	binary.LittleEndian.PutUint32(b[40:], 0xFFFFFFFF)
	out, _, err := DecodeMono16(b, 16000, 0)
	if err != nil || len(out) != 100 {
		t.Fatalf("len=%d err=%v", len(out), err)
	}
}

func TestExtraChunkSkipped(t *testing.T) {
	b := EncodeMono16(make([]float32, 10), 16000)
	// insert a LIST chunk between fmt and data
	list := append([]byte("LIST\x04\x00\x00\x00"), []byte("abcd")...)
	nb := append(append(append([]byte{}, b[:36]...), list...), b[36:]...)
	out, _, err := DecodeMono16(nb, 16000, 0)
	if err != nil || len(out) != 10 {
		t.Fatalf("len=%d err=%v", len(out), err)
	}
}

func FuzzParseHeader(f *testing.F) {
	f.Add(EncodeMono16(make([]float32, 8), 16000))
	f.Add([]byte("RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = DecodeMono16(b, 16000, 20)
	})
}

func TestDownsampleHalvesAndFilters(t *testing.T) {
	const rate = 44100
	n := rate / 10
	low := make([]float32, n)  // 440 Hz: must survive
	high := make([]float32, n) // 18 kHz: above the new Nyquist, must be attenuated
	for i := range low {
		low[i] = float32(math.Sin(2 * math.Pi * 440 * float64(i) / rate))
		high[i] = float32(math.Sin(2 * math.Pi * 18000 * float64(i) / rate))
	}
	rms := func(x []float32) float64 {
		s := 0.0
		for _, v := range x[50 : len(x)-50] {
			s += float64(v) * float64(v)
		}
		return math.Sqrt(s / float64(len(x)-100))
	}
	lo, r := Downsample(low, rate, 22050)
	if r != 22050 || len(lo) != n/2 {
		t.Fatalf("rate=%d len=%d", r, len(lo))
	}
	hi, _ := Downsample(high, rate, 22050)
	if rms(lo) < 0.6 || rms(hi) > 0.1 {
		t.Fatalf("passband rms %.3f, stopband rms %.3f", rms(lo), rms(hi))
	}
	if same, r := Downsample(low, 22050, 22050); r != 22050 || len(same) != len(low) {
		t.Fatal("22.05 kHz input must be unchanged")
	}
	if same, r := Downsample(low, 24000, 22050); r != 24000 || len(same) != len(low) {
		t.Fatal("rate below 2× max must be unchanged")
	}
}
