package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func (e *env) rpc(token, method string, params any, headers ...string) (int, map[string]any) {
	e.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		msg["params"] = params
	}
	body, _ := json.Marshal(msg)
	h := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
	for i := 0; i+1 < len(headers); i += 2 {
		h[headers[i]] = headers[i+1]
	}
	return e.do("POST", "/mcp", token, body, h)
}

// tool returns (isError, structuredContent).
func (e *env) tool(token, name string, args map[string]any) (bool, map[string]any) {
	e.t.Helper()
	s, out := e.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := out["result"].(map[string]any)
	if s != 200 || !ok {
		e.t.Fatalf("%s: %d %v", name, s, out)
	}
	sc, _ := res["structuredContent"].(map[string]any)
	return res["isError"] == true, sc
}

func (e *env) stage(token string, audio []byte) (int, map[string]any) {
	return e.do("POST", "/v1/audio-uploads", token, audio,
		map[string]string{"X-Content-SHA256": sha256Hex(audio), "Content-Type": "audio/wav"})
}

func toolCode(sc map[string]any) string {
	if e, ok := sc["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func TestMCPHandshakeAndGuards(t *testing.T) {
	e := newEnv(t, t.TempDir(), Limits{})
	s, out := e.rpc("alice-token", "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	res := out["result"].(map[string]any)
	if s != 200 || res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "openplayx-asr" {
		t.Fatalf("initialize %d %v", s, out)
	}
	_, out = e.rpc("alice-token", "tools/list", nil)
	names := map[string]bool{}
	for _, raw := range out["result"].(map[string]any)["tools"].([]any) {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"list_models", "create_transcription", "get_transcription", "get_transcription_result", "cancel_transcription"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}
	if s, _ := e.rpc("", "ping", nil); s != 401 {
		t.Fatalf("no token: %d", s)
	}
	if s, _ := e.rpc("alice-token", "ping", nil, "Origin", "https://evil.example"); s != 403 {
		t.Fatalf("origin: %d", s)
	}
	if s, _ := e.rpc("alice-token", "ping", nil, "MCP-Protocol-Version", "1999-01-01"); s != 400 {
		t.Fatalf("version: %d", s)
	}
	if s, _ := e.do("GET", "/mcp", "alice-token", nil, nil); s != 405 {
		t.Fatalf("GET: %d", s)
	}
	notify, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if s, _ := e.do("POST", "/mcp", "alice-token", notify, map[string]string{"Content-Type": "application/json"}); s != 202 {
		t.Fatalf("notification: %d", s)
	}
	if _, out := e.rpc("alice-token", "resources/list", nil); out["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatal(out)
	}
	if isErr, sc := e.tool("alice-token", "list_models", nil); isErr || len(sc["models"].([]any)) != 1 {
		t.Fatalf("models %v", sc)
	}
}

func TestMCPTranscriptionFromStagedAudio(t *testing.T) {
	e := newEnv(t, t.TempDir(), Limits{})
	audio := wav(1000, 16000, 1)
	s, up := e.stage("alice-token", audio)
	if s != 201 || up["audio_ms"].(float64) != 1000 {
		t.Fatalf("stage %d %v", s, up)
	}
	ref := up["audio_ref"].(string)
	// References are per user.
	if isErr, sc := e.tool("bob-token", "create_transcription", map[string]any{
		"audio_ref": ref, "model": "whisper-base-multilingual", "operation_id": "op-bob-0123456789abcd",
	}); !isErr || toolCode(sc) != "audio_ref_not_found" {
		t.Fatalf("other user: %v", sc)
	}
	args := map[string]any{"audio_ref": ref, "model": "whisper-base-multilingual", "language": "en", "operation_id": "op-0123456789abcdef"}
	isErr, sc := e.tool("alice-token", "create_transcription", args)
	if isErr || sc["created"] != true {
		t.Fatalf("create %v", sc)
	}
	id := sc["job"].(map[string]any)["id"].(string)
	// Retrying the operation returns the same job even though the ref is consumed.
	if _, again := e.tool("alice-token", "create_transcription", args); again["created"] != false || again["job"].(map[string]any)["id"] != id {
		t.Fatalf("retry %v", again)
	}
	if _, sc := e.tool("alice-token", "create_transcription", map[string]any{
		"audio_ref": ref, "model": "whisper-base-multilingual", "operation_id": "op-new-0123456789abcd",
	}); toolCode(sc) != "audio_ref_not_found" {
		t.Fatalf("consumed ref: %v", sc)
	}
	if _, sc := e.tool("alice-token", "get_transcription_result", map[string]any{"job_id": id}); toolCode(sc) != "not_ready" {
		t.Fatalf("early result: %v", sc)
	}
	close(e.model.release)
	e.waitStatus("alice-token", id, stSucceeded)
	if isErr, sc := e.tool("alice-token", "get_transcription", map[string]any{"job_id": id}); isErr || sc["status"] != stSucceeded {
		t.Fatalf("status %v", sc)
	}
	isErr, sc = e.tool("alice-token", "get_transcription_result", map[string]any{"job_id": id})
	if isErr || sc["result"].(map[string]any)["language"] != "en" {
		t.Fatalf("result %v", sc)
	}
	// Same job store as the HTTP API.
	if s, j := e.do("GET", "/v1/transcription-jobs/"+id, "alice-token", nil, nil); s != 200 || j["status"] != stSucceeded {
		t.Fatalf("http view %d %v", s, j)
	}
	if _, sc := e.tool("bob-token", "get_transcription", map[string]any{"job_id": id}); toolCode(sc) != "not_found" {
		t.Fatalf("other user job: %v", sc)
	}
}

func TestMCPToolErrorsAndStagingLimits(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir, Limits{})
	if _, sc := e.tool("alice-token", "create_transcription", map[string]any{
		"audio_ref": "aud_" + "00000000000000000000000000000000", "model": "nope", "operation_id": "op-0123456789abcdef",
	}); toolCode(sc) != "model_unknown" {
		t.Fatalf("model: %v", sc)
	}
	if _, sc := e.tool("alice-token", "create_transcription", map[string]any{"audio_ref": "/etc/passwd", "model": "whisper-base-multilingual", "operation_id": "op-0123456789abcdef"}); toolCode(sc) != "audio_ref_not_found" {
		t.Fatalf("path as ref: %v", sc)
	}
	if _, sc := e.tool("alice-token", "list_models", map[string]any{"extra": true}); toolCode(sc) != "invalid_request" {
		t.Fatalf("unknown field: %v", sc)
	}
	if s, out := e.do("POST", "/v1/audio-uploads", "alice-token", []byte("not wav"), map[string]string{"X-Content-SHA256": sha256Hex([]byte("not wav"))}); s != 415 || errCode(out) != "audio_format_unsupported" {
		t.Fatalf("bad audio %d %v", s, out)
	}
	audio := wav(100, 16000, 1)
	for i := 0; i < stagedPerUser; i++ {
		if s, _ := e.stage("alice-token", audio); s != 201 {
			t.Fatalf("stage %d: %d", i, s)
		}
	}
	if s, out := e.stage("alice-token", audio); s != 429 || errCode(out) != "too_many_staged_uploads" {
		t.Fatalf("limit %d %v", s, out)
	}
	if s, _ := e.stage("bob-token", audio); s != 201 {
		t.Fatal("limit is per user")
	}
}

func TestStagedAudioExpires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.wav")
	_ = os.WriteFile(path, []byte("x"), 0o600)
	now := time.Now()
	s := newStagedUploads()
	s.now = func() time.Time { return now }
	ref, err := s.put(&stagedAudio{User: "alice", Path: path, SHA256: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.get("alice", ref); !ok {
		t.Fatal("fresh ref")
	}
	now = now.Add(stagedTTL)
	if _, ok := s.get("alice", ref); ok {
		t.Fatal("expired ref still usable")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired audio not deleted")
	}
}
