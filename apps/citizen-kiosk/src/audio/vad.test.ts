import { describe, expect, it } from "vitest";
import { CaptureBuffer } from "./recorder";
import { EnergyVad } from "./vad";

const RATE = 16_000;
const ms = (n: number) => Math.round((RATE * n) / 1000);

/** Deterministic pseudo-random noise (LCG) so tests are repeatable. */
function noise(n: number, amp: number, seed = 1): Float32Array {
  let x = seed;
  return Float32Array.from({ length: n }, () => {
    x = (x * 1664525 + 1013904223) >>> 0;
    return ((x / 0xffffffff) * 2 - 1) * amp;
  });
}

function tone(n: number, amp: number, hz = 220): Float32Array {
  return Float32Array.from({ length: n }, (_, i) => amp * Math.sin((2 * Math.PI * hz * i) / RATE));
}

function concat(...parts: Float32Array[]): Float32Array {
  const out = new Float32Array(parts.reduce((s, p) => s + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

/** Feed in 128-sample chunks like an audio render quantum; return end time. */
function run(vad: EnergyVad, signal: Float32Array): { end: string | null; atMs: number } {
  for (let i = 0; i < signal.length; i += 128) {
    const end = vad.push(signal.subarray(i, i + 128));
    if (end) return { end, atMs: vad.elapsedMs };
  }
  return { end: null, atMs: vad.elapsedMs };
}

describe("EnergyVad", () => {
  it("stops ~1.2 s after speech ends", () => {
    const vad = new EnergyVad();
    const sig = concat(noise(ms(500), 0.002), tone(ms(1500), 0.3), noise(ms(3000), 0.002, 7));
    const r = run(vad, sig);
    expect(r.end).toBe("silence");
    expect(vad.hasSpeech).toBe(true);
    // speech ends at 2000 ms → stop at ≈3200 ms
    expect(r.atMs).toBeGreaterThanOrEqual(3150);
    expect(r.atMs).toBeLessThanOrEqual(3300);
  });

  it("does not stop on short pauses inside speech", () => {
    const vad = new EnergyVad();
    const sig = concat(
      noise(ms(300), 0.002),
      tone(ms(800), 0.3),
      noise(ms(700), 0.002, 3), // pause shorter than 1.2 s
      tone(ms(800), 0.3),
      noise(ms(2000), 0.002, 5),
    );
    const r = run(vad, sig);
    expect(r.end).toBe("silence");
    // second phrase ends at 2600 ms → stop at ≈3800 ms, not during the pause
    expect(r.atMs).toBeGreaterThan(3700);
  });

  it("gives up after the no-speech timeout", () => {
    const vad = new EnergyVad({ noSpeechMs: 3000 });
    const r = run(vad, noise(ms(5000), 0.003));
    expect(r.end).toBe("no_speech");
    expect(vad.hasSpeech).toBe(false);
    expect(r.atMs).toBe(3000);
  });

  it("caps continuous speech at maxMs", () => {
    const vad = new EnergyVad({ maxMs: 15_000 });
    const r = run(vad, concat(noise(ms(200), 0.002), tone(ms(16_000), 0.3)));
    expect(r.end).toBe("max_length");
    expect(r.atMs).toBe(15_000);
  });

  it("adapts to a noisy room (steady fan noise is not speech)", () => {
    const vad = new EnergyVad({ noSpeechMs: 4000 });
    const fan = noise(ms(5000), 0.05); // about -31 dBFS, well above minSpeechDb
    expect(run(vad, fan).end).toBe("no_speech");
  });

  it("detects speech over room noise", () => {
    const vad = new EnergyVad();
    const room = 0.03;
    const sig = concat(
      noise(ms(400), room),
      Float32Array.from(tone(ms(1200), 0.4), (v, i) => v + (noise(1, room, i + 9)[0] as number)),
      noise(ms(2000), room, 11),
    );
    const r = run(vad, sig);
    expect(r.end).toBe("silence");
    expect(vad.hasSpeech).toBe(true);
  });

  it("exposes a 0..1 level", () => {
    const vad = new EnergyVad();
    vad.push(tone(ms(400), 0.5));
    expect(vad.level).toBeGreaterThan(0.5);
    expect(vad.level).toBeLessThanOrEqual(1);
  });
});

describe("CaptureBuffer", () => {
  it("downsamples 48 kHz input, stops on silence and yields a 16 kHz WAV", async () => {
    const cap = new CaptureBuffer(48_000);
    const r48 = (n: number) => Math.round((48_000 * n) / 1000);
    const t48 = (n: number, amp: number) =>
      Float32Array.from({ length: n }, (_, i) => amp * Math.sin((2 * Math.PI * 220 * i) / 48_000));
    const sig = concat(
      new Float32Array(r48(300)),
      t48(r48(1000), 0.3),
      new Float32Array(r48(3000)),
    );
    let end: string | null = null;
    for (let i = 0; i < sig.length && !end; i += 2048) end = cap.push(sig.subarray(i, i + 2048));
    expect(end).toBe("silence");
    expect(cap.durationMs).toBeGreaterThan(2400);
    expect(cap.durationMs).toBeLessThan(2700);
    const wav = cap.toWav();
    expect(wav.type).toBe("audio/wav");
    expect(wav.size).toBe(44 + cap.samples * 2);
    const head = new DataView(await wav.slice(0, 44).arrayBuffer());
    expect(head.getUint32(24, true)).toBe(16_000);
  });

  it("never exceeds its preallocated bound", () => {
    const cap = new CaptureBuffer(16_000, { maxMs: 1000, noSpeechMs: 60_000 });
    let end: string | null = null;
    // Loud tone → speech, never silent; feed 3 s in one oversized chunk.
    end = cap.push(tone(ms(3000), 0.3));
    expect(end).toBe("max_length");
    expect(cap.samples).toBeLessThanOrEqual(16_000 + 4096);
  });
});
