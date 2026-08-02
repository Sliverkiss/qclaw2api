// Package jprx 封装 QClaw jprx 网关：HMAC-SHA256 签名、多级信封解析、
// X-New-Token 捕获与各业务 cmd。
package jprx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// SPEC §1.1 固定常量。
const (
	BaseURL       = "https://jprx.m.qq.com"
	Secret        = "2fc7c82b2cdc2a6083239d343843adf314b571dd0ee036163b61fb209be47492"
	ClientVersion = "1.4.0"
)

// Timestamp 返回当前毫秒时间戳字符串（参与 canonical 与 X-Sign-Timestamp）。
func Timestamp() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// Canonical 按 SPEC §1.1 构造签名串：
// 取 body 所有 key + "timestamp"，按 key 字典序排序，拼接为 k=v&k=v&...
// 嵌套值用原始 JSON（Compact 保序），字符串值去掉引号。
// timestamp 为毫秒字符串。
func Canonical(body []byte, timestamp string) (string, error) {
	var m map[string]json.RawMessage
	if len(body) > 0 {
		if err := json.Unmarshal(body, &m); err != nil {
			return "", fmt.Errorf("canonical: parse body: %w", err)
		}
	}
	keys := make([]string, 0, len(m)+1)
	for k := range m {
		keys = append(keys, k)
	}
	keys = append(keys, "timestamp")
	sort.Strings(keys)

	var sb []byte
	for i, k := range keys {
		if i > 0 {
			sb = append(sb, '&')
		}
		sb = append(sb, k...)
		sb = append(sb, '=')
		if k == "timestamp" {
			sb = append(sb, timestamp...)
			continue
		}
		sb = append(sb, rawValueString(m[k])...)
	}
	return string(sb), nil
}

// rawValueString 将 json.RawMessage 转为 canonical 拼接用的字符串：
// JSON 字符串去引号，数字/布尔/null 保留字面量，嵌套结构 Compact 保序。
func rawValueString(raw json.RawMessage) string {
	var comp bytes.Buffer
	if err := json.Compact(&comp, raw); err != nil {
		return string(raw)
	}
	v := comp.String()
	if len(v) >= 2 && v[0] == '"' {
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
	}
	return v
}

// Sign 计算 HMAC-SHA256(Secret, canonical) 的 hex 摘要。
func Sign(body []byte, timestamp string) (signature, canonical string, err error) {
	canonical, err = Canonical(body, timestamp)
	if err != nil {
		return "", "", err
	}
	mac := hmac.New(sha256.New, []byte(Secret))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil)), canonical, nil
}
