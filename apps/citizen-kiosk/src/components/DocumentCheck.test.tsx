import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { DeviceApi } from "../api/client";
import type { ConfirmResult, DocumentResult, SessionSnapshot } from "../api/types";
import type { Camera } from "../camera/camera";
import { CameraError } from "../camera/camera";
import {
  CAPS_ALL,
  FakeAgent,
  FakeEventSource,
  FakePlayer,
  FakeRecorder,
  reply,
} from "../test/fakes";
import { DocumentCheck } from "./DocumentCheck";

// All document data here is synthetic and FAKE.

class FakeTrack {
  stopped = false;
  stop(): void {
    this.stopped = true;
  }
}

class FakeCamera implements Camera {
  opened = 0;
  captured = 0;
  tracks: FakeTrack[] = [];
  failOpen: Error | null = null;
  log: string[];

  constructor(log: string[] = []) {
    this.log = log;
  }

  async open(): Promise<MediaStream> {
    this.log.push("camera.open");
    if (this.failOpen) throw this.failOpen;
    this.opened++;
    const t = new FakeTrack();
    this.tracks.push(t);
    // A real (happy-dom) MediaStream, so <video>.srcObject accepts it.
    const stream = new MediaStream();
    Object.defineProperty(stream, "getTracks", { value: () => [t] });
    return stream;
  }

  async capture(): Promise<Blob> {
    this.captured++;
    return new Blob([new Uint8Array([0xff, 0xd8, 0xff, 0xe0, 0, 0])], { type: "image/jpeg" });
  }

  get live(): number {
    return this.tracks.filter((t) => !t.stopped).length;
  }
}

const S = (over: Partial<SessionSnapshot> = {}): SessionSnapshot => ({
  id: "s1",
  activity: "READY",
  language: "hi",
  camera_consent: false,
  turns: 1,
  ...over,
});

const docResult = (over: Partial<DocumentResult> = {}): DocumentResult => ({
  session: S({ camera_consent: true }),
  document: {
    doc_type: "aadhaar",
    label: "आधार कार्ड",
    confidence: "high",
    masked_ids: [{ kind: "aadhaar", value: "XXXX XXXX 9012" }],
  },
  text: "यह आधार कार्ड लगता है — क्या यह सही है?",
  can_confirm: true,
  notice: "यह जाँच कागज़ के असली होने की पुष्टि नहीं करती।",
  ...over,
});

const confirmResult: ConfirmResult = {
  session: S({ camera_consent: true }),
  text: "आपने ये कागज़ दिखाए:",
  confirmed: [{ doc_type: "aadhaar", label: "आधार कार्ड" }],
  coverage: {
    scheme_id: "test-scheme",
    scheme_name: "परीक्षण योजना (FIXTURE)",
    mode: "factual",
    verified: true,
    items: [
      { name: "आधार कार्ड", optional: false, status: "seen" },
      { name: "बैंक पासबुक", optional: false, status: "not_seen" },
      { name: "आवेदन पत्र", optional: false, status: "not_checkable" },
    ],
  },
  caveat: "यह मिलान सिर्फ़ कागज़ के प्रकार का है।",
  notice: "यह जाँच कागज़ के असली होने की पुष्टि नहीं करती।",
};

function setupDoc() {
  const log: string[] = [];
  const agent = new FakeAgent();
  agent.session = S();
  agent.on("POST /session/:id/camera-consent", (call) => {
    const consent = (call.body as { consent: boolean }).consent;
    log.push(`consent:${consent}`);
    agent.session = S({ camera_consent: consent });
    return { body: { session: agent.session } };
  });
  agent.on("POST /session/:id/document", () => {
    log.push("document");
    return { body: docResult() };
  });
  agent.on("POST /session/:id/document/confirm", (call) => {
    const correct = (call.body as { correct: boolean }).correct;
    log.push(`confirm:${correct}`);
    return {
      body: correct ? confirmResult : { ...confirmResult, confirmed: [], coverage: undefined },
    };
  });
  const camera = new FakeCamera(log);
  const onClose = vi.fn();
  const api = new DeviceApi({ fetch: agent.fetch });
  const view = render(
    <DocumentCheck api={api} camera={camera} sessionId="s1" lang="hi" onClose={onClose} />,
  );
  return { agent, camera, onClose, log, view };
}

describe("DocumentCheck", () => {
  it("consent → camera → capture (tracks stopped) → confirm → coverage", async () => {
    const { agent, camera, onClose, log, view } = setupDoc();
    // Consent screen first; nothing touched yet.
    expect(screen.getByTestId("doc-consent").textContent).toContain("कैमरा सिर्फ़ इस जाँच के लिए");
    expect(camera.opened).toBe(0);
    expect(agent.calls).toHaveLength(0);

    fireEvent.click(screen.getByTestId("doc-agree"));
    await screen.findByTestId("doc-video");
    // The camera opened only after the agent accepted the consent.
    expect(log.slice(0, 2)).toEqual(["consent:true", "camera.open"]);
    expect(camera.live).toBe(1);

    // Hold the upload so we can see the camera is already off.
    let release: () => void = () => {};
    agent.on("POST /session/:id/document", (call) => {
      expect(call.headers["content-type"]).toBe("image/jpeg");
      expect(call.headers["x-js-kiosk"]).toBe("1");
      expect(call.body).toBeInstanceOf(Blob);
      return new Promise((resolve) => {
        release = () => resolve({ body: docResult() });
      });
    });
    fireEvent.click(screen.getByTestId("doc-capture"));
    await screen.findByTestId("doc-reading");
    expect(camera.captured).toBe(1);
    expect(camera.live).toBe(0); // stopped right after the photo
    act(() => release());

    expect((await screen.findByTestId("doc-question")).textContent).toBe(
      "यह आधार कार्ड लगता है — क्या यह सही है?",
    );
    expect(screen.getByTestId("doc-fields").textContent).toContain("XXXX XXXX 9012");
    expect(screen.getByTestId("doc-notice").textContent).toContain("असली होने की पुष्टि नहीं");

    fireEvent.click(screen.getByTestId("doc-yes"));
    await screen.findByTestId("doc-coverage");
    expect(screen.getByTestId("doc-item-seen").textContent).toContain("आधार कार्ड");
    expect(screen.getByTestId("doc-item-not_seen").textContent).toContain("बैंक पासबुक");
    expect(screen.getByTestId("doc-item-not_checkable").textContent).toContain("आवेदन पत्र");
    expect(screen.getByTestId("doc-coverage").textContent).toContain("सिर्फ़ कागज़ के प्रकार");

    fireEvent.click(screen.getByTestId("doc-done"));
    expect(onClose).toHaveBeenCalledTimes(1);
    view.unmount();
    // Leaving withdraws the consent.
    await waitFor(() => expect(log.at(-1)).toBe("consent:false"));
    expect(camera.live).toBe(0);
  });

  it("does not open the camera when consent is refused by the agent", async () => {
    const { agent, camera } = setupDoc();
    agent.on("POST /session/:id/camera-consent", () => ({
      status: 503,
      body: { error: { code: "unavailable", message: "off" } },
    }));
    fireEvent.click(screen.getByTestId("doc-agree"));
    expect((await screen.findByTestId("doc-error")).textContent).toContain("उपलब्ध नहीं");
    expect(camera.opened).toBe(0);
  });

  it("shows a message when the camera is denied, and withdraws consent on leaving", async () => {
    const { camera, log, view } = setupDoc();
    camera.failOpen = new CameraError("denied", "no");
    fireEvent.click(screen.getByTestId("doc-agree"));
    expect((await screen.findByTestId("doc-error")).textContent).toContain("कैमरा नहीं चल रहा");
    view.unmount();
    await waitFor(() => expect(log.at(-1)).toBe("consent:false"));
  });

  it("'No, retake' reopens the camera; unknown documents offer only a retake", async () => {
    const { agent, camera, log } = setupDoc();
    fireEvent.click(screen.getByTestId("doc-agree"));
    await screen.findByTestId("doc-video");
    fireEvent.click(screen.getByTestId("doc-capture"));
    await screen.findByTestId("doc-question");
    fireEvent.click(screen.getByTestId("doc-retake"));
    await screen.findByTestId("doc-video");
    expect(log).toContain("confirm:false");
    expect(camera.opened).toBe(2);
    expect(camera.live).toBe(1);

    agent.on("POST /session/:id/document", () => ({
      body: docResult({
        document: { doc_type: "unknown", label: "पहचान नहीं हो पाई", confidence: "none" },
        text: "यह कौन-सा कागज़ है, पहचान नहीं पाए।",
        can_confirm: false,
      }),
    }));
    fireEvent.click(screen.getByTestId("doc-capture"));
    await screen.findByTestId("doc-question");
    expect(screen.queryByTestId("doc-yes")).toBeNull();
    expect(screen.queryByTestId("doc-fields")).toBeNull();
    expect(camera.live).toBe(0);
    fireEvent.click(screen.getByTestId("doc-retake"));
    await screen.findByTestId("doc-video");
    expect(camera.opened).toBe(3);
  });

  it("stops the camera when the screen goes away mid-preview", async () => {
    const { camera, log, view } = setupDoc();
    fireEvent.click(screen.getByTestId("doc-agree"));
    await screen.findByTestId("doc-video");
    expect(camera.live).toBe(1);
    view.unmount();
    expect(camera.live).toBe(0);
    await waitFor(() => expect(log.at(-1)).toBe("consent:false"));
  });
});

// ---- integration with the conversation screen ---------------------------

beforeEach(() => {
  FakeEventSource.instances = [];
});

async function conversationWithCard(cameraCap: boolean) {
  const agent = new FakeAgent();
  agent.caps = { ...CAPS_ALL, camera: cameraCap };
  agent.session = S({ turns: 1 });
  const recorder = new FakeRecorder();
  const camera = new FakeCamera();
  const deps = {
    api: new DeviceApi({ fetch: agent.fetch }),
    recorder,
    player: new FakePlayer(),
    camera,
    createEventSource: (url: string) => new FakeEventSource(url),
    timings: { endedReturnMs: 40, endedMaxMs: 200, noticeMs: 5_000 },
  };
  agent.on("POST /session/:id/text", () => {
    // listen_next with the agent in LISTENING: normally the UI would record.
    agent.session = S({ activity: "LISTENING", turns: 2 });
    return {
      body: {
        session: agent.session,
        reply: reply({
          text: "इस योजना के बारे में जानकारी।",
          listen_next: true,
          cards: [
            { scheme_id: "test-scheme", title: "परीक्षण योजना", verified: true, sections: [] },
          ],
        }),
      },
    };
  });
  agent.on("POST /session/:id/camera-consent", (call) => {
    agent.session = S({ camera_consent: (call.body as { consent: boolean }).consent });
    return { body: { session: agent.session } };
  });
  agent.on("POST /session/:id/end", () => {
    agent.session = { activity: "IDLE", camera_consent: false, turns: 0 };
    return { body: { session: agent.session, reply: reply({ kind: "goodbye", text: "नमस्ते" }) } };
  });
  render(<App deps={deps} />);
  // Session already open with turns > 0 → conversation screen.
  const input = await screen.findByRole("textbox");
  return { agent, recorder, camera, input };
}

describe("document check in the conversation", () => {
  it("is offered only with a scheme card and capabilities.camera", async () => {
    const { input, recorder } = await conversationWithCard(false);
    fireEvent.change(input, { target: { value: "योजना" } });
    fireEvent.submit(screen.getByTestId("text-form"));
    await waitFor(() =>
      expect(screen.getByTestId("reply-text").textContent).toBe("इस योजना के बारे में जानकारी।"),
    );
    expect(screen.queryByTestId("doc-open")).toBeNull();
    recorder.abort();
  });

  it("keeps the mic closed and stops the camera when the session ends", async () => {
    const { input, recorder, camera, agent } = await conversationWithCard(true);
    recorder.failWith = null;
    fireEvent.change(input, { target: { value: "योजना" } });
    fireEvent.submit(screen.getByTestId("text-form"));
    const open = await screen.findByTestId("doc-open");
    // Auto-listen may have started recording (listen_next); opening the
    // document check aborts it.
    fireEvent.click(open);
    await screen.findByTestId("doc-consent");
    expect(recorder.active).toBe(false);
    expect(screen.queryByTestId("mic-button")).toBeNull();

    fireEvent.click(screen.getByTestId("doc-agree"));
    await screen.findByTestId("doc-video");
    expect(camera.live).toBe(1);
    expect(recorder.active).toBe(false);

    // The session ends (SSE): the screen goes away, the camera goes off.
    act(() => {
      agent.session = { activity: "IDLE", camera_consent: false, turns: 0 };
      FakeEventSource.latest().session({ type: "session.ended", session: agent.session });
    });
    await waitFor(() => expect(screen.queryByTestId("doc-check")).toBeNull());
    expect(camera.live).toBe(0);
  });
});
