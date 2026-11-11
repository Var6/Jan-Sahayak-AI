import { describe, expect, it } from "vitest";
import { Downsampler, downsample, encodeWav, floatToInt16, WAV_HEADER_BYTES } from "./wav";

const sine = (hz: number, rate: number, n: number, amp = 0.5) =>
  Float32Array.from({ length: n }, (_, i) => amp * Math.sin((2 * Math.PI * hz * i) / rate));

describe("encodeWav", () => {
  it("writes a valid 16 kHz mono 16-bit RIFF header", () => {
    const pcm = Int16Array.from([0, 1, -1, 32767, -32768]);
    const buf = encodeWav(pcm, 16_000);
    const v = new DataView(buf);
    const str = (o: number, n: number) =>
      String.fromCharCode(...new Uint8Array(buf.slice(o, o + n)));
    expect(buf.byteLength).toBe(WAV_HEADER_BYTES + pcm.length * 2);
    expect(str(0, 4)).toBe("RIFF");
    expect(v.getUint32(4, true)).toBe(36 + pcm.length * 2);
    expect(str(8, 4)).toBe("WAVE");
    expect(str(12, 4)).toBe("fmt ");
    expect(v.getUint32(16, true)).toBe(16);
    expect(v.getUint16(20, true)).toBe(1); // PCM
    expect(v.getUint16(22, true)).toBe(1); // mono
    expect(v.getUint32(24, true)).toBe(16_000);
    expect(v.getUint32(28, true)).toBe(32_000);
    expect(v.getUint16(32, true)).toBe(2);
    expect(v.getUint16(34, true)).toBe(16);
    expect(str(36, 4)).toBe("data");
    expect(v.getUint32(40, true)).toBe(pcm.length * 2);
    const samples = Array.from(pcm, (_, i) => v.getInt16(WAV_HEADER_BYTES + i * 2, true));
    expect(samples).toEqual([0, 1, -1, 32767, -32768]);
  });

  it("encodes an empty buffer as a header-only file", () => {
    expect(encodeWav(new Int16Array(0)).byteLength).toBe(WAV_HEADER_BYTES);
  });

  it("15 s of audio stays under the agent's 1 MB upload limit", () => {
    expect(encodeWav(new Int16Array(16_000 * 15)).byteLength).toBeLessThan(1024 * 1024);
  });
});

describe("floatToInt16", () => {
  it("scales, clips and zeroes NaN", () => {
    const out = floatToInt16(Float32Array.from([0, 1, -1, 2, -2, 0.5, Number.NaN]));
    expect(Array.from(out)).toEqual([0, 32767, -32768, 32767, -32768, 16384, 0]);
  });
});

describe("Downsampler", () => {
  it.each([48_000, 44_100, 32_000, 22_050])("produces the right length from %i Hz", (rate) => {
    const n = rate * 2; // 2 s
    const out = downsample(new Float32Array(n), rate, 16_000);
    expect(Math.abs(out.length - 32_000)).toBeLessThanOrEqual(1);
  });

  it("is a no-op copy at equal rates", () => {
    const x = Float32Array.from([0.1, 0.2, 0.3]);
    const y = downsample(x, 16_000, 16_000);
    expect(Array.from(y)).toEqual(Array.from(x));
    expect(y).not.toBe(x);
  });

  it("preserves DC level (after the filter's start-up transient)", () => {
    const out = downsample(new Float32Array(4_800).fill(0.25), 48_000);
    for (const v of out.subarray(16)) expect(v).toBeCloseTo(0.25, 5);
  });

  it("keeps a 440 Hz tone and attenuates content near the input Nyquist", () => {
    const rms = (a: Float32Array) => Math.sqrt(a.reduce((s, v) => s + v * v, 0) / a.length);
    const low = downsample(sine(440, 48_000, 48_000), 48_000);
    const high = downsample(sine(23_000, 48_000, 48_000), 48_000);
    expect(rms(low)).toBeGreaterThan(0.3); // ~0.354 for amp 0.5
    expect(rms(high)).toBeLessThan(0.005); // ≥ 40 dB down (anti-alias FIR)
  });

  it("gives the same result in chunks as in one pass (no drift at boundaries)", () => {
    const x = sine(300, 44_100, 44_100);
    const whole = downsample(x, 44_100);
    const ds = new Downsampler(44_100);
    const parts: number[] = [];
    for (let i = 0; i < x.length; i += 997) parts.push(...ds.process(x.subarray(i, i + 997)));
    expect(parts.length).toBe(whole.length);
    for (let i = 0; i < whole.length; i++) expect(parts[i]).toBeCloseTo(whole[i] as number, 5);
  });

  it("rejects upsampling and bad rates", () => {
    expect(() => new Downsampler(8_000, 16_000)).toThrow(RangeError);
    expect(() => new Downsampler(0)).toThrow(RangeError);
  });
});
