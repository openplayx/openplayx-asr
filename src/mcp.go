package main

// MCP entry point (Streamable HTTP, POST /mcp).
//
// A thin protocol layer over the same job store, tokens and validation as
// /v1: no second job state, no loopback HTTP. Audio never travels in a tool
// call: it is uploaded with POST /v1/audio-uploads and referenced by
// audio_ref.
//
// Supported subset: single JSON-RPC 2.0 messages over POST answered with
// application/json (no SSE, no server-initiated messages); stateless (no
// Mcp-Session-Id); initialize, ping, tools/list, tools/call and
// notifications. Business failures are tool results with isError=true and
// the same error codes as the HTTP API.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
)

var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	maxMCPBody        = 64 << 10
)

const mcpInstructions = "Speech recognition on the user's own server. Upload 16 kHz mono " +
	"PCM WAV with POST /v1/audio-uploads to get an audio_ref, then create_transcription. " +
	"Jobs run asynchronously: poll get_transcription and fetch get_transcription_result " +
	"once it succeeded. Closing the conversation does not cancel a job."

var (
	audioRefRe = regexp.MustCompile(`^aud_[0-9a-f]{32}$`)
	jobIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type mcpTool struct {
	Name        string
	Title       string
	Description string
	Schema      map[string]any
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	Run         func(a *API, user string, args json.RawMessage) (any, *apiError)
}

func (t mcpTool) describe() map[string]any {
	return map[string]any{
		"name": t.Name, "title": t.Title, "description": t.Description,
		"inputSchema": t.Schema,
		"annotations": map[string]any{
			"title": t.Title, "readOnlyHint": t.ReadOnly, "destructiveHint": t.Destructive,
			"idempotentHint": t.Idempotent, "openWorldHint": false,
		},
	}
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var invalid = &apiError{400, "invalid_request"}

func decodeArgs(raw json.RawMessage, v any) *apiError {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		return invalid
	}
	return nil
}

var jobIDSchema = map[string]any{"type": "string", "maxLength": 64, "description": "Job id from create_transcription."}

func jobTool(name, title, desc string, readOnly bool, run func(a *API, user, id string) (any, *apiError)) mcpTool {
	return mcpTool{
		Name: name, Title: title, Description: desc, ReadOnly: readOnly, Idempotent: true,
		Schema: object(map[string]any{"job_id": jobIDSchema}, "job_id"),
		Run: func(a *API, user string, raw json.RawMessage) (any, *apiError) {
			var args struct {
				JobID string `json:"job_id"`
			}
			if e := decodeArgs(raw, &args); e != nil {
				return nil, e
			}
			if !jobIDRe.MatchString(args.JobID) {
				return nil, invalid
			}
			return run(a, user, args.JobID)
		},
	}
}

var mcpTools = []mcpTool{
	{
		Name: "list_models", Title: "List models", ReadOnly: true, Idempotent: true,
		Description: "Recognition models this server has loaded, with language support.",
		Schema:      object(map[string]any{}),
		Run: func(a *API, _ string, raw json.RawMessage) (any, *apiError) {
			if e := decodeArgs(raw, &struct{}{}); e != nil {
				return nil, e
			}
			return map[string]any{"models": a.modelsView()}, nil
		},
	},
	{
		Name: "create_transcription", Title: "Create transcription", Idempotent: true,
		Description: "Queues recognition of staged audio. operation_id makes retries safe: " +
			"repeating it returns the same job instead of a duplicate. Returns the job; it " +
			"does not wait for the transcript.",
		Schema: object(map[string]any{
			"audio_ref":    map[string]any{"type": "string", "maxLength": 64, "description": "audio_ref from POST /v1/audio-uploads."},
			"model":        map[string]any{"type": "string", "maxLength": 64, "description": "Model id from list_models."},
			"language":     map[string]any{"type": "string", "maxLength": 8, "default": "auto", "description": "auto or an ISO 639-1 code."},
			"operation_id": map[string]any{"type": "string", "minLength": 16, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$", "description": "Client-chosen idempotency key."},
		}, "audio_ref", "model", "operation_id"),
		Run: func(a *API, user string, raw json.RawMessage) (any, *apiError) {
			var args struct {
				AudioRef    string `json:"audio_ref"`
				Model       string `json:"model"`
				Language    string `json:"language"`
				OperationID string `json:"operation_id"`
			}
			if e := decodeArgs(raw, &args); e != nil {
				return nil, e
			}
			if args.Language == "" {
				args.Language = "auto"
			}
			if !idemKeyRe.MatchString(args.OperationID) {
				return nil, &apiError{400, "idempotency_key_required"}
			}
			if !audioRefRe.MatchString(args.AudioRef) {
				return nil, &apiError{404, "audio_ref_not_found"}
			}
			if code := a.checkModel(args.Model, args.Language); code != "" {
				return nil, &apiError{400, code}
			}
			j, created, e := a.submitStaged(user, args.OperationID, args.AudioRef, args.Model, args.Language)
			if e != nil {
				return nil, e
			}
			return map[string]any{"created": created, "job": j.view(0)}, nil
		},
	},
	jobTool("get_transcription", "Transcription status", "A job's status and progress.", true,
		func(a *API, user, id string) (any, *apiError) {
			j, pos, err := a.store.Get(user, id)
			if err != nil {
				return nil, &apiError{404, "not_found"}
			}
			return j.view(pos), nil
		}),
	jobTool("get_transcription_result", "Transcription result",
		"Segments with audio_ms timestamps for a succeeded job. Fails with not_ready before that.", true,
		func(a *API, user, id string) (any, *apiError) {
			j, _, err := a.store.Get(user, id)
			if err != nil {
				return nil, &apiError{404, "not_found"}
			}
			if j.Status != stSucceeded {
				return nil, &apiError{409, "not_ready"}
			}
			data, err := os.ReadFile(a.store.resultPath(j.ID))
			if err != nil {
				return nil, &apiError{500, "storage_failed"}
			}
			var result map[string]any
			if json.Unmarshal(data, &result) != nil {
				return nil, &apiError{500, "storage_failed"}
			}
			return map[string]any{"job_id": j.ID, "result": result}, nil
		}),
	jobTool("cancel_transcription", "Cancel transcription",
		"Requests cancellation. Check the returned status: a running job may still finish.", false,
		func(a *API, user, id string) (any, *apiError) {
			j, err := a.store.Cancel(user, id)
			if err != nil {
				return nil, &apiError{404, "not_found"}
			}
			return j.view(0), nil
		}),
}

func rpcReply(w http.ResponseWriter, status int, id json.RawMessage, result any, rpcErr map[string]any) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if len(id) == 0 {
		body["id"] = nil
	}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	writeJSON(w, status, body)
}

func rpcError(code int, msg string) map[string]any {
	return map[string]any{"code": code, "message": msg}
}

func toolResult(payload any, isError bool) map[string]any {
	text, _ := json.Marshal(payload)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": payload,
		"isError":           isError,
	}
}

func toolError(e *apiError) map[string]any {
	return toolResult(map[string]any{"error": map[string]any{
		"code": e.Code, "status": e.Status, "retryable": e.Status >= 500,
	}}, true)
}

// originAllowed is the DNS-rebinding guard: browsers may only call from our
// own origin; native clients send no Origin.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	_, host, _ := strings.Cut(o, "://")
	return strings.TrimSuffix(host, "/") == r.Host
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (a *API) mcp(w http.ResponseWriter, r *http.Request, user string) {
	if !originAllowed(r) {
		fail(w, 403, "origin_not_allowed")
		return
	}
	if acc := r.Header.Get("Accept"); acc != "" && !strings.Contains(acc, "application/json") && !strings.Contains(acc, "*/*") {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcpVersions, v) {
		rpcReply(w, 400, nil, nil, rpcError(rpcInvalidRequest, "Unsupported protocol version."))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		rpcReply(w, 413, nil, nil, rpcError(rpcInvalidRequest, "Message too large."))
		return
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] != '{' {
		rpcReply(w, 400, nil, nil, rpcError(rpcInvalidRequest, "Expected one JSON-RPC 2.0 message."))
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		rpcReply(w, 400, nil, nil, rpcError(rpcParseError, "Parse error."))
		return
	}
	if msg.JSONRPC != "2.0" {
		rpcReply(w, 400, nil, nil, rpcError(rpcInvalidRequest, "Expected one JSON-RPC 2.0 message."))
		return
	}
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		// Notifications and client responses: nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		v := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		rpcReply(w, 200, msg.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "openplayx-asr", "title": "OpenPlayX ASR", "version": version},
			"instructions":    mcpInstructions,
		}, nil)
	case "ping":
		rpcReply(w, 200, msg.ID, map[string]any{}, nil)
	case "tools/list":
		tools := make([]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, t.describe())
		}
		rpcReply(w, 200, msg.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Name == "" {
			rpcReply(w, 200, msg.ID, nil, rpcError(rpcInvalidParams, "params.name is required."))
			return
		}
		i := slices.IndexFunc(mcpTools, func(t mcpTool) bool { return t.Name == p.Name })
		if i < 0 {
			rpcReply(w, 200, msg.ID, nil, rpcError(rpcInvalidParams, "Unknown tool."))
			return
		}
		v, e := mcpTools[i].Run(a, user, p.Arguments)
		if e != nil {
			rpcReply(w, 200, msg.ID, toolError(e), nil)
			return
		}
		rpcReply(w, 200, msg.ID, toolResult(v, false), nil)
	default:
		rpcReply(w, 200, msg.ID, nil, rpcError(rpcMethodNotFound, "Method not found."))
	}
}
