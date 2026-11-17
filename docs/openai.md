# OpenAI dialogue adapter (C7)

`internal/adapters/dialogue/openai` is the **second** `ports.Dialogue` adapter. One
alternative AI port is enough to prove DESIGN.md §1.2's swap promise: the dialogue contract
is pure text + tools + outcome, so no audio codec or WebSocket surface is involved. OpenAI
Chat Completions is SSE over plain HTTP — the shape the Mistral adapter already parses — so
the adapter is a near drop-in variant with its own base URL, model, and request fields.

STT and TTS stay on Mistral in C7. Adding an OpenAI STT/TTS adapter later is one adapter
package plus one `case` in `internal/app/providers.go`; the `stt`/`tts` enums deliberately
remain `{mistral, stub}`.

## Selecting it

```bash
INSTEADLY_DIALOGUE_PROVIDER=openai
INSTEADLY_OPENAI_API_KEY=sk-…                                 # secret
INSTEADLY_OPENAI_BASE_URL=https://api.openai.com/v1           # default
INSTEADLY_OPENAI_DIALOGUE_MODEL=gpt-4o-mini                   # default
```

`config.Validate` requires the API key only when `dialogue.provider=openai`. The base URL
must be an absolute **https** URL with no userinfo, query, or fragment; a path is allowed
because the OpenAI API base itself ends in `/v1`. The adapter additionally permits plain
`http` **only for loopback hosts**, so tests can point it at `httptest` while a real key can
never be sent over plaintext.

## Protocol surface

- `POST {base}/chat/completions`, `Authorization: Bearer <api_key>`,
  `Content-Type: application/json`, `Accept: text/event-stream`.
- Request body: `model`, `messages`, `tools` (`{type:"function",function:{name,
  description,parameters}}`), `tool_choice:"auto"`, `parallel_tool_calls:false`,
  `stream:true`, `temperature:0.1`, and **`max_completion_tokens`** (current OpenAI models
  deprecate the older `max_tokens`; a test pins the field name).
- SSE response: `choices[].delta.{role,content,tool_calls[]}`,
  `choices[].finish_reason`, `[DONE]`. `stop` and `tool_calls` are valid; `length` (and
  `model_length`) map to `ErrTurnTruncated`; EOF before `[DONE]` is truncation.
- Tool-call fragments are assembled by `index`; arguments must be exactly one JSON object;
  unknown tool names are rejected. The reserved `finish_call` tool carries the caller's
  outcome JSON Schema and is decoded into a strictly validated `domain.Outcome` with the
  same rules as the Mistral adapter (unknown fields rejected, conditional fields checked,
  `booked` requires a slot, `callback_needed` a callback time, `failed` a reason).
- `Interrupt(confirmedText)` and `SendControl(instruction)` implement the same conservative
  history reconciliation as the Mistral adapter: only provider-confirmed assistant text is
  retained, unexecuted tool calls/outcomes from an interrupted turn are dropped, and a
  control instruction is a system-role message, never user speech.

Errors are `*APIError` with only the HTTP status, `type`, `code`, `param`, and
`Retry-After` — the provider's message text (which can echo request content) is never
retained or logged. `Temporary()` classifies 429/5xx.

## Testing

`internal/adapters/dialogue/openai` is tested against a single `httptest` SSE server:
request-body shape (`model`, tools incl. `finish_call`, `stream:true`,
`max_completion_tokens`), streamed text chunks, fragmented tool-call assembly,
`finish_call` outcome decoding, truncation/error paths, interrupt/control reconciliation,
and caller-slice immutability. Zero network, zero key.

`internal/app/swapability_test.go` drives the real OpenAI adapter through the real agent to
a `booked` outcome, with a scripted three-turn SSE conversation (propose → confirm →
finish_call).
