import { useEffect, useState } from "react";
import type { Lang } from "../api/types";
import { STRINGS, t } from "../i18n";
import type { KioskController } from "../state/controller";
import type { UiState } from "../state/machine";

export function Welcome({ state, ctl }: { state: UiState; ctl: KioskController }) {
  const hi = STRINGS.hi;
  const en = STRINGS.en;
  const sim = !!state.caps?.sim_button;
  const unavailable = state.overall === "UNAVAILABLE";
  return (
    <main className="welcome">
      <h1 className="welcome__title">
        <span lang="hi">{hi.appName}</span>
        <span className="welcome__en" lang="en">
          {en.appName}
        </span>
      </h1>
      <p className="welcome__tag">
        <span lang="hi">{hi.tagline}</span>
        <br />
        <span lang="en">{en.tagline}</span>
      </p>
      {unavailable ? (
        <p className="welcome__unavailable" role="status">
          <span lang="hi">{hi.unavailable}</span>
          <br />
          <span lang="en">{en.unavailable}</span>
        </p>
      ) : sim ? (
        <button
          type="button"
          className="btn btn--start"
          onClick={() => void ctl.pressStart()}
          disabled={state.busy}
          data-testid="start-button"
        >
          <span lang="hi">{hi.start}</span>
          <span className="btn--start__sep" aria-hidden="true">
            {" / "}
          </span>
          <span lang="en">{en.start}</span>
        </button>
      ) : (
        <div className="welcome__press" data-testid="press-instruction">
          <span className="welcome__dot" aria-hidden="true" />
          <p>
            <span lang="hi">{hi.pressButton}</span>
            <br />
            <span lang="en">{en.pressButton}</span>
          </p>
        </div>
      )}
    </main>
  );
}

export function LanguageScreen({ ctl }: { ctl: KioskController }) {
  const choose = (l: Lang) => void ctl.chooseLanguage(l);
  return (
    <main className="setup">
      <h1 className="setup__title">
        <span lang="hi">{STRINGS.hi.chooseLanguage}</span>
        <span className="setup__sub" lang="en">
          {STRINGS.en.chooseLanguage}
        </span>
      </h1>
      <div className="langs">
        <button
          type="button"
          className="btn btn--lang btn--primary"
          lang="hi"
          onClick={() => choose("hi")}
        >
          हिन्दी
        </button>
        <button type="button" className="btn btn--lang" lang="en" onClick={() => choose("en")}>
          English
        </button>
      </div>
    </main>
  );
}

export function Privacy({ state, ctl }: { state: UiState; ctl: KioskController }) {
  const s = t(state.lang);
  return (
    <main className="setup">
      <h1 className="setup__title">{s.privacyTitle}</h1>
      <ul className="privacy" data-testid="privacy-list">
        {s.privacy.map((line) => (
          <li key={line}>{line}</li>
        ))}
      </ul>
      <button
        type="button"
        className="btn btn--primary btn--wide"
        onClick={() => void ctl.ackPrivacy()}
      >
        {s.continue}
      </button>
    </main>
  );
}

export function Ended({ state }: { state: UiState }) {
  const s = t(state.lang);
  return (
    <main className="ended" data-testid="ended-screen">
      <h1 className="ended__title">{s.endedTitle}</h1>
      {state.farewell?.text && <p className="ended__farewell">{state.farewell.text}</p>}
      <p className="ended__body">{s.endedBody}</p>
      <p className="ended__returning">{s.returning}</p>
    </main>
  );
}

export function Boot() {
  return (
    <main className="welcome" aria-busy="true">
      <h1 className="welcome__title">
        <span lang="hi">{STRINGS.hi.appName}</span>
      </h1>
      <span className="spinner spinner--big" aria-hidden="true" />
    </main>
  );
}

/** Seconds until an ISO deadline, ticking every second. */
function useCountdown(deadline: string | undefined): number | null {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!deadline) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [deadline]);
  if (!deadline) return null;
  const ms = Date.parse(deadline);
  if (Number.isNaN(ms)) return null;
  return Math.max(0, Math.ceil((ms - now) / 1000));
}

export function InactivityOverlay({ state, ctl }: { state: UiState; ctl: KioskController }) {
  const s = t(state.lang);
  const final = state.overlay === "final_warning";
  const left = useCountdown(final ? state.session?.idle_deadline : undefined);
  if (state.overlay === "none") return null;
  return (
    <div className="overlay" role="alertdialog" aria-modal="true" aria-labelledby="ov-title">
      <div className={`overlay__box ${final ? "overlay__box--final" : ""}`}>
        <h2 id="ov-title">{final ? s.finalWarning : s.reminder}</h2>
        <p>{final ? s.finalWarningBody : s.reminderBody}</p>
        {final && left !== null && (
          <p className="overlay__count" data-testid="countdown">
            {s.secondsLeft(left)}
          </p>
        )}
        <button
          type="button"
          className="btn btn--primary btn--wide"
          onClick={() => void ctl.keepAlive()}
          data-testid="keep-alive"
        >
          {s.keepGoing}
        </button>
      </div>
    </div>
  );
}
