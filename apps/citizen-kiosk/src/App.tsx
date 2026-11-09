import { t } from "./i18n";
import { Conversation } from "./components/Conversation";
import {
  Boot,
  Ended,
  InactivityOverlay,
  LanguageScreen,
  Privacy,
  Welcome,
} from "./components/Screens";
import type { ControllerDeps } from "./state/controller";
import type { UiState } from "./state/machine";
import { useKiosk } from "./state/useKiosk";

const inSession = (s: UiState) =>
  s.screen === "language" || s.screen === "privacy" || s.screen === "conversation";

function Banners({ state }: { state: UiState }) {
  const s = t(state.lang);
  const items: { key: string; text: string; tone: "warn" | "info" }[] = [];
  if (state.conn === "offline") items.push({ key: "offline", text: s.offline, tone: "warn" });
  if (inSession(state) && state.caps) {
    if (!state.caps.voice_input) items.push({ key: "noVoiceIn", text: s.noVoiceIn, tone: "info" });
    else if (state.micBlocked) items.push({ key: "micBlocked", text: s.micBlocked, tone: "info" });
    if (!state.caps.voice_output) items.push({ key: "noVoiceOut", text: s.noVoiceOut, tone: "info" });
  }
  if (items.length === 0) return null;
  return (
    <div className="banners" role="status">
      {items.map((b) => (
        <p key={b.key} className={`banner banner--${b.tone}`} data-testid={`banner-${b.key}`}>
          {b.text}
        </p>
      ))}
    </div>
  );
}

function Notice({ state, onClose }: { state: UiState; onClose: () => void }) {
  if (!state.notice || state.notice === "micBlocked") return null;
  const s = t(state.lang);
  return (
    <button type="button" className="notice" onClick={onClose} role="alert" data-testid="notice">
      {s[state.notice]}
    </button>
  );
}

export function App({ deps }: { deps: ControllerDeps }) {
  const { state, ctl } = useKiosk(deps);
  const s = t(state.lang);
  const session = inSession(state);

  let screen: React.ReactNode;
  switch (state.screen) {
    case "boot":
      screen = <Boot />;
      break;
    case "welcome":
      screen = <Welcome state={state} ctl={ctl} />;
      break;
    case "language":
      screen = <LanguageScreen ctl={ctl} />;
      break;
    case "privacy":
      screen = <Privacy state={state} ctl={ctl} />;
      break;
    case "conversation":
      screen = <Conversation state={state} ctl={ctl} />;
      break;
    case "ended":
      screen = <Ended state={state} />;
      break;
  }

  return (
    <div className={`app app--${state.screen}`} lang={state.lang}>
      {session && (
        <header className="topbar">
          <span className="topbar__name">{s.appName}</span>
          <button
            type="button"
            className="btn btn--end"
            onClick={() => void ctl.endSession()}
            data-testid="end-button"
          >
            {s.end}
          </button>
        </header>
      )}
      <Banners state={state} />
      {screen}
      <Notice state={state} onClose={() => ctl.dismissNotice()} />
      {session && <InactivityOverlay state={state} ctl={ctl} />}
      {/* Screen-reader announcement of each new reply. */}
      <div className="sr-only" aria-live="polite" aria-atomic="true" data-testid="live-region">
        {state.screen === "conversation" && state.reply ? state.reply.text : ""}
      </div>
    </div>
  );
}
