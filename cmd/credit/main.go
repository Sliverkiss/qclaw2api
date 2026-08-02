// credit.go — QClaw 积分查询（全部账号 + 总计）：4110 Q 点 + 4075 今日额度。
//
// 用法:
//
//	./credit              # JSON 输出到 stdout
//	./credit -pretty      # 人类可读日报
//
// 输出结构:
//
//	{"service":"qclaw","ts":N,
//	 "total":{"q_points":N,"daily_token_limit":N,"daily_token_used":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"user_id","nickname","q_points","daily_token_limit","daily_token_used","ok","error?"}]}
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"qclaw2api/internal/auth"
	"qclaw2api/internal/jprx"
)

type accountResult struct {
	UserID          string  `json:"user_id"`
	Nickname        string  `json:"nickname"`
	QPoints         *float64 `json:"q_points"`
	DailyTokenLimit *int64  `json:"daily_token_limit"`
	DailyTokenUsed  *int64  `json:"daily_token_used"`
	OK              bool   `json:"ok"`
	Error           string `json:"error,omitempty"`
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("QC2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	files, _ := filepath.Glob(filepath.Join(authDir, "qclaw-*.json"))
	sort.Strings(files)

	jc := jprx.New()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		res := accountResult{UserID: a.UserID, Nickname: a.Nickname}
		if a.JWTToken == "" {
			res.Error = "no jwt_token"
			accounts = append(accounts, res)
			continue
		}
		// 4110 Q 点
		qb, err := jc.GetQBalance(ctx, a)
		if err != nil {
			res.Error = "4110: " + err.Error()
			accounts = append(accounts, res)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		q := qb.Balance
		res.QPoints = &q
		// 4075 今日额度
		if tt, err := jc.GetTodayTokens(ctx, a); err == nil {
			dl := tt.DailyTokenLimit
			du := tt.DailyTokenUsed
			res.DailyTokenLimit = &dl
			res.DailyTokenUsed = &du
		}
		res.OK = true
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}

	var totalQ float64
	var totalLimit, totalUsed int64
	okCount := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.QPoints != nil {
				totalQ += *a.QPoints
			}
			if a.DailyTokenLimit != nil {
				totalLimit += *a.DailyTokenLimit
			}
			if a.DailyTokenUsed != nil {
				totalUsed += *a.DailyTokenUsed
			}
		}
	}
	out := map[string]any{
		"service": "qclaw",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"q_points":          totalQ,
			"daily_token_limit": totalLimit,
			"daily_token_used":  totalUsed,
			"accounts":          len(accounts),
			"ok":                okCount,
			"failed":            len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	if pretty {
		printPretty(accounts, totalQ, okCount)
		return
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty 人类可读日报。
func printPretty(accounts []accountResult, totalQ float64, okCount int) {
	fmt.Printf("📊 QClaw 积分日报\n")
	fmt.Printf("账号: %d/%d\n", okCount, len(accounts))
	fmt.Printf("Q 点总计: %.2f\n", totalQ)
	for _, a := range accounts {
		if a.OK {
			q := float64(0)
			if a.QPoints != nil {
				q = *a.QPoints
			}
			used := int64(0)
			if a.DailyTokenUsed != nil {
				used = *a.DailyTokenUsed
			}
			name := a.Nickname
			if name == "" {
				name = a.UserID
			}
			fmt.Printf("✅ %s (uid=%s): Q点=%.2f 今日已用=%d\n", name, a.UserID, q, used)
		} else {
			fmt.Printf("⚠️  %s: %s\n", a.UserID, a.Error)
		}
	}
}
