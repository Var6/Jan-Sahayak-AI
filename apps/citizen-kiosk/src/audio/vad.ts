// Simple energy-based end-of-utterance detector, run on the 16 kHz stream.
//
// It is not a speech/non-speech classifier (the agent runs Silero VAD before
// STT); it only decides when to stop recording:
//   * after speech was heard and then ~1.2 s of trailing silence,
//   * after `noSpeechMs` with no speech at all,
//   * at `maxMs` (hard cap, the agent accepts ≤ 20 s).
// The threshold adapts to the room's noise floor.

export type VadEnd = "silence" | "no_speech" | "max_length";

export interface VadOptions {
  sampleRate?: number;
  frameMs?: number;
  trailingSilenceMs?: number;
  noSpeechMs?: number;
  maxMs?: number;
  /** Speech must be this many dB above the noise floor. */
  marginDb?: number;
  /** Absolute minimum level for speech, dBFS. */
  minSpeechDb?: number;
  /** Consecutive loud frames needed to count as speech onset. */
  onsetFrames?: number;
  /** Initial frames used only to measure the noise floor (minimum level). */
  calibrationFrames?: number;
}

export const VAD_DEFAULTS = {
  sampleRate: 16_000,
  frameMs: 20,
  trailingSilenceMs: 1_200,
  noSpeechMs: 8_000,
  maxMs: 15_000,
  marginDb: 10,
  minSpeechDb: -50,
  onsetFrames: 3,
  calibrationFrames: 8,
} as const satisfies Required<VadOptions>;

const FLOOR_INIT_DB = -60;

export class EnergyVad {
  readonly frameSize: number;
  private readonly o: Required<VadOptions>;
  private readonly frame: Float32Array;
  private fill = 0;
  private frames = 0;
  private floorDb = FLOOR_INIT_DB;
  private loudRun = 0;
  private silentMs = 0;
  private speechStarted = false;
  private ended: VadEnd | null = null;
  private lastDb = -120;

  constructor(opts: VadOptions = {}) {
    this.o = { ...VAD_DEFAULTS, ...opts };
    this.frameSize = Math.round((this.o.sampleRate * this.o.frameMs) / 1000);
    this.frame = new Float32Array(this.frameSize);
  }

  get hasSpeech(): boolean {
    return this.speechStarted;
  }

  get result(): VadEnd | null {
    return this.ended;
  }

  /** Milliseconds of audio consumed so far (whole frames). */
  get elapsedMs(): number {
    return this.frames * this.o.frameMs;
  }

  /** Level of the last frame, 0..1, for a UI meter. */
  get level(): number {
    const l = (this.lastDb + 60) / 50;
    return l < 0 ? 0 : l > 1 ? 1 : l;
  }

  /** Feed samples; returns the end reason once recording should stop. */
  push(samples: Float32Array): VadEnd | null {
    for (let i = 0; i < samples.length && !this.ended; i++) {
      this.frame[this.fill++] = samples[i] as number;
      if (this.fill === this.frameSize) {
        this.fill = 0;
        this.onFrame();
      }
    }
    return this.ended;
  }

  private onFrame(): void {
    this.frames++;
    let sum = 0;
    for (let i = 0; i < this.frameSize; i++) {
      const s = this.frame[i] as number;
      sum += s * s;
    }
    const rms = Math.sqrt(sum / this.frameSize);
    const db = 20 * Math.log10(rms + 1e-9);
    this.lastDb = db;

    if (this.frames <= this.o.calibrationFrames) {
      // Minimum is robust to the citizen starting to speak mid-calibration.
      this.floorDb = this.frames === 1 ? db : Math.min(this.floorDb, db);
      return;
    }

    const threshold = Math.max(this.floorDb + this.o.marginDb, this.o.minSpeechDb);
    const loud = db >= threshold;

    if (loud) {
      this.loudRun++;
      if (!this.speechStarted && this.loudRun >= this.o.onsetFrames) this.speechStarted = true;
      if (this.speechStarted) this.silentMs = 0;
    } else {
      this.loudRun = 0;
      if (this.speechStarted) this.silentMs += this.o.frameMs;
      // Track the noise floor only on quiet frames: fall fast, rise slowly.
      this.floorDb = db < this.floorDb ? 0.7 * this.floorDb + 0.3 * db : 0.98 * this.floorDb + 0.02 * db;
    }

    const elapsed = this.elapsedMs;
    if (this.speechStarted && this.silentMs >= this.o.trailingSilenceMs) this.ended = "silence";
    else if (!this.speechStarted && elapsed >= this.o.noSpeechMs) this.ended = "no_speech";
    else if (elapsed >= this.o.maxMs) this.ended = "max_length";
  }
}
