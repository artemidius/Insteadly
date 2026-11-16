# Insteadly — Technical Design & Decisions

*Brainstorm summary from the architecture discussion (2026-11-16). This document is the
starting point for implementation planning. Status: design only — no code has been written.
Decisions marked **Decided** are settled; everything else is open for planning.*

---

## 0. Product in one paragraph

**Insteadly** is an AI voice assistant that makes phone calls on behalf of users — starting
with booking medical appointments in Vienna (Austria), where clinics often only accept calls
during narrow windows and many users struggle with German or simply dislike phone calls.

**Business flow (rough vision):** users register and pay on a website → order a phone call
(pick a clinic, preferred times, reason, personal details) → the backend picks up paid orders
and makes the calls automatically → user gets the result (booked / failed / callback needed)
via notification and dashboard.

**Out of scope for now (explicitly deferred by decision):** compliance, GDPR, legal.
*Listed again in §8 as a production gate, but not part of prototype work.*
Recording/storage **configuration flags** are in scope — see §3.4.

---

## 1. Decisions taken in this session

1. **Stack (Decided):** backend in **Go**; web app in **React + TypeScript**. Website comes
   later; the backend API is designed for it from day one.
2. **Architecture (Decided): dependency inversion / ports & adapters.** The core depends only
   on contracts (interfaces); implementations are adapters; only the composition root knows
   concrete providers. **Every external capability — telephony, STT, TTS, dialogue LLM,
   storage, payments, notifications — is swappable at any time via config**, with no core
   changes. Twilio "Voice API" vs Twilio "SIP trunk" are just two adapters behind one port.
3. **Telephony (Decided):** first provider **Twilio** (trial account already registered;
   `TWILIO_ACCOUNT_SID` + `TWILIO_AUTH_TOKEN` are in `.env`). First adapter: **Twilio Voice
   API + Media Streams**. Second adapter (same port): **Twilio Elastic SIP trunk**. Later
   candidates: **Telnyx**, **fonira** (local Austrian trunk, +43 caller ID).
4. **AI providers (Decided):** first implementations are **Mistral for STT, TTS, and the
   dialogue LLM**. Swap candidates before production: OpenAI, Google, ElevenLabs, Deepgram.
   (Mistral has **no Go SDK** — adapters will speak its raw WebSocket/JSON APIs.)
5. **Canonical audio (Decided):** **PCM16 mono, 16 kHz** everywhere inside the core.
   All codec/sample-rate conversion happens *inside adapters* (Twilio delivers 8 kHz µ-law →
   Twilio adapter converts; OpenAI Realtime's 24 kHz requirement would be handled by its own
   adapter). Providers must not leak their formats past the port boundary.
6. **Turn-taking (Decided):** the **core owns VAD / end-of-turn detection** on canonical audio,
   so conversational pacing stays identical after any provider swap. The STT port remains pure
   streaming transcription. (Mistral's server-side turn detection may later be used as an
   optional hint — implementation detail of the Mistral adapter only.)
7. **Milestones (Decided):** M0 → M4, see §7.
8. **No implementation until explicitly requested** — this document is the contract between
   the design phase and the implementation phase.

---

## 2. System shape

Two deployables:

```
React + TS web app (later)        Go backend (the hexagon, one binary)
  registration / payments    ──REST + SSE──►   API layer
  order form (clinic, slots,                    ├─ Order lifecycle (state machine)
  patient data)                                 ├─ Scheduler (windows, retries)
  dashboard (status, outcomes)                  ├─ Call runner (per-call goroutines)
                                                ├─ Agent engine (voice loop, tools)
                                                └─ Policy & config (recording toggles)
                                                        │ depends only on ports
        ┌───────────────┬───────────────┬──────────────┼──────────────┬──────────────┐
        ▼               ▼               ▼              ▼              ▼              ▼
   Telephony         STT              TTS          Dialogue(LLM)   Storage      Payments/Notify
   port              port             port         port            port         ports
```

- **Core** (hexagon inside): pure domain logic, zero provider SDK imports.
- **Adapters**: implement ports; each provider gets its own package.
- **Composition root**: reads config, constructs adapters, injects them into the core.
  This is the only place that imports both core and adapters.

### 2.1 Core components

1. **Order lifecycle** — the spine state machine every order follows:
   `created → paid → scheduled → dialing → ringing → in-call → finished`
   where `finished` resolves to a structured outcome:
   - `booked` — slot confirmed (store date/time, doctor, practice)
   - `callback_needed` — reception asked to call back; store the agreed time → dynamic re-schedule
   - `failed` — busy / no answer / rejected / IVR dead-end
   - `aborted` — user or system cancelled
   Payment gate, scheduler, call runner, and dashboard all hang off this machine.
2. **Scheduler** — maps orders onto clinic opening windows (timezone-aware CET/CEST), enforces
   quiet hours, retry/backoff policy for busy lines (retry after N minutes, capped attempts,
   no spam), supports re-scheduling from `callback_needed`. In-process for the prototype;
   **River (Postgres-backed queue) before production** so task state survives restarts.
3. **Agent engine (conversation loop)** — runs inside a live call: turn-taking + VAD on
   canonical audio; composes STT → Dialogue → TTS; executes booking tools (propose slot,
   confirm, capture info, escalate to human); sends DTMF through IVR menus; detects hold
   music; emits the structured outcome. Holds no provider knowledge.
4. **Policy & config** — recording/storage toggles, per call:
   `recording.enabled` / `transcript.enabled` / `audio.enabled` (global defaults +
   per-order override), resolved by the orchestrator, enforced at the storage boundary.

### 2.2 Ports (contracts the core consumes)

Each port is defined by the *consumer* (the core) as the minimal surface it needs.
Contracts in prose (signatures to be designed in M0):

| Port | Contract (what the core sees) | Prototype adapter | Later adapters |
|---|---|---|---|
| **Telephony** | dial(order) → call session; normalized events (dialing / ringing / answered / DTMF / ended+reason); duplex canonical audio; hangup; DTMF send | **Twilio Voice API + Media Streams** | Twilio SIP trunk, Telnyx, fonira |
| **SpeechToText** | streaming audio → transcript events (partial/final); batch mode for post-call transcripts of recordings | **Mistral** realtime + offline | OpenAI, Google, Deepgram |
| **TextToSpeech** | streaming text chunks → audio chunks (LLM output spoken as it is generated) | **Mistral** realtime | ElevenLabs, OpenAI, Google |
| **Dialogue (LLM)** | conversation state + tools → streaming reply + tool calls; the booking brain | **Mistral** chat | OpenAI, Google |
| **Storage** | blobs (recordings, transcripts) + structured order state | local disk + Postgres | S3 / MinIO |
| **Payments** | charge / refund / webhook (added with the website) | Stripe (assumed) | alternatives |
| **Notifications** | email / SMS confirmations and outcomes | — (later) | — |

Optional coarse port noted for later: **DialogueEngine** — would let a fully managed pipeline
(Mistral Voice Agents, Vapi, …) run as one more adapter, as an escape hatch. Not needed now.

### 2.3 Config surface (the "swap anytime" promise)

```
telephony.provider           = twilio | telnyx | fonira
telephony.twilio.mode        = voice-api | sip-trunk
stt.provider                 = mistral | openai | google | deepgram
tts.provider                 = mistral | openai | google | elevenlabs
dialogue.provider            = mistral | openai | google
storage.provider             = local | s3
recording.enabled            = true|false   (global defaults)
transcript.enabled           = true|false
audio.enabled                = true|false
# + per-provider settings namespaces, e.g. telephony.twilio.*, stt.mistral.*
```

Adding a provider = one adapter package + one wiring entry. No core edits.

### 2.4 End-to-end flow

1. User registers and pays on the web app → order becomes `paid` (Payments port).
2. Scheduler slots the order into the clinic's next opening window (Scheduler + Storage).
3. At T₀ the call runner dials via the Telephony port; events stream into the core.
4. Agent engine loops: receptionist audio → STT → Dialogue (with tools) → TTS → line out,
   with barge-in, DTMF for IVRs, hold detection.
5. Dialogue emits the outcome → order reaches `finished` with structured result →
   notifications sent, dashboard updated live (SSE), recordings/transcripts persisted only
   per the flags.

Concurrency: one goroutine per live call; ports are asynchronous streams — a single Go binary
runs the whole prototype. If volume grows, the call runner is the only component that splits
out (it scales on connections/CPU, the API on requests).

---

## 3. Verified provider research (2026-11-16)

### 3.1 Telephony candidates

| Provider | Signup / trial | Cost to call AT mobile | Integration path | Notes |
|---|---|---|---|---|
| **Twilio** (chosen) | No card needed; **$15.15 free credit**; outbound calls only to **verified numbers** on trial | ~$0.10–0.15/min (origin-based; exact rate to verify in console); US caller-ID number ~$1.15/mo | REST dial + **Media Streams** (WebSocket audio, 8 kHz µ-law) + webhooks; DTMF | Trial fits PoC (call own verified mobile); adding a card lifts verified-only restriction, credit stays |
| **Telnyx** | Magic-link signup, no card; "pretrial" = **$25 AI-only credit** + free US number; telephony needs account upgrade (LinkedIn/GitHub verify) → **$5 general credit** | SIP outbound "from $0.005/min" (destination rate deck; AT mobile ~$0.01–0.03, verify in portal) | SIP-credentials trunk **or** Media Streams WebSocket; pay-as-you-go, $0/month min | Strong second adapter; cheap termination; also has Voice AI agent $0.05/min all-in |
| **fonira** | Austrian provider, business-oriented | **€20 setup + €7/mo** (4 channels, 1 number) + per-minute "fonira value" tariff | **Raw SIP trunk only** — needs our own SIP/RTP endpoint (e.g. LiveKit SIP) | Production candidate for local +43 CLI |
| **yuutel** | Austrian provider | €59/mo + €299 setup | Raw SIP trunk | **Eliminated** for PoC |

Sources: [Twilio free trial details](https://www.telphiconsulting.com/blog/twilio-free-trial),
[Telnyx pricing](https://telnyx.com/pricing.md),
[Telnyx pretrial accounts](https://support.telnyx.com/en/articles/14327893-telnyx-pretrial-accounts),
[fonira SIP-Trunk tariff PDF](https://fonira.at/cms/wp-content/uploads/EB-fonira-SIP-Trunk.pdf),
[yuutel SIP trunk](https://www.yuutel.at/sip-trunk).

### 3.2 Mistral audio/AI products (verified current lineup)

- **Realtime STT:** `voxtral-mini-transcribe-realtime-3` — WebSocket streaming, PCM16 input at
  8–48 kHz, text-delta events, configurable streaming delay, **end-of-turn detection**,
  context biasing. 16 kHz canonical input is supported natively.
- **Offline STT:** `voxtral-mini-transcribe-3` / `voxtral-small-transcribe-3` — file
  transcription with timestamps, diarization, custom vocabulary → for post-call transcripts.
- **TTS:** `voxtral-mini-tts-3-0` — offline (`/v1/audio/speech`, formats mp3/wav/pcm/flac/opus)
  and **realtime WebSocket** (`/v1/audio/speech/realtime`) for streaming LLM text; preset
  voices, zero-shot cloning, emotion tags.
- **Managed Voice Agents SDK** exists (Python/TS, LiveKit-based, SIP telephony enterprise-only
  at launch) — possible future managed fallback behind a coarse `DialogueEngine` port.
- **No Go SDK** — adapters implement the WebSocket/JSON protocols directly.

Sources: [Mistral STT overview](https://docs.mistral.ai/studio/audio/speech-to-text/overview),
[Mistral TTS overview](https://docs.mistral.ai/studio/audio/text-to-speech/overview),
[Mistral voice agents](https://docs.mistral.ai/studio/audio/voice-agents/overview).

---

## 4. Environment facts (for the implementation session)

- Workspace: `/Users/q/Insteadly` — currently empty except `.env`, `.git`, `.idea`.
- `.env` contains: Twilio `ACCOUNT_SID` + `AUTH_TOKEN` (trial).
  **Still needed before M0's first call:** `TWILIO_PHONE_NUMBER` (buy a US local number in the
  Twilio console, ~$1.15/mo from credit), `TEST_PHONE` (the owner's Austrian mobile, added as a
  **Verified Caller ID** in the console — trial accounts may only call verified numbers),
  `MISTRAL_API_KEY`.
- Toolchain: Node 26 + npm (irrelevant now — backend is Go); no pnpm; no tunnel tools yet
  (cloudflared/ngrok needed for Media Streams inbound WSS in M0/M1).

---

## 5. Milestones

- **M0 — skeleton walk.** Define all port contracts; implement one adapter per port
  (Twilio voice-api, Mistral STT/TTS/Dialogue, local storage); composition root + config;
  dial the owner's mobile and speak through the whole Mistral chain. Proves every seam.
- **M1 — conversation.** Real agent loop against a test target: turn-taking, barge-in,
  DTMF, structured outcome extraction.
- **M2 — orchestration.** Order store, scheduler (windows, retries, callback-re-scheduling),
  status API + SSE for the future web app.
- **M3 — web app.** React+TS: registration, payments (Stripe), order form, dashboard.
- **M4 — swapability & production hardening.** Second adapters (Telnyx or Twilio SIP trunk;
  one alternative AI provider) to prove the swap promise; recording/storage flags end-to-end;
  notifications.

---

## 6. Open questions for the planning session

1. **Dialogue tool surface** — which tools the LLM gets (propose_slot, confirm_booking,
   capture_insurance, escalate_to_human, …) and the structured outcome JSON schema.
2. **Scheduler semantics** — retry counts/backoff per failure class; where reception-hours
   data comes from (clinic directory, §7).
3. **Clinic directory** — data source (praxisplan.at), licensing, freshness strategy.
4. **Payments provider** — Stripe assumed; confirm when M3 is planned.
5. **Deployment target** — where the Go binary runs (EU region preference for production).
6. **Compliance gate (before production, deferred by decision)** — EU AI Act transparency
   (bot must disclose it is AI), GDPR Art. 9 health data handling with EU data residency,
   Austrian TKG rules for automated calls, abuse prevention (booking under someone else's
   name). Not prototype scope; must be revisited before launch.
7. **`DialogueEngine` coarse port** — adopt now or only if a managed-pipeline fallback is needed.
