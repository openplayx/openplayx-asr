# openplayx-asr

English | [简体中文](README.zh-CN.md)

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Standalone speech recognition service for [OpenPlayX](https://github.com/openplayx/openplayx):
an HTTP API and MCP endpoint that turns audio into timestamped transcripts. One
resident Go process; a single worker calls **whisper.cpp in-process** through
the same `opx_asr` C ABI the app embeds
([package-whisper](https://github.com/openplayx/package-whisper)), so both
backends return identical result structures.

- No `ffmpeg`, `whisper-cli` or `whisper-server` process is ever started.
- Accepts only audio the app already normalised (16 kHz mono 16-bit PCM WAV).
  It never accepts video URLs, NAS credentials or client paths, and does not
  convert formats.
- Transcription only; there is no translation option.
- CPU only. Business concurrency is 1, with a bounded queue, upload/duration
  limits and a per-job timeout.
- Asynchronous jobs with idempotency keys, per-user isolation, restart
  recovery and automatic expiry.

## API

Project-specific asynchronous API (OpenAPI 3.1: `src/openapi.yaml`, served at
`/openapi.yaml`; summary page `/docs`). It is not the whisper.cpp example server
and does not claim OpenAI audio API compatibility.

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness (no token) |
| `GET /v1/capabilities` | Models + revision, audio format, timestamp level (`segment`), limits |
| `POST /v1/transcription-jobs?model=&language=` | Upload audio with `Idempotency-Key` and `X-Content-SHA256` headers. Returns 202 + job; the same key returns the existing job. |
| `GET /v1/transcription-jobs?idempotency_key=` | Reconcile after a lost response |
| `GET /v1/transcription-jobs/{id}` | State, progress, error code |
| `GET /v1/transcription-jobs/{id}/result` | Windows/segments in `audio_ms`. Returns 409 `not_ready` until it succeeds. |
| `POST /v1/transcription-jobs/{id}/cancel` | Idempotent. A running job reports `cancelling` until whisper stops. |
| `DELETE /v1/transcription-jobs/{id}` | Removes a terminal job with its data. An active job gets 409. |
| `POST /v1/audio-uploads` | Stage audio for MCP and get a short-lived `audio_ref` |

Result times are milliseconds relative to the uploaded audio. The app adds its
extraction start to map them onto the media timeline. A segment that starts
inside a window's overlap tail belongs to the next window.

Every user only sees their own jobs. Unknown or foreign job ids return 404.
Inputs are deleted when a job ends, and finished jobs expire after `ttl_hours`.
Jobs that were queued or running during a restart are run again from their
stored input.

## MCP

`POST /mcp` exposes `list_models`, `create_transcription`,
`get_transcription`, `get_transcription_result` and `cancel_transcription`.
Audio is never a tool argument: upload it with `POST /v1/audio-uploads`
(same checks as job creation) to get a short-lived, per-user `audio_ref`, then
call `create_transcription` with it and an `operation_id` (idempotency key).

Streamable HTTP subset: single JSON-RPC 2.0 messages over `POST`, answered
with `application/json` (no SSE, no server-initiated messages); stateless (no
`Mcp-Session-Id`); `initialize`, `ping`, `tools/list`, `tools/call` and
notifications; protocol versions 2025-11-25, 2025-06-18 and 2025-03-26. The
same bearer token as the HTTP API is required on every request; requests with
a foreign `Origin` are refused. Business failures are tool results with
`isError: true` and the HTTP API's error codes. The MCP layer calls the same
service code as the HTTP handlers (no second state, no loopback request).

## Build

Requirements: Go 1.24+, a C/C++ toolchain and CMake, and a checkout of
[package-whisper](https://github.com/openplayx/package-whisper) next to this
repository (or set `OPX_WHISPER_PACKAGE` to its path).

```sh
git clone https://github.com/openplayx/package-whisper ../package-whisper
scripts/build.sh            # builds ../package-whisper (pinned whisper.cpp 1.9.5) and dist/openplayx-asr
dist/openplayx-asr -version # "available=true" means the engine is linked
cd src && go test ./...     # API/store tests with a fake engine (no native build)
```

## Quick start

```sh
mkdir -p models data
curl -fL -o models/ggml-base.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.bin
cp deploy/config.example.json config.json   # point listen, data_dir, tokens_file and model path at the local dirs

head -c 24 /dev/urandom | base64 | tr -d '/+=' > token
printf '{"tokens":[{"user":"me","sha256":"%s"}]}\n' "$(dist/openplayx-asr -hash-token < token)" > tokens.json

dist/openplayx-asr -config config.json
curl -s -H "Authorization: Bearer $(cat token)" http://127.0.0.1:8787/v1/capabilities
```

At startup the service checks each model's size and SHA-256 against the
configuration and refuses a mismatch; the example configuration contains the
values for `ggml-base.bin`. Use multilingual models for Chinese; `.en` models
are English-only. Start with base or small and measure.

## Deploy

1. Create the user and directories: `/opt/openplayx-asr`, `/etc/openplayx-asr`,
   `/var/lib/openplayx-asr/{models,data}`.
2. Put the model in `models/` and copy `deploy/config.example.json` to
   `/etc/openplayx-asr/config.json`.
3. Create a token as in the quick start, store only its hash in
   `/etc/openplayx-asr/tokens.json`, and give the token to the app
   (Settings → Speech recognition → Add HTTP service).
4. Install `deploy/openplayx-asr.service`. Its `CPUQuota`/`MemoryMax`/`Nice`
   keep it from starving other work on the same host.
5. Expose it via HTTPS: set `tls_cert`/`tls_key` or use a reverse proxy. Plain
   HTTP on a LAN works only after the app user explicitly opts in, and it
   travels unencrypted.

GPU and faster-whisper are possible later backends behind the same API. Their
model files are not interchangeable with ggml models.

## Status

Verified on macOS arm64 with base/tiny models (unit tests, plus app integration
tests against a live instance). The Linux build and Linux performance are not
verified yet.

## Contributing

Issues and pull requests are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md).
Report security problems privately as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). whisper.cpp and the Whisper model weights are MIT licensed by
their authors.
