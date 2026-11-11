// Types of the device local API v1 (docs/API_DOCUMENTATION.md section 1).
// Keep in sync with services/device-agent/internal/session and
// services/device-agent/internal/dialogue/contract.go.

export type Activity = "IDLE" | "READY" | "LISTENING" | "PROCESSING" | "SPEAKING";
export type Lang = "hi" | "en";

export interface SessionSnapshot {
  id?: string;
  activity: Activity;
  language?: Lang;
  started_at?: string;
  last_active_at?: string;
  idle_deadline?: string;
  camera_consent: boolean;
  turns: number;
}

export interface Option {
  id: string;
  label: string;
}

export interface CardSection {
  title: string;
  items: string[];
}

export interface Card {
  scheme_id: string;
  title: string;
  verified: boolean;
  last_verified?: string;
  sections: CardSection[];
  official_url?: string;
  helpline?: string;
  disclaimer?: string;
}

export type ReplyKind =
  | "greeting"
  | "question"
  | "answer"
  | "repeat"
  | "redirect"
  | "escalate"
  | "crisis"
  | "goodbye";

export interface Reply {
  kind: ReplyKind;
  text: string;
  speech?: string;
  options?: Option[];
  cards?: Card[];
  listen_next: boolean;
  end_session: boolean;
  used_llm: boolean;
}

export interface Transcript {
  text: string;
  confidence: number;
  no_speech: boolean;
}

export interface TurnResult {
  session: SessionSnapshot;
  transcript?: Transcript;
  reply: Reply;
  /** GET this to play the spoken reply (WAV); absent if TTS unavailable. */
  audio_url?: string;
}

/** Response of POST /session/start, /session/{id}/greeting and /session/{id}/end. */
export interface ReplyResult {
  session: SessionSnapshot;
  reply: Reply;
  audio_url?: string;
}

/** Response of listen, speaking-done, language and touch. */
export interface SessionResult {
  session: SessionSnapshot;
}

export type HealthState =
  | "HEALTHY"
  | "DEGRADED"
  | "UNAVAILABLE"
  | "RECOVERING"
  | "REQUIRES_ATTENTION";

export interface Capabilities {
  voice_input: boolean;
  voice_output: boolean;
  llm: boolean;
  sim_button: boolean;
  camera: boolean;
}

export interface DeviceStatus {
  device_id: string;
  version: string;
  overall: HealthState;
  capabilities: Capabilities;
  session: SessionSnapshot;
}

export type SessionEventType =
  | "session.started"
  | "session.activity"
  | "session.reminder"
  | "session.final_warning"
  | "session.ended"
  | "session.language"
  | "session.camera_consent";

/** Payload of the SSE `session` event. */
export interface SessionEvent {
  type: SessionEventType;
  session: SessionSnapshot;
  end_reason?: string;
  /**
   * Only on `session.started` for sessions started by the physical button:
   * the greeting (no HTTP response carries it).
   */
  reply?: Reply;
  audio_url?: string;
}

/** Payload of the SSE `health` event. */
export interface HealthEvent {
  overall: HealthState;
  capabilities: Capabilities;
}

export type ErrorCode =
  | "no_session"
  | "wrong_session"
  | "bad_state"
  | "bad_request"
  | "too_large"
  | "bad_audio"
  | "unavailable"
  | "button_disabled"
  | "internal";

/** Error body of every non-2xx response. */
export interface ErrorBody {
  error: { code: string; message: string };
}
