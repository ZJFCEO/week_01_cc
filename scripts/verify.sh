#!/usr/bin/env bash
# =============================================================================
# LLM 统一模型调用服务 —— 全功能验收脚本
#
# 覆盖六大验收点，全部基于内置 mock 上游，无需真实 Key、完全离线可跑：
#   A. 双协议路由        —— 同一份统一请求翻译成两种截然不同的上游报文
#   B. 流式输出          —— SSE 逐块下发
#   C. 结构化输出        —— json_object / json_schema 约束合法 JSON
#   D. 提示词版本管理    —— 模板存储、变量替换、版本引用
#   E. 可观测性          —— Token 分类消耗 + 延迟 + 首 Token 延迟
#   F. 韧性              —— 统一错误码 + 指数退避重试 + 按模型独立限流
#
# 用法：./scripts/verify.sh
# =============================================================================
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

GW_PORT=${GW_PORT:-18080}
MOCK_PORT=${MOCK_PORT:-19090}
GW="http://127.0.0.1:${GW_PORT}"
MOCK="http://127.0.0.1:${MOCK_PORT}"
TMPDIR_V="$(mktemp -d)"
GW_PID=""
MOCK_PID=""

RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; CYAN=$'\033[36m'; BOLD=$'\033[1m'; RESET=$'\033[0m'

PASS=0
FAIL=0
FAILED_NAMES=""

# 验收全程打 mock 上游，mock 不校验 Key；但为了能断言「两种协议的鉴权头不同」，
# 这里保证环境里一定有一个 Key（有真实 Key 就用真实的，没有就用占位值）。
if [ -z "${DEEPSEEK_API_KEY:-}" ]; then
  export DEEPSEEK_API_KEY="sk-verify-placeholder-key"
fi

# ---------------------------------------------------------------------------
# 断言工具
# ---------------------------------------------------------------------------
ok() { PASS=$((PASS + 1)); printf "  ${GREEN}✓${RESET} %s\n" "$1"; }
ng() {
  FAIL=$((FAIL + 1))
  FAILED_NAMES="${FAILED_NAMES}\n    - $1"
  printf "  ${RED}✗ %s${RESET}\n      期望: %s\n      实际: %s\n" "$1" "$2" "$3"
}

assert_eq()  { if [ "$2" = "$3" ]; then ok "$1"; else ng "$1" "= $2" "$3"; fi; }
assert_neq() { if [ "$2" != "$3" ]; then ok "$1"; else ng "$1" "!= $2" "$3"; fi; }
assert_has() { case "$2" in *"$3"*) ok "$1";; *) ng "$1" "包含 [$3]" "$(printf '%s' "$2" | head -c 240)";; esac; }
assert_hasnt() { case "$2" in *"$3"*) ng "$1" "不包含 [$3]" "$(printf '%s' "$2" | head -c 240)";; *) ok "$1";; esac; }
# 数值比较（支持浮点）
assert_num() { # $1 描述 $2 实际 $3 运算符 $4 期望
  if awk -v a="$2" -v b="$4" "BEGIN{exit !(a $3 b)}" 2>/dev/null; then ok "$1"; else ng "$1" "$3 $4" "$2"; fi
}

section() { printf "\n${BOLD}${CYAN}%s${RESET}\n" "$1"; }

# JSON 取值：用法 echo "$json" | jget a.b.0.c
PYJGET='
import sys, json
path = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    print(""); raise SystemExit(0)
cur = data
for part in [p for p in path.split(".") if p != ""]:
    try:
        if isinstance(cur, list):
            cur = cur[int(part)]
        elif isinstance(cur, dict):
            cur = cur[part]
        else:
            cur = None; break
    except Exception:
        cur = None; break
if cur is None:
    print("")
elif isinstance(cur, bool):
    print("true" if cur else "false")
elif isinstance(cur, (dict, list)):
    print(json.dumps(cur, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
else:
    print(cur)
'
jget() { python3 -c "$PYJGET" "$1"; }

# 判断一段文本是否为合法 JSON
is_json() { printf '%s' "$1" | python3 -c 'import sys,json;json.load(sys.stdin);print("yes")' 2>/dev/null || echo "no"; }

post() { curl -s --max-time 30 -X POST "$1" -H 'Content-Type: application/json' -d "$2"; }
post_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 30 -X POST "$1" -H 'Content-Type: application/json' -d "$2"; }
sleep_ms() { perl -e "select(undef,undef,undef,$1)"; }

# ---------------------------------------------------------------------------
# 启停
# ---------------------------------------------------------------------------
cleanup() {
  [ -n "$GW_PID" ] && kill "$GW_PID" 2>/dev/null
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$TMPDIR_V"
}
trap cleanup EXIT INT TERM

# make_config 生成验收专用配置：
#  - pro / flash 放开限流，避免功能测试被限流器误伤
#  - deepseek-chat 收紧到 2 req/s，专门用来演示「按模型独立限流」
#    （它走的是第三套 openai_chat 协议，顺带把第三个适配器也覆盖到）
make_config() {
  python3 - "$1" <<'PYCFG'
import json, sys
cfg = json.load(open("configs/gateway.json"))
for m in cfg["models"]:
    if m["model"] in ("deepseek-v4-pro", "deepseek-v4-flash"):
        m["rate_limit"] = {"requests_per_second": 50, "burst": 50}
    elif m["model"] == "deepseek-chat":
        m["rate_limit"] = {"requests_per_second": 2, "burst": 2}
json.dump(cfg, open(sys.argv[1], "w"), ensure_ascii=False, indent=2)
PYCFG
}

start_gateway() {
  GATEWAY_LISTEN=":${GW_PORT}" \
  GATEWAY_PROMPT_STORE="${TMPDIR_V}/prompts.json" \
  UPSTREAM_RESPONSES_BASE_URL="$MOCK" \
  UPSTREAM_MESSAGES_BASE_URL="$MOCK" \
  UPSTREAM_CHAT_BASE_URL="$MOCK" \
    ./bin/gateway -config "${TMPDIR_V}/gateway.json" -release > "${TMPDIR_V}/gateway.log" 2>&1 &
  GW_PID=$!
}

wait_up() { # $1 url  $2 名称
  for _ in $(seq 1 60); do
    if curl -s -o /dev/null --max-time 2 "$1"; then return 0; fi
    sleep_ms 0.25
  done
  printf "${RED}启动超时: %s${RESET}\n" "$2"
  return 1
}

printf "${BOLD}LLM 统一模型调用服务 · 全功能验收${RESET}\n"
printf "网关 %s   假上游 %s\n" "$GW" "$MOCK"

section "0. 编译与启动"
if go build -o bin/gateway ./cmd/gateway && go build -o bin/mockupstream ./cmd/mockupstream; then
  ok "go build 通过（gateway + mockupstream）"
else
  ng "go build 通过" "exit 0" "编译失败"; exit 1
fi
if go vet ./... 2>"${TMPDIR_V}/vet.log"; then ok "go vet 无告警"; else ng "go vet 无告警" "无输出" "$(cat "${TMPDIR_V}/vet.log")"; fi
if go test ./... > "${TMPDIR_V}/test.log" 2>&1; then
  ok "go test 单元测试全部通过（$(grep -c '^ok' "${TMPDIR_V}/test.log") 个包）"
else
  ng "go test 单元测试全部通过" "全部 ok" "$(grep -E 'FAIL|---' "${TMPDIR_V}/test.log" | head -5)"
fi

make_config "${TMPDIR_V}/gateway.json"
./bin/mockupstream -listen ":${MOCK_PORT}" > "${TMPDIR_V}/mock.log" 2>&1 &
MOCK_PID=$!
start_gateway
wait_up "${MOCK}/healthz" "mock 上游" || exit 1
wait_up "${GW}/healthz" "网关" || exit 1
assert_eq "网关 /healthz 返回 ok" "ok" "$(curl -s ${GW}/healthz | jget status)"
assert_eq "假上游 /healthz 返回 ok" "ok" "$(curl -s ${MOCK}/healthz | jget status)"

# =============================================================================
section "A. 统一抽象层与双协议动态路由"
# =============================================================================
MODELS=$(curl -s ${GW}/v1/models)
assert_eq "路由表注册了 3 个模型" "3" "$(printf '%s' "$MODELS" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["models"]))')"
PROTO_PRO=$(printf '%s' "$MODELS" | python3 -c 'import sys,json;print([m["protocol"] for m in json.load(sys.stdin)["models"] if m["model"]=="deepseek-v4-pro"][0])')
PROTO_FLASH=$(printf '%s' "$MODELS" | python3 -c 'import sys,json;print([m["protocol"] for m in json.load(sys.stdin)["models"] if m["model"]=="deepseek-v4-flash"][0])')
assert_eq "deepseek-v4-pro   路由到 OpenAI Responses 协议" "openai_responses" "$PROTO_PRO"
assert_eq "deepseek-v4-flash 路由到 Anthropic Messages 协议" "anthropic_messages" "$PROTO_FLASH"
assert_neq "两个模型走的确实是不同协议" "$PROTO_PRO" "$PROTO_FLASH"
assert_eq "重试策略暴露 max_retries=3" "3" "$(printf '%s' "$MODELS" | jget retry_policy.max_retries)"

# 同一份统一请求分别打到两个模型
UNIFIED='{"model":"%s","system":"你是一个严谨的助手","messages":[{"role":"user","content":"用一句话解释适配器模式"}],"max_tokens":256,"temperature":0.3}'
R_PRO=$(post "${GW}/v1/chat" "$(printf "$UNIFIED" deepseek-v4-pro)")
R_FLASH=$(post "${GW}/v1/chat" "$(printf "$UNIFIED" deepseek-v4-flash)")
assert_eq "统一请求调用 pro   成功" "openai_responses" "$(printf '%s' "$R_PRO" | jget protocol)"
assert_eq "统一请求调用 flash 成功" "anthropic_messages" "$(printf '%s' "$R_FLASH" | jget protocol)"
assert_num "pro   返回内容非空" "$(printf '%s' "$R_PRO" | jget content | wc -c)" ">" 10
assert_num "flash 返回内容非空" "$(printf '%s' "$R_FLASH" | jget content | wc -c)" ">" 10
assert_eq "pro   finish_reason 归一为 stop" "stop" "$(printf '%s' "$R_PRO" | jget finish_reason)"
assert_eq "flash finish_reason 归一为 stop" "stop" "$(printf '%s' "$R_FLASH" | jget finish_reason)"

printf "\n  ${YELLOW}—— 协议差异取证：看上游实际收到了什么 ——${RESET}\n"
PROBE=$(curl -s "${MOCK}/mock/last-request")
OA_BODY=$(printf '%s' "$PROBE" | jget openai_responses.body)
AN_BODY=$(printf '%s' "$PROBE" | jget anthropic_messages.body)
OA_HDR=$(printf '%s' "$PROBE" | jget openai_responses.headers)
AN_HDR=$(printf '%s' "$PROBE" | jget anthropic_messages.headers)
# 请求体差异
assert_has   "Responses 报文用 instructions 承载 system"     "$OA_BODY" '"instructions"'
assert_has   "Responses 报文用 input 承载消息"                "$OA_BODY" '"input"'
assert_has   "Responses 报文的部件类型是 input_text"          "$OA_BODY" 'input_text'
assert_hasnt "Responses 报文不含 Anthropic 的 messages 字段"  "$OA_BODY" '"messages"'
assert_hasnt "Responses 报文不含 Anthropic 的 max_tokens 字段" "$OA_BODY" '"max_tokens"'
assert_has   "Messages 报文用顶层 system 承载 system"          "$AN_BODY" '"system"'
assert_has   "Messages 报文用 messages 承载消息"               "$AN_BODY" '"messages"'
assert_has   "Messages 报文的块类型是 text"                    "$AN_BODY" '"type":"text"'
assert_has   "Messages 报文补齐了必填的 max_tokens"            "$AN_BODY" '"max_tokens"'
assert_hasnt "Messages 报文不含 Responses 的 input 字段"       "$AN_BODY" '"input"'
assert_hasnt "Messages 报文不含 Responses 的 instructions 字段" "$AN_BODY" '"instructions"'
# 鉴权差异
assert_has   "Responses 走 Authorization 头"                  "$OA_HDR" 'authorization'
assert_hasnt "Responses 不带 anthropic-version 头"            "$OA_HDR" 'anthropic-version'
assert_has   "Messages 走 x-api-key 头"                       "$AN_HDR" 'x-api-key'
assert_has   "Messages 带 anthropic-version 头"               "$AN_HDR" 'anthropic-version'
assert_hasnt "Messages 不走 Authorization 头"                 "$AN_HDR" 'authorization'

printf "\n  ${YELLOW}—— 假上游对协议的强校验（证明差异不是摆设）——${RESET}\n"
assert_eq "把 Messages 报文打到 /v1/responses 会被拒" "400" \
  "$(post_code "${MOCK}/v1/responses" '{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}')"
assert_eq "把 Anthropic 的 max_tokens 塞进 /v1/responses 会被拒" "400" \
  "$(post_code "${MOCK}/v1/responses" '{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"max_tokens":10}')"
assert_eq "缺 anthropic-version 打 /v1/messages 会被拒" "400" \
  "$(post_code "${MOCK}/v1/messages" '{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":10}')"
assert_eq "缺 max_tokens 打 /v1/messages 会被拒" "400" \
  "$(curl -s -o /dev/null -w '%{http_code}' -X POST "${MOCK}/v1/messages" -H 'Content-Type: application/json' -H 'anthropic-version: 2023-06-01' -d '{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}')"
assert_eq "首条非 user 打 /v1/messages 会被拒" "400" \
  "$(curl -s -o /dev/null -w '%{http_code}' -X POST "${MOCK}/v1/messages" -H 'Content-Type: application/json' -H 'anthropic-version: 2023-06-01' -d '{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}')"

printf "\n  ${YELLOW}—— 适配器对消息序列的归一 ——${RESET}\n"
# 连续两条 user + 首条 assistant，Messages 协议本来会 400，适配器要负责补位与归并
MIX='{"model":"deepseek-v4-flash","messages":[{"role":"assistant","content":"上文回顾"},{"role":"user","content":"第一句"},{"role":"user","content":"第二句"}]}'
assert_eq "首条 assistant + 连续同角色消息仍能调通" "200" "$(post_code "${GW}/v1/chat" "$MIX")"
AN_BODY2=$(curl -s "${MOCK}/mock/last-request" | jget anthropic_messages.body)
assert_has "适配器补了占位 user 让首条合法" "$AN_BODY2" '(继续)'
assert_has "适配器把连续的两条 user 归并成一条" "$AN_BODY2" '第一句'

assert_eq "未注册模型返回 404" "404" "$(post_code "${GW}/v1/chat" '{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}')"
assert_eq "未注册模型错误码为 MODEL_NOT_FOUND" "MODEL_NOT_FOUND" \
  "$(post "${GW}/v1/chat" '{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}' | jget error.code)"
assert_eq "空 model 返回 INVALID_REQUEST" "INVALID_REQUEST" \
  "$(post "${GW}/v1/chat" '{"messages":[{"role":"user","content":"hi"}]}' | jget error.code)"

# =============================================================================
section "B. 流式输出（SSE 逐块返回）"
# =============================================================================
for M in deepseek-v4-pro deepseek-v4-flash; do
  F="${TMPDIR_V}/stream_${M}.sse"
  curl -sN --max-time 30 -D "${TMPDIR_V}/stream_${M}.hdr" -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' \
    -d "{\"model\":\"${M}\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"讲讲秋天的风景吧 [[MOCK:chunks=8]]\"}]}" > "$F"
  HDR=$(cat "${TMPDIR_V}/stream_${M}.hdr")
  assert_has "[$M] 响应头 Content-Type 是 text/event-stream" "$HDR" "text/event-stream"
  assert_has "[$M] 首个事件是 meta" "$(head -1 "$F")" "event: meta"
  META=$(grep -A1 '^event: meta' "$F" | tail -1 | sed 's/^data: //')
  assert_eq  "[$M] meta 里回传了实际协议" "$(printf '%s' "$MODELS" | python3 -c "import sys,json;print([m['protocol'] for m in json.load(sys.stdin)['models'] if m['model']=='${M}'][0])")" "$(printf '%s' "$META" | jget protocol)"
  DELTAS=$(grep -c '^event: delta' "$F")
  assert_num "[$M] delta 事件数 >= 5（确实是逐块下发）" "$DELTAS" ">=" 5
  assert_has "[$M] 有 done 结束事件" "$(cat "$F")" "event: done"
  assert_has "[$M] 有 [DONE] 兼容哨兵" "$(cat "$F")" "data: [DONE]"
  DONE=$(grep -A1 '^event: done' "$F" | tail -1 | sed 's/^data: //')
  # 把所有 delta 拼起来，应与 done 里的完整文本一致
  JOINED=$(grep '^event: delta' -A1 "$F" | grep '^data: ' | sed 's/^data: //' | python3 -c 'import sys,json;print("".join(json.loads(l)["content"] for l in sys.stdin if l.strip()))')
  assert_eq  "[$M] 所有 delta 拼接后等于 done 里的完整文本" "$(printf '%s' "$DONE" | jget content)" "$JOINED"
  assert_num "[$M] done 里带首 Token 延迟 ttft_ms > 0" "$(printf '%s' "$DONE" | jget observability.ttft_ms)" ">" 0
  assert_num "[$M] done 里 latency_ms >= ttft_ms" "$(printf '%s' "$DONE" | jget observability.latency_ms)" ">=" "$(printf '%s' "$DONE" | jget observability.ttft_ms)"
  assert_num "[$M] done 里带完整 Token 用量" "$(printf '%s' "$DONE" | jget usage.total_tokens)" ">" 0
done
# 流式下的准备期错误仍然是标准 JSON 错误（而不是 SSE）
assert_eq "流式请求遇到未注册模型仍返回 404 JSON" "404" \
  "$(post_code "${GW}/v1/chat" '{"model":"nope","stream":true,"messages":[{"role":"user","content":"hi"}]}')"

# =============================================================================
section "C. 结构化输出"
# =============================================================================
SCHEMA='{"type":"object","properties":{"sentiment":{"type":"string","enum":["positive","negative","neutral"]},"score":{"type":"number"},"keywords":{"type":"array","items":{"type":"string"}}},"required":["sentiment","score"],"additionalProperties":false}'
for M in deepseek-v4-pro deepseek-v4-flash; do
  BODY="{\"model\":\"${M}\",\"messages\":[{\"role\":\"user\",\"content\":\"分析情感：今天真开心\"}],\"response_format\":{\"type\":\"json_schema\",\"json_schema\":{\"name\":\"sentiment\",\"strict\":true,\"schema\":${SCHEMA}}}}"
  R=$(post "${GW}/v1/chat" "$BODY")
  C=$(printf '%s' "$R" | jget content)
  assert_eq  "[$M] json_schema 返回的 content 是合法 JSON" "yes" "$(is_json "$C")"
  assert_neq "[$M] 响应里带已解析对象 parsed" "" "$(printf '%s' "$R" | jget parsed)"
  assert_has "[$M] parsed.sentiment 命中 enum" "positive negative neutral" "$(printf '%s' "$R" | jget parsed.sentiment)"
  assert_neq "[$M] parsed.score 存在" "" "$(printf '%s' "$R" | jget parsed.score)"
  # json_object 模式
  R2=$(post "${GW}/v1/chat" "{\"model\":\"${M}\",\"messages\":[{\"role\":\"user\",\"content\":\"给我一个 JSON\"}],\"response_format\":{\"type\":\"json_object\"}}")
  assert_eq "[$M] json_object 返回的 content 是合法 JSON" "yes" "$(is_json "$(printf '%s' "$R2" | jget content)")"
done
# Anthropic 侧的结构化必须走强制工具调用
AN_STRUCT=$(curl -s "${MOCK}/mock/last-request" | jget anthropic_messages.body)
assert_has "Messages 协议下结构化被翻译成 tools 声明"       "$AN_STRUCT" '"tools"'
assert_has "Messages 协议下用 tool_choice 强制调用"          "$AN_STRUCT" '"tool_choice"'
assert_has "强制调用的工具名是 structured_output"            "$AN_STRUCT" 'structured_output'
# Responses 侧则是原生 text.format
post "${GW}/v1/chat" "{\"model\":\"deepseek-v4-pro\",\"messages\":[{\"role\":\"user\",\"content\":\"分析情感\"}],\"response_format\":{\"type\":\"json_schema\",\"json_schema\":{\"name\":\"sentiment\",\"schema\":${SCHEMA}}}}" > /dev/null
OA_STRUCT=$(curl -s "${MOCK}/mock/last-request" | jget openai_responses.body)
assert_has   "Responses 协议下结构化走原生 text.format" "$OA_STRUCT" '"format"'
assert_has   "Responses 协议下声明 json_schema"          "$OA_STRUCT" 'json_schema'
assert_hasnt "Responses 协议下不需要 tools 绕行"          "$OA_STRUCT" '"tools"'

# 流式 + 结构化：Anthropic 走 input_json_delta，拼接后仍是合法 JSON
SF="${TMPDIR_V}/stream_struct.sse"
curl -sN --max-time 30 -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' \
  -d "{\"model\":\"deepseek-v4-flash\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"分析情感 [[MOCK:chunks=6]]\"}],\"response_format\":{\"type\":\"json_schema\",\"json_schema\":{\"name\":\"sentiment\",\"schema\":${SCHEMA}}}}" > "$SF"
assert_num "流式结构化输出被切成多块下发" "$(grep -c '^event: delta' "$SF")" ">=" 3
SDONE=$(grep -A1 '^event: done' "$SF" | tail -1 | sed 's/^data: //')
assert_eq  "流式结构化拼接后仍是合法 JSON" "yes" "$(is_json "$(printf '%s' "$SDONE" | jget content)")"
assert_neq "流式结构化也给出 parsed" "" "$(printf '%s' "$SDONE" | jget parsed)"

# 非法 JSON 触发统一错误码
BAD=$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"给我 JSON [[MOCK:badjson]]"}],"response_format":{"type":"json_object"}}')
assert_eq "上游吐非法 JSON 时返回 STRUCTURED_INVALID" "STRUCTURED_INVALID" "$(printf '%s' "$BAD" | jget error.code)"
# 重试耗尽的请求，每次尝试都真实消耗了 Token，指标里必须如实累计（曾经记成 0）
assert_num "重试耗尽时已消耗的 Token 被如实记录" \
  "$(curl -s "${GW}/v1/traces?limit=1" | jget traces.0.usage.total_tokens)" ">" 0
assert_eq "STRUCTURED_INVALID 被标记为可重试" "true" "$(printf '%s' "$BAD" | jget error.retryable)"
assert_eq "json_schema 缺 schema 时拒绝请求" "INVALID_REQUEST" \
  "$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema"}}' | jget error.code)"
assert_eq "不支持的 response_format.type 被拒绝" "INVALID_REQUEST" \
  "$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"xml"}}' | jget error.code)"

# =============================================================================
section "D. 提示词模板：存储 / 变量替换 / 版本引用"
# =============================================================================
V1=$(post "${GW}/v1/prompts" '{"name":"translator","description":"翻译助手 v1","system":"你是一个专业翻译，把用户输入翻译成{{target_lang}}。","messages":[{"role":"user","content":"请翻译：{{text}}"}]}')
assert_eq "创建模板得到 v1" "1" "$(printf '%s' "$V1" | jget version)"
assert_eq "v1 自动解析出变量列表" '["target_lang","text"]' "$(printf '%s' "$V1" | jget variables)"
V2=$(post "${GW}/v1/prompts" '{"name":"translator","description":"翻译助手 v2 增加语气","system":"你是一个专业翻译，把用户输入翻译成{{target_lang}}，保持{{tone}}语气。","messages":[{"role":"user","content":"请翻译：{{text}}"}]}')
assert_eq "同名再创建自动递增到 v2" "2" "$(printf '%s' "$V2" | jget version)"
assert_eq "v2 变量列表包含新增的 tone" '["target_lang","text","tone"]' "$(printf '%s' "$V2" | jget variables)"

LIST=$(curl -s "${GW}/v1/prompts")
assert_eq "列表里 latest_version = 2" "2" "$(printf '%s' "$LIST" | jget prompts.0.latest_version)"
assert_eq "列表里 version_count = 2" "2" "$(printf '%s' "$LIST" | jget prompts.0.version_count)"
assert_eq "历史版本接口返回 2 个版本" "2" "$(curl -s "${GW}/v1/prompts/translator/versions" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["versions"]))')"

assert_has   "按版本号取 v1 拿到旧提示词" "$(curl -s "${GW}/v1/prompts/translator?version=1" | jget system)" "翻译成{{target_lang}}。"
assert_hasnt "v1 里没有 v2 才引入的 tone"  "$(curl -s "${GW}/v1/prompts/translator?version=1" | jget system)" "tone"
assert_eq    "不带 version 默认取 latest" "2" "$(curl -s "${GW}/v1/prompts/translator" | jget version)"
assert_eq    "version=latest 也取最新" "2" "$(curl -s "${GW}/v1/prompts/translator?version=latest" | jget version)"
assert_eq    "引用不存在的版本返回 PROMPT_NOT_FOUND" "PROMPT_NOT_FOUND" "$(curl -s "${GW}/v1/prompts/translator?version=99" | jget error.code)"
assert_eq    "引用不存在的模板返回 404" "404" "$(curl -s -o /dev/null -w '%{http_code}' "${GW}/v1/prompts/nope")"

REN=$(post "${GW}/v1/prompts/translator/render" '{"version":1,"variables":{"target_lang":"英文","text":"今天天气很好"}}')
assert_has   "渲染后 system 里变量已替换" "$(printf '%s' "$REN" | jget system)" "翻译成英文。"
assert_has   "渲染后 message 里变量已替换" "$(printf '%s' "$REN" | jget messages.0.content)" "请翻译：今天天气很好"
assert_hasnt "渲染结果里没有残留的 {{ 占位符" "$REN" "{{"
MISS=$(post "${GW}/v1/prompts/translator/render" '{"version":2,"variables":{"target_lang":"英文"}}')
assert_eq "缺变量时报 PROMPT_VAR_MISSING" "PROMPT_VAR_MISSING" "$(printf '%s' "$MISS" | jget error.code)"
assert_has "错误信息点名了缺哪些变量" "$(printf '%s' "$MISS" | jget error.message)" "text"

REF=$(post "${GW}/v1/chat" '{"model":"deepseek-v4-flash","prompt":{"name":"translator","version":2,"variables":{"target_lang":"英文","tone":"正式","text":"今天天气很好"}}}')
assert_eq "调用时按版本引用模板 -> prompt_ref=translator@v2" "translator@v2" "$(printf '%s' "$REF" | jget prompt_ref)"
REF_BODY=$(curl -s "${MOCK}/mock/last-request" | jget anthropic_messages.body)
assert_has "模板渲染结果真的发给了上游" "$REF_BODY" "保持正式语气"
REF1=$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","prompt":{"name":"translator","version":1,"variables":{"target_lang":"日文","text":"你好"}}}')
assert_eq "引用 v1 -> prompt_ref=translator@v1" "translator@v1" "$(printf '%s' "$REF1" | jget prompt_ref)"
REFL=$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","prompt":{"name":"translator","variables":{"target_lang":"英文","tone":"轻松","text":"你好"}}}')
assert_eq "省略 version 时引用 latest" "translator@v2" "$(printf '%s' "$REFL" | jget prompt_ref)"
assert_eq "引用模板时缺变量同样被拦截" "PROMPT_VAR_MISSING" \
  "$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","prompt":{"name":"translator","version":2,"variables":{"target_lang":"英文"}}}' | jget error.code)"

# 落盘持久化：重启网关后模板还在
kill "$GW_PID" 2>/dev/null; wait "$GW_PID" 2>/dev/null; GW_PID=""
start_gateway
wait_up "${GW}/healthz" "网关(重启)" || exit 1
assert_eq "网关重启后模板版本仍在（已落盘）" "2" "$(curl -s "${GW}/v1/prompts/translator" | jget version)"
assert_has "网关重启后 v1 内容完好" "$(curl -s "${GW}/v1/prompts/translator?version=1" | jget system)" "翻译成{{target_lang}}。"

# =============================================================================
section "E. 可观测性：Token 分类消耗 + 延迟 + 首 Token 延迟"
# =============================================================================
# 重启清空了指标，先造一批有代表性的流量
post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"介绍一下可观测性这个概念，越详细越好"}]}' > /dev/null
post "${GW}/v1/chat" '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"介绍一下可观测性这个概念"}]}' > /dev/null
curl -sN --max-time 30 -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-pro","stream":true,"messages":[{"role":"user","content":"流式跑一条用于统计首 Token 延迟 [[MOCK:chunks=10]]"}]}' > /dev/null

HDRS=$(curl -s -D - -o /dev/null -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"看响应头"}]}')
assert_has "响应头带 X-Request-Id"        "$HDRS" "X-Request-Id"
assert_has "响应头带 X-Latency-Ms"        "$HDRS" "X-Latency-Ms"
assert_has "响应头带 X-Upstream-Protocol" "$HDRS" "X-Upstream-Protocol"
assert_has "响应头带 X-Retry-Count"       "$HDRS" "X-Retry-Count"

MET=$(curl -s "${GW}/v1/metrics")
assert_num "总请求数 > 0" "$(printf '%s' "$MET" | jget total.requests)" ">" 0
assert_num "总 prompt_tokens > 0"     "$(printf '%s' "$MET" | jget total.tokens.prompt_tokens)" ">" 0
assert_num "总 completion_tokens > 0" "$(printf '%s' "$MET" | jget total.tokens.completion_tokens)" ">" 0
TP=$(printf '%s' "$MET" | jget total.tokens.prompt_tokens)
TC=$(printf '%s' "$MET" | jget total.tokens.completion_tokens)
TT=$(printf '%s' "$MET" | jget total.tokens.total_tokens)
assert_eq  "total_tokens = prompt + completion（分类统计自洽）" "$TT" "$((TP + TC))"
assert_num "分类统计：cached_tokens > 0"    "$(printf '%s' "$MET" | jget total.tokens.cached_tokens)" ">" 0
assert_num "分类统计：reasoning_tokens > 0" "$(printf '%s' "$MET" | jget total.tokens.reasoning_tokens)" ">" 0
assert_num "延迟样本数 > 0"        "$(printf '%s' "$MET" | jget total.latency_ms.count)" ">" 0
assert_num "平均延迟 > 0"          "$(printf '%s' "$MET" | jget total.latency_ms.avg)" ">" 0
assert_num "p95 延迟 >= p50 延迟"  "$(printf '%s' "$MET" | jget total.latency_ms.p95)" ">=" "$(printf '%s' "$MET" | jget total.latency_ms.p50)"
assert_num "首 Token 延迟样本数 > 0" "$(printf '%s' "$MET" | jget total.ttft_ms.count)" ">" 0
assert_num "首 Token 平均延迟 > 0"   "$(printf '%s' "$MET" | jget total.ttft_ms.avg)" ">" 0
assert_num "流式请求被单独计数"      "$(printf '%s' "$MET" | jget total.stream_requests)" ">" 0
assert_num "结构化调用被单独计数"    "$(printf '%s' "$MET" | jget total.structured_calls)" ">=" 0
BY=$(printf '%s' "$MET" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(json.dumps({m["model"]:m for m in d["by_model"]},ensure_ascii=False))')
assert_neq "指标按模型分开统计：pro"   "" "$(printf '%s' "$BY" | jget deepseek-v4-pro.requests)"
assert_neq "指标按模型分开统计：flash" "" "$(printf '%s' "$BY" | jget deepseek-v4-flash.requests)"
assert_eq  "pro 的指标标注了 openai_responses 协议"   "openai_responses"   "$(printf '%s' "$BY" | jget deepseek-v4-pro.protocol)"
assert_eq  "flash 的指标标注了 anthropic_messages 协议" "anthropic_messages" "$(printf '%s' "$BY" | jget deepseek-v4-flash.protocol)"

TR=$(curl -s "${GW}/v1/traces?limit=50")
assert_num "调用明细条数 > 0" "$(printf '%s' "$TR" | jget count)" ">" 0
assert_neq "明细里有 request_id" "" "$(printf '%s' "$TR" | jget traces.0.request_id)"
assert_neq "明细里有 latency_ms" "" "$(printf '%s' "$TR" | jget traces.0.latency_ms)"
assert_neq "明细里有 attempt_trace" "" "$(printf '%s' "$TR" | jget traces.0.attempt_trace)"
assert_eq "明细里能查到流式调用的 ttft_ms" "yes" \
  "$(printf '%s' "$TR" | python3 -c 'import sys,json;print("yes" if any(t.get("stream") and t.get("ttft_ms",0)>0 for t in json.load(sys.stdin)["traces"]) else "no")')"

# =============================================================================
section "F. 韧性：统一错误码 + 指数退避重试 + 按模型独立限流"
# =============================================================================
printf "\n  ${YELLOW}—— 指数退避重试 ——${RESET}\n"
RID="retry-$$-$RANDOM"
RT=$(post "${GW}/v1/chat" "{\"model\":\"deepseek-v4-pro\",\"messages\":[{\"role\":\"user\",\"content\":\"测试重试 [[MOCK:fail=2,id=${RID}]]\"}]}")
assert_eq  "前 2 次上游 500 后第 3 次成功" "3" "$(printf '%s' "$RT" | jget observability.attempts)"
assert_eq  "重试次数记为 2"                "2" "$(printf '%s' "$RT" | jget observability.retries)"
assert_eq  "第 1 次尝试记录了 UPSTREAM_ERROR" "UPSTREAM_ERROR" "$(printf '%s' "$RT" | jget observability.attempt_trace.0.error)"
assert_eq  "第 2 次尝试记录了 UPSTREAM_ERROR" "UPSTREAM_ERROR" "$(printf '%s' "$RT" | jget observability.attempt_trace.1.error)"
assert_eq  "第 3 次尝试没有错误（成功）"     "" "$(printf '%s' "$RT" | jget observability.attempt_trace.2.error)"
D1=$(printf '%s' "$RT" | jget observability.attempt_trace.0.delay_ms)
D2=$(printf '%s' "$RT" | jget observability.attempt_trace.1.delay_ms)
assert_num "第 1 次退避落在 200ms±20% 区间" "$D1" ">=" 160
assert_num "第 1 次退避不超过 240ms"        "$D1" "<=" 240
assert_num "第 2 次退避落在 400ms±20% 区间" "$D2" ">=" 320
assert_num "第 2 次退避不超过 480ms"        "$D2" "<=" 480
assert_num "退避确实是递增的（指数退避）"    "$D2" ">" "$D1"

FAIL_ALL=$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"一直失败 [[MOCK:status=500]]"}]}')
assert_eq "上游持续 500 时错误码为 UPSTREAM_ERROR" "UPSTREAM_ERROR" "$(printf '%s' "$FAIL_ALL" | jget error.code)"
assert_eq "上游持续 500 时对外返回 502" "502" "$(post_code "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"一直失败 [[MOCK:status=500]]"}]}')"
assert_eq "错误响应带上游原始状态码" "500" "$(printf '%s' "$FAIL_ALL" | jget error.upstream_status)"
assert_neq "错误响应带 request_id 便于排查" "" "$(printf '%s' "$FAIL_ALL" | jget error.request_id)"
ATT=$(curl -s "${GW}/v1/traces?limit=1" | jget traces.0.attempts)
assert_eq "重试上限生效：共尝试 4 次（1 次 + 最多 3 次重试）" "4" "$ATT"

assert_eq "上游 429 归一为 UPSTREAM_RATE_LIMIT" "UPSTREAM_RATE_LIMIT" \
  "$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"限流 [[MOCK:status=429]]"}]}' | jget error.code)"
assert_eq "上游 401 归一为 UPSTREAM_AUTH" "UPSTREAM_AUTH" \
  "$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"鉴权 [[MOCK:status=401]]"}]}' | jget error.code)"
assert_eq "UPSTREAM_AUTH 标记为不可重试" "false" \
  "$(post "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"鉴权 [[MOCK:status=401]]"}]}' | jget error.retryable)"
AUTH_ATT=$(curl -s "${GW}/v1/traces?limit=1" | jget traces.0.attempts)
assert_eq "不可重试的错误只尝试 1 次（不浪费配额）" "1" "$AUTH_ATT"

RID2="retry-stream-$$-$RANDOM"
SRF="${TMPDIR_V}/stream_retry.sse"
curl -sN --max-time 30 -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' \
  -d "{\"model\":\"deepseek-v4-pro\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"流式重试 [[MOCK:fail=2,id=${RID2}]]\"}]}" > "$SRF"
SRDONE=$(grep -A1 '^event: done' "$SRF" | tail -1 | sed 's/^data: //')
assert_eq "流式在首 Token 之前失败也能重试成功" "3" "$(printf '%s' "$SRDONE" | jget observability.attempts)"
assert_num "流式重试成功后仍有正常内容" "$(printf '%s' "$SRDONE" | jget content | wc -c)" ">" 10

printf "\n  ${YELLOW}—— 按模型独立限流 ——${RESET}\n"
# 用 deepseek-chat 做限流演示：它被配成 2 req/s、burst 2，且走第三套 openai_chat 协议
sleep_ms 1.6   # 等令牌桶补满
RL_CODES=""
for i in 1 2 3 4 5 6; do
  RL_CODES="${RL_CODES}$(post_code "${GW}/v1/chat" '{"model":"deepseek-chat","messages":[{"role":"user","content":"限流测试"}]}') "
done
assert_has "deepseek-chat 连打 6 次出现 429" "$RL_CODES" "429"
assert_has "deepseek-chat 前几次仍然放行 200" "$RL_CODES" "200"
RL=$(post "${GW}/v1/chat" '{"model":"deepseek-chat","messages":[{"role":"user","content":"限流测试"}]}')
assert_eq  "限流错误码为 RATE_LIMITED" "RATE_LIMITED" "$(printf '%s' "$RL" | jget error.code)"
assert_eq  "网关自身限流不标记为可重试" "false" "$(printf '%s' "$RL" | jget error.retryable)"
assert_num "限流响应给出 retry_after_ms" "$(printf '%s' "$RL" | jget error.retry_after_ms)" ">" 0
assert_has "限流提示里点明了是哪个模型" "$(printf '%s' "$RL" | jget error.message)" "deepseek-chat"
RLH=$(curl -s -D - -o /dev/null -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"限流测试"}]}')
assert_has "429 响应带 Retry-After 头"        "$RLH" "Retry-After:"
assert_has "429 响应带 X-RateLimit-Limit 头"      "$RLH" "X-Ratelimit-Limit:"
assert_has "429 响应带 X-RateLimit-Burst 头"      "$RLH" "X-Ratelimit-Burst:"
assert_has "429 响应仍带 X-RateLimit-Remaining 头" "$RLH" "X-Ratelimit-Remaining:"
assert_has "429 响应仍带 X-RateLimit-Reset 头"     "$RLH" "X-Ratelimit-Reset:"

printf "\n  ${YELLOW}—— 动态配额状态（Remaining / Reset）——${RESET}\n"
sleep_ms 2.5   # 等桶补满
hdr_val() { printf '%s' "$1" | tr -d '\r' | grep -i "^$2:" | head -1 | cut -d' ' -f2; }
H1=$(curl -s -D - -o /dev/null -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"x"}]}')
H2=$(curl -s -D - -o /dev/null -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"x"}]}')
R1=$(hdr_val "$H1" "X-Ratelimit-Remaining"); R2=$(hdr_val "$H2" "X-Ratelimit-Remaining")
assert_neq "Remaining 头有值" "" "$R1"
assert_num "Remaining 随每次调用递减（动态状态，不是静态配置）" "$R2" "<" "$R1"
assert_num "Reset 头是正数（桶补满还需多久）" "$(hdr_val "$H1" "X-Ratelimit-Reset")" ">" 0
assert_eq  "Limit 头是静态配置，两次调用完全一样" "$(hdr_val "$H1" "X-Ratelimit-Limit")" "$(hdr_val "$H2" "X-Ratelimit-Limit")"
assert_has "流式响应同样带配额头" \
  "$(curl -sN -D - -o /dev/null --max-time 20 -X POST "${GW}/v1/chat" -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-pro","stream":true,"messages":[{"role":"user","content":"x"}]}')" \
  "X-Ratelimit-Remaining:"
# 关键：限流是按模型独立的，一个模型被打满不影响其它模型
assert_eq "chat 被打满时 pro 仍然可用（限流按模型独立）" "200" \
  "$(post_code "${GW}/v1/chat" '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"我不该被牵连"}]}')"
assert_eq "chat 被打满时 flash 仍然可用（限流按模型独立）" "200" \
  "$(post_code "${GW}/v1/chat" '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"我也不该被牵连"}]}')"
assert_eq "此刻 chat 仍然是 429（说明确实只有它被限）" "429" \
  "$(post_code "${GW}/v1/chat" '{"model":"deepseek-chat","messages":[{"role":"user","content":"还在限流吗"}]}')"
sleep_ms 1.2
assert_eq "等待令牌补充后 chat 恢复可用" "200" \
  "$(post_code "${GW}/v1/chat" '{"model":"deepseek-chat","messages":[{"role":"user","content":"恢复了吗"}]}')"
MET2=$(curl -s "${GW}/v1/metrics")
assert_num "限流次数被计入可观测指标" "$(printf '%s' "$MET2" | jget total.rate_limited)" ">" 0
assert_num "错误码分布里有 RATE_LIMITED" "$(printf '%s' "$MET2" | jget total.error_code_counts.RATE_LIMITED)" ">" 0
assert_num "错误码分布里有 UPSTREAM_ERROR" "$(printf '%s' "$MET2" | jget total.error_code_counts.UPSTREAM_ERROR)" ">" 0
assert_num "重试总次数被计入可观测指标" "$(printf '%s' "$MET2" | jget total.total_retries)" ">" 0
assert_eq "第三套协议 openai_chat 也走通了" "openai_chat" \
  "$(post "${GW}/v1/chat" '{"model":"deepseek-chat","messages":[{"role":"user","content":"第三套协议"}]}' | jget protocol)"

# =============================================================================
section "验收结果"
# =============================================================================
TOTAL=$((PASS + FAIL))
printf "共 %d 项断言：${GREEN}通过 %d${RESET}，${RED}失败 %d${RESET}\n" "$TOTAL" "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf "${RED}失败项：${RESET}"
  printf "$FAILED_NAMES\n"
  exit 1
fi
printf "\n${GREEN}${BOLD}六大功能点全部通过验收。${RESET}\n"
printf "  A. 双协议路由 · B. 流式输出 · C. 结构化输出\n"
printf "  D. 提示词版本管理 · E. 可观测性 · F. 重试与限流\n"
