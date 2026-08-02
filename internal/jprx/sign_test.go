package jprx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestCanonicalOrder 校验 canonical 按键字典序拼接，timestamp 加入排序。
func TestCanonicalOrder(t *testing.T) {
	// body keys: model, messages, stream + timestamp
	// 排序后: messages, model, stream, timestamp
	body := []byte(`{"model":"pool-deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	ts := "1722500000000"
	got, err := Canonical(body, ts)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	want := `messages=[{"role":"user","content":"hi"}]&model=pool-deepseek-v4-flash&stream=true&timestamp=1722500000000`
	if got != want {
		t.Errorf("Canonical = %q\nwant %q", got, want)
	}
}

// TestCanonicalEmptyBody 校验空 body 时 canonical 仅含 timestamp。
func TestCanonicalEmptyBody(t *testing.T) {
	got, err := Canonical(nil, "123")
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if got != "timestamp=123" {
		t.Errorf("Canonical = %q, want timestamp=123", got)
	}
}

// TestCanonicalNested 校验嵌套值用紧凑 JSON，字符串按原值。
func TestCanonicalNested(t *testing.T) {
	body := []byte(`{"a":{"b":1,"c":"x"},"s":"y"}`)
	got, err := Canonical(body, "1")
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	want := `a={"b":1,"c":"x"}&s=y&timestamp=1`
	if got != want {
		t.Errorf("Canonical = %q\nwant %q", got, want)
	}
}

// TestSignDeterministic 校验同一 body + timestamp 签名一致，且为 64 hex。
func TestSignDeterministic(t *testing.T) {
	body := []byte(`{"guid":"qclawmp_test"}`)
	ts := "1722500000000"
	s1, c1, err := Sign(body, ts)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	s2, c2, err := Sign(body, ts)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if s1 != s2 {
		t.Errorf("signature not deterministic: %s vs %s", s1, s2)
	}
	if c1 != c2 {
		t.Errorf("canonical not deterministic: %s vs %s", c1, c2)
	}
	if len(s1) != 64 {
		t.Errorf("signature len = %d, want 64 (hex HMAC-SHA256)", len(s1))
	}
	if _, err := hex.DecodeString(s1); err != nil {
		t.Errorf("signature not hex: %v", err)
	}
}

// TestSignMatchesManual 用 SPEC §1.1 算法手工构造一次 HMAC 对比。
func TestSignMatchesManual(t *testing.T) {
	body := []byte(`{"guid":"qclawmp_test","x":"1"}`)
	ts := "1722500000000"
	// canonical = guid=qclawmp_test&timestamp=...&x=1
	canonical, err := Canonical(body, ts)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "guid=qclawmp_test&timestamp=1722500000000&x=1" {
		t.Fatalf("canonical = %q", canonical)
	}
	sig, _, err := Sign(body, ts)
	if err != nil {
		t.Fatal(err)
	}
	// 与标准库独立计算比对（等价于算法一致）
	want := hmacSHA256Hex(Secret, canonical)
	if sig != want {
		t.Errorf("signature = %q, want %q", sig, want)
	}
}

// TestCanonicalNonStringValue 校验数字/布尔字面量拼接。
func TestCanonicalNonStringValue(t *testing.T) {
	body := []byte(`{"n":42,"b":false,"f":3.14,"nil":null}`)
	got, err := Canonical(body, "9")
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	want := `b=false&f=3.14&n=42&nil=null&timestamp=9`
	if got != want {
		t.Errorf("Canonical = %q\nwant %q", got, want)
	}
}

// hmacSHA256Hex 独立实现，用于与 Sign 交叉验证。
func hmacSHA256Hex(secret, data string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}
