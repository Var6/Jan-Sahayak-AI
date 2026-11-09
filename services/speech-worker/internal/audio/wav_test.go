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
