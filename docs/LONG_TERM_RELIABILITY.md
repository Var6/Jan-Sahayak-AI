# Long-Term Reliability

Goal: unattended operation in public places for years, with predictable
resource use, automatic recovery and easy servicing. We do **not** claim any
device runs ten years without maintenance; we engineer for recovery and
measure. Claims below are marked *measured*, *tested* or *planned*.

## 1. Memory budgets (Pi 5, 8 GB — provisional until Pi benchmark)

| Process | Budget (systemd `MemoryMax`) | Expected | Basis |
|---|---|---|---|
| device-agent (Go) | 256 MB, `GOMEMLIMIT` 192 MB | 20–60 MB | *measured on M1* (Phase 12 soak): 19–29 MB RSS over 60 min with faults, 34 MB peak under the 80-req/s outbox-full stress |
| speech-worker (STT + VAD + TTS) | 1 GB | ~0.8 GB | 441 MB STT + 320–400 MB TTS (*measured on M1*) |
| llama-server (Gemma 4 E2B Q4_0, `-c 4096`) | 2.5 GB | 1.3–1.5 GB | *measured on M1* |
| Chromium kiosk | 1 GB | 300–600 MB | planned measurement |
| OS + journald + headroom | ≥ 1.5 GB | | |

## 2. Bounded everything (device agent)

| Resource | Bound | Where |
|---|---|---|
| Sessions | exactly 1 active | `session.Manager` |
| Session event subscribers (SSE) | 8, each with a 32-event drop-oldest buffer | `session` |
| Conversation history | 8 turns, 16 slots, 500-rune inputs | `dialogue` |
| Audio upload | 1 MB / 20–30 s, validated WAV header | `ai.ParseWAV`, API |
| LLM prompt | 24 KB; ≤ 256 output tokens | `ai.Llama` |
| AI calls | 1 in flight per runtime; per-call timeout; circuit breaker (3 failures → 30 s open) | `ai.gate` |
| Reply audio cache | ≤ 8 clips, RAM only, cleared at session end | API |
| Outbox | 100,000 rows; refuses new events when full, never drops old | `outbox` |
| Sync | ≤ 100 events/batch, ≤ 50 batches per wake-up, 30 s HTTP timeout, 1 MB response cap | `syncer` |
| Health checks | sequential, one goroutine, per-check timeout, panic-contained | `health` |
| SQLite page cache | ~8 MB, 4 connections | `store` |
| Backups | keep 7 | `store.Backup` |
| Logs | journald `SystemMaxUse=50M` | systemd drop-in |
| Goroutines | fixed set of long-lived workers started from `main`, all stopped via context | `cmd/device-agent` |

## 3. Supervision and watchdog

* Every process is a systemd unit with `Restart=always`, `RestartSec=5` and
  `StartLimitIntervalSec=0`: no start rate limit, so a persistent startup
  error is retried every 5 s forever instead of leaving the unit failed
  until a reboot (Pi OS bookworm's systemd 252 has no `RestartSteps`, so the
  delay is fixed). Startup errors that are not real faults degrade instead
  of failing (a missing GPIO button → `button` component, retried).
* The agent is `Type=notify` with `WatchdogSec=30`. `READY=1` is retried
  until the self-check passes. It sends `WATCHDOG=1` only while the SQLite
  ping and HTTP self-probe pass and, after a 90 s grace period, the health
  monitor (a completed database check), the session timer and the sync
  loop keep stamping their own heartbeats (a check that never completed
  counts as stalled). A genuine hang → no ping → systemd kills and restarts it.
* Shutdown gives each step its own budget: in-flight requests are cancelled
  and drained, then workers stop and the open session is recorded as ended,
  then the database closes (all within `TimeoutStopSec=30`).
* The speech worker exits(1) when a native inference has held its engine
  for 3× its timeout (its `/health` answers 503 `wedged` meanwhile), so
  systemd restarts it; the agent also treats three "busy" answers in a row
  as a fault, and a worker still loading after 5 min as REQUIRES_ATTENTION.
  Repeated restarts are counted and reported in the next heartbeat, so the
  watchdog never silently hides a recurring fault.
* The Pi's hardware watchdog (`/dev/watchdog`, bcm2835-wdt) is enabled in
  systemd (`RuntimeWatchdogSec=15`) so a kernel/systemd lock-up reboots.
* An AI runtime failure never crashes the kiosk: the agent marks it
  `UNAVAILABLE`, switches to text input / on-screen text / deterministic
  answers, and systemd restarts the runtime.

## 4. Health model

States: HEALTHY · DEGRADED · UNAVAILABLE · RECOVERING · REQUIRES_ATTENTION.
Components: database (critical), stt, tts, llm, speech worker, sync, disk,
memory, cpu temperature/throttling, goroutines, camera/mic/speaker (when HAL
reports them). A component that fails 10 consecutive checks escalates to
`REQUIRES_ATTENTION`. Optional components can only make the device
`DEGRADED`. *Tested* (`health` package).

## 5. Storage reliability

* SQLite WAL, `synchronous=FULL`, transactions for every multi-row change,
  migrations in one transaction each (power cut → previous version).
* `PRAGMA quick_check` at boot; corrupt DB → quarantined + newest verified
  backup restored (*tested*). Daily `VACUUM INTO` backup verified before
  rotation (*tested*).
* Binary refuses to open a DB schema newer than itself (protects rollbacks;
  *tested*, including against a real v1 store built from commit 6286b80).
* Before migrating an existing DB the agent writes a verified copy,
  `backups/pre-migration-v<N>-<time>.db`, and refuses to migrate if it
  cannot (the DB stays at the old schema, so the old binary still runs).
  These copies are not rotated (D-029; *tested*).
* Sessions a killed agent left open are closed at the next start with
  `end_reason = "error"` and their `session.ended` event (D-028; *tested*,
  found by the Phase 12 fault soak).
* Disk monitor raises DEGRADED at 80 % and REQUIRES_ATTENTION at 90 %.
* SD-card wear: no audio/image files, journald size cap, low write volume.
  **Recommendation:** boot from an NVMe SSD via the Pi 5 PCIe HAT, or at
  minimum a high-endurance (industrial / "endurance") microSD; mount `/tmp`
  as tmpfs; `noatime`.

## 6. Power and thermal

* Official 27 W USB-C PSU; optional UPS HAT for graceful shutdown on outage.
* Active cooler mandatory (LLM inference throttles a bare Pi quickly).
* Agent monitors `/sys/class/thermal/thermal_zone0/temp` and
  `vcgencmd get_throttled` (under-voltage, throttling flags) and reports them.

## 7. Software maintenance

* Semantic versions embedded at build (`-ldflags`), reported in heartbeats.
* Signed update bundles; A/B install directory with symlink switch; health
  gate after update; automatic rollback if the new version fails health
  within N minutes; DB migrations are forward-only and backed up first.
* Upgrade tests from each supported older version: v1 → v2 done in Phase 12
  (`scripts/dev/upgradetest/run.sh`, see §7a).

### 7a. Device database upgrade and rollback (tested 2026-11-11)

Schema versions so far: v1 (`0001_init.sql`), v2 (`0002_outbox_counter_claimed_at.sql`:
outbox row counter, `claimed_at`, fixed-width retry timestamps).

**Upgrade** (automatic): the new binary opens the DB, writes
`<data_dir>/backups/pre-migration-v1-<time>.db` (verified), then applies each
pending migration in its own transaction. A power cut mid-migration leaves the
previous version. Verified by `scripts/dev/upgradetest/run.sh` against a DB
written by the v1 store (commit 6286b80): all 15 original outbox rows and 5
session rows byte-identical after migration (id, type, hash, state, attempts,
payload), v1 variable-width retry times normalised to 30 characters,
`outbox_counter` = row count, `quick_check` ok, a new session recorded.

**An older binary on a newer DB** refuses to start:
`store: database schema v2 is newer than this binary (v1)` (tested). Under
systemd that is a restart loop with a clear log line, never silent data loss.

**Rollback procedure** (to the previous binary after an upgrade):

1. Stop the agent: `systemctl stop jan-sahayak-agent` (it records the open
   session and checkpoints the WAL).
2. If the device is online, let the outbox drain first if you can
   (`curl -s 127.0.0.1:8730/api/v1/health | jq` → sync component, or wait for
   the next sync); otherwise step 5 carries the unsent events across.
3. Quarantine the live DB — never delete it:
   `mkdir <data_dir>/rollback-<time> && mv <data_dir>/device.db* <data_dir>/rollback-<time>/`
4. Restore the newest pre-migration copy for the target version:
   `cp <data_dir>/backups/pre-migration-v1-<time>.db <data_dir>/device.db`
   (check `sqlite3 <data_dir>/device.db 'PRAGMA user_version'` = 1).
5. Carry over sessions and **unacknowledged** events written after the
   upgrade (already-acknowledged ones are safe centrally; the IDs are the
   idempotency keys, so a duplicate upload is harmless):
   ```sql
   ATTACH '<data_dir>/rollback-<time>/device.db' AS n;
   BEGIN;
   INSERT OR IGNORE INTO sessions SELECT id, started_at, ended_at, language, end_reason, turns FROM n.sessions;
   -- A session open at backup time and closed after the upgrade: take the
   -- newer end, so rolling forward again does not close it a second time.
   UPDATE sessions SET (ended_at, end_reason, turns) = (SELECT s.ended_at, s.end_reason, max(sessions.turns, s.turns) FROM n.sessions s WHERE s.id = sessions.id)
     WHERE ended_at IS NULL AND id IN (SELECT id FROM n.sessions WHERE ended_at IS NOT NULL);
   INSERT OR IGNORE INTO outbox_events (id, type, schema_version, payload, payload_sha256, created_at, state, attempts, next_attempt_at)
     SELECT id, type, schema_version, payload, payload_sha256, created_at, 'PENDING', 0, strftime('%Y-%m-%dT%H:%M:%fZ','now')
     FROM n.outbox_events WHERE state NOT IN ('ACKNOWLEDGED','DELETE_ELIGIBLE');
   COMMIT;
   ```
6. Install the previous binary (A/B symlink switch) and start the agent.
   `PRAGMA quick_check` runs at boot.
7. Keep `rollback-<time>/` until the device has synced; then delete it.

Tested end to end on 2026-11-11: after rollback the v1 store opened the DB
with all 17 events (15 pre-upgrade + 2 written by the new binary), payloads
identical, `quick_check` ok; rolling forward again wrote a second
pre-migration copy and migrated cleanly. **Not yet automated:** the A/B
updater should run steps 1–6 itself when the new version fails its health
gate; until then this is a technician procedure.

## 8. Endurance testing

| Test | Status |
|---|---|
| Randomised outbox invariant (3,000 ops incl. crashes) | *tested* |
| Sync against hostile server responses | *tested* |
| Corrupt-DB recovery | *tested* |
| 10-minute mini soak of the integrated agent (RSS, goroutines trend) | *measured* 2026-11-09 (text only) |
| 60-minute fault-injection soak on dev (full stack, voice + text, runtime kills, central outages, agent SIGKILL) | *measured* 2026-11-11, results below |
| Storage-pressure test (outbox capped at 1,000 rows, central unreachable) | *measured* 2026-11-11, results below |
| Upgrade/rollback test v1 → v2 | *tested* 2026-11-11 (§7a) |
| 8-hour soak on dev | planned — same tool: `scripts/dev/faultsoak/run.sh -duration 8h -faults …` |
| 72-hour soak on Pi 5 incl. power-cut injection (relay or `echo b > /proc/sysrq-trigger`) | planned (Phase 13) |
| Fill the disk to 95 % (real filesystem quota) | planned on the Pi (not practical on macOS) |

Metrics recorded per soak: RSS per process, goroutines, open FDs, DB size,
WAL size, outbox counts, p50/p95 turn latency, error counts, temperature.

### 8a. 60-minute fault-injection soak (2026-11-11, M1 8 GB, swapping)

Tool: `scripts/dev/faultsoak` (Go, stdlib only; `run.sh` owns central-api on
the dev DB, llama-server, the speech worker and an agent provisioned with
`provision.sh`). Load: continuous sessions (greeting → 3 text turns → end, 2 s
apart) plus one voice turn (`var/testaudio` clips) every 2 min; every reply
clip fetched and `speaking-done` sent, like the kiosk. Samples every 10 s →
`var/soak/20261111T064613Z/` (`samples.csv`, `turns.csv`, `events.csv`,
`summary.json`). Times are awake time: the Mac's lid was closed on battery, so
the system slept four times (≈23 min in total) during the first 30 minutes;
the soak clock (monotonic) does not advance during sleep.

| Result | Value |
|---|---|
| Sessions / answered turns | 259 / 802 (774 text, 28 voice); **0 failed requests of any kind** |
| Events | 1,061 new on the device = 802 turns + 257 ended sessions + 2 sessions closed after agent kills (D-028); **all 1,186 device events ACKNOWLEDGED, central holds exactly the same 1,186 IDs** (0 missing, 0 extra) |
| Text turn latency (no fault active) | p50 1.92 s, p95 3.42 s (mostly speech synthesis) |
| Voice turn latency (no fault active) | p50 3.19 s, p95 6.45 s (n = 25, nearest rank; STT + dialogue + TTS). Corrected 2026-11-11: the earlier 6.16 s left out the 23.3 s turn during the Mac's sleep. All 28 voice turns: p95 6.45 s, max 23.3 s |
| Outliers | 5 turns of 8–23 s, all during the sleep/dark-wake period |
| Agent RSS | 19–29 MB (median 18.6); trend over the last 30-min process segment −3.5 MB/h |
| Agent goroutines / open FDs | 19–27 / 21–26; trend −0.09/h and +1.65/h over 30 min (no leak) |
| Device DB / WAL | 0.43 → 0.96 MB (1,186 rows) / WAL ≤ 4.1 MB |
| Health | HEALTHY in 298 of 303 samples with no fault active (4 DEGRADED and 1 RECOVERING right after a fault ended, while the next health check ran). During faults: DEGRADED in 37 samples, HEALTHY in 20 (sync is checked once a minute, so short outages can fall between checks) |

| Fault (count) | Agent behaviour | Recovery |
|---|---|---|
| SIGKILL speech worker, restart after 20 s (×3) | `voice_input` off, text continues (text turns 6 ms p50 without TTS), replies without audio; 2 TTS calls failed cleanly | voice back 3.5–14.6 s after restart (bounded by the 15 s AI health interval) |
| SIGKILL llama-server, restart after 20 s (×2) | deterministic replies, no errors | `llm` back 16.7–17.4 s after restart (≈5 s model load + health interval) |
| SIGTERM central-api for 3 min (×2) | sync DEGRADED (`upload_failures`), events queue (backlog 62 / 45), heartbeats fail and retry | every queued event ACKNOWLEDGED 12.3 / 14.4 s after central returned |
| SIGKILL agent mid-session, restart after 5 s (×2) | open session lost from RAM | serving again 0.25 s after restart (`RECOVERING` → `HEALTHY`); turns before the kill all in the outbox; the open session closed with `session.ended` (`error`) at startup — before the fix the row stayed open forever (D-028) |

### 8b. Storage pressure (2026-11-11)

Separate agent, text only, `sync.outbox_max_rows = 1000` (the minimum),
central unreachable, driven as fast as possible (≈80 sessions/s; 61,074
turns in 250 s):

* The outbox filled within 10 s; the device went `REQUIRES_ATTENTION`
  (`outbox_full`) and sync `DEGRADED` (`outbox_above_90pct`). Citizens were
  still served (every turn 200, p95 9 ms); new events were refused, none of the
  1,000 stored events was touched.
* Restarting with the normal limit and a reachable central: all stored events
  ACKNOWLEDGED within 7 s, central ID set identical to the device's, health
  back to HEALTHY. **No data loss.**
* Two fixes came out of it: the "outbox full" error was logged once per
  refused event (80,433 lines in 4 minutes — enough to push everything else
  out of journald); it is now logged at the 1st, 10th, 100th and every
  1,000th refusal with a running total (`outbox_refused` in `/api/v1/health`).
  And a session whose `session.ended` was refused kept an open local row,
  which the startup sweep would later misreport as a crash; the row is now
  closed even when the event is refused.
* Note: acknowledged rows count toward `outbox_max_rows` until they are
  deleted after `ack_retention` (7 days). At ~1,000 events/day that is ~7,000
  of the default 100,000.
