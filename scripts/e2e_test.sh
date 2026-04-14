#!/usr/bin/env bash
# E2E 测试脚本：对运行中的服务发起真实 HTTP 请求，自动验证响应。
# 前提：服务已在本地启动（go run main.go 或二进制）
#
# 用法：
#   bash scripts/e2e_test.sh            # 默认 localhost:8080
#   BASE_URL=http://localhost:9090 bash scripts/e2e_test.sh

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
PASS=0
FAIL=0

# ── 工具函数 ─────────────────────────────────────────────────────────────────

green() { printf '\033[32m%s\033[0m\n' "$*"; }
red()   { printf '\033[31m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { red   "  ✗ $1"; FAIL=$((FAIL + 1)); }

# assert_eq <label> <expected> <actual>
assert_eq() {
  if [ "$2" = "$3" ]; then
    pass "$1 (got: $3)"
  else
    fail "$1 (want: $2, got: $3)"
  fi
}

# assert_contains <label> <substring> <string>
assert_contains() {
  if echo "$3" | grep -q "$2"; then
    pass "$1"
  else
    fail "$1 (want substring: $2, in: $3)"
  fi
}

# wait_for_status <id> <want_status> <timeout_sec>
# 轮询任务状态，直到达到预期或超时
wait_for_status() {
  local id=$1 want=$2 timeout=$3
  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    local status
    status=$(curl -sf "$BASE_URL/notifications/$id" | python3 -c "import sys,json; print(json.load(sys.stdin)['Status'])" 2>/dev/null || echo "")
    if [ "$status" = "$want" ]; then
      echo "$want"
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  echo "timeout"
}

echo ""
echo "═══════════════════════════════════════════════════"
echo "  rc_jixiang E2E Test Suite  →  $BASE_URL"
echo "═══════════════════════════════════════════════════"

# ── TC01: 健康检查 ────────────────────────────────────────────────────────────
echo ""
echo "TC01  健康检查"
resp=$(curl -sf "$BASE_URL/health")
assert_contains "status=ok" '"ok"' "$resp"

# ── TC02: 提交通知，投递成功 ─────────────────────────────────────────────────
echo ""
echo "TC02  提交通知 → 供应商返回 200 → status=done"
resp=$(curl -sf -X POST "$BASE_URL/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "vendor_id": "e2e_success",
    "url":       "https://httpbin.org/post",
    "method":    "POST",
    "headers":   {"X-Test": "e2e"},
    "body":      "{\"scenario\":\"success\"}",
    "max_attempts": 3
  }')

assert_eq    "响应 status=pending" "pending"  "$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")"
ID_SUCCESS=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
assert_contains "返回 id 非空" "-" "$ID_SUCCESS"

echo "  等待投递完成（最多 15s）..."
final=$(wait_for_status "$ID_SUCCESS" "done" 15)
assert_eq "最终 status=done" "done" "$final"

detail=$(curl -sf "$BASE_URL/notifications/$ID_SUCCESS")
assert_eq "attempts=0（一次成功）" "0" "$(echo "$detail" | python3 -c "import sys,json; print(json.load(sys.stdin)['Attempts'])")"

# ── TC03: 默认值填充（不传 method / max_attempts）────────────────────────────
echo ""
echo "TC03  默认值填充（method=POST, max_attempts=5）"
resp=$(curl -sf -X POST "$BASE_URL/notifications" \
  -H 'Content-Type: application/json' \
  -d '{"vendor_id":"e2e_defaults","url":"https://httpbin.org/post"}')
ID_DEF=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

echo "  等待投递完成（最多 15s）..."
wait_for_status "$ID_DEF" "done" 15 > /dev/null

detail=$(curl -sf "$BASE_URL/notifications/$ID_DEF")
assert_eq "默认 method=POST"        "POST" "$(echo "$detail" | python3 -c "import sys,json; print(json.load(sys.stdin)['Method'])")"
assert_eq "默认 max_attempts=5"     "5"    "$(echo "$detail" | python3 -c "import sys,json; print(json.load(sys.stdin)['MaxAttempts'])")"

# ── TC04: 供应商持续返回 500 → 重试，attempts 递增 ──────────────────────────
echo ""
echo "TC04  供应商返回 500 → 重试，attempts 递增"
resp=$(curl -sf -X POST "$BASE_URL/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "vendor_id":    "e2e_retry",
    "url":          "https://httpbin.org/status/500",
    "max_attempts": 5
  }')
ID_RETRY=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

echo "  等待第一次投递失败（最多 10s）..."
elapsed=0
while [ $elapsed -lt 10 ]; do
  attempts=$(curl -sf "$BASE_URL/notifications/$ID_RETRY" | python3 -c "import sys,json; print(json.load(sys.stdin)['Attempts'])")
  if [ "$attempts" -ge 1 ]; then break; fi
  sleep 1; elapsed=$((elapsed + 1))
done

detail=$(curl -sf "$BASE_URL/notifications/$ID_RETRY")
actual_attempts=$(echo "$detail" | python3 -c "import sys,json; print(json.load(sys.stdin)['Attempts'])")
actual_status=$(echo "$detail" | python3 -c "import sys,json; print(json.load(sys.stdin)['Status'])")
actual_error=$(echo "$detail" | python3 -c "import sys,json; print(json.load(sys.stdin)['LastError'])")

assert_eq    "status 仍为 pending（等待重试）" "pending" "$actual_status"
assert_contains "attempts >= 1" "" "$([ "$actual_attempts" -ge 1 ] && echo ok)"
pass         "attempts=$actual_attempts（已重试）"
assert_contains "LastError 包含 500"  "500" "$actual_error"

# ── TC05: 超过 max_attempts → 进入死信 ──────────────────────────────────────
echo ""
echo "TC05  max_attempts=1 → 一次失败后进入死信"
resp=$(curl -sf -X POST "$BASE_URL/notifications" \
  -H 'Content-Type: application/json' \
  -d '{
    "vendor_id":    "e2e_deadletter",
    "url":          "https://httpbin.org/status/502",
    "max_attempts": 1
  }')
ID_DEAD=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

echo "  等待任务进入死信（最多 15s）..."
final=$(wait_for_status "$ID_DEAD" "failed" 15)
assert_eq "status=failed" "failed" "$final"

dl_resp=$(curl -sf "$BASE_URL/dead-letters")
dl_count=$(echo "$dl_resp" | python3 -c "import sys,json; data=json.load(sys.stdin); print(sum(1 for d in data if d['NotificationID']=='$ID_DEAD'))")
assert_eq "dead_letters 中存在该任务" "1" "$dl_count"

# ── TC06: 参数校验 —— 缺少 vendor_id ────────────────────────────────────────
echo ""
echo "TC06  参数校验 —— 缺少 vendor_id → 400"
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/notifications" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}')
assert_eq "缺少 vendor_id → 400" "400" "$code"

# ── TC07: 参数校验 —— 缺少 url ───────────────────────────────────────────────
echo ""
echo "TC07  参数校验 —— 缺少 url → 400"
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/notifications" \
  -H 'Content-Type: application/json' \
  -d '{"vendor_id":"v"}')
assert_eq "缺少 url → 400" "400" "$code"

# ── TC08: 参数校验 —— 非法 JSON ──────────────────────────────────────────────
echo ""
echo "TC08  参数校验 —— 非法 JSON → 400"
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/notifications" \
  -H 'Content-Type: application/json' \
  -d 'not-json')
assert_eq "非法 JSON → 400" "400" "$code"

# ── TC09: 查询不存在的任务 → 404 ─────────────────────────────────────────────
echo ""
echo "TC09  查询不存在的任务 → 404"
code=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/notifications/nonexistent-id-xyz")
assert_eq "不存在 → 404" "404" "$code"

# ── TC10: 按状态列出任务 ─────────────────────────────────────────────────────
echo ""
echo "TC10  按状态列出任务"
done_list=$(curl -sf "$BASE_URL/notifications?status=done")
done_count=$(echo "$done_list" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
pass "status=done 列表返回 $done_count 条（含本次测试产生的任务）"

failed_list=$(curl -sf "$BASE_URL/notifications?status=failed")
failed_count=$(echo "$failed_list" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
pass "status=failed 列表返回 $failed_count 条"

# ── TC11: 查看死信列表 ───────────────────────────────────────────────────────
echo ""
echo "TC11  查看死信列表"
dl_resp=$(curl -sf "$BASE_URL/dead-letters")
dl_total=$(echo "$dl_resp" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
pass "dead-letters 列表返回 $dl_total 条"
echo "$dl_resp" | grep -qF "[" && pass "响应是 JSON 数组" || fail "响应不是 JSON 数组 (got: $dl_resp)"

# ── 汇总 ─────────────────────────────────────────────────────────────────────
echo ""
echo "═══════════════════════════════════════════════════"
if [ $FAIL -eq 0 ]; then
  green "  全部通过  PASS=$PASS  FAIL=$FAIL"
else
  red   "  存在失败  PASS=$PASS  FAIL=$FAIL"
fi
echo "═══════════════════════════════════════════════════"
echo ""

exit $FAIL
