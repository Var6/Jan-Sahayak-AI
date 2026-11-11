import { type FormEvent, useEffect, useRef, useState } from "react";
import { MAX_TEXT_CHARS } from "../api/client";
import type { Card, Lang, Reply } from "../api/types";
import { extractNumbers } from "../format";
import { t } from "../i18n";
import type { KioskController } from "../state/controller";
import { type UiState, voiceInputAvailable, voiceOutputAvailable } from "../state/machine";
import { DocumentCheck } from "./DocumentCheck";
import { PhoneIcon, ReplayIcon, SendIcon } from "./Icons";
import { indicatorOf, MicButton } from "./MicButton";
import { SchemeCard } from "./SchemeCard";

function crisisNumbers(reply: Reply): string[] {
  const fromCards = (reply.cards ?? [])
    .map((c: Card) => c.helpline)
    .filter((h): h is string => !!h);
  const all = [...fromCards, ...extractNumbers(reply.text)];
  return [...new Set(all)].slice(0, 4);
}

function CrisisPanel({ reply, lang }: { reply: Reply; lang: Lang }) {
  const s = t(lang);
  const numbers = crisisNumbers(reply);
  return (
    <section className="crisis" role="alert" data-testid="crisis-panel">
      <h2 className="crisis__title">{s.crisisTitle}</h2>
      <p className="crisis__text">{reply.text}</p>
      {numbers.length > 0 && (
        <div className="crisis__numbers">
          <p className="crisis__call">{s.crisisCall}</p>
          <ul>
            {numbers.map((n) => (
              <li key={n} className="crisis__number" data-testid="crisis-number">
                <PhoneIcon />
                <span>{n}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}

function TextInput({
  lang,
  disabled,
  onSend,
}: {
  lang: Lang;
  disabled: boolean;
  onSend: (text: string) => Promise<boolean>;
}) {
  const s = t(lang);
  const [text, setText] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!text.trim()) return;
    if (await onSend(text)) setText("");
  };
  return (
    <form className="textin" onSubmit={submit} data-testid="text-form">
      <input
        className="textin__input"
        type="text"
        lang={lang}
        inputMode="text"
        enterKeyHint="send"
        autoComplete="off"
        autoCorrect="off"
        spellCheck={false}
        maxLength={MAX_TEXT_CHARS}
        placeholder={s.typeHere}
        aria-label={s.typeHere}
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      <button
        type="submit"
        className="btn btn--send"
        disabled={disabled || !text.trim()}
        aria-label={s.send}
      >
        <SendIcon />
        <span className="btn__text">{s.send}</span>
      </button>
    </form>
  );
}

export function Conversation({ state, ctl }: { state: UiState; ctl: KioskController }) {
  const s = t(state.lang);
  const reply = state.reply;
  const scrollRef = useRef<HTMLDivElement>(null);
  const indicator = indicatorOf(state);
  const voiceIn = voiceInputAvailable(state);
  const voiceOut = voiceOutputAvailable(state);
  const interacting = state.busy || state.voice === "uploading" || state.voice === "opening";
  const [docOpen, setDocOpen] = useState(false);
  const sessionId = state.session?.id;
  // Offer the document check while a scheme is on screen (its card) and
  // the kiosk can read documents (camera + OCR healthy).
  const canCheckDocs =
    !!state.caps?.camera &&
    !!sessionId &&
    reply?.kind !== "crisis" &&
    (reply?.cards?.length ?? 0) > 0;

  // Leaving the conversation (session end) also leaves document mode.
  useEffect(() => () => ctl.setDocumentMode(false), [ctl]);

  const openDocs = () => {
    ctl.touchSoon(true);
    ctl.setDocumentMode(true);
    setDocOpen(true);
  };
  const closeDocs = () => {
    ctl.setDocumentMode(false);
    setDocOpen(false);
  };

  // New reply → scroll to its top.
  // biome-ignore lint/correctness/useExhaustiveDependencies: replySeq is the trigger
  useEffect(() => {
    scrollRef.current?.scrollTo?.({ top: 0 });
  }, [state.replySeq]);

  if (docOpen && sessionId) {
    return (
      <main className="conv conv--doc" onPointerDown={() => ctl.touchSoon()}>
        <DocumentCheck
          api={ctl.api}
          camera={ctl.camera}
          sessionId={sessionId}
          lang={state.lang}
          onActivity={() => ctl.touchSoon(true)}
          onClose={closeDocs}
        />
      </main>
    );
  }

  return (
    <main className="conv" onPointerDown={() => ctl.touchSoon()}>
      <div className="conv__scroll" ref={scrollRef}>
        {state.transcript?.text && !state.transcript.no_speech && (
          <p className="you" data-testid="transcript">
            <span className="you__label">{s.you}:</span> “{state.transcript.text}”
          </p>
        )}
        {reply?.kind === "crisis" ? (
          <CrisisPanel reply={reply} lang={state.lang} />
        ) : (
          <p className={`reply reply--${reply?.kind ?? "intro"}`} data-testid="reply-text">
            {reply ? reply.text : s.intro}
          </p>
        )}
        {reply?.cards && reply.cards.length > 0 && (
          <div className="cards">
            {reply.cards.map((c) => (
              <SchemeCard key={c.scheme_id} card={c} lang={state.lang} />
            ))}
          </div>
        )}
        {canCheckDocs && (
          <button
            type="button"
            className="btn btn--doc"
            onClick={openDocs}
            disabled={interacting}
            data-testid="doc-open"
          >
            {s.doc.open}
          </button>
        )}
        {reply?.options && reply.options.length > 0 && (
          <div className="options">
            {reply.options.map((o) => (
              <button
                key={o.id}
                type="button"
                className="btn btn--option"
                disabled={interacting}
                onClick={() => void ctl.chooseOption(o.id)}
              >
                {o.label}
              </button>
            ))}
          </div>
        )}
      </div>

      <div className="dock">
        {voiceIn && (
          <MicButton
            indicator={indicator}
            level={state.level}
            lang={state.lang}
            disabled={false}
            onPress={() => void ctl.micPressed()}
          />
        )}
        <div className="dock__side">
          <TextInput lang={state.lang} disabled={interacting} onSend={(x) => ctl.sendText(x)} />
          {voiceOut && (
            <button
              type="button"
              className="btn btn--repeat"
              onClick={() => void ctl.repeat()}
              disabled={!state.lastAudioUrl || state.voice !== "idle" || interacting}
              data-testid="repeat-button"
            >
              <ReplayIcon />
              <span className="btn__text">{s.repeat}</span>
            </button>
          )}
        </div>
      </div>
    </main>
  );
}
