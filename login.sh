#!/usr/bin/env bash
# login.sh — QClaw 微信扫码登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh
#
# 流程（SPEC §3.2）:
#   1. ./login url  → 4050 拿 state → 打印微信授权 URL
#   2. 你在浏览器/手机打开 URL 扫码完成登录
#   3. 浏览器会回调到 https://security.guanjia.qq.com/login?code=...&state=...
#   4. 把完整回调 URL（或 code 本身）粘贴回终端
#   5. ./login code <arg> → 4026 登录 → is_new_user 判定 → 4055 生成 sk-apiKey
#      → 4110 积分 → stdout 完整 auth JSON
#   6. python3 解析落盘 auths/qclaw-<user_id>.json → 打印积分 → docker restart
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="qclaw2api"
LOGIN_BIN="./login"

mkdir -p "$AUTH_DIR"

# login 工具：不存在才编译（源码改动后手动 go build -o login ./cmd/login）
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  QClaw 微信扫码登录"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url)

echo "请复制以下链接在浏览器或手机中打开，完成微信扫码："
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "扫码完成后，请把浏览器回调地址（含 code= 的完整 URL 或 code 本身）粘贴到这里: " INPUT
if [[ -z "$INPUT" ]]; then
    echo "未输入内容，已取消"
    exit 1
fi

echo ""
echo "正在完成登录..."

# 若 exit code = 2 → is_new_user（新账号需转正）
RESULT=$("$LOGIN_BIN" code "$INPUT")
RC=$?
if [[ $RC -eq 2 ]]; then
    echo ""
    echo "新账号需再登录一次转正：请重新运行 ./login.sh 再扫一次（第二次返回 is_new_user=false）"
    exit 2
fi
if [[ $RC -ne 0 ]]; then
    echo ""
    echo "登录失败。可能原因："
    echo "  - code 已过期（重新运行 ./login.sh 获取新链接）"
    echo "  - state 不匹配（重新运行 ./login.sh）"
    exit 1
fi

# 解析完整 auth JSON（与 internal/auth 读取格式一致）
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['account']['user_id'])")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['account'].get('nickname',''))")
Q_BALANCE=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('q_balance','?'))")

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 user_id，登录结果异常"
    exit 1
fi

# ─── 落盘 auth 文件（嵌套形，与 internal/auth.Parse 一致）─────────────────
AUTH_FILE="$AUTH_DIR/qclaw-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（user_id=$USER_ID），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（user_id=$USER_ID），新增 auth 文件"
    ACTION="新增"
fi
echo "$RESULT" | python3 -c "
import json, sys
auth = json.load(sys.stdin)
# 去掉 q_balance（运行时展示字段，不属于 auth 文件格式）
auth.pop('q_balance', None)
with open('$AUTH_FILE', 'w') as f:
    json.dump(auth, f, indent=2)
" || {
    echo "落盘 auth 文件失败"
    exit 1
}
chmod 600 "$AUTH_FILE"
# 容器内 app 用户(uid 10001) 需可写 auth 文件（refresh 落盘 X-New-Token）
chown 10001:10001 "$AUTH_FILE" 2>/dev/null || true
echo "已保存（$ACTION）: $AUTH_FILE"

# ─── 打印积分摘要 ────────────────────────────────────────────
echo ""
echo "Q 点余额: ${Q_BALANCE:-?}"

# ─── 重启服务 ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    COUNT=$(curl -s http://127.0.0.1:7865/status -H "Authorization: Bearer ${API_KEY:-change-me}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  UserID: $USER_ID"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Q 点: ${Q_BALANCE:-?}"
echo "============================================================"
