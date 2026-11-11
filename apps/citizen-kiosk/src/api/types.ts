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
  | "no_consent"
  | "bad_image"
  | "internal";

/** Error body of every non-2xx response. */
export interface ErrorBody {
  error: { code: string; message: string };
}

// ---- Document check (POST /session/{id}/camera-consent, /document, /document/confirm)

export type DocType =
  | "aadhaar"
  | "ration_card"
  | "caste_certificate"
  | "income_certificate"
  | "residence_certificate"
  | "bank_passbook"
  | "voter_id"
  | "pan"
  | "land_record"
  | "birth_certificate"
  | "school_certificate"
  | "unknown";

/** An identifier seen on the document, masked to its last 4 characters. */
export interface MaskedId {
  kind: string;
  value: string;
}

/** What the agent read from one photo. Never contains the OCR text. */
export interface DocumentInfo {
  doc_type: DocType;
  label: string;
  confidence: "high" | "low" | "none";
  name?: string;
  issuing_authority?: string;
  issue_date?: string;
  masked_ids?: MaskedId[];
}

export interface DocumentResult {
  session: SessionSnapshot;
  document: DocumentInfo;
  /** "यह <type> लगता है — क्या यह सही है?" or how to retake. */
  text: string;
  can_confirm: boolean;
  /** OCR does not verify authenticity; the photo was not kept. */
  notice: string;
}

export type CoverageStatus = "seen" | "not_seen" | "not_checkable";

export interface CoverageItem {
  name: string;
  note?: string;
  optional: boolean;
  status: CoverageStatus;
}

export interface Coverage {
  scheme_id: string;
  scheme_name: string;
  mode: "factual" | "unverified_dev" | "name_only";
  verified: boolean;
  items?: CoverageItem[];
  message?: string;
  disclaimer?: string;
}

export interface ConfirmResult {
  session: SessionSnapshot;
  text: string;
  confirmed: { doc_type: DocType; label: string }[] | null;
  coverage?: Coverage;
  caveat?: string;
  notice: string;
}
