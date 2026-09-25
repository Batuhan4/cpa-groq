# cpa-groq

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) plugin that exposes
[Groq Whisper](https://console.groq.com/docs/speech-to-text) speech-to-text as a CPA
**executor** model. A client sends a normal Gemini `generateContent` request with the
audio as base64 `inline_data`, and gets the transcript back as a Gemini-format response.

```
client ──POST /v1beta/models/groq-whisper-large-v3:generateContent──▶ CPA
           (Gemini JSON, audio as inline_data)                          │ auth manager: provider "cpa-groq"
                                                                        ▼
                                                        cpa-groq executor (this plugin, in-process)
                                                                        │ host.http.do (CPA's HTTP client/proxy)
                                                                        ▼
                                           POST https://api.groq.com/openai/v1/audio/transcriptions
                                           (multipart/form-data, response_format=verbose_json)
```

Built and tested against **CPA v7.3.17** (commit `9bdde54`), plugin C ABI version 1.

## Why

CPA has no `/v1/audio/*` route, and its `openai-compatibility` executor always POSTs JSON to
`/chat/completions`, so Groq's multipart transcription endpoint cannot be added as an ordinary
provider. Gemini audio, on the other hand, already works through CPA via `generateContent` with
`inline_data`. This plugin gives Groq Whisper that same request shape, so an application that
sends every model call through CPA can switch between Gemini and Whisper by changing only the
model name.

## Models

| CPA model id                   | Groq model               |
|--------------------------------|--------------------------|
| `groq-whisper-large-v3`        | `whisper-large-v3`       |
| `groq-whisper-large-v3-turbo`  | `whisper-large-v3-turbo` |

The ids contain no `/`, so they cannot collide with CPA's `prefix/model` routing. If you want
slash ids, give the auth record a `prefix` (see below): with `"prefix": "groq"` the models are
also served as `groq/groq-whisper-large-v3` (raw or `%2F`-encoded in the URL). Both forms were
verified end to end.

## Request contract

```http
POST /v1beta/models/groq-whisper-large-v3-turbo:generateContent
x-goog-api-key: <CPA client key>
Content-Type: application/json

{
  "contents": [{
    "role": "user",
    "parts": [
      {"inline_data": {"mime_type": "audio/ogg", "data": "<base64 audio>"}}
    ]
  }],
  "cpa_groq": {"language": "tr", "prompt": "CLIProxyAPI, Groq, Gemini"}
}
```

`streamGenerateContent` (with or without `?alt=sse`) works too. Whisper is not incremental, so
the stream carries exactly one final chunk with the whole transcript, `finishReason` and usage.

### Audio

- The audio is the **first `inline_data` part** in document order (`inlineData`/`mimeType` and
  `inline_data`/`mime_type` are both accepted). Other parts are ignored. `file_data` is not
  supported (400).
- Accepted: Ogg/Opus/Vorbis (`audio/ogg`, `audio/oga`, `audio/opus`, `application/ogg`), MP3
  (`audio/mpeg`, `audio/mp3`, `audio/mpga`), WAV, FLAC, WebM (`audio/webm`, `video/webm`),
  M4A/MP4 (`audio/mp4`, `audio/m4a`, `audio/x-m4a`, `video/mp4`). `application/octet-stream` or
  a missing MIME type is fine when the bytes carry a recognisable container signature.
- Groq checks the upload's file extension strictly, so the plugin names the upload after the
  real container: Telegram voice notes (Opus in Ogg, often saved as `.oga`) are sent as
  `audio.ogg`. When the bytes carry a signature (`OggS`, `RIFF…WAVE`, `fLaC`, EBML, `ftyp`,
  `ID3`/MPEG frame sync) the bytes decide; otherwise the declared MIME type does.
- Base64 may be standard or URL-safe, padded or not, and may contain line breaks.
- Size is checked **before** uploading: decoded audio above `max_audio_bytes` (default
  25,000,000 bytes, Groq's free-tier limit) is rejected with 413 and never leaves CPA.

### Language

- Plugin default: `language` in the plugin config (default `tr`).
- Per request: `cpa_groq.language`. `"auto"` or `""` lets Whisper detect the language.
- ISO-639-1 codes; BCP-47 tags are reduced to their primary subtag (`tr-TR` → `tr`).

### Prompt (vocabulary and spelling hints)

Whisper's `prompt` is **not an instruction**. It is a short text that biases spelling and
vocabulary. A Gemini request, however, usually carries an instruction such as "Transcribe this
audio verbatim…" in a text part. Forwarding that to Whisper would bias it towards that phrase
(and can leak into the transcript). So:

- **Text parts, `systemInstruction`, `generationConfig`, tools etc. are never used.** The same
  request body that works for a Gemini model therefore works unchanged here.
- The only prompt sources are the explicit, namespaced `cpa_groq.prompt` field and the plugin's
  `prompt` config default. `cpa_groq.prompt: ""` clears the default for one request.
- Groq rejects prompts longer than 896 characters (Whisper uses at most 224 tokens), so longer
  prompts are refused locally with 400.

`cpa_groq` is strict: unknown keys (for example a `lang` typo) are rejected with 400 instead of
being silently ignored. **Do not send `cpa_groq` to real Gemini models**: Google rejects unknown
fields. Add it only when the model is a `groq-whisper-*` id.

### Fixed upstream parameters

`temperature=0` and `response_format=verbose_json` are always sent. `verbose_json` returns
segments with `no_speech_prob` and `avg_logprob`, which the silence filter uses.

### Silence / hallucination filter

Whisper tends to invent text for silence (on Turkish audio, for example "Altyazı M.K."). The
plugin drops a segment only when **both** hold, which is the rule openai-whisper's own
`transcribe()` uses:

- `no_speech_prob > no_speech_threshold` (default **0.6**), and
- `avg_logprob < logprob_threshold` (default **-1.0**).

`no_speech_threshold: 1` disables filtering. `logprob_threshold: 0` filters on
`no_speech_prob` alone. If every segment is dropped, the transcript is an empty string (still
HTTP 200). If nothing is dropped, Groq's own `text` is returned unchanged (trimmed).

What Groq actually returns (observed September 2026) limits what this filter can do:

- `no_speech_prob` and `avg_logprob` are reported per 30-second decoding window: every segment
  in a window carries the same values, so the filter effectively keeps or drops whole windows.
- `whisper-large-v3-turbo` reports `no_speech_prob: 0` for every segment, so the filter never
  fires for turbo.
- A one-second synthetic tone sent to `whisper-large-v3` came back as a hallucinated caption
  with `no_speech_prob` 0.958 and `avg_logprob` -0.20. The default rule keeps it (the decode is
  "confident"); `logprob_threshold: 0` would drop it, at a higher risk of dropping quiet real
  speech. Clients that must reject silence should also check the audio length or the text.

### Response

```json
{
  "candidates": [{
    "content": {"role": "model", "parts": [{"text": "…transcript…"}]},
    "finishReason": "STOP",
    "index": 0
  }],
  "usageMetadata": {
    "promptTokenCount": 320,
    "candidatesTokenCount": 42,
    "totalTokenCount": 362,
    "promptTokensDetails": [{"modality": "AUDIO", "tokenCount": 320}],
    "candidatesTokensDetails": [{"modality": "TEXT", "tokenCount": 42}]
  },
  "modelVersion": "groq-whisper-large-v3-turbo",
  "responseId": "req_…"
}
```

- `promptTokenCount` counts audio the way Gemini does, **32 tokens per second** of audio
  (ceil), so Groq and Gemini voice requests are comparable in CPA's usage statistics. Audio
  seconds = `promptTokenCount / 32`. (Groq bills at least 10 s per request.)
- `candidatesTokenCount` is the number of Whisper output tokens in the kept segments (estimated
  as characters/4 if Groq ever omits token ids).
- `responseId` is Groq's request id (`x_groq.id`) when present.
- `countTokens` is not supported (400).

## Errors

Every failure is a pluginabi error envelope with `http_status`, in both `executor.execute` and
`executor.execute_stream`. CPA turns it into its usual error body.

| Situation | Status to the client |
|-----------|----------------------|
| Malformed body, no `inline_data`, bad base64, not audio, unsupported container, bad `cpa_groq`, prompt too long | 400 |
| Audio larger than `max_audio_bytes` (checked before upload) | 413 |
| Groq 400 / 401 / 403 / 404 / 413 / 422 / 429 and other 4xx | same status, Groq's message (sanitized) |
| Groq 408 | 504 |
| Groq 498 (flex capacity) | 503 |
| Groq 500 / 502 / 503 / 504 | same status |
| Other Groq 5xx or unexpected status, invalid Groq response, network failure (DNS, TLS, reset) | 502 |
| No answer within `timeout_seconds` | 504 |
| Client disconnected | no status, message `context canceled` (see below) |

- When Groq sends `retry-after`, the message ends with `(retry after Ns)`. The plugin ABI error
  envelope has no structured retry-after field, so the hint travels in the message text only.
- Groq's error text is truncated to 300 bytes, stripped of control characters, and any API key
  (anything shaped like a Groq key) is redacted before it is surfaced.
- Client cancellation is reported without an HTTP status and with the exact message
  `context canceled`, which CPA classifies as a connection-lifecycle event, so a caller hanging
  up never puts the credential into cooldown.

### CPA cooldowns (host behaviour, not plugin behaviour)

CPA's auth manager reacts to executor errors per auth record and model, as it does for every
provider. Useful to know when operating this plugin:

- 400 / 409 / 413 / 422 are request faults: no cooldown.
- 401 / 402 / 403 put the auth record into a **30-minute** cooldown that survives fixing the key.
  In testing, right after a 401 CPA refused **both** models on that auth record (HTTP 503
  `auth_unavailable`) even with the correct key reloaded. To recover immediately, disable and
  re-enable the auth record (Management Center, or set `"disabled": true` in the auth file and
  remove it again); both models then worked again. This was verified end to end.
- 404 cools the model down for 12 hours; 429 uses CPA's quota backoff; 408/5xx/504 use the
  transient cooldown (1 minute by default, `transient-error-cooldown-seconds`).
- `request-retry` and multiple auth records make CPA retry on another auth; each retry uploads
  the audio again.

## Configuration

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-groq:
      enabled: true
      priority: 1
      api_key: "<groq api key>"       # required; never logged
      language: "tr"                  # default "tr"; "auto" or "" = detect
      prompt: ""                      # default vocabulary hint, max 896 characters
      timeout_seconds: 60             # 1-600, bound on one Groq call
      max_audio_bytes: 25000000       # 1024-100000000 (dev tier allows 100 MB)
      no_speech_threshold: 0.6        # 0-1; 1 disables the silence filter
      logprob_threshold: -1.0         # -100-0; 0 = filter on no_speech_prob alone
      # base_url: "https://api.groq.com/openai/v1"   # https only (http only for loopback)
```

- The config is re-read on every CPA config reload (`plugin.reconfigure`) and swapped
  atomically; in-flight requests finish with the settings they started with.
- An invalid config (missing key, out-of-range value, broken YAML) is rejected with a message
  that names fields and line numbers but never values. CPA then unregisters the plugin's models
  until a valid config arrives; the previous settings stay in memory unused.
- Unknown keys are ignored and reported (names only) in CPA's log.

## The auth record CPA needs

CPA only routes a model to a plugin executor through its auth manager: the model must be
registered under an **auth record whose provider key equals the executor's provider key**, or
the request fails with `auth_not_found` / `unknown provider`. Concretely (v7.3.17):

- The plugin registers its models for provider key `cpa-groq` (`model.register`) and its
  executor identifies as `cpa-groq`.
- CPA attaches those models to every auth record with provider `cpa-groq` and picks one of those
  records for each request.
- Auth records come from built-in config sections (only for built-in providers or
  `openai-compatibility`, which would bind CPA's own JSON executor instead of this plugin) or from
  JSON files in `auth-dir`. Any file with `"type": "<provider>"` becomes an auth record for that
  provider, with no plugin auth capability needed.

So the cleanest supported way is a secret-free marker file in the auth directory:

```json
{"type": "cpa-groq", "label": "Groq Whisper (cpa-groq)"}
```

(also in `examples/cpa-groq.auth.json`). The Groq key stays in the plugin config. Optional fields that CPA understands on any auth file:
`"prefix": "groq"` (adds `groq/<model>` ids), `"disabled": true`, `"priority"`. The marker file
is picked up live by CPA's auth-dir watcher, or can be uploaded through the Management API
(`POST /v0/management/auth-files`).

## Build

Everything runs in pinned containers; nothing is installed on the host.

```sh
make check   # go vet, golangci-lint v2.13.2, go test -race (with and without -tags cabitest)
make build   # dist/linux-amd64/cpa-groq-v0.1.0.so (+ .sha256)
```

The build uses `golang:1.26.8-bookworm@sha256:9fdc884a…`, `GOFLAGS=-mod=readonly`,
`GOTOOLCHAIN=local`, `-trimpath`, `-buildvcs=true`, `-ldflags='-s -w -buildid='` and
`-buildmode=c-shared`. The CPA SDK is pinned to `github.com/router-for-me/CLIProxyAPI/v7
v7.3.17` (commit `9bdde54`). Building the tagged commit from a clean checkout reproduces the
release checksum byte for byte. The library links only against `libc.so.6`, carries
`DF_1_NODELETE` (Go runtimes cannot be unloaded, so CPA's `dlclose` is a no-op), and exports
`cliproxy_plugin_init` plus Go's own export wrappers.

The image is Debian bookworm, the same base as CPA's release image, so the glibc baseline matches.

## Install from the CLIProxyAPI plugin store

When installed from the CLIProxyAPI plugin store (id `cpa-groq`, linux/amd64), the Management
Center puts the library in place; the auth marker file and the config block below are still
needed, because the plugin needs a Groq API key and CPA only routes plugin models
through an auth record of the plugin's provider (see "The auth record CPA needs").

Tested end to end (store install → auth marker → key → transcription, no restart). Two things
to know:

- A store install makes CPA **rewrite `config.yaml`**: it adds a `plugins.configs.cpa-groq` block
  with a `store:` section and fills in every default setting. Put `api_key` (and any other
  option) **into that block**; a second `configs:` key makes the YAML invalid, and CPA then keeps
  running on the previous config and logs `failed to reload config`.
- Until `api_key` is set, CPA logs `cpa-groq: rejected configuration reason="invalid config:
  api_key is required"` and the models stay hidden. This is expected; the plugin registers as
  soon as a valid config is reloaded.

Release assets follow the store layout: `cpa-groq_<version>_linux_amd64.zip` (holding
`cpa-groq.so` at the zip root) and `checksums.txt`. `make build && make package` produces them in
`dist/store/`, reproducibly.

## Install into CPA manually

1. **The plugin directory must be a bind mount.** `plugins.dir` resolves against CPA's working
   directory (`/CLIProxyAPI/plugins` in the Docker image). If it is not bind-mounted, the plugin
   lives in the container's writable layer and disappears on the next image pull/recreate.
   ```yaml
   volumes:
     - ./plugins:/CLIProxyAPI/plugins
   ```
2. Copy the library as `plugins/linux/amd64/cpa-groq-v<version>.so` (the file name defines the
   plugin id `cpa-groq` and the version) and verify its SHA-256.
3. Create the auth marker file `cpa-groq.json` (above) in CPA's `auth-dir`.
4. Add the `plugins.configs.cpa-groq` block (above) to `config.yaml` and let CPA reload it.
   `plugins.enabled` must be `true`.
5. Check the log for `pluginhost: plugin registered plugin_id=cpa-groq` and
   `cpa-groq: configuration applied`, and that `GET /v1/models` lists the two models. Then run
   `scripts/smoke.py` against the instance.

Rollback: set `plugins.configs.cpa-groq.enabled: false` (or remove the block) and remove the
auth file; no restart is needed. On startup CPA **deletes older versions of a loaded plugin
from the plugin directory**, so keep rollback copies outside it.

## Security

- CPA plugins are **trusted in-process code**: this library is `dlopen`ed into the proxy, runs
  with the proxy's privileges (root in the official container) and could read every credential
  CPA holds. It is not sandboxed. Review the source and build it yourself, or verify the
  release checksum.
- The plugin never logs audio, transcripts, prompts or the API key; its log lines carry sizes,
  durations, statuses and model ids only. Error messages redact anything shaped like a Groq key.
- **CPA itself** records traffic when `request-log: true`: the inbound request (base64 audio),
  the outbound multipart body (raw audio), Groq's response (the transcript) and the outbound
  `Authorization` header, which CPA masks to its first and last four characters. Treat CPA's
  log directory accordingly.
- The Groq key lives in CPA's `config.yaml` next to every other provider credential.
- All upstream traffic goes through CPA's HTTP client (`host.http.do`), so CPA's proxy settings
  apply. `base_url` must be `https` (plain `http` only for loopback, for local testing).

## Robustness

- Every exported C entry point and every goroutine recovers from panics; a panic is turned into
  an `internal_error` envelope and logged, never propagated into CPA.
- One Groq call is bounded by `timeout_seconds`. The host offers cancellation but no deadline,
  so the plugin opens a host HTTP operation and a watchdog cancels it when the deadline passes.
  The watchdog is always joined before the call returns, so no plugin goroutine outlives a
  request or touches the host after it.
- Client disconnects cancel the upstream request (the host ties the operation to the request).
- The only mutable global state is an atomically swapped config snapshot; the host table
  pointer is published with C11 atomics.
- The plugin refuses to initialise against a host with a different C ABI version.
- Requests far larger than the audio limit are rejected before any JSON decoding, and after
  large requests the plugin returns its heap to the OS immediately.

## Limitations

- Transcription only: no translation endpoint, no word timestamps, no URL input.
- Memory: the ABI passes the request as JSON, so CPA hands the plugin the audio twice (request
  and original request), each base64-encoded twice. A 25 MB file means roughly 90 MB crossing
  the boundary. Voice notes are tiny; very large files are costly for the shared process.
- The models are visible to, and usable with, every CPA client key; CPA v7.3.17 has no per-key
  model scoping for plugin models.
- The plugin ABI and JSON contract are CPA-internal and can change between CPA releases. On an
  ABI version change the plugin refuses to load (CPA logs it; the models disappear) rather than
  guessing at struct layouts; a JSON contract change surfaces as request errors, not crashes.
  Re-run `scripts/smoke.py` after CPA upgrades.
- **Groq's Turkish output can be silently damaged.** On a 4:45 real Turkish voice note, both
  `whisper-large-v3` and `-turbo` returned six ~30-second stretches where every word stops at its
  first non-ASCII letter ("fabrikası" → "fabrikas", "lazım" → "laz") and punctuation disappears,
  while `no_speech_prob` and `avg_logprob` looked normal. Calling Groq directly (no CPA, no plugin)
  gives the same text, with `json` or `verbose_json`, with a Turkish prompt and as 16 kHz FLAC, so
  it is upstream behaviour the plugin cannot detect. Test on your own language before relying on it.

## Development

- `make check` runs vet, lint and race tests. `-tags cabitest` adds tests that drive the real
  exported C entry points against an in-process C-ABI test host.
- Test audio is generated (sine-tone WAV and signature stubs). No recorded speech or transcript
  is committed to this repository.
- `scripts/smoke.py` sends a generated tone through a running CPA and checks the response shape.
- Release checklist: bump `VERSION` (Makefile) and `pluginVersion` (models.go), update the
  changelog, tag `v<version>`, `make check build package`, then upload `dist/linux-amd64/*.so*` and
  `dist/store/*` to the GitHub release. The store always installs from the latest release, so a
  release without the zip and `checksums.txt` breaks store installs.

## License

MIT, see [LICENSE](LICENSE). CLIProxyAPI is MIT-licensed as well.
