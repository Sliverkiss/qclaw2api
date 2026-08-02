package upstream

import (
	"encoding/json"
	"testing"
)

// TestCleanBodyWhitelist 表驱动：白名单字段保留，非标字段丢弃。
func TestCleanBodyWhitelist(t *testing.T) {
	raw := []byte(`{
  "model": "pool-deepseek-v4-flash",
  "messages": [{"role":"system","content":"hi"}],
  "max_tokens": 100,
  "max_completion_tokens": 200,
  "stream": true,
  "temperature": 0.7,
  "top_p": 0.9,
  "stop": ["END"],
  "tools": [],
  "tool_choice": "auto",
  "frequency_penalty": 0.1,
  "presence_penalty": 0.2,
  "n": 1,
  "user": "u1",
  "seed": 42,
  "logprobs": true,
  "top_logprobs": 2,
  "response_format": {"type":"text"},
  "logit_bias": {"1": 2},
  "cache_control": {"ttl": 3600},
  "bogus_field": "should drop",
  "extra_nested": {"x": 1}
}`)
	out, err := CleanBody(raw)
	if err != nil {
		t.Fatalf("CleanBody: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal cleaned: %v", err)
	}
	for _, k := range QClawAllowedKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("allowed key %q was dropped", k)
		}
	}
	if _, ok := m["bogus_field"]; ok {
		t.Errorf("bogus_field should be dropped")
	}
	if _, ok := m["extra_nested"]; ok {
		t.Errorf("extra_nested should be dropped")
	}
	if len(m) != len(QClawAllowedKeys) {
		t.Errorf("cleaned has %d keys, want %d", len(m), len(QClawAllowedKeys))
	}
}

// TestCleanBodyAllDropped 全非白名单 → 空对象。
func TestCleanBodyAllDropped(t *testing.T) {
	out, err := CleanBody([]byte(`{"foo":1,"bar":"x"}`))
	if err != nil {
		t.Fatalf("CleanBody: %v", err)
	}
	if string(out) != "{}" {
		t.Errorf("cleaned = %s, want {}", out)
	}
}

// TestCleanBodyEmpty 空对象保留。
func TestCleanBodyEmpty(t *testing.T) {
	out, err := CleanBody([]byte(`{}`))
	if err != nil {
		t.Fatalf("CleanBody: %v", err)
	}
	if string(out) != "{}" {
		t.Errorf("cleaned = %s, want {}", out)
	}
}

// TestCleanBodyInvalidJSON 非法 JSON 报错。
func TestCleanBodyInvalidJSON(t *testing.T) {
	if _, err := CleanBody([]byte(`not-json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// TestPrepareBodyAddsSystem 校验缺 system 时前插。
func TestPrepareBodyAddsSystem(t *testing.T) {
	out, err := PrepareBody([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("first role = %v, want system", first["role"])
	}
	if len(msgs) != 2 {
		t.Errorf("messages len = %d, want 2", len(msgs))
	}
}

// TestPrepareBodyKeepsExistingSystem 已有 system 不重复插入。
func TestPrepareBodyKeepsExistingSystem(t *testing.T) {
	out, err := PrepareBody([]byte(`{"model":"m","messages":[
		{"role":"system","content":"定制"},{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Errorf("messages len = %d, want 2 (no dup system)", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "定制" {
		t.Errorf("first content = %v, want 定制", first["content"])
	}
}

// TestPrepareBodyDropsNonWhitelist 校验清洗在注入前执行。
func TestPrepareBodyDropsNonWhitelist(t *testing.T) {
	out, err := PrepareBody([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"bogus":1}`))
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["bogus"]; ok {
		t.Errorf("bogus should be dropped")
	}
	if _, ok := m["messages"]; !ok {
		t.Errorf("messages missing")
	}
}
