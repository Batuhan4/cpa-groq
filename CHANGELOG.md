# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- `make package` and `tools/storezip`: reproducible plugin-store assets
  (`cpa-groq_<version>_linux_amd64.zip` + `checksums.txt`). The v0.1.0 release carries them too,
  wrapping the unchanged v0.1.0 library.
- README: plugin-store install, release checklist, and a known limitation (Groq's Turkish output
  can be silently truncated upstream).

## [0.1.0] - 2026-09-25

First release, built and tested against CLIProxyAPI v7.3.17 (commit `9bdde54`, plugin C ABI 1).

### Added
- CPA executor plugin exposing Groq Whisper as `groq-whisper-large-v3` and
  `groq-whisper-large-v3-turbo` for Gemini `generateContent` and `streamGenerateContent`
  (one final chunk).
- Audio from the first `inline_data` part; Ogg/Opus, MP3, WAV, FLAC, WebM and M4A/MP4, with
  container sniffing so the upload name matches the content (Telegram `.oga` → `audio.ogg`).
- Plugin config: `api_key`, `language` (default `tr`), `prompt`, `timeout_seconds` (60),
  `max_audio_bytes` (25,000,000), `no_speech_threshold` (0.6), `logprob_threshold` (-1.0),
  `base_url`; hot reload via `plugin.reconfigure`.
- Per-request `cpa_groq` extension object (`language`, `prompt`). Text parts are never used
  as the Whisper prompt.
- `temperature=0`, `response_format=verbose_json`, openai-whisper style silence filter.
- Gemini-shaped responses with usage metadata (audio at 32 tokens/s, Whisper output tokens).
- Error envelopes with `http_status` for Groq 4xx/5xx, timeouts, network failures and client
  cancellation; `retry-after` hint in the message; key redaction.
- Upstream calls through CPA's `host.http.do` with a plugin-enforced deadline
  (`host.http.operation_open` + `host.http.cancel`).
- Panic containment at every C entry point and goroutine; C-ABI tests (`-tags cabitest`).
- Reproducible c-shared build in a pinned `golang:1.26.8-bookworm` image; lint with pinned
  golangci-lint; `scripts/smoke.py`.
