// Shapes of the central admin API (services/central-api/openapi.yaml and the
// Go handlers in services/central-api/internal/server). Keep in sync.

export type Role =
  | "viewer"
  | "device_admin"
  | "scheme_editor"
  | "scheme_reviewer"
  | "auditor"
  | "admin";

export interface User {
  id: string;
  username: string;
  display_name: string;
  roles: Role[];
}

export type HealthState =
  | "HEALTHY"
  | "DEGRADED"
  | "UNAVAILABLE"
  | "RECOVERING"
  | "REQUIRES_ATTENTION";

export interface Device {
  id: string;
  name: string;
  district: string;
  public_key: string;
  status: "active" | "revoked";
  registered_at: string;
  revoked_at?: string;
  revoked_reason?: string;
  last_seen_at?: string;
}

export interface HeartbeatSummary {
  received_at: string;
  agent_version: string;
  bundle_version: string;
  overall_health: HealthState;
  outbox_pending: number;
  outbox_failed: number;
  outbox_conflict: number;
}

export interface DeviceListItem extends Device {
  last_heartbeat?: HeartbeatSummary;
}

export interface HealthComponent {
  name: string;
  state: HealthState;
  critical?: boolean;
  detail?: string;
}

export interface HeartbeatRecord {
  received_at: string;
  device_time: string;
  agent_version: string;
  os: string;
  bundle_version: string;
  models: Record<string, string> | null;
  overall_health: HealthState;
  components: HealthComponent[] | null;
  outbox_pending: number;
  outbox_uploading: number;
  outbox_acknowledged: number;
  outbox_delete_eligible: number;
  outbox_failed: number;
  outbox_conflict: number;
  outbox_total: number;
  outbox_capacity: number;
  outbox_oldest_pending_at?: string;
  sync_last_success_at?: string;
  sync_consecutive_failures: number;
  disk_free_bytes?: number;
  disk_total_bytes?: number;
  mem_available_bytes?: number;
  mem_total_bytes?: number;
  cpu_temp_c?: number;
  throttled?: boolean;
  uptime_seconds?: number;
}

export interface DeviceDetail {
  device: Device;
  last_heartbeat?: HeartbeatRecord;
  recent_heartbeats: HeartbeatRecord[];
  sync: {
    events_total: number;
    events_24h: number;
    last_event_received_at?: string;
    heartbeats_kept: number;
  };
}

export type RecordStatus = "VERIFIED" | "NEEDS_REVIEW" | "OUTDATED" | "UNVERIFIED";

export interface SchemeListItem {
  id: string;
  current_version: number;
  status: RecordStatus;
  kind: string;
  category: string;
  name_hi: string;
  name_en: string;
  updated_at: string;
  latest_verified_version?: number;
  withdrawn: boolean;
}

export interface Text {
  hi: string;
  en: string;
}

export interface Doc {
  name: Text;
  note?: Text;
}

export interface Source {
  url: string;
  title: string;
  accessed_on: string;
  excerpt?: string;
}

export interface Verification {
  status: RecordStatus;
  verified_by?: string;
  verified_at?: string;
  review_due?: string;
  notes?: string;
}

export interface Question {
  id: string;
  ask: Text;
  kind: string;
  choices?: Text[];
  ineligible_if?: string[];
}

/** internal/scheme.Record (Go). */
export interface SchemeRecord {
  id: string;
  version: number;
  kind: "scheme" | "service" | "";
  name: Text;
  aliases: string[] | null;
  state: string;
  department: Text;
  category: string;
  keywords: string[] | null;
  summary: Text;
  eligibility: Text[] | null;
  benefits: Text[] | null;
  age_rule?: Text;
  income_rule?: Text;
  questions?: Question[];
  mandatory_docs: Doc[] | null;
  optional_docs?: Doc[];
  alternative_evidence?: Text[];
  application_steps: Text[] | null;
  where_to_apply: Text[] | null;
  official_url: string;
  helpline?: string;
  fees_note?: Text;
  exceptions?: Text[];
  sources: Source[] | null;
  last_verified?: string;
  verification: Verification;
}

export interface VersionMeta {
  version: number;
  status: RecordStatus;
  edited_by: string;
  edited_at: string;
  verified_by?: string;
  verified_at?: string;
  withdrawn_by?: string;
  based_on_version?: number;
  change_note?: string;
}

export interface SchemeDetail {
  id: string;
  current: VersionMeta;
  record: SchemeRecord;
  history: VersionMeta[];
}

export interface SchemeVersionResponse {
  id: string;
  version: VersionMeta;
  record: SchemeRecord;
}

export interface BundleMeta {
  bundle_version: string;
  created_at: string;
  published_by: string;
  key_id: string;
  record_count: number;
  verified_count: number;
  sha256: string;
}

/** A null count means "suppressed" (1–4, or secondary suppression). */
export type Count = number | null;

export interface CountCell {
  key: string;
  count: Count;
}

export interface AnalyticsSummary {
  from: string;
  to: string;
  timezone: string;
  device_id?: string;
  district?: string;
  suppression: { applied: boolean; min_count: number };
  sessions_total: Count;
  avg_turns_per_session: number | null;
  sessions_per_day: { day: string; sessions: Count }[];
  languages: CountCell[];
  end_reasons: CountCell[];
  categories: { category: string; turns: Count; sessions: Count; unresolved_turns: Count }[];
  turns_total: Count;
  turns_by_resolution: CountCell[];
  turns_by_source: CountCell[];
  turns_using_llm: Count;
  sessions_with_turns: Count;
  sessions_answered: Count;
  sessions_unresolved: Count;
  resolution_rate: number | null;
}

export interface AuditEntry {
  id: string;
  at: string;
  actor_user_id?: string;
  actor: string;
  action: string;
  target_type: string;
  target_id: string;
  details: unknown;
}

export interface AuditPage {
  entries: AuditEntry[];
  next_before?: string;
}
