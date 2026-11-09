// DEV-ONLY mock of the device local API (docs/API_DOCUMENTATION.md §1),
// mounted by `pnpm dev:mock`. It is never part of the production build.
//
// All conversational content here is obviously-fake DEMO text used to
// exercise the screens. It contains no real scheme names, amounts,
// documents, URLs or phone numbers — real facts only ever come from
// human-verified knowledge records served by the Go agent.
//
// Env switches:
//   MOCK_FAST=1         inactivity 20 s / 40 s / 60 s instead of 2 / 9 / 10 min
//   MOCK_VOICE_IN=0     capabilities.voice_input = false
//   MOCK_VOICE_OUT=0    capabilities.voice_output = false (no audio_url)
//   MOCK_SIM_BUTTON=0   capabilities.sim_button = false (use POST /__mock/button)
//   MOCK_LATENCY_MS=700 simulated processing time per turn

import { randomBytes } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { Plugin } from "vite";

type Activity = "IDLE" | "READY" | "LISTENING" | "PROCESSING" | "SPEAKING";
type Lang = "hi" | "en";

interface Snapshot {
  id?: string;
  activity: Activity;
  language?: Lang;
  started_at?: string;
  last_active_at?: string;
  idle_deadline?: string;
  camera_consent: boolean;
  turns: number;
}

interface Reply {
  kind: string;
  text: string;
  options?: { id: string; label: string }[];
  cards?: unknown[];
  listen_next: boolean;
  end_session: boolean;
  used_llm: boolean;
}

const env = (k: string, d: string) => process.env[k] ?? d;
const FAST = env("MOCK_FAST", "0") === "1";
const TIMEOUTS = FAST
  ? { reminder: 20_000, warning: 40_000, close: 60_000 }
  : { reminder: 120_000, warning: 540_000, close: 600_000 };
const CAPS = {
  voice_input: env("MOCK_VOICE_IN", "1") !== "0",
  voice_output: env("MOCK_VOICE_OUT", "1") !== "0",
  llm: false,
  sim_button: env("MOCK_SIM_BUTTON", "1") !== "0",
  camera: false,
};
const LATENCY = Math.min(Number(env("MOCK_LATENCY_MS", "700")) || 0, 10_000);
const MAX_JSON = 16 * 1024;
const MAX_AUDIO = 1024 * 1024;
const MAX_SSE = 8;
const MAX_CLIPS = 8;

// ---- demo content (UI exercise only; NOT real information) --------------

const DEMO = {
  hi: {
    greeting: "नमस्ते! (डेमो मोड) मैं जन सहायक का नमूना हूँ। नीचे से कोई विकल्प चुनें या बोलें।",
    options: [
      { id: "demo_cards", label: "कार्ड दिखाएँ (डेमो)" },
      { id: "demo_question", label: "सवाल पूछें (डेमो)" },
      { id: "demo_crisis", label: "संकट स्क्रीन (डेमो)" },
      { id: "demo_bye", label: "अलविदा (डेमो)" },
    ],
    cards: "(डेमो) ये नमूना कार्ड केवल स्क्रीन जाँचने के लिए हैं — इनमें कोई असली जानकारी नहीं है।",
    question: "(डेमो) क्या आप हाँ या नहीं में जवाब देंगे?",
    yesNo: [
      { id: "demo_yes", label: "हाँ" },
      { id: "demo_no", label: "नहीं" },
    ],
    crisis:
      "(डेमो) यह संकट-उत्तर की नमूना स्क्रीन है। असली हेल्पलाइन नंबर केवल सत्यापित रिकॉर्ड से आएँगे। नमूना: 00000",
    heard: (s: string) => `(डेमो) आपकी बात मिली: “${s}”। यह नमूना जवाब है।`,
    voice: (sec: string) => `(डेमो) ${sec} सेकंड की आवाज़ मिली। यह नमूना जवाब है।`,
    noSpeech: "(डेमो) कुछ सुनाई नहीं दिया। कृपया फिर से बोलें।",
    bye: "(डेमो) धन्यवाद! फिर मिलेंगे।",
  },
  en: {
    greeting: "Hello! (demo mode) I am a Jan Sahayak sample. Pick an option below or speak.",
    options: [
      { id: "demo_cards", label: "Show cards (demo)" },
      { id: "demo_question", label: "Ask a question (demo)" },
      { id: "demo_crisis", label: "Crisis screen (demo)" },
      { id: "demo_bye", label: "Goodbye (demo)" },
    ],
    cards: "(demo) These sample cards only exercise the layout — they contain no real information.",
    question: "(demo) Will you answer yes or no?",
    yesNo: [
      { id: "demo_yes", label: "Yes" },
      { id: "demo_no", label: "No" },
    ],
    crisis:
      "(demo) This is the sample crisis screen. Real helpline numbers only come from verified records. Sample: 00000",
    heard: (s: string) => `(demo) I received: “${s}”. This is a sample reply.`,
    voice: (sec: string) => `(demo) Received ${sec} s of audio. This is a sample reply.`,
    noSpeech: "(demo) I could not hear anything. Please speak again.",
    bye: "(demo) Thank you! Goodbye.",
  },
};

function demoCards(lang: Lang) {
  const hi = lang === "hi";
  return [
    {
      scheme_id: "demo-verified-layout",
      title: hi ? "डेमो कार्ड A — असली योजना नहीं" : "Demo card A — not a real scheme",
      verified: true, // layout fixture for the badge only; not knowledge
      last_verified: "2000-01-01",
      sections: [
        {
          title: hi ? "नमूना खंड" : "Sample section",
          items: [hi ? "नमूना पंक्ति एक" : "Sample line one", hi ? "नमूना पंक्ति दो" : "Sample line two"],
        },
      ],
      helpline: "00000",
      official_url: "example.invalid",
      disclaimer: hi ? "डेमो — स्क्रीन जाँच के लिए।" : "Demo — for screen testing.",
    },
    {
      scheme_id: "demo-unverified-layout",
      title: hi ? "डेमो कार्ड B — असली योजना नहीं" : "Demo card B — not a real scheme",
      verified: false,
      sections: [
        {
          title: hi ? "नमूना खंड" : "Sample section",
          items: [hi ? "नमूना पंक्ति" : "Sample line"],
        },
      ],
    },
  ];
}

function reply(kind: string, text: string, extra: Partial<Reply> = {}): Reply {
  return { kind, text, listen_next: false, end_session: false, used_llm: false, ...extra };
}

function replyFor(lang: Lang, input: { text?: string; option?: string; seconds?: number; silent?: boolean }): Reply {
  const d = DEMO[lang];
  const opt = input.option ?? "";
  const text = (input.text ?? "").toLowerCase();
  if (input.silent) return reply("repeat", d.noSpeech, { listen_next: true });
  if (opt === "demo_bye" || /bye|अलविदा/.test(text)) return reply("goodbye", d.bye, { end_session: true });
  if (opt === "demo_crisis" || /crisis|संकट/.test(text)) return reply("crisis", d.crisis);
  if (opt === "demo_cards" || /card|कार्ड/.test(text)) {
    return reply("answer", d.cards, { cards: demoCards(lang), options: d.options });
  }
  if (opt === "demo_question") return reply("question", d.question, { options: d.yesNo, listen_next: true });
  if (input.seconds !== undefined) {
    return reply("answer", d.voice(input.seconds.toFixed(1)), { options: d.options, listen_next: false });
  }
  return reply("answer", d.heard(input.text ?? opt), { options: d.options });
}

// ---- tone WAV for reply audio --------------------------------------------

function toneWav(ms: number, hz: number): Buffer {
  const rate = 16_000;
  const n = Math.round((rate * ms) / 1000);
  const b = Buffer.alloc(44 + n * 2);
  b.write("RIFF", 0);
  b.writeUInt32LE(36 + n * 2, 4);
  b.write("WAVE", 8);
  b.write("fmt ", 12);
  b.writeUInt32LE(16, 16);
  b.writeUInt16LE(1, 20);
  b.writeUInt16LE(1, 22);
  b.writeUInt32LE(rate, 24);
  b.writeUInt32LE(rate * 2, 28);
  b.writeUInt16LE(2, 32);
  b.writeUInt16LE(16, 34);
  b.write("data", 36);
  b.writeUInt32LE(n * 2, 40);
  for (let i = 0; i < n; i++) {
    const env = Math.min(1, i / 400, (n - i) / 400);
    b.writeInt16LE(Math.round(Math.sin((2 * Math.PI * hz * i) / rate) * 6000 * env), 44 + i * 2);
  }
  return b;
}

/** Parse a 16 kHz mono 16-bit WAV; returns seconds and RMS, or null. */
function inspectWav(buf: Buffer): { seconds: number; rms: number } | null {
  if (buf.length < 44 || buf.toString("ascii", 0, 4) !== "RIFF" || buf.toString("ascii", 8, 12) !== "WAVE") {
    return null;
  }
  const channels = buf.readUInt16LE(22);
  const rate = buf.readUInt32LE(24);
  const bits = buf.readUInt16LE(34);
  if (channels !== 1 || rate !== 16_000 || bits !== 16) return null;
  const samples = Math.floor((buf.length - 44) / 2);
  let sum = 0;
  for (let i = 0; i < samples; i++) {
    const s = buf.readInt16LE(44 + i * 2) / 32768;
    sum += s * s;
  }
  return { seconds: samples / rate, rms: samples ? Math.sqrt(sum / samples) : 0 };
}

// ---- mock agent ------------------------------------------------------------

class MockAgent {
  session: Snapshot = { activity: "IDLE", camera_consent: false, turns: 0 };
  private lastActive = 0;
  private reminded = false;
  private warned = false;
  private pendingListen = false;
  private pendingEnd = false;
  private clips = new Map<number, Buffer>();
  private clipN = 0;
  private sse = new Set<ServerResponse>();
  private ticker: NodeJS.Timeout | null = null;

  start(): void {
    this.ticker = setInterval(() => this.tick(), 1000);
  }

  stop(): void {
    if (this.ticker) clearInterval(this.ticker);
    for (const r of this.sse) r.end();
    this.sse.clear();
  }

  addSse(res: ServerResponse): boolean {
    if (this.sse.size >= MAX_SSE) return false;
    this.sse.add(res);
    res.on("close", () => this.sse.delete(res));
    return true;
  }

  emit(event: string, data: unknown): void {
    const line = `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
    for (const r of this.sse) r.write(line);
  }

  ping(): void {
    this.emit("ping", {});
  }

  status() {
    return {
      device_id: "mock-device",
      version: "mock",
      overall: CAPS.voice_input && CAPS.voice_output ? "HEALTHY" : "DEGRADED",
      capabilities: CAPS,
      session: this.snap(),
    };
  }

  private snap(): Snapshot {
    const s = { ...this.session };
    if (s.id) {
      s.last_active_at = new Date(this.lastActive).toISOString();
      s.idle_deadline = new Date(this.lastActive + TIMEOUTS.close).toISOString();
    }
    return s;
  }

  private event(type: string, end_reason?: string): void {
    this.emit("session", { type, session: this.snap(), end_reason, at: new Date().toISOString() });
  }

  touch(): void {
    this.lastActive = Date.now();
    this.reminded = false;
    this.warned = false;
  }

  setActivity(a: Activity): void {
    this.session.activity = a;
    this.event("session.activity");
  }

  begin(lang: Lang) {
    this.session = {
      id: randomBytes(8).toString("hex"),
      activity: "READY",
      language: lang,
      started_at: new Date().toISOString(),
      camera_consent: false,
      turns: 0,
    };
    this.clips.clear();
    this.touch();
    this.event("session.started");
    const r = reply("greeting", DEMO[lang].greeting, { options: DEMO[lang].options, listen_next: true });
    return this.speak(r);
  }

  /** Attach audio (if voice output works) and set the post-reply activity. */
  speak(r: Reply) {
    const audio_url = CAPS.voice_output ? this.addClip(r.text.length) : undefined;
    this.pendingListen = r.listen_next;
    this.pendingEnd = r.end_session;
    if (audio_url) this.setActivity("SPEAKING");
    else if (r.end_session) this.finish("citizen_ended");
    else this.setActivity(r.listen_next && CAPS.voice_input ? "LISTENING" : "READY");
    return { session: this.snap(), reply: r, audio_url };
  }

  speakingDone(): void {
    if (this.pendingEnd) {
      this.finish("citizen_ended");
      return;
    }
    this.setActivity(this.pendingListen && CAPS.voice_input ? "LISTENING" : "READY");
  }

  finish(reason: string): void {
    if (!this.session.id) return;
    this.session = { activity: "IDLE", camera_consent: false, turns: 0 };
    this.clips.clear();
    this.event("session.ended", reason);
  }

  clip(n: number): Buffer | undefined {
    return this.clips.get(n);
  }

  private addClip(chars: number): string {
    const n = ++this.clipN;
    this.clips.set(n, toneWav(Math.min(400 + chars * 25, 4_000), 440 + (n % 4) * 110));
    while (this.clips.size > MAX_CLIPS) {
      const oldest = this.clips.keys().next().value;
      if (oldest === undefined) break;
      this.clips.delete(oldest);
    }
    return `/api/v1/session/${this.session.id}/audio/${n}.wav`;
  }

  private tick(): void {
    if (!this.session.id) return;
    const idle = Date.now() - this.lastActive;
    if (this.session.activity === "PROCESSING" || this.session.activity === "SPEAKING") return;
    if (idle >= TIMEOUTS.close) this.finish("inactivity_timeout");
    else if (idle >= TIMEOUTS.warning && !this.warned) {
      this.warned = true;
      this.event("session.final_warning");
    } else if (idle >= TIMEOUTS.reminder && !this.reminded) {
      this.reminded = true;
      this.event("session.reminder");
    }
  }
}

// ---- HTTP plumbing -----------------------------------------------------------

class HttpError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

function readBody(req: IncomingMessage, limit: number): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on("data", (c: Buffer) => {
      size += c.length;
      if (size > limit) {
        reject(new HttpError(413, "too_large", "body too large"));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

async function readJson<T>(req: IncomingMessage): Promise<T> {
  const b = await readBody(req, MAX_JSON);
  if (b.length === 0) return {} as T;
  try {
    return JSON.parse(b.toString("utf8")) as T;
  } catch {
    throw new HttpError(400, "bad_request", "invalid JSON");
  }
}

function send(res: ServerResponse, status: number, body: unknown): void {
  const s = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  res.end(s);
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function deviceApiMock(): Plugin {
  const agent = new MockAgent();
  let pinger: NodeJS.Timeout | null = null;

  const requireSession = (id: string) => {
    if (!agent.session.id) throw new HttpError(404, "no_session", "no active session");
    if (agent.session.id !== id) throw new HttpError(409, "wrong_session", "wrong session");
  };

  const turn = async (lang: Lang, input: Parameters<typeof replyFor>[1], transcript?: unknown) => {
    agent.touch();
    agent.setActivity("PROCESSING");
    await sleep(LATENCY);
    if (!agent.session.id) throw new HttpError(404, "no_session", "session ended");
    agent.session.turns++;
    const r = replyFor(lang, input);
    const out = agent.speak(r);
    return { ...out, transcript };
  };

  async function handle(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const url = new URL(req.url ?? "/", "http://mock");
    const path = url.pathname;
    if (path === "/__mock/button" && req.method === "POST") {
      if (agent.session.id) throw new HttpError(409, "bad_state", "session active");
      agent.begin("hi");
      send(res, 200, { ok: true });
      return true;
    }
    if (!path.startsWith("/api/v1/")) return false;
    const p = path.slice("/api/v1".length);
    if (req.method === "POST" && req.headers["x-js-kiosk"] !== "1") {
      throw new HttpError(403, "bad_request", "missing X-JS-Kiosk header");
    }

    if (req.method === "GET" && p === "/status") return send(res, 200, agent.status()), true;
    if (req.method === "GET" && p === "/health") {
      return send(res, 200, { overall: agent.status().overall, components: [] }), true;
    }
    if (req.method === "GET" && p === "/events") {
      if (!agent.addSse(res)) throw new HttpError(503, "unavailable", "too many subscribers");
      res.writeHead(200, {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-store",
        Connection: "keep-alive",
      });
      res.write(": connected\n\n");
      return true;
    }
    if (req.method === "POST" && p === "/session/start") {
      if (!CAPS.sim_button) throw new HttpError(403, "button_disabled", "simulated button disabled");
      if (agent.session.id) throw new HttpError(409, "bad_state", "session already active");
      const b = await readJson<{ language?: Lang }>(req);
      send(res, 200, agent.begin(b.language === "en" ? "en" : "hi"));
      return true;
    }

    const audio = /^\/session\/([^/]+)\/audio\/(\d+)\.wav$/.exec(p);
    if (req.method === "GET" && audio) {
      requireSession(decodeURIComponent(audio[1] ?? ""));
      const clip = agent.clip(Number(audio[2]));
      if (!clip) throw new HttpError(404, "bad_request", "clip not found");
      res.writeHead(200, { "Content-Type": "audio/wav", "Content-Length": clip.length, "Cache-Control": "no-store" });
      res.end(clip);
      return true;
    }

    const m = /^\/session\/([^/]+)\/([a-z-]+)$/.exec(p);
    if (!m || req.method !== "POST") throw new HttpError(404, "bad_request", "unknown endpoint");
    const id = decodeURIComponent(m[1] ?? "");
    const action = m[2];
    requireSession(id);
    const lang: Lang = agent.session.language ?? "hi";
    const act = agent.session.activity;

    switch (action) {
      case "listen":
        if (!CAPS.voice_input) throw new HttpError(503, "unavailable", "voice input unavailable");
        if (act !== "READY" && act !== "LISTENING") throw new HttpError(409, "bad_state", `cannot listen in ${act}`);
        agent.touch();
        agent.setActivity("LISTENING");
        return send(res, 200, { session: agent.status().session }), true;
      case "audio": {
        if (act !== "LISTENING") throw new HttpError(409, "bad_state", `not listening (${act})`);
        const buf = await readBody(req, MAX_AUDIO);
        const info = inspectWav(buf);
        if (!info || info.seconds > 20) throw new HttpError(400, "bad_audio", "expected 16 kHz mono 16-bit WAV ≤ 20 s");
        const silent = info.rms < 0.003;
        const transcript = { text: silent ? "" : `(डेमो) ${info.seconds.toFixed(1)}s`, confidence: silent ? 0 : 0.5, no_speech: silent };
        return send(res, 200, await turn(lang, { seconds: info.seconds, silent }, transcript)), true;
      }
      case "text": {
        const b = await readJson<{ text?: string }>(req);
        const text = (b.text ?? "").trim();
        if (!text || [...text].length > 500) throw new HttpError(400, "bad_request", "text must be 1–500 chars");
        if (act === "PROCESSING") throw new HttpError(409, "bad_state", "busy");
        return send(res, 200, await turn(lang, { text })), true;
      }
      case "option": {
        const b = await readJson<{ option_id?: string }>(req);
        if (!b.option_id) throw new HttpError(400, "bad_request", "option_id required");
        if (act === "PROCESSING") throw new HttpError(409, "bad_state", "busy");
        return send(res, 200, await turn(lang, { option: b.option_id })), true;
      }
      case "speaking-done":
        if (act !== "SPEAKING") throw new HttpError(409, "bad_state", `not speaking (${act})`);
        agent.speakingDone();
        return send(res, 200, { session: agent.status().session }), true;
      case "language": {
        const b = await readJson<{ language?: string }>(req);
        if (b.language !== "hi" && b.language !== "en") throw new HttpError(400, "bad_request", "language must be hi or en");
        agent.session.language = b.language;
        agent.touch();
        agent.emit("session", { type: "session.language", session: agent.status().session });
        return send(res, 200, { session: agent.status().session }), true;
      }
      case "touch":
        agent.touch();
        return send(res, 200, { session: agent.status().session }), true;
      case "end": {
        const r = reply("goodbye", DEMO[lang].bye, { end_session: true });
        const audio_url = CAPS.voice_output ? undefined : undefined; // agent drops clips at end
        agent.finish("citizen_ended");
        return send(res, 200, { session: agent.status().session, reply: r, audio_url }), true;
      }
      default:
        throw new HttpError(404, "bad_request", "unknown endpoint");
    }
  }

  return {
    name: "jan-sahayak-device-api-mock",
    apply: "serve",
    configureServer(server) {
      agent.start();
      pinger = setInterval(() => agent.ping(), 15_000);
      server.httpServer?.once("close", () => {
        if (pinger) clearInterval(pinger);
        agent.stop();
      });
      server.middlewares.use((req, res, next) => {
        handle(req, res)
          .then((handled) => {
            if (!handled) next();
          })
          .catch((e: unknown) => {
            if (res.headersSent) {
              res.end();
              return;
            }
            const he = e instanceof HttpError ? e : new HttpError(500, "internal", String(e));
            send(res, he.status, { error: { code: he.code, message: he.message } });
          });
      });
      server.config.logger.info(
        `  device API mock: fast=${FAST} caps=${JSON.stringify(CAPS)} (POST /__mock/button = GPIO press)`,
      );
    },
  };
}
