// Downsampling to 16 kHz mono and WAV (RIFF, PCM 16-bit) encoding, all in
// memory. Nothing here touches storage.

export const TARGET_RATE = 16_000;

/**
 * Streaming windowed-sinc (Blackman) FIR low-pass filter. History is kept
 * across chunks, so chunked and one-shot processing give identical output.
 */
export class LowPassFir {
  private readonly taps: Float32Array;
  private readonly hist: Float32Array; // last taps-1 input samples

  /** @param cutoff cutoff frequency as a fraction of the sample rate (0..0.5) */
  constructor(cutoff: number, numTaps = 33) {
    if (!(cutoff > 0 && cutoff < 0.5)) throw new RangeError("cutoff must be in (0, 0.5)");
    const n = numTaps | 1; // odd → symmetric, integer group delay
    const m = n - 1;
    const taps = new Float32Array(n);
    let sum = 0;
    for (let i = 0; i < n; i++) {
      const x = i - m / 2;
      const sinc = x === 0 ? 2 * cutoff : Math.sin(2 * Math.PI * cutoff * x) / (Math.PI * x);
      const w =
        0.42 - 0.5 * Math.cos((2 * Math.PI * i) / m) + 0.08 * Math.cos((4 * Math.PI * i) / m);
      taps[i] = sinc * w;
      sum += sinc * w;
    }
    for (let i = 0; i < n; i++) taps[i] = (taps[i] as number) / sum; // unity DC gain
    this.taps = taps;
    this.hist = new Float32Array(m);
  }

  process(input: Float32Array): Float32Array {
    const m = this.hist.length;
    const n = this.taps.length;
    // Work buffer = history followed by the new chunk.
    const buf = new Float32Array(m + input.length);
    buf.set(this.hist, 0);
    buf.set(input, m);
    const out = new Float32Array(input.length);
    for (let i = 0; i < input.length; i++) {
      let acc = 0;
      for (let k = 0; k < n; k++) acc += (this.taps[k] as number) * (buf[i + k] as number);
      out[i] = acc;
    }
    this.hist.set(buf.subarray(buf.length - m));
    return out;
  }
}

/**
 * Streaming downsampler: anti-aliasing FIR low-pass (cutoff 0.45 × outRate,
 * i.e. 7.2 kHz for 16 kHz output) followed by fractional box decimation —
 * each output sample is the mean of the input samples in its time window.
 * The fractional position is carried across chunks, so chunk boundaries do
 * not drift.
 */
export class Downsampler {
  private readonly ratio: number;
  private readonly fir: LowPassFir | null;
  private acc = 0; // sum of samples in the current window
  private accN = 0; // number of samples (fractional weights) in the window
  private pos = 0; // position within the current window, in input samples

  constructor(
    readonly inRate: number,
    readonly outRate: number = TARGET_RATE,
  ) {
    if (!(inRate > 0) || !(outRate > 0)) throw new RangeError("sample rates must be positive");
    if (outRate > inRate) throw new RangeError("upsampling is not supported");
    this.ratio = inRate / outRate;
    this.fir = inRate === outRate ? null : new LowPassFir((0.45 * outRate) / inRate);
  }

  /**
   * Process one chunk and return the produced output samples (float, -1..1).
   * Allocates at most ceil(len / ratio) + 1 samples per call.
   */
  process(chunk: Float32Array): Float32Array {
    const input = this.fir ? this.fir.process(chunk) : chunk;
    const out = new Float32Array(Math.ceil(input.length / this.ratio) + 1);
    let n = 0;
    const r = this.ratio;
    for (let i = 0; i < input.length; i++) {
      const x = input[i] as number;
      const room = r - this.pos; // input samples (fractional) left in this window
      if (room > 1) {
        this.acc += x;
        this.accN += 1;
        this.pos += 1;
        continue;
      }
      // The sample straddles the window boundary: split its weight.
      this.acc += x * room;
      this.accN += room;
      out[n++] = this.accN > 0 ? this.acc / this.accN : 0;
      const rest = 1 - room;
      this.acc = x * rest;
      this.accN = rest;
      this.pos = rest;
    }
    return out.subarray(0, n);
  }
}

/** One-shot helper for a complete buffer. */
export function downsample(
  input: Float32Array,
  inRate: number,
  outRate = TARGET_RATE,
): Float32Array {
  if (inRate === outRate) return input.slice();
  return new Downsampler(inRate, outRate).process(input);
}

/** Convert float samples (-1..1, clipped) to signed 16-bit PCM. */
export function floatToInt16(
  input: Float32Array,
  out: Int16Array = new Int16Array(input.length),
): Int16Array {
  for (let i = 0; i < input.length; i++) {
    let s = input[i] as number;
    if (Number.isNaN(s)) s = 0;
    s = Math.min(1, Math.max(-1, s));
    out[i] = s < 0 ? Math.round(s * 0x8000) : Math.round(s * 0x7fff);
  }
  return out;
}

export const WAV_HEADER_BYTES = 44;

/** Encode mono 16-bit PCM as a WAV file. */
export function encodeWav(pcm: Int16Array, sampleRate = TARGET_RATE): ArrayBuffer {
  const dataBytes = pcm.length * 2;
  const buf = new ArrayBuffer(WAV_HEADER_BYTES + dataBytes);
  const v = new DataView(buf);
  const ascii = (off: number, s: string) => {
    for (let i = 0; i < s.length; i++) v.setUint8(off + i, s.charCodeAt(i));
  };
  ascii(0, "RIFF");
  v.setUint32(4, 36 + dataBytes, true);
  ascii(8, "WAVE");
  ascii(12, "fmt ");
  v.setUint32(16, 16, true); // fmt chunk size
  v.setUint16(20, 1, true); // PCM
  v.setUint16(22, 1, true); // mono
  v.setUint32(24, sampleRate, true);
  v.setUint32(28, sampleRate * 2, true); // byte rate
  v.setUint16(32, 2, true); // block align
  v.setUint16(34, 16, true); // bits per sample
  ascii(36, "data");
  v.setUint32(40, dataBytes, true);
  let off = WAV_HEADER_BYTES;
  for (let i = 0; i < pcm.length; i++, off += 2) v.setInt16(off, pcm[i] as number, true);
  return buf;
}

export function wavBlob(pcm: Int16Array, sampleRate = TARGET_RATE): Blob {
  return new Blob([encodeWav(pcm, sampleRate)], { type: "audio/wav" });
}
