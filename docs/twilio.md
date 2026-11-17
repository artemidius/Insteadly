# Twilio telephony adapter

C1 implements `ports.Telephony` with **Twilio Programmable Voice + bidirectional Media
Streams** (`telephony.twilio.mode=voice-api`). The architecture is described in
[architecture.md](architecture.md); this page is the operator's setup and troubleshooting
guide.

## What you need

1. A Twilio account with credit.
2. A Twilio phone number used as caller ID (`INSTEADLY_TWILIO_PHONE_NUMBER`).
   Trial restrictions still apply: **outbound calls may only go to Verified Caller IDs**.
3. The destination number added as a Verified Caller ID in the Twilio console.
4. Geo permissions that allow calling the destination country (Austria for this project).
5. A **public HTTPS/WSS** endpoint pointing at this process. Twilio cannot call
   `127.0.0.1`.
6. **An upgraded (non-trial) Twilio account for live audio.** Trial accounts strip the
   `<Stream>` TwiML verb and replace it with a "not available on trial accounts" message;
   calls connect and status callbacks work, but no Media Stream/audio is possible. The
   adapter's fake-Twilio tests cover the stream path; live audio needs an upgrade.

## Configuration

Only `INSTEADLY_*` variables are read. The unprefixed `TWILIO_*` names that may already be
in `.env` are ignored — either add the prefixed names to `.env` (never commit it) or export
them:

```bash
export INSTEADLY_TELEPHONY_PROVIDER=twilio
export INSTEADLY_TWILIO_ACCOUNT_SID="$TWILIO_ACCOUNT_SID"
export INSTEADLY_TWILIO_AUTH_TOKEN="$TWILIO_AUTH_TOKEN"
export INSTEADLY_TWILIO_PHONE_NUMBER="$TWILIO_PHONE_NUMBER"
export INSTEADLY_HTTP_PUBLIC_URL="https://<tunnel>.trycloudflare.com"
# optional (full accounts only; trial accounts reject extra Calls parameters):
export INSTEADLY_TWILIO_RING_TIMEOUT_SECONDS=30
```

Validation is strict and fails fast:

- `http.public_url` is required for `voice-api`, must be an absolute **https** origin with
  no path/query/fragment (a trailing slash is trimmed);
- the Account SID must look like `AC` + 32 hex characters;
- the auth token must be non-empty;
- the caller ID must be E.164 (`+…`);
- the ring timeout must be 0 (Twilio default) or 5–600 seconds. Trial accounts reject
  the `Timeout` parameter entirely, so leave it at 0 there.

`/healthz` reports `"telephony": "twilio"` when the adapter is wired. Startup logs print the
public URL (never the token/SID), which is the first thing to check when signatures fail.

## Exposing the process

Any public HTTPS tunnel that forwards to `INSTEADLY_HTTP_ADDR` works, including WebSockets:

```bash
# example only; installing tools is a machine-level change
cloudflared tunnel --url http://127.0.0.1:8080
# prints https://<random>.trycloudflare.com
./bin/insteadly
```

The tunnel URL must be **byte-for-byte** the value of `INSTEADLY_HTTP_PUBLIC_URL` (scheme
and host; no trailing slash). If the tunnel restarts with a new URL, restart the process
with the new value.

Routes Twilio uses (all under the public origin):

| Route | Called for |
|---|---|
| `POST /webhooks/telephony/twilio/voice/{callID}` | fetching TwiML after the call is answered |
| `POST /webhooks/telephony/twilio/status/{callID}` | call progress (`initiated`, `ringing`, `answered`, `completed`) |
| `POST /webhooks/telephony/twilio/stream-status/{callID}` | stream started/stopped/error diagnostics |
| `GET /webhooks/telephony/twilio/stream/{callID}` | the Media Streams WebSocket |

The HTTP routes require `X-Twilio-Signature`; the WebSocket is validated by the
unguessable call ID, the Twilio AccountSID, and the `call_id` custom parameter.

## Live test

There is no public "place a call" API yet (the call runner is C3). The build-tagged live
test starts the real HTTP server, dials `TEST_PHONE`, sends a 440 Hz tone, optionally sends
a DTMF digit, and hangs up:

```bash
# terminal 1: tunnel
cloudflared tunnel --url http://127.0.0.1:8080

# terminal 2: credentials + test
set -a; . ./.env; set +a
export INSTEADLY_TWILIO_ACCOUNT_SID="$TWILIO_ACCOUNT_SID"
export INSTEADLY_TWILIO_AUTH_TOKEN="$TWILIO_AUTH_TOKEN"
export INSTEADLY_TWILIO_PHONE_NUMBER="$TWILIO_PHONE_NUMBER"
export INSTEADLY_TELEPHONY_PROVIDER=twilio
export INSTEADLY_HTTP_PUBLIC_URL="https://<tunnel>.trycloudflare.com"
export LIVE_TWILIO=1
export LIVE_DTMF=1   # optional
make live-test
```

This costs a little credit and rings the phone for roughly 6 seconds. It is never run in
CI. On a **trial** account the test fails at "Media Stream never attached" — that is
expected; upgrade the account to exercise the audio path.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| every webhook returns `403`, Twilio shows `11200` | `INSTEADLY_HTTP_PUBLIC_URL` does not exactly match the URL Twilio was told to call, or the proxy rewrites the path/query. The startup log prints the configured value. |
| `403` only from the live test | The tunnel is down, points at a different port, or the process is not listening on `INSTEADLY_HTTP_ADDR`. |
| Dial fails with `21215` | Account geo permissions block the destination; enable Austria in the Twilio console. |
| Dial fails with `21210` | The caller ID is not a verified/owned Twilio number. |
| Dial fails with `400`/`404` and a Twilio error code | Check the number format (E.164), that the number is owned, and that trial credit remains. |
| Call answers but no audio | The WebSocket was rejected (check adapter logs for `rejected stream start`), or the `<Connect><Stream>` URL is not `wss://`. |
| DTMF does not reach an IVR | Outbound DTMF is implemented as a TwiML redirect (Twilio Media Streams has no outbound DTMF message). It briefly interrupts the stream; see the limitations below. |
| `connection refused` in the live test | Port 8080 is already in use (for example a previously started `bin/insteadly`). Stop it first. |

## Known limitations (C1)

- **Trial accounts cannot use `<Stream>`** (Twilio strips it), so live media is blocked
  until the account is upgraded. Everything up to TwiML is verified live.
- Outbound DTMF uses a call-update redirect with `<Play digits>` and a stream reconnect.
  Expect a short audio gap and one new stream SID. C4 may replace this with a more robust
  IVR flow.
- The adapter only handles **outbound** calls; inbound calls to the Twilio number are not
  routed.
- No Twilio recordings (`Record=true`) and no storage of call audio yet — recording flags
  are wired in C7.
- A live session has a 30-minute safety cap. The scheduler (C5) will own real time limits
  and retries.
- On SIGTERM, hijacked WebSocket connections are not drained; the process exits and Twilio
  ends the call. Graceful call draining is C3.
- Do not log or commit credentials. Use only `INSTEADLY_TWILIO_*`; never paste the auth
  token or full phone numbers into issues.
