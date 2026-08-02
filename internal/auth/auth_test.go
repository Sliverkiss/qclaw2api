package auth

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const (
	testJWT  = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.test"
	testSK   = "sk-test-0123456789abcdef0123456789abcdef"
	testGUID = "qclawmp_11111111-2222-3333-4444-555555555555"
)

func nestedDoc(jwt, sk string, expires int64) string {
	return `{
  "auth": {
    "jwt_token": "` + jwt + `",
    "openclaw_channel_token": "ct-test-1234",
    "sk_api_key": "` + sk + `",
    "expires_at": ` + itoa(expires) + `,
    "guid": "` + testGUID + `"
  },
  "account": {
    "user_id": "123456789",
    "nickname": "测试用户"
  }
}`
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestParseNested 校验嵌套形解析。
func TestParseNested(t *testing.T) {
	a, err := Parse([]byte(nestedDoc(testJWT, testSK, 1783000000)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.JWTToken != testJWT {
		t.Errorf("JWTToken = %q", a.JWTToken)
	}
	if a.SKAPIKey != testSK {
		t.Errorf("SKAPIKey = %q", a.SKAPIKey)
	}
	if a.ChannelToken != "ct-test-1234" {
		t.Errorf("ChannelToken = %q", a.ChannelToken)
	}
	if a.UserID != "123456789" {
		t.Errorf("UserID = %q", a.UserID)
	}
	if a.Nickname != "测试用户" {
		t.Errorf("Nickname = %q", a.Nickname)
	}
	if a.GUID != testGUID {
		t.Errorf("GUID = %q", a.GUID)
	}
	if a.ExpiresAt != 1783000000 {
		t.Errorf("ExpiresAt = %d", a.ExpiresAt)
	}
}

// TestParseFlat 校验扁平形解析。
func TestParseFlat(t *testing.T) {
	raw := `{
  "jwt_token": "` + testJWT + `",
  "openclaw_channel_token": "ct-test-5678",
  "sk_api_key": "` + testSK + `",
  "expires_at": 1783000000,
  "guid": "` + testGUID + `",
  "user_id": "123456789",
  "nickname": "扁平用户"
}`
	a, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.JWTToken != testJWT || a.SKAPIKey != testSK || a.UserID != "123456789" {
		t.Errorf("flat parse mismatch: %+v", a)
	}
}

// TestParseErrors 校验缺 JWT / 空 / 非法 JSON。
func TestParseErrors(t *testing.T) {
	cases := []string{
		"",
		`{"auth":{"sk_api_key":"sk-x"}}`, // 缺 jwt_token
		`{"account":{"user_id":"1"}}`,    // 缺 auth 且扁平也缺 jwt
		`{"auth":{"jwt_token":""}}`,      // jwt 空
		`not-json{{{`,                    // 非法 JSON
	}
	for i, c := range cases {
		if _, err := Parse([]byte(c)); err == nil {
			t.Errorf("case %d: expected error for %q", i, c)
		}
	}
}

// TestSaveAtomic 校验 tmp+rename 原子写回与 0600 权限。
func TestSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qclaw-123456789.json")
	a := &Auth{
		JWTToken:     testJWT,
		ChannelToken: "ct-test-9999",
		SKAPIKey:     testSK,
		UserID:       "123456789",
		Nickname:     "原子写",
		GUID:         testGUID,
		ExpiresAt:    1783000000,
		FilePath:     path,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	// 重新解析，确认内容一致
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got.JWTToken != testJWT || got.SKAPIKey != testSK || got.Nickname != "原子写" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	// 不应残留 .tmp
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp should not remain")
	}
}

// TestSaveAtomicNoPath 校验未设 FilePath 时报错。
func TestSaveAtomicNoPath(t *testing.T) {
	a := &Auth{JWTToken: testJWT}
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("expected error without FilePath")
	}
}

// TestLoadDir 校验 glob qclaw-*.json 与解析过滤。
func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	// 合法文件
	good := filepath.Join(dir, "qclaw-123456789.json")
	if err := os.WriteFile(good, []byte(nestedDoc(testJWT, testSK, 1783000000)), 0o600); err != nil {
		t.Fatal(err)
	}
	// 非法文件（缺 jwt）
	bad := filepath.Join(dir, "qclaw-bad.json")
	if err := os.WriteFile(bad, []byte(`{"auth":{"sk_api_key":"sk-x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 不匹配 glob 的文件
	other := filepath.Join(dir, "other.json")
	if err := os.WriteFile(other, []byte(`{"jwt_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadDir returned %d auths, want 1", len(got))
	}
	if got[0].UserID != "123456789" {
		t.Errorf("UserID = %q", got[0].UserID)
	}
	if got[0].FilePath != good {
		t.Errorf("FilePath = %q, want %q", got[0].FilePath, good)
	}
}

// TestLoadDirMissing 校验目录不存在时不报错（返回空）。
func TestLoadDirMissing(t *testing.T) {
	got, err := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

// TestSnapshotConcurrent 校验 JWTToken 并发写读无数据竞争（F4）。
// 模拟 captureNewToken 持锁写 vs SnapshotJWT 无锁读侧。
func TestSnapshotConcurrent(t *testing.T) {
	a := &Auth{
		JWTToken:  "jwt-a",
		ExpiresAt: 1783000000,
	}
	var wg sync.WaitGroup
	// 写侧：持锁覆盖 JWTToken
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.Lock()
			a.JWTToken = "jwt-new"
			a.ExpiresAt = 1783000000 + int64(i)
			a.Unlock()
		}
	}()
	// 读侧：快照读
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = a.SnapshotJWT()
			}
		}()
	}
	wg.Wait()
}
