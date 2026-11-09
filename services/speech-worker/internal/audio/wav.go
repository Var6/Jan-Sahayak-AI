// Package audio converts between RIFF/WAVE PCM bytes and float32 samples.
// Everything happens in memory; nothing is written to disk.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrBadWAV is returned for input that is not a PCM WAV file of the
// expected format.
var ErrBadWAV = errors.New("audio: not a 16-bit PCM mono WAV")

// Info describes a decoded WAV header.
type Info struct {
	SampleRate int
	Channels   int
	Bits       int
	Samples    int
}

// Seconds is the clip duration.
func (i Info) Seconds() float64 {
	if i.SampleRate <= 0 {
		return 0
	}
	return float64(i.Samples) / float64(i.SampleRate)
}

// ParseHeader walks the RIFF chunks and returns the format and the byte
// range of the data chunk. It tolerates a bogus data-chunk size (streamed
// writers often put 0 or 0xFFFFFFFF there) by clamping to the file end.
func ParseHeader(b []byte) (info Info, dataOff, dataLen int, err error) {
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return info, 0, 0, ErrBadWAV
	}
	off := 12
	gotFmt := false
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if size < 0 || body+size > len(b) {
			if id != "data" {
				return info, 0, 0, ErrBadWAV
			}
			size = len(b) - body
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return info, 0, 0, ErrBadWAV
			}
			format := binary.LittleEndian.Uint16(b[body:])
			info.Channels = int(binary.LittleEndian.Uint16(b[body+2:]))
			info.SampleRate = int(binary.LittleEndian.Uint32(b[body+4:]))
			info.Bits = int(binary.LittleEndian.Uint16(b[body+14:]))
			if format != 1 && format != 0xFFFE { // PCM or WAVE_FORMAT_EXTENSIBLE
				return info, 0, 0, ErrBadWAV
			}
			gotFmt = true
		case "data":
			if !gotFmt {
				return info, 0, 0, ErrBadWAV
			}
			size -= size % 2
			if info.Channels > 0 && info.Bits > 0 {
				info.Samples = size / (info.Channels * info.Bits / 8)
			}
			return info, body, size, nil
		}
		off = body + size + size%2
	}
	return info, 0, 0, ErrBadWAV
}

// DecodeMono16 decodes a 16-bit PCM mono WAV at wantRate into samples
// normalised to [-1, 1). maxSecs bounds the work (0 = unbounded).
func DecodeMono16(b []byte, wantRate int, maxSecs float64) ([]float32, Info, error) {
	info, off, n, err := ParseHeader(b)
	if err != nil {
		return nil, info, err
	}
	if info.Channels != 1 || info.Bits != 16 || info.SampleRate != wantRate {
		return nil, info, fmt.Errorf("%w (got %d Hz, %d ch, %d bit; want %d Hz mono 16-bit)",
			ErrBadWAV, info.SampleRate, info.Channels, info.Bits, wantRate)
	}
	if maxSecs > 0 && info.Seconds() > maxSecs {
		return nil, info, fmt.Errorf("%w: %.1f s exceeds %.0f s", ErrTooLong, info.Seconds(), maxSecs)
	}
	out := make([]float32, n/2)
	for i := range out {
		v := int16(binary.LittleEndian.Uint16(b[off+2*i:]))
		out[i] = float32(v) / 32768
	}
	return out, info, nil
}

// ErrTooLong is returned when a clip exceeds the configured duration.
var ErrTooLong = errors.New("audio: clip too long")

// EncodeMono16 renders samples in [-1, 1] as a canonical 44-byte-header
// 16-bit PCM mono WAV. Out-of-range samples are clipped.
func EncodeMono16(samples []float32, rate int) []byte {
	dataLen := 2 * len(samples)
	b := make([]byte, 44+dataLen)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+dataLen))
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // mono
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(dataLen))
	for i, s := range samples {
		v := float64(s) * 32767
		if math.IsNaN(v) {
			v = 0
		}
		v = math.Max(-32768, math.Min(32767, math.Round(v)))
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(int16(v)))
	}
	return b
}
