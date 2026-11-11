# Long-Term Reliability

Goal: unattended operation in public places for years, with predictable
resource use, automatic recovery and easy servicing. We do **not** claim any
device runs ten years without maintenance; we engineer for recovery and
measure. Claims below are marked *measured*, *tested* or *planned*.

## 1. Memory budgets (Pi 5, 8 GB — provisional until Pi benchmark)

| Process | Budget (systemd `MemoryMax`) | Expected | Basis |
|---|---|---|---|
| device-agent (Go) | 256 MB, `GOMEMLIMIT` 192 MB | 20–60 MB | to be measured in Phase 12 soak |
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
  *tested*).
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
* Upgrade tests from each supported older version (Phase 12).

## 8. Endurance testing

| Test | Status |
|---|---|
| Randomised outbox invariant (3,000 ops incl. crashes) | *tested* |
| Sync against hostile server responses | *tested* |
| Corrupt-DB recovery | *tested* |
| 10-minute mini soak of the integrated agent (RSS, goroutines trend) | in progress (integration workflow) |
| 8-hour soak on dev: repeated sessions, STT/TTS/LLM calls, network flaps, runtime kills | planned (Phase 12) |
| 72-hour soak on Pi 5 incl. power-cut injection (relay or `echo b > /proc/sysrq-trigger`) | planned (Phase 13) |
| Storage-pressure test (fill disk to 95 %) | planned |
| Upgrade/rollback test | planned |

Metrics recorded per soak: RSS per process, goroutines, open FDs, DB size,
WAL size, outbox counts, p50/p95 turn latency, error counts, temperature.
