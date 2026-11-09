import type { CSSProperties } from "react";
import type { Lang } from "../api/types";
import { t } from "../i18n";
import type { UiState } from "../state/machine";
import { MicIcon, StopIcon } from "./Icons";

export type Indicator = "idle" | "opening" | "listening" | "processing" | "speaking";

export function indicatorOf(s: UiState): Indicator {
  switch (s.voice) {
    case "recording":
      return "listening";
    case "opening":
      return "opening";
    case "uploading":
      return "processing";
    case "playing":
      return "speaking";
    default:
      if (s.busy || s.session?.activity === "PROCESSING") return "processing";
      return "idle";
  }
}

interface Props {
  indicator: Indicator;
  level: number;
  lang: Lang;
  disabled: boolean;
  onPress: () => void;
}

export function MicButton({ indicator, level, lang, disabled, onPress }: Props) {
  const s = t(lang);
  const label = s.mic[indicator];
  const action =
    indicator === "listening" ? s.mic.stop : indicator === "speaking" ? s.mic.interrupt : s.mic.idle;
  const style = { "--level": level.toFixed(2) } as CSSProperties;
  return (
    <div className={`mic mic--${indicator}`} style={style}>
      <button
        type="button"
        className="mic__btn"
        onClick={onPress}
        disabled={disabled || indicator === "processing" || indicator === "opening"}
        aria-label={action}
        data-testid="mic-button"
      >
        <span className="mic__ring" aria-hidden="true" />
        <span className="mic__ring mic__ring--2" aria-hidden="true" />
        {indicator === "listening" ? <StopIcon /> : <MicIcon />}
      </button>
      <div className="mic__status" data-testid="mic-status" aria-hidden="true">
        {indicator === "processing" && <span className="spinner" />}
        {indicator === "speaking" && (
          <span className="bars">
            <i />
            <i />
            <i />
            <i />
          </span>
        )}
        <span className="mic__label">{label}</span>
      </div>
    </div>
  );
}
