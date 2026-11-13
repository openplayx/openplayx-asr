# openplayx-asr

Standalone speech recognition service for OpenPlayX. One resident Go process; a single worker calls **whisper.cpp in-process** through the same `opx_asr` C ABI the App embeds (`whisper-package/src/opx_asr.h`). Both backends therefore return identical result structures.

- No `ffmpeg`, `whisper-cli` or `whisper-server` process is ever started.
- It accepts only the audio the App already normalised (16 kHz mono 16-bit PCM WAV). It never accepts video URLs, NAS credentials or client paths, and it does not convert formats.
- It only transcribes. There is no translation option.
- CPU only. Business concurrency is 1, with a bounded queue, upload/duration limits and a per-job timeout.
- It is not part of Relay/Home or the Jenkins gateway, and nothing deploys it automatically.

## API

Project-specific asynchronous API (OpenAPI 3.1: `src/openapi.yaml`, served at `/openapi.yaml`; summary page `/docs`). It is not the whisper.cpp example server and does not claim OpenAI audio API compatibility.

| Endpoint | Purpose |
|---|---|
| `GET /v1/capabilities` | Models + revision, audio format, timestamp level (`segment`), limits |
| `POST /v1/transcription-jobs?model=&language=` | Upload audio with `Idempotency-Key` and `X-Content-SHA256` headers. Returns 202 + job; the same key returns the existing job. |
| `GET /v1/transcription-jobs?idempotency_key=` | Reconcile after a lost response |
| `GET /v1/transcription-jobs/{id}` | State, progress, error code |
| `GET /v1/transcription-jobs/{id}/result` | Windows/segments in `audio_ms`. Returns 409 `not_ready` until it succeeds. |
| `POST /v1/transcription-jobs/{id}/cancel` | Idempotent. A running job reports `cancelling` until whisper stops. |
| `DELETE /v1/transcription-jobs/{id}` | Removes a terminal job with its data. An active job gets 409. |

Result times are milliseconds relative to the uploaded audio. The App adds its extraction start to map them onto the media timeline. A segment that starts inside a window's overlap tail belongs to the next window.

Every user only sees their own jobs. Unknown or foreign job ids return 404. Inputs are deleted when a job ends, and finished jobs expire after `ttl_hours`. Jobs that were queued or running during a restart are run again from their stored input.

## Build

```sh
scripts/build.sh            # builds ../whisper-package (pinned whisper.cpp 1.9.5) and dist/openplayx-asr
dist/openplayx-asr -version # "available=true" means the engine is linked
cd src && go test ./...     # API/store tests with a fake engine (no native build)
```

`OPX_WHISPER_PACKAGE` overrides the location of whisper-package. CI checks it out as a companion (see `build/projects/openplayx-asr.yaml`).

## Deploy

1. Create the user and directories: `/opt/openplayx-asr`, `/etc/openplayx-asr`, `/var/lib/openplayx-asr/{models,data}`.
2. Put the model in `models/` and copy `deploy/config.example.json` to `/etc/openplayx-asr/config.json`. At startup the service checks each model's size and SHA-256, and refuses a mismatch. Use multilingual models for Chinese; `.en` models are English-only. Start with base or small and measure.
3. Create a token, store only its hash, and give the token to the App (Settings → Speech recognition → Add HTTP service):
   ```sh
   head -c 24 /dev/urandom | base64 | tr -d '/+=' > token
   printf '{"tokens":[{"user":"me","sha256":"%s"}]}\n' "$(openplayx-asr -hash-token < token)" > /etc/openplayx-asr/tokens.json
   ```
4. Install `deploy/openplayx-asr.service`. Its `CPUQuota`/`MemoryMax`/`Nice` keep it from starving other work (for example CI on the same host).
5. Expose it via HTTPS: set `tls_cert`/`tls_key` or use a reverse proxy. Plain HTTP on a LAN works only after the App user explicitly opts in, and it travels unencrypted.

GPU and faster-whisper are possible later backends behind the same API. Their model files are not interchangeable with ggml models.

## Status

Verified on macOS arm64 with base/tiny models (unit tests, plus App integration tests against a live instance). The Linux build and the measured performance on 211 are not verified yet.
