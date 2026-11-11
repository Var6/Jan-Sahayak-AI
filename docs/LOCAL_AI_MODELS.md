# Local AI Models — Registry and Benchmarks

Machine-readable registry: `models/manifests/models.json`.
Installer: `scripts/models/install.sh <dev-mac|pi5>` (`--verify` to check).
Weights live in `models/weights/` (git-ignored). Nothing downloads at app
startup. Every file has a pinned SHA-256 — archive members too (the
Supertonic members were hashed from the copy extracted from the
SHA-verified archive) — and `--verify` re-hashes all of them (15/15 ok on
2026-11-09).

Status values: NOT INSTALLED · DOWNLOADED · INSTALLED · TESTED · FAILED.

## Registry (as of 2026-11-09)

| Component | Model / tool | Version | Size | Licence | Source | SHA-256 | Path | Profile | Status |
|---|---|---|---|---|---|---|---|---|---|
| STT | AI4Bharat IndicConformer-Hindi, CTC head, int8 ONNX | export 2026-09-22 | 197.6 MB + 67.6 KB tokens | MIT (upstream); export labelled Apache-2.0 | huggingface.co/parismitaglobalsolutions/indicconformer-sherpa-onnx | `915c71e0…05fa` / `ee609676…dfb2` | `stt/indicconformer-hi/` | dev-mac, pi5 | DOWNLOADED (tested by research; speech-worker integration in progress) |
| VAD | Silero VAD | v5 | 644 KB | MIT | github.com/k2-fsa/sherpa-onnx releases (asr-models) | `9e2449e1…1fd6` | `vad/silero_vad.onnx` | dev-mac, pi5 | DOWNLOADED |
| TTS | Supertonic-3 int8 (lang `hi`) | 2026-05-11 | 145 MB extracted (128.8 MB archive) | OpenRAIL-M (weights), MIT (code) | github.com/k2-fsa/sherpa-onnx releases (tts-models) | archive `82fa96f9…c427` | `tts/sherpa-onnx-supertonic-3-tts-int8-2026-05-11/` | dev-mac, pi5 | DOWNLOADED (tested by research) |
| LLM | Gemma 4 E2B-it Q4_0 GGUF | ggml-org | 2.84 GB | Apache-2.0 | huggingface.co/ggml-org/gemma-4-E2B-it-GGUF | `8e30dff3…6a52` | `llm/gemma-4-E2B-it-Q4_0.gguf` | dev-mac, pi5 | DOWNLOADED (tested by research) |
| LLM draft | Gemma 4 E2B MTP head | ggml-org | 59 MB | Apache-2.0 | same repo | `718d3a44…5ef4` | `llm/mtp-gemma-4-E2B-it-Q4_0.gguf` | dev-mac, pi5 | DOWNLOADED |
| STT (dev 2nd opinion) | Whisper large-v3-turbo q5_0 | ggml | 574 MB | MIT | huggingface.co/ggerganov/whisper.cpp | `39422170…a7e2` | `stt/ggml-large-v3-turbo-q5_0.bin` | dev-mac | TESTED |
| STT (baseline) | Whisper small q5_1 | ggml | 190 MB | MIT | same | `ae85e4a9…11bb` | `stt/ggml-small-q5_1.bin` | — | TESTED |

Runtimes: llama.cpp 0.9.0 (build 12110, Homebrew, Metal), whisper.cpp 1.9.5
(Homebrew), sherpa-onnx 2.0.1 via `github.com/k2-fsa/sherpa-onnx-go/v2`.

## Benchmarks

All dev numbers are from the Apple M1 / 8 GB MacBook Air **while the machine
was swapping heavily (≈9 GB swap in use)**, so they are pessimistic. Test
audio is synthetic (macOS Hindi voice "Lekha"), clean speech — these are
speed and sanity checks, **not** accuracy (WER) measurements.

### STT — whisper.cpp (measured by lead engineer)

`whisper-cli`, 4 threads, Metal, includes model load per run:

| Clip | large-v3-turbo q5_0 | small q5_1 |
|---|---|---|
| "मुझे लोन चाहिए।" | मुझी लोन चाहिए. | मुजी लों चाहिए |
| "मुझे जाति प्रमाण पत्र बनवाना है।" | मुझी जाती प्रमान पत्र बनवाना है। (3.3 s) | मुजी जाती प्रमान पत्र बन्वाना है. (1.1 s) |
| "मेरी माँ की उम्र सत्तर साल है, …पेंशन…" | …व्रिधावस्था पेशन… (3.1 s) | …भ्रिगावस्ता पैंशिन… (1.4 s) |
| "मैं गया जिले का किसान हूँ, पीएम किसान…" | …गया जीले का किसान हूँ, PM किसान… (3.0 s) | …पीम किसान… (1.2 s) |
| Peak RSS | ~800 MB | ~540 MB |

`whisper-server` (model resident), large-v3-turbo: steady **2.1–2.3 s per
short utterance** (encoder processes the full 30 s window), RSS 618 MB.

### STT — comparison (measured by STT research, same machine)

| Engine / model | Time (RTF) | Peak RAM | District names "सीतामढ़ी… किशनगंज… खगड़िया" |
|---|---|---|---|
| sherpa-onnx IndicConformer-hi int8, CPU 4 thr | 0.16–0.24 s (RTF 0.03–0.05) | 441 MB | सीतामढ़ी, किशनगंज, खगरिया |
| whisper.cpp vasista22 hi-small q5_1, Metal | 1.3–1.8 s | ~0.5 GB | all correct |
| whisper.cpp large-v3-turbo q5_0, Metal | 3.7–4.8 s | 846 MB | सीतामर्ली, किशनकंच, खगर्या |
| whisper.cpp large-v3-turbo q5_0, CPU | 21.3 s | — | — |

All engines mis-heard "card" (कोड/कोर्ट/कॉर्ड) → handled by the dialogue
engine's normalisation and fuzzy matching.

### TTS (measured by TTS research)

| Voice | RTF (M1, 2 threads, loaded machine) | Peak RAM | Round-trip intelligibility |
|---|---|---|---|
| Supertonic-3 int8, `hi`, 4 steps (Go bindings) | 0.135 | 320–400 MB | word-perfect |
| Supertonic-3 int8, 8 steps (CLI) | 0.36–0.38 | — | — |

Supertonic does not read digits/₹ reliably → the agent normalises numbers,
dates and currency into Hindi words before synthesis (`ai.PrepareForTTS`).

### LLM (measured by LLM research)

| Model | Decode tok/s Metal / CPU-4 | Prompt tok/s (Metal) | RAM | Hindi quality |
|---|---|---|---|---|
| Gemma 4 E2B Q4_0 | 22–31 / 5–10 | 915 | 1.52 GB (0.77 GB with `-lzm on`) | good; occasionally assumes unstated facts → validator required |
| Qwen3.5-2B Q4_K_M | 36 / 14–28 | — | 1.06–1.41 GB | invented claims and a scheme name |
| Qwen3.5-0.8B, Gemma 3 1B | fast | — | ~1 GB | unusable for Hindi |

Pi 5 (published, not yet measured by us): Gemma 4 E2B Q4_0 llama.cpp 4–7
tok/s decode; peak 4.4 GB with default 128K context on an 8 GB Pi → we pin
`-c 4096`.

### Integrated stack under faults (Phase 12 soak, 2026-11-11, M1, swapping)

60 minutes of continuous kiosk sessions through the agent, with the
runtimes killed on purpose (details: `LONG_TERM_RELIABILITY.md` §8a). RSS is
what `ps` reports on a machine with ~10 GB of swap in use, so resident pages
were often compressed or swapped out — read these as lower bounds, not
budgets.

| Measure | Value |
|---|---|
| llama-server RSS (Gemma 4 E2B Q4_0, `-c 4096`, mmap) | median 281 MB, max 1,352 MB (after a restart the weights were resident again) |
| Speech worker RSS (IndicConformer + VAD + Supertonic) | median 201 MB, max 463 MB |
| llama-server start → `/health` 200 | ≈5 s; agent re-enables the LLM 16.7–17.4 s after the restart (next AI health check, 15 s interval) |
| Speech worker start → `/health` 200 | 1.3–1.8 s; agent re-enables voice 3.5–14.6 s after the restart (same interval) |
| Text turn incl. Hindi TTS (no fault) | p50 1.92 s, p95 3.42 s (n = 614) |
| Text turn while the speech worker is down (no TTS) | p50 6 ms (n = 70) |
| Voice turn (STT + dialogue + TTS) | p50 3.19 s, p95 6.45 s (n = 25, synthetic clips, no fault active; max 23.3 s during the Mac's sleep) |
| Live check transcripts (`var/testaudio`, synthetic "Lekha" voice) | t1–t3 exact, t4 one vowel-sign error (आाया); confidence 0.77–0.80 |
| Document OCR through the API (tesseract 5.5.4 `eng+hin`) | 0.41 s for a synthetic income certificate, read as `income_certificate` / high; Aadhaar fixture masked to `XXXX XXXX 9012` |

Restart tuning note: recovery is dominated by `health.ai_interval` (15 s).
Lowering it to 5 s would cut voice/LLM recovery to ~5–7 s for a few extra
loopback health requests; not changed until measured on the Pi.

### Pi 5

Not measured yet — no hardware purchased. See `HARDWARE_SETUP.md` for the
benchmark plan. Model sizes are final only after that run.

## Recommended server flags

```
# dev (M1)
llama-server -m models/weights/llm/gemma-4-E2B-it-Q4_0.gguf -c 4096 -np 1 -fa on \
  --reasoning off -ngl 99 --cache-reuse 256 --host 127.0.0.1 --port 8742
# Pi 5 (8 GB)
llama-server -m gemma-4-E2B-it-Q4_0.gguf -c 4096 -np 1 -t 4 -tb 4 -fa on --reasoning off \
  -lm mmap+mlock --spec-type draft-mtp -md mtp-gemma-4-E2B-it-Q4_0.gguf \
  --cache-reuse 256 --host 127.0.0.1 --port 8742
```

## Open items

1. Record ~200 real, consented utterances (Bhojpuri/Maithili/Magahi-accented
   Hindi, background noise) and measure WER for IndicConformer vs alternatives.
2. Re-export IndicConformer-Hindi ourselves from AI4Bharat's checkpoint and
   compare outputs with the pinned community export.
3. Benchmark the full stack on a Pi 5 8 GB (RTF, RAM, thermals, 8-hour soak).
