# openplayx-asr

[English](README.md) | 简体中文

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[OpenPlayX](https://github.com/openplayx/openplayx) 的独立语音识别服务：提供 HTTP API 和 MCP 端点，把音频转成带时间戳的文字。单个常驻 Go 进程，由一个 worker 在进程内调用 **whisper.cpp**，走的是 App 内嵌的同一套 `opx_asr` C ABI（[whisper-package](https://github.com/openplayx/whisper-package)），所以两种后端返回的结果结构完全相同。

- 不启动任何 `ffmpeg`、`whisper-cli` 或 `whisper-server` 进程。
- 只接受 App 已经规范化好的音频（16 kHz 单声道 16 位 PCM WAV）。不接受视频链接、NAS 凭据或客户端路径，也不做格式转换。
- 只做转写，没有翻译选项。
- 只用 CPU。业务并发为 1，带有限长度的队列、上传大小/时长限制和单任务超时。
- 异步任务，支持幂等键、按用户隔离、重启恢复和自动过期。

## API

项目自定义的异步 API（OpenAPI 3.1：`src/openapi.yaml`，在 `/openapi.yaml` 提供；概要页面 `/docs`）。它不是 whisper.cpp 的示例服务器，也不声称兼容 OpenAI 音频 API。

| 端点 | 用途 |
|---|---|
| `GET /healthz` | 存活检查（不需要 token） |
| `GET /v1/capabilities` | 模型及版本、音频格式、时间戳粒度（`segment`）、限制 |
| `POST /v1/transcription-jobs?model=&language=` | 上传音频，带 `Idempotency-Key` 和 `X-Content-SHA256` 请求头。返回 202 和任务；相同的键返回已有任务。 |
| `GET /v1/transcription-jobs?idempotency_key=` | 响应丢失后对账 |
| `GET /v1/transcription-jobs/{id}` | 状态、进度、错误码 |
| `GET /v1/transcription-jobs/{id}/result` | 以 `audio_ms` 表示的窗口/片段。成功之前返回 409 `not_ready`。 |
| `POST /v1/transcription-jobs/{id}/cancel` | 幂等。运行中的任务在 whisper 停止前显示 `cancelling`。 |
| `DELETE /v1/transcription-jobs/{id}` | 删除已结束的任务及其数据。活动中的任务返回 409。 |
| `POST /v1/audio-uploads` | 为 MCP 暂存音频，返回短期有效的 `audio_ref` |

结果时间是相对于上传音频的毫秒数。App 加上自己的提取起点，映射到媒体时间轴上。起点落在某个窗口重叠尾部的片段，归下一个窗口。

每个用户只能看到自己的任务。未知或属于他人的任务 id 返回 404。任务结束时删除输入，已结束的任务在 `ttl_hours` 后过期。重启时处于排队或运行中的任务，会用保存的输入重新执行。

## MCP

`POST /mcp` 提供 `list_models`、`create_transcription`、`get_transcription`、`get_transcription_result` 和 `cancel_transcription`。音频永远不作为工具参数：先用 `POST /v1/audio-uploads` 上传（检查规则与创建任务相同），得到短期有效、按用户隔离的 `audio_ref`，再带上它和一个 `operation_id`（幂等键）调用 `create_transcription`。

支持 Streamable HTTP 的一个子集：通过 `POST` 发送单条 JSON-RPC 2.0 消息，以 `application/json` 应答（不用 SSE，服务器不主动发消息）；无状态（不发 `Mcp-Session-Id`）；支持 `initialize`、`ping`、`tools/list`、`tools/call` 和通知；协议版本 2025-11-25、2025-06-18 和 2025-03-26。每个请求都需要与 HTTP API 相同的 Bearer token；来自外部 `Origin` 的请求会被拒绝。业务失败返回 `isError: true` 的工具结果，错误码与 HTTP API 相同。MCP 层调用的是与 HTTP 处理器相同的服务代码（没有第二份状态，也不回环请求自己）。

## 构建

依赖：Go 1.24+、C/C++ 工具链和 CMake，以及与本仓库放在同级目录的 [whisper-package](https://github.com/openplayx/whisper-package) 检出（或用 `OPX_WHISPER_PACKAGE` 指定路径）。

```sh
git clone https://github.com/openplayx/whisper-package ../whisper-package
scripts/build.sh            # 构建 ../whisper-package（锁定 whisper.cpp 1.9.5）和 dist/openplayx-asr
dist/openplayx-asr -version # 显示 "available=true" 表示引擎已链接
cd src && go test ./...     # 使用模拟引擎的 API/存储测试（不需要原生构建）
```

## 快速开始

```sh
mkdir -p models data
curl -fL -o models/ggml-base.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.bin
cp deploy/config.example.json config.json   # 把 listen、data_dir、tokens_file 和模型路径改成本地目录

head -c 24 /dev/urandom | base64 | tr -d '/+=' > token
printf '{"tokens":[{"user":"me","sha256":"%s"}]}\n' "$(dist/openplayx-asr -hash-token < token)" > tokens.json

dist/openplayx-asr -config config.json
curl -s -H "Authorization: Bearer $(cat token)" http://127.0.0.1:8787/v1/capabilities
```

启动时服务会按配置检查每个模型的大小和 SHA-256，不一致就拒绝启动；示例配置里已经填好了 `ggml-base.bin` 的值。中文请使用多语言模型，`.en` 模型只支持英文。建议先用 base 或 small，实测后再调整。

## 部署

1. 创建用户和目录：`/opt/openplayx-asr`、`/etc/openplayx-asr`、`/var/lib/openplayx-asr/{models,data}`。
2. 把模型放进 `models/`，把 `deploy/config.example.json` 复制为 `/etc/openplayx-asr/config.json`。
3. 按快速开始里的方法生成 token，只把它的哈希保存到 `/etc/openplayx-asr/tokens.json`，把 token 交给 App（设置 → 语音识别 → 添加 HTTP 服务）。
4. 安装 `deploy/openplayx-asr.service`。其中的 `CPUQuota`/`MemoryMax`/`Nice` 防止它挤占同一台机器上的其他工作。
5. 通过 HTTPS 提供服务：设置 `tls_cert`/`tls_key`，或使用反向代理。局域网内的明文 HTTP 只有在 App 用户明确同意后才能使用，且传输不加密。

GPU 和 faster-whisper 可以作为以后在同一 API 之后的后端。它们的模型文件与 ggml 模型不通用。

## 状态

已在 macOS arm64 上用 base/tiny 模型验证（单元测试，以及 App 对运行实例的集成测试）。Linux 构建和 Linux 上的性能尚未验证。

## 参与贡献

欢迎提 issue 和 PR，见 [CONTRIBUTING.md](CONTRIBUTING.md)（英文）。安全问题请按 [SECURITY.md](SECURITY.md)（英文）私下报告。

## 许可证

[MIT](LICENSE)。whisper.cpp 和 Whisper 模型权重由其作者以 MIT 许可证发布。
