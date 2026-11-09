// Microphone capture for one utterance at a time.
//
// Privacy rules enforced here:
//   * getUserMedia is called only from record(), which the controller calls
//     only after the agent has put the session in LISTENING;
//   * audio stays in a bounded in-memory buffer (≤ maxMs of 16 kHz Int16)
//     and is handed to the caller as a WAV Blob — never persisted;
//   * the mic stream is released (tracks.stop()) as soon as the utterance
//     ends, is stopped, or is aborted.

import workletUrl from "./capture-worklet.js?url";
import { EnergyVad, VAD_DEFAULTS, type VadEnd, type VadOptions } from "./vad";
import { Downsampler, TARGET_RATE, floatToInt16, wavBlob } from "./wav";

export type RecordEnd = VadEnd | "stopped";

export interface RecordResult {
  wav: Blob;
  durationMs: number;
  end: RecordEnd;
  hadSpeech: boolean;
}

export interface RecordOptions {
  onLevel?: (level: number) => void;
}

export interface Recorder {
  /** Open the mic, capture one utterance, release the mic. */
  record(opts?: RecordOptions): Promise<RecordResult>;
  /** Finish early and deliver what was captured. */
  stop(): void;
  /** Discard the utterance and release the mic; record() rejects with MicError("aborted"). */
  abort(): void;
}

export type MicErrorCode = "unsupported" | "denied" | "no_device" | "busy" | "aborted" | "failed";

export class MicError extends Error {
  constructor(
    readonly code: MicErrorCode,
    message?: string,
  ) {
    super(message ?? code);
    this.name = "MicError";
  }
}

/**
 * Pure capture pipeline: native-rate float chunks in → 16 kHz Int16 in a
 * preallocated buffer + end-of-utterance detection. Separated from the Web
 * Audio plumbing so it can be unit-tested.
 */
export class CaptureBuffer {
  readonly vad: EnergyVad;
  private readonly ds: Downsampler | null;
  private readonly pcm: Int16Array;
  private n = 0;

  constructor(inRate: number, vadOpts: VadOptions = {}) {
    const maxMs = vadOpts.maxMs ?? VAD_DEFAULTS.maxMs;
    this.ds = inRate === TARGET_RATE ? null : new Downsampler(inRate, TARGET_RATE);
    this.vad = new EnergyVad({ ...vadOpts, sampleRate: TARGET_RATE });
    // Hard bound: maxMs of audio plus one render batch of slack.
    this.pcm = new Int16Array(Math.ceil((TARGET_RATE * maxMs) / 1000) + 4096);
  }

  get samples(): number {
    return this.n;
  }

  get durationMs(): number {
    return Math.round((this.n * 1000) / TARGET_RATE);
  }

  /** Returns the end reason once capture should stop. */
  push(chunk: Float32Array): VadEnd | null {
    if (this.vad.result) return this.vad.result;
    const s = this.ds ? this.ds.process(chunk) : chunk;
    const room = this.pcm.length - this.n;
    const take = s.length <= room ? s : s.subarray(0, room);
    floatToInt16(take, this.pcm.subarray(this.n, this.n + take.length));
    this.n += take.length;
    const end = this.vad.push(take);
    if (end) return end;
    return room <= s.length ? "max_length" : null;
  }

  toWav(): Blob {
    return wavBlob(this.pcm.subarray(0, this.n), TARGET_RATE);
  }
}

interface Active {
  finish: (end: RecordEnd) => void;
  fail: (err: MicError) => void;
}

/** Web Audio implementation (AudioWorklet, ScriptProcessor fallback). */
export class MicRecorder implements Recorder {
  private active: Active | null = null;
  private pendingAbort = false;

  constructor(private readonly vadOpts: VadOptions = {}) {}

  async record(opts: RecordOptions = {}): Promise<RecordResult> {
    if (this.active) throw new MicError("busy");
    const md = globalThis.navigator?.mediaDevices;
    if (!md?.getUserMedia) throw new MicError("unsupported");
    const Ctx = globalThis.AudioContext;
    if (!Ctx) throw new MicError("unsupported");

    this.pendingAbort = false;
    let finished = false;
    let resolveFn!: (r: RecordResult) => void;
    let rejectFn!: (e: MicError) => void;
    const done = new Promise<RecordResult>((res, rej) => {
      resolveFn = res;
      rejectFn = rej;
    });

    let stream: MediaStream | null = null;
    let ctx: AudioContext | null = null;
    let src: MediaStreamAudioSourceNode | null = null;
    let node: AudioWorkletNode | ScriptProcessorNode | null = null;
    let sink: GainNode | null = null;
    let cap: CaptureBuffer | null = null;
    let guard: ReturnType<typeof setTimeout> | null = null;

    const release = () => {
      if (guard) clearTimeout(guard);
      guard = null;
      for (const t of stream?.getTracks() ?? []) t.stop();
      stream = null;
      if (node instanceof AudioWorkletNode) {
        node.port.onmessage = null;
        node.port.close();
      } else if (node) {
        node.onaudioprocess = null;
      }
      try {
        src?.disconnect();
        node?.disconnect();
        sink?.disconnect();
      } catch {
        // already disconnected
      }
      void ctx?.close().catch(() => undefined);
      ctx = null;
      if (this.active === me) this.active = null;
    };

    const finish = (end: RecordEnd) => {
      if (finished) return;
      finished = true;
      const c = cap;
      release();
      if (!c) {
        rejectFn(new MicError("aborted"));
        return;
      }
      resolveFn({ wav: c.toWav(), durationMs: c.durationMs, end, hadSpeech: c.vad.hasSpeech });
    };
    const fail = (err: MicError) => {
      if (finished) return;
      finished = true;
      release();
      rejectFn(err);
    };
    const me: Active = { finish, fail };
    this.active = me;

    const onChunk = (chunk: Float32Array) => {
      if (finished || !cap) return;
      const end = cap.push(chunk);
      opts.onLevel?.(cap.vad.level);
      if (end) finish(end);
    };

    try {
      try {
        stream = await md.getUserMedia({
          audio: {
            channelCount: 1,
            echoCancellation: true,
            noiseSuppression: true,
            autoGainControl: true,
          },
          video: false,
        });
      } catch (e) {
        const name = e instanceof DOMException ? e.name : "";
        throw new MicError(
          name === "NotAllowedError" || name === "SecurityError"
            ? "denied"
            : name === "NotFoundError" || name === "OverconstrainedError"
              ? "no_device"
              : "failed",
          e instanceof Error ? e.message : undefined,
        );
      }
      if (this.pendingAbort || finished) throw new MicError("aborted");

      ctx = new Ctx({ latencyHint: "interactive" });
      if (ctx.state === "suspended") await ctx.resume();
      src = ctx.createMediaStreamSource(stream);
      sink = ctx.createGain();
      sink.gain.value = 0; // keep the graph pulling without audible output
      sink.connect(ctx.destination);
      cap = new CaptureBuffer(ctx.sampleRate, this.vadOpts);

      try {
        await ctx.audioWorklet.addModule(workletUrl);
        if (finished) throw new MicError("aborted");
        const w = new AudioWorkletNode(ctx, "js-capture", {
          numberOfInputs: 1,
          numberOfOutputs: 1,
          channelCount: 1,
          channelCountMode: "explicit",
          channelInterpretation: "speakers",
        });
        w.port.onmessage = (e: MessageEvent<Float32Array>) => onChunk(e.data);
        node = w;
      } catch (e) {
        if (e instanceof MicError) throw e;
        // Fallback for runtimes without AudioWorklet.
        const sp = ctx.createScriptProcessor(4096, 1, 1);
        sp.onaudioprocess = (ev) => onChunk(new Float32Array(ev.inputBuffer.getChannelData(0)));
        node = sp;
      }
      if (finished) throw new MicError("aborted");
      src.connect(node);
      node.connect(sink);

      const maxMs = this.vadOpts.maxMs ?? VAD_DEFAULTS.maxMs;
      // Safety net if the audio graph stalls and no chunks arrive.
      guard = setTimeout(() => finish("max_length"), maxMs + 2_000);
    } catch (e) {
      // If already aborted, a stream obtained meanwhile must still be released.
      if (finished) release();
      else fail(e instanceof MicError ? e : new MicError("failed", String(e)));
    }
    return done;
  }

  stop(): void {
    this.active?.finish("stopped");
  }

  abort(): void {
    this.pendingAbort = true;
    this.active?.fail(new MicError("aborted"));
  }
}
