// login.go — QClaw 微信扫码 OAuth 登录（jprx 4050 → 授权 URL → 4026 → 4055 → 4320）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login url        → POST 4050 拿 state，拼微信授权 URL，
//	                   {state,guid} 落 /tmp/qclaw-login-state.json，stdout 打印 URL
//	login code <arg> → 读 state，解析 arg（含 code= 提取，否则整体当 code），
//	                   POST 4026 → is_new_user 分支(exit 2) → 4055 sk-apiKey
//	                   → 4320 模型列表 → 写 ./data/models.json → stdout 打印完整 auth JSON（嵌套形）
//
// 注意：本程序不落盘 auths/（由 login.sh 用 python3 解析 stdout 后落盘）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
)

const (
	stateFile = "/tmp/qclaw-login-state.json"
	// 微信授权 URL（SPEC §1.2）
	wxAuthURL  = "https://open.weixin.qq.com/connect/qrconnect?appid=%s&redirect_uri=%s&response_type=code&scope=snsapi_login&state=%s#wechat_redirect"
	wxAppID    = "wx9d11056dd75b7240"
	wxRedirect = "https%3A%2F%2Fsecurity.guanjia.qq.com%2Flogin"
	// modelsFile 模型列表落盘路径（SPEC §2.6），env QC2A_MODELS_FILE 可覆盖。
	modelsFile = "./data/models.json"
)

// modelsFilePath 读 QC2A_MODELS_FILE env，缺省 ./data/models.json。
func modelsFilePath() string {
	if v := os.Getenv("QC2A_MODELS_FILE"); v != "" {
		return v
	}
	return modelsFile
}

// writeModelsFile 把 4320 模型列表包装成 OpenAI /v1/models 形状，原子写（tmp+rename）0644。
// 失败返回错误（调用方仅警告，不阻塞登录）。
func writeModelsFile(path string, ms *jprx.ModelStatus) error {
	items := make([]map[string]any, 0, len(ms.ModelStatusList))
	for _, m := range ms.ModelStatusList {
		if m.ID == "" {
			continue
		}
		items = append(items, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "qclaw",
		})
	}
	if len(items) == 0 {
		return fmt.Errorf("4320: empty model list, skip write")
	}
	doc := map[string]any{
		"object":     "list",
		"updated_at": time.Now().Unix(),
		"data":       items,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loginState 落盘状态。
type loginState struct {
	State string `json:"state"`
	GUID  string `json:"guid"`
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

// newGUID 生成 qclawmp_ + UUIDv4（无横线）。
func newGUID() string {
	u, err := uuidv4()
	if err != nil {
		fatal("uuid: %v", err)
	}
	return "qclawmp_" + strings.ReplaceAll(u, "-", "")
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|code> [arg]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c := jprx.New()
	// 仅登录链路开启 X-New-Token 捕获（4055/4320 若返回新 JWT 会覆盖内存值并落盘）
	c.SetCaptureNewToken(true)

	switch os.Args[1] {
	case "url":
		guid := newGUID()
		state, err := c.WxLoginState(ctx, guid)
		if err != nil {
			fatal("4050 获取登录 state 失败: %v", err)
		}
		raw, _ := json.Marshal(loginState{State: state, GUID: guid})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("写 state 文件失败: %v", err)
		}
		// 组装微信授权 URL
		authURL := fmt.Sprintf(wxAuthURL, wxAppID, wxRedirect, url.QueryEscape(state))
		fmt.Println(authURL)

	case "code":
		if len(os.Args) < 3 {
			fatal("usage: login code <回调 URL 或 code>")
		}
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			fatal("读 state 文件失败: %v（先运行 ./login url）", err)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("解析 state 失败: %v", err)
		}
		code := extractCode(os.Args[2])
		if code == "" {
			fatal("无法从参数中提取 code: %q", os.Args[2])
		}

		res, err := c.WxLogin(ctx, ls.GUID, code, ls.State)
		if err != nil {
			fatal("4026 登录失败: %v", err)
		}
		if res.IsNewUser {
			// SPEC §1.2：is_new_user=true → 提示转正，exit 2，不落盘
			fmt.Fprintln(os.Stderr, "新账号需再登录一次转正，请重新运行 ./login.sh（第二次扫码返回 is_new_user=false）")
			os.Exit(2)
		}

		// 组装 Auth 并取 sk-apiKey（4055）
		a := &auth.Auth{
			JWTToken:     res.JWTToken,
			ChannelToken: res.ChannelToken,
			UserID:       res.UserID,
			Nickname:     res.Nickname,
			GUID:         ls.GUID,
			ExpiresAt:    time.Now().Add(time.Duration(res.ExpiresIn) * time.Second).Unix(),
		}
		key, err := c.GetAPIKey(ctx, a)
		if err != nil {
			// 部分新账号 403 api_key_inactive（账号态问题）→ 警告仍落盘（SPEC §1.3）
			fmt.Fprintf(os.Stderr, "警告: 4055 获取 sk-apiKey 失败: %v（该账号可能未激活，可先用客户端激活一次）\n", err)
		} else {
			a.SKAPIKey = key.Key
		}

		// 4320 模型列表 → 写 ./data/models.json（失败仅警告，保留旧文件；SPEC §2.6）
		if ms, err := c.GetModelStatus(ctx, a); err == nil {
			if werr := writeModelsFile(modelsFilePath(), ms); werr != nil {
				fmt.Fprintf(os.Stderr, "警告: 写模型文件失败: %v（运行期回退内置静态表）\n", werr)
			}
		} else {
			fmt.Fprintf(os.Stderr, "警告: 4320 获取模型列表失败: %v（运行期回退内置静态表）\n", err)
		}

		out := map[string]any{
			"auth": map[string]any{
				"jwt_token":              a.JWTToken,
				"openclaw_channel_token": a.ChannelToken,
				"sk_api_key":             a.SKAPIKey,
				"expires_at":             a.ExpiresAt,
				"guid":                   a.GUID,
			},
			"account": map[string]any{
				"user_id":  a.UserID,
				"nickname": a.Nickname,
			},
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q (want url|code)", os.Args[1])
	}
}

// extractCode 从回调 URL 或裸 code 中提取 code。
func extractCode(arg string) string {
	if i := strings.Index(arg, "code="); i >= 0 {
		rest := arg[i+len("code="):]
		if j := strings.IndexAny(rest, "& "); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return strings.TrimSpace(arg)
}
