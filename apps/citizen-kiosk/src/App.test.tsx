import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { DeviceApi } from "./api/client";
import type { Capabilities, SessionSnapshot } from "./api/types";
import { MicError } from "./audio/recorder";
import {
  CAPS_ALL,
  FakeAgent,
  FakeEventSource,
  FakePlayer,
  FakeRecorder,
  IDLE,
  reply,
} from "./test/fakes";

const S = (over: Partial<SessionSnapshot> = {}): SessionSnapshot => ({
  id: "s1",
  activity: "READY",
  language: "hi",
  camera_consent: false,
  turns: 0,
  ...over,
});

/** The reply also appears in the ARIA live region, so match the visible one. */
const findReply = (text: string) =>
  waitFor(() => expect(screen.getByTestId("reply-text").textContent).toBe(text));

const AUDIO = (n: number) => `/api/v1/session/s1/audio/${n}.wav`;

function setup(caps: Partial<Capabilities> = {}) {
  const agent = new FakeAgent();
  agent.caps = { ...CAPS_ALL, ...caps };
  const recorder = new FakeRecorder();
  const player = new FakePlayer();
  const deps = {
    api: new DeviceApi({ fetch: agent.fetch }),
    recorder,
    player,
    createEventSource: (url: string) => new FakeEventSource(url),
    timings: { endedReturnMs: 40, endedMaxMs: 200, noticeMs: 5_000 },
  };
  render(<App deps={deps} />);
  const es = () => FakeEventSource.latest();
  return { agent, recorder, player, es };
}

/** Drive welcome → language → privacy → conversation with a simulated button. */
async function startConversation(ctx: ReturnType<typeof setup>, greetingAudio = true) {
  const { agent } = ctx;
  agent.on("POST /session/start", () => {
    agent.session = S({ activity: greetingAudio ? "SPEAKING" : "READY" });
    return {
      body: {
        session: agent.session,
        reply: reply({ kind: "greeting", text: "नमस्ते, मैं जन सहायक हूँ।" }),
        audio_url: greetingAudio ? AUDIO(1) : undefined,
      },
    };
  });
  agent.on("POST /session/:id/speaking-done", () => {
    agent.session = S({ activity: "READY", turns: agent.session.turns });
    return { body: { session: agent.session } };
  });
  fireEvent.click(await screen.findByTestId("start-button"));
  fireEvent.click(await screen.findByRole("button", { name: "हिन्दी" }));
  expect(await screen.findByTestId("privacy-list")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "आगे बढ़ें" }));
  await findReply("नमस्ते, मैं जन सहायक हूँ।");
}

beforeEach(() => {
  FakeEventSource.instances = [];
});

describe("main flow", () => {
  it("start → listen → reply → options → end", async () => {
    const ctx = setup();
    const { agent, recorder, player } = ctx;
    await startConversation(ctx);

    // Greeting audio played, then speaking-done.
    await waitFor(() => expect(agent.paths()).toContain("POST /session/s1/speaking-done"));
    expect(player.played).toEqual([AUDIO(1)]);

    // The mic is opened only after POST /listen has succeeded.
    let recordsWhenListenCalled = -1;
    agent.on("POST /session/:id/listen", () => {
      recordsWhenListenCalled = recorder.records;
      agent.session = S({ activity: "LISTENING" });
      return { body: { session: agent.session } };
    });
    agent.on("POST /session/:id/audio", (call) => {
      expect(call.headers["content-type"]).toBe("audio/wav");
      expect(call.headers["x-js-kiosk"]).toBe("1");
      agent.session = S({ activity: "SPEAKING", turns: 1 });
      return {
        body: {
          session: agent.session,
          transcript: { text: "मुझे लोन चाहिए", confidence: 0.9, no_speech: false },
          reply: reply({
            kind: "answer",
            text: "किस काम के लिए लोन चाहिए?",
            options: [
              { id: "opt_a", label: "विकल्प A" },
              { id: "opt_b", label: "विकल्प B" },
            ],
            cards: [
              {
                scheme_id: "rec-1",
                title: "रिकॉर्ड एक",
                verified: true,
                last_verified: "2026-10-01",
                sections: [{ title: "खंड", items: ["पंक्ति"] }],
              },
              { scheme_id: "rec-2", title: "रिकॉर्ड दो", verified: false, sections: [] },
            ],
          }),
          audio_url: AUDIO(2),
        },
      };
    });

    expect(recorder.records).toBe(0);
    fireEvent.click(screen.getByTestId("mic-button"));
    await waitFor(() => expect(recorder.active).toBe(true));
    expect(recordsWhenListenCalled).toBe(0);
    expect(screen.getByTestId("mic-status").textContent).toContain("सुन रहा हूँ");

    act(() => recorder.finish()); // VAD detected end of utterance
    await findReply("किस काम के लिए लोन चाहिए?");
    expect(screen.getByTestId("transcript").textContent).toContain("मुझे लोन चाहिए");
    expect(screen.getByTestId("live-region").textContent).toBe("किस काम के लिए लोन चाहिए?");

    // Cards with explicit verification badges and date.
    const cards = screen.getAllByTestId("scheme-card");
    expect(cards).toHaveLength(2);
    const [verified, pending] = cards as [HTMLElement, HTMLElement];
    expect(within(verified).getByTestId("verify-badge").textContent).toContain("सत्यापित");
    expect(within(verified).getByTestId("verify-badge").textContent).toContain("Verified");
    expect(verified.textContent).toContain("01-10-2026");
    expect(within(pending).getByTestId("verify-badge").textContent).toContain("सत्यापन बाकी");
    expect(within(pending).getByTestId("verify-badge").textContent).toContain("Not yet verified");

    // Reply audio played and speaking-done posted.
    await waitFor(() => expect(player.played).toEqual([AUDIO(1), AUDIO(2)]));
    await waitFor(() =>
      expect(agent.paths().filter((p) => p.endsWith("speaking-done"))).toHaveLength(2),
    );
    const order = agent.paths();
    expect(order.indexOf("POST /session/s1/listen")).toBeLessThan(
      order.indexOf("POST /session/s1/audio"),
    );

    // Repeat replays the last clip.
    await waitFor(() =>
      expect((screen.getByTestId("repeat-button") as HTMLButtonElement).disabled).toBe(false),
    );
    fireEvent.click(screen.getByTestId("repeat-button"));
    await waitFor(() => expect(player.played.at(-1)).toBe(AUDIO(2)));
    expect(player.played).toHaveLength(3);

    // Options are large buttons that post option_id.
    agent.on("POST /session/:id/option", (call) => {
      expect(call.body).toEqual({ option_id: "opt_b" });
      agent.session = S({ activity: "READY", turns: 2 });
      return { body: { session: agent.session, reply: reply({ text: "ठीक है।" }) } };
    });
    fireEvent.click(screen.getByRole("button", { name: "विकल्प B" }));
    await findReply("ठीक है।");

    // End session → goodbye on the ended screen → back to welcome.
    agent.on("POST /session/:id/end", () => {
      agent.session = { ...IDLE };
      return { body: { session: IDLE, reply: reply({ kind: "goodbye", text: "फिर मिलेंगे।" }) } };
    });
    fireEvent.click(screen.getByTestId("end-button"));
    const ended = await screen.findByTestId("ended-screen");
    expect(ended.textContent).toContain("फिर मिलेंगे।");
    expect(await screen.findByTestId("start-button")).toBeTruthy();
  });

  it("listen_next opens the mic again only when the agent reports LISTENING", async () => {
    const ctx = setup();
    const { agent, recorder } = ctx;
    await startConversation(ctx, false);
    agent.on("POST /session/:id/text", () => {
      agent.session = S({ activity: "SPEAKING", turns: 1 });
      return {
        body: {
          session: agent.session,
          reply: reply({ kind: "question", text: "आपकी उम्र क्या है?", listen_next: true }),
          audio_url: AUDIO(2),
        },
      };
    });
    agent.on("POST /session/:id/speaking-done", () => {
      agent.session = S({ activity: "LISTENING", turns: 1 });
      return { body: { session: agent.session } };
    });
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "पेंशन" } });
    fireEvent.submit(screen.getByTestId("text-form"));
    await findReply("आपकी उम्र क्या है?");
    await waitFor(() => expect(recorder.active).toBe(true));
    expect(agent.calls.find((c) => c.path.endsWith("/text"))?.body).toEqual({ text: "पेंशन" });
    // Typing while listening discards the recording and releases the mic.
    agent.on("POST /session/:id/text", () => ({
      body: { session: S({ activity: "READY", turns: 2 }), reply: reply({ text: "ठीक" }) },
    }));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "सत्तर" } });
    fireEvent.submit(screen.getByTestId("text-form"));
    await findReply("ठीक");
    expect(recorder.aborted).toBe(1);
    expect(recorder.active).toBe(false);
  });

  it("stops auto-listening after two turns in which nothing was heard", async () => {
    const ctx = setup({ voice_output: false });
    const { agent, recorder } = ctx;
    await startConversation(ctx, false);
    agent.on("POST /session/:id/listen", () => ({
      body: { session: S({ activity: "LISTENING" }) },
    }));
    agent.on("POST /session/:id/audio", () => ({
      body: {
        // No TTS: the agent goes straight back to LISTENING for listen_next.
        session: S({ activity: "LISTENING", turns: 1 }),
        transcript: { text: "", confidence: 0, no_speech: true },
        reply: reply({ kind: "repeat", text: "फिर से बोलिए", listen_next: true }),
      },
    }));
    fireEvent.click(screen.getByTestId("mic-button"));
    await waitFor(() => expect(recorder.active).toBe(true));
    act(() => recorder.finish()); // silent turn 1 → auto-listen again
    await waitFor(() => expect(recorder.records).toBe(2));
    await waitFor(() => expect(recorder.active).toBe(true));
    act(() => recorder.finish()); // silent turn 2 → stop auto-listening
    await waitFor(() => expect(agent.paths().filter((p) => p.endsWith("/audio")).length).toBe(2));
    await waitFor(() =>
      expect(screen.getByTestId("mic-status").textContent).toContain("बोलने के लिए"),
    );
    expect(recorder.records).toBe(2);
    expect(recorder.active).toBe(false);
  });

  it("never opens the mic when POST /listen fails", async () => {
    const ctx = setup();
    const { agent, recorder } = ctx;
    await startConversation(ctx, false);
    agent.on("POST /session/:id/listen", () => ({
      status: 409,
      body: { error: { code: "bad_state", message: "busy" } },
    }));
    fireEvent.click(screen.getByTestId("mic-button"));
    await waitFor(() => expect(agent.paths()).toContain("POST /session/s1/listen"));
    await waitFor(() =>
      expect(screen.getByTestId("mic-status").textContent).toContain("बोलने के लिए"),
    );
    expect(recorder.records).toBe(0);
  });

  it("records after a bad_state /listen only if /status confirms LISTENING", async () => {
    const ctx = setup();
    const { agent, recorder } = ctx;
    await startConversation(ctx, false);
    agent.on("POST /session/:id/listen", () => ({
      status: 409,
      body: { error: { code: "bad_state", message: "already listening" } },
    }));
    agent.session = S({ activity: "LISTENING" });
    fireEvent.click(screen.getByTestId("mic-button"));
    await waitFor(() => expect(recorder.active).toBe(true));
    const paths = agent.paths();
    expect(paths.lastIndexOf("GET /status")).toBeGreaterThan(
      paths.indexOf("POST /session/s1/listen"),
    );
  });

  it("microphone permission failure switches to text-only", async () => {
    const ctx = setup();
    const { agent, recorder } = ctx;
    await startConversation(ctx, false);
    recorder.failWith = new MicError("denied");
    agent.on("POST /session/:id/listen", () => ({
      body: { session: S({ activity: "LISTENING" }) },
    }));
    fireEvent.click(screen.getByTestId("mic-button"));
    expect(await screen.findByTestId("banner-micBlocked")).toBeTruthy();
    expect(screen.queryByTestId("mic-button")).toBeNull();
    expect(screen.getByRole("textbox")).toBeTruthy();
  });
});

describe("inactivity", () => {
  it("reminder and final warning overlays post touch on continue; timeout ends the session", async () => {
    const ctx = setup();
    const { agent, es } = ctx;
    await startConversation(ctx, false);

    act(() => es().session({ type: "session.reminder", session: S() }));
    expect(await screen.findByText("क्या आप अभी भी यहाँ हैं?")).toBeTruthy();
    fireEvent.click(screen.getByTestId("keep-alive"));
    await waitFor(() => expect(screen.queryByText("क्या आप अभी भी यहाँ हैं?")).toBeNull());
    expect(agent.paths()).toContain("POST /session/s1/touch");

    const deadline = new Date(Date.now() + 60_000).toISOString();
    act(() =>
      es().session({ type: "session.final_warning", session: S({ idle_deadline: deadline }) }),
    );
    expect(await screen.findByText("बातचीत जल्द बंद हो जाएगी")).toBeTruthy();
    const count = screen.getByTestId("countdown").textContent ?? "";
    expect(Number.parseInt(count, 10)).toBeGreaterThanOrEqual(58);
    fireEvent.click(screen.getByTestId("keep-alive"));
    await waitFor(() => expect(screen.queryByText("बातचीत जल्द बंद हो जाएगी")).toBeNull());
    expect(agent.paths().filter((p) => p.endsWith("/touch")).length).toBeGreaterThanOrEqual(2);

    act(() =>
      es().session({ type: "session.ended", session: IDLE, end_reason: "inactivity_timeout" }),
    );
    expect(await screen.findByTestId("ended-screen")).toBeTruthy();
    agent.session = { ...IDLE };
    expect(await screen.findByTestId("start-button")).toBeTruthy();
  });
  it("session end while recording releases the microphone", async () => {
    const ctx = setup();
    const { agent, recorder, es } = ctx;
    await startConversation(ctx, false);
    agent.on("POST /session/:id/listen", () => ({
      body: { session: S({ activity: "LISTENING" }) },
    }));
    fireEvent.click(screen.getByTestId("mic-button"));
    await waitFor(() => expect(recorder.active).toBe(true));
    act(() =>
      es().session({ type: "session.ended", session: IDLE, end_reason: "inactivity_timeout" }),
    );
    expect(await screen.findByTestId("ended-screen")).toBeTruthy();
    expect(recorder.aborted).toBe(1);
    expect(recorder.active).toBe(false);
    expect(agent.paths()).not.toContain("POST /session/s1/audio");
  });
});

describe("degraded mode", () => {
  it("no voice in/out: banners, no mic, no repeat, text works, no audio played", async () => {
    const ctx = setup({ voice_input: false, voice_output: false });
    const { agent, player } = ctx;
    await startConversation(ctx, false);
    expect(screen.getByTestId("banner-noVoiceIn")).toBeTruthy();
    expect(screen.getByTestId("banner-noVoiceOut")).toBeTruthy();
    expect(screen.queryByTestId("mic-button")).toBeNull();
    expect(screen.queryByTestId("repeat-button")).toBeNull();

    agent.on("POST /session/:id/text", () => ({
      body: { session: S({ activity: "READY", turns: 1 }), reply: reply({ text: "लिखित उत्तर" }) },
    }));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "जाति प्रमाण पत्र" } });
    fireEvent.click(screen.getByRole("button", { name: "भेजें" }));
    await findReply("लिखित उत्तर");
    expect(player.played).toEqual([]);
  });

  it("health events update banners live", async () => {
    const ctx = setup();
    await startConversation(ctx, false);
    expect(screen.queryByTestId("banner-noVoiceIn")).toBeNull();
    act(() =>
      ctx.es().emit("health", {
        overall: "DEGRADED",
        capabilities: { ...CAPS_ALL, voice_input: false },
      }),
    );
    expect(await screen.findByTestId("banner-noVoiceIn")).toBeTruthy();
    expect(screen.queryByTestId("mic-button")).toBeNull();
  });

  it("SSE loss shows the offline banner and reconnect resyncs from /status", async () => {
    const ctx = setup();
    const { agent, es } = ctx;
    await screen.findByTestId("start-button");
    act(() => es().open());
    const before = agent.paths().filter((p) => p === "GET /status").length;
    act(() => es().fail());
    expect(await screen.findByTestId("banner-offline")).toBeTruthy();
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2), { timeout: 2_500 });
    agent.session = S({ activity: "READY" }); // GPIO session started while offline
    act(() => es().open());
    await waitFor(() =>
      expect(agent.paths().filter((p) => p === "GET /status").length).toBe(before + 1),
    );
    expect(await screen.findByRole("button", { name: "हिन्दी" })).toBeTruthy();
    expect(screen.queryByTestId("banner-offline")).toBeNull();
  });
});

describe("crisis", () => {
  it("shows a prominent alert panel with large numbers from the reply", async () => {
    const ctx = setup();
    const { agent } = ctx;
    await startConversation(ctx, false);
    agent.on("POST /session/:id/text", () => ({
      body: {
        session: S({ activity: "READY", turns: 1 }),
        reply: reply({
          kind: "crisis",
          text: "आप अकेले नहीं हैं। अभी 12345 पर फ़ोन करें।",
          cards: [
            { scheme_id: "help", title: "मदद", verified: true, sections: [], helpline: "67890" },
          ],
        }),
      },
    }));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "मदद" } });
    fireEvent.submit(screen.getByTestId("text-form"));
    const panel = await screen.findByTestId("crisis-panel");
    expect(panel.getAttribute("role")).toBe("alert");
    expect(panel.textContent).toContain("आप अकेले नहीं हैं।");
    const numbers = screen.getAllByTestId("crisis-number").map((n) => n.textContent);
    expect(numbers).toEqual(["67890", "12345"]);
  });
});

describe("physical button (Pi)", () => {
  it("shows the press instruction; GPIO start via SSE; English switches language", async () => {
    const ctx = setup({ sim_button: false });
    const { agent, es } = ctx;
    expect(await screen.findByTestId("press-instruction")).toBeTruthy();
    expect(screen.queryByTestId("start-button")).toBeNull();

    agent.session = S({ activity: "READY" });
    act(() => es().session({ type: "session.started", session: agent.session }));
    agent.on("POST /session/:id/language", (call) => {
      expect(call.body).toEqual({ language: "en" });
      agent.session = S({ language: "en" });
      return { body: { session: agent.session } };
    });
    fireEvent.click(await screen.findByRole("button", { name: "English" }));
    expect(await screen.findByText("Before we start")).toBeTruthy();
    expect(
      screen.getByText("Please do not share your Aadhaar number, OTP, PIN or bank details."),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(
      await screen.findByText("Press the mic button and ask your question, or type below."),
    ).toBeTruthy();
    expect(screen.getByTestId("end-button").textContent).toBe("End session");
  });

  it("GPIO start: shows and plays the greeting carried on session.started", async () => {
    const ctx = setup({ sim_button: false });
    const { agent, es, player } = ctx;
    expect(await screen.findByTestId("press-instruction")).toBeTruthy();
    agent.session = S({ activity: "SPEAKING" });
    agent.on("POST /session/:id/speaking-done", () => {
      agent.session = S({ activity: "READY" });
      return { body: { session: agent.session } };
    });
    act(() =>
      es().session({
        type: "session.started",
        session: agent.session,
        reply: reply({ kind: "greeting", text: "नमस्ते, बटन से शुरू।" }),
        audio_url: AUDIO(0),
      }),
    );
    fireEvent.click(await screen.findByRole("button", { name: "हिन्दी" }));
    fireEvent.click(await screen.findByRole("button", { name: "आगे बढ़ें" }));
    await findReply("नमस्ते, बटन से शुरू।");
    await waitFor(() => expect(player.played).toContain(AUDIO(0)));
    await waitFor(() =>
      expect(agent.calls.some((c) => c.path === "/session/s1/speaking-done")).toBe(true),
    );
    // The greeting came with the event: no extra greeting request.
    expect(agent.calls.some((c) => c.path === "/session/s1/greeting")).toBe(false);
  });

  it("language change asks the agent for a greeting in the new language", async () => {
    const ctx = setup({ sim_button: false });
    const { agent, es, player } = ctx;
    expect(await screen.findByTestId("press-instruction")).toBeTruthy();
    agent.session = S({ activity: "SPEAKING" });
    act(() =>
      es().session({
        type: "session.started",
        session: agent.session,
        reply: reply({ kind: "greeting", text: "नमस्ते" }),
        audio_url: AUDIO(0),
      }),
    );
    agent.on("POST /session/:id/language", () => {
      agent.session = S({ language: "en", activity: "SPEAKING" });
      return { body: { session: agent.session } };
    });
    agent.on("POST /session/:id/greeting", () => {
      agent.session = S({ language: "en", activity: "READY" });
      return {
        body: {
          session: agent.session,
          reply: reply({ kind: "greeting", text: "Hello, I am Jan Sahayak." }),
        },
      };
    });
    fireEvent.click(await screen.findByRole("button", { name: "English" }));
    await waitFor(() =>
      expect(agent.calls.some((c) => c.path === "/session/s1/greeting")).toBe(true),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Continue" }));
    await findReply("Hello, I am Jan Sahayak.");
    // The Hindi greeting audio is never played in an English session.
    expect(player.played).not.toContain(AUDIO(0));
  });

  it("button_disabled from start shows a notice and stays on welcome", async () => {
    const { agent } = setup();
    agent.on("POST /session/start", () => ({
      status: 403,
      body: { error: { code: "button_disabled", message: "use GPIO" } },
    }));
    fireEvent.click(await screen.findByTestId("start-button"));
    expect((await screen.findByTestId("notice")).textContent).toBe("कृपया कियोस्क का हरा बटन दबाएँ।");
    expect(screen.getByTestId("start-button")).toBeTruthy();
  });
});
