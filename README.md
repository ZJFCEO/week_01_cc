# LLM 统一模型调用服务（Week 01）

用 Go 1.25 + Gin 实现的 LLM 统一网关。核心训练点是**用适配器模式把两种截然不同的上游 API 协议的差异彻底吃掉**：调用方永远只面对一套统一请求/响应结构，网关根据请求里的 `model` 字段动态路由到对应协议的适配器。

| 逻辑模型 | 上游协议 | 端点 | 鉴权方式 |
|---|---|---|---|
| `deepseek-v4-pro` | **OpenAI Responses API** | `POST /v1/responses` | `Authorization: Bearer <key>` |
| `deepseek-v4-flash` | **Anthropic Messages API** | `POST /v1/messages` | `x-api-key` + `anthropic-version` |
| `deepseek-chat` | OpenAI Chat Completions | `POST /chat/completions` | `Authorization: Bearer <key>` |

三个模型都能打**真实 DeepSeek**，官方三套协议的端点分别是：

| 协议 | 真实端点 |
|---|---|
| OpenAI Responses | `https://api.deepseek.com/v1/responses` |
| Anthropic Messages | `https://api.deepseek.com/anthropic/v1/messages` |
| OpenAI Chat Completions | `https://api.deepseek.com/chat/completions` |

仓库给了两份配置，用途不同：

| 配置 | 上游 | 用途 |
|---|---|---|
| `configs/gateway.json` | 内置 mock（`127.0.0.1:9090`） | 离线验收、故障注入、不花钱 |
| `configs/gateway.real.json` | 真实 DeepSeek | 真实效果验证 |

两份都留着是有原因的：重试、错误码分流、结构化校验失败这些**只能靠故障注入验证**，真实上游不会配合你按需返回 500。`scripts/verify.sh` 的 177 项断言里有相当一部分依赖这个，而且评审复现时不需要 Key。

---

## 目录

- [快速开始](#快速开始)
- [核心设计](#核心设计)
- [调用时序图](#调用时序图)
- [代码阅读指南](docs/reading-guide.md)
- [六大功能的 curl 示例](#六大功能的-curl-示例)
  - [分布式限流：两个问题，别混](#分布式限流两个问题别混)
- [用真实 DeepSeek Key 跑通](#用真实-deepseek-key-跑通)
- [接口清单](#接口清单)
- [统一错误码](#统一错误码)
- [验收脚本](#验收脚本)
- [内置假上游与故障注入](#内置假上游与故障注入)
- [目录结构](#目录结构)

---

## 快速开始

环境要求：Go 1.25+。除 Gin 外无第三方依赖。

```bash
make build
```

开两个终端。**终端 1** 起内置假上游（同时提供三套协议）：

```bash
./bin/mockupstream -listen :9090
```

**终端 2** 起网关：

```bash
./bin/gateway -config configs/gateway.json
```

启动日志会打印路由表，一眼能看出两个模型走的是不同协议：

```
[boot] 注册模型 deepseek-v4-pro      协议=openai_responses     上游=http://127.0.0.1:9090 限流=5.00 req/s(burst 5) Key=已加载(DEEPSEEK_API_KEY, 长度 35)
[boot] 注册模型 deepseek-v4-flash    协议=anthropic_messages   上游=http://127.0.0.1:9090 限流=2.00 req/s(burst 2) Key=已加载(DEEPSEEK_API_KEY, 长度 35)
[boot] 注册模型 deepseek-chat        协议=openai_chat          上游=https://api.deepseek.com 限流=3.00 req/s(burst 3) Key=已加载(DEEPSEEK_API_KEY, 长度 35)
[boot] LLM 统一网关监听 :8080（重试最多 3 次，退避 200ms 起步）
```

启动日志的 `Key=` 一栏有三种状态，照着读就行：

| 显示 | 含义 |
|---|---|
| `已加载(DEEPSEEK_API_KEY, 长度 35)` | 正常 |
| `未设置（上游是本机 mock，不需要 Key）` | 正常，离线验收就是这个状态 |
| `⚠ 未设置：上游是远端，但没读到 …` | 需要处理，否则调用会 401 |

> **从 IDE（GoLand / VS Code）启动读不到 Key？**
> macOS 的 GUI 应用由 launchd 拉起，不会执行登录 shell，`~/.zshrc` 根本没跑过（可以看 IDE 进程的 `PATH`，通常只有 `/usr/bin:/bin:/usr/sbin:/sbin`）。
> 在运行配置里补上 `Environment variables: DEEPSEEK_API_KEY=sk-...` 和 `Program arguments: -config configs/gateway.json`，或者直接用终端 `make run`。
> 注意 IDE 的运行配置会写进 `.idea/`，别把 Key 提交上去。

冒烟：

```bash
curl -s http://127.0.0.1:8080/v1/models | python3 -m json.tool
```

一键跑全部验收（会自己拉起两个进程，用 18080/19090 端口，不影响上面手动起的实例）：

```bash
./scripts/verify.sh
```

---

## 核心设计

### 分层

```
HTTP 接入层 (internal/server)      只负责搬运：解析请求、写 SSE、映射状态码
        ↓
编排层     (internal/service)      主流程：模板渲染 → 限流 → 重试 → 适配器 → 结构化校验 → 落指标
        ↓
统一抽象层 (internal/llm)          Adapter 接口 + Registry 路由表 + 协议中立的 Request/Response/StreamEvent
        ↓
   ┌────────────────┬────────────────────┬──────────────────┐
OpenAIResponses  AnthropicMessages    OpenAIChat            ← 三个适配器，各自消化协议差异
```

统一调用接口只有三个方法：

```go
type Adapter interface {
    Protocol() string
    Invoke(ctx context.Context, req *Request) (*Response, error)
    Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error)
}
```

### 两种协议的差异，以及适配器怎么吃掉它

| 差异点 | OpenAI Responses | Anthropic Messages | 适配器做了什么 |
|---|---|---|---|
| 鉴权 | `Authorization: Bearer` | `x-api-key` + `anthropic-version: 2023-06-01` | 各自在 `headers()` 里装配 |
| 系统提示词 | 顶层 `instructions` 字段 | 顶层 `system` 字段，且**不能**出现在 `messages` 里 | 统一层的 `system` + 角色为 `system` 的消息一起提取归并 |
| 消息容器 | `input[]`，content 是部件数组，用户侧 `input_text`／助手侧 `output_text` | `messages[]`，content 是块数组，类型 `text` | 分别构造 |
| `max_tokens` | 可选，叫 `max_output_tokens` | **必填**，不填直接 400 | Anthropic 侧兜底 2048 |
| 消息序列约束 | 无 | 必须首条 `user`、`user/assistant` 严格交替 | 归并相邻同角色消息；首条是 assistant 时补一条占位 user |
| 结构化输出 | 原生 `text.format` = `json_object` / `json_schema` | **没有** `response_format` | 翻译成 `tools` + `tool_choice` 强制调用一个入参就是目标 schema 的工具，再把 `tool_use.input` 还原成 JSON 文本 |
| 输出位置 | `output[].content[].text` | `content[].text`（结构化时是 `content[].input`） | 归一到 `Response.Content` |
| 用量字段 | `input_tokens` / `output_tokens` / `input_tokens_details.cached_tokens` / `output_tokens_details.reasoning_tokens` | `input_tokens` / `output_tokens` / `cache_read_input_tokens` | 归一到 `Usage{PromptTokens, CompletionTokens, TotalTokens, CachedTokens, ReasoningTokens}` |
| 结束原因 | `status` = completed/incomplete/failed | `stop_reason` = end_turn/max_tokens/tool_use/refusal | 归一到 `stop` / `length` / `content_filter` / `error` |
| 流式事件 | `response.output_text.delta` → `response.completed` | `message_start` → `content_block_delta` → `message_delta` → `message_stop` | 都归一成 `StreamEvent{Delta/Done/Error}` |
| 流式增量 | `delta` 字段 | 文本走 `delta.text`，**结构化时走 `delta.partial_json`** | 两者都转发成同一种 delta |
| 流式用量 | 全在 `response.completed` 里 | 输入用量在 `message_start`，输出用量在 `message_delta` | 分段累积 |

**怎么自证不是"两个都接上就完事"**：`cmd/mockupstream` 对协议做了强校验（Messages 端点缺 `anthropic-version` 或 `max_tokens` 直接 400、首条非 user 直接 400；Responses 端点收到 `messages` 字段直接 400），而且提供了取证端点 `GET /mock/last-request`，能直接看到上游实际收到的两份报文。同一句 `"你是一个严谨的助手" + "用一句话解释适配器模式"` 发出去，落到上游是这样的：

```jsonc
// deepseek-v4-pro 走 OpenAI Responses：instructions + input 部件数组
{
  "model": "deepseek-v4-pro",
  "instructions": "你是一个严谨的助手",
  "input": [{"role":"user","content":[{"type":"input_text","text":"用一句话解释适配器模式"}]}],
  "max_output_tokens": 256
}
// 头：Authorization: Bearer sk-***

// deepseek-v4-flash 走 Anthropic Messages：顶层 system + messages 块数组 + 必填 max_tokens
{
  "model": "deepseek-v4-flash",
  "system": "你是一个严谨的助手",
  "messages": [{"role":"user","content":[{"type":"text","text":"用一句话解释适配器模式"}]}],
  "max_tokens": 2048
}
// 头：x-api-key: sk-***  /  anthropic-version: 2023-06-01
```

### 主流程

`internal/service/gateway.go` 里 `Invoke` / `InvokeStream` 用 ①②③ 编号串起了全流程：

1. **准备**：校验 → 按 `model` 动态路由 → 若引用了模板则取版本 + 变量替换
2. **限流**：按模型独立令牌桶，超限直接 429（不进重试，本地配额问题重试没意义）
3. **调用**：指数退避重试包住「适配器调用 + 结构化校验」
4. **校验**：结构化输出抽 JSON（容忍 ```` ```json ```` 围栏）+ JSON Schema 校验，失败返回 `STRUCTURED_INVALID`（可重试，等于让模型重新生成一次）
5. **观测**：成功失败都落一条明细，更新按模型的聚合指标

流式的特殊处理：**只有在还没吐出任何一块内容之前才允许重试**，一旦开始输出就标记为不可重试（否则客户端会看到重复内容）。

---

## 调用时序图

五张 mermaid 时序图放在 [docs/sequence.md](docs/sequence.md)，覆盖六大功能点的全部流转，编号与 `internal/service/gateway.go` 里的 ①②③④⑤ 注释一一对应：

| 图 | 主题 | 对应源码 |
|---|---|---|
| 1 | 非流式主流程（限流 · 重试 · 统一错误码 · 可观测） | `internal/service/gateway.go` · `Invoke()` |
| 2 | 流式输出：SSE 逐块下发、首 Token 延迟打点、首块之前可重试 | `internal/service/gateway.go` · `InvokeStream()` |
| 3 | 双协议翻译：同一请求落到上游是两份完全不同的报文 | `internal/llm/openai_responses.go` · `internal/llm/anthropic_messages.go` |
| 4 | 结构化输出：原生 `text.format` vs 强制工具调用两条路径 | `internal/llm` · `internal/structured` |
| 5 | 提示词版本引用：存储 · 变量替换 · 按版本引用 | `internal/prompt/store.go` |

---

## 六大功能的 curl 示例

### ① 双协议路由

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好，用一句话介绍你自己"}]}'
```

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"你好，用一句话介绍你自己"}]}'
```

两次请求体只有 `model` 不同，响应结构完全一致，`protocol` 字段会分别是 `openai_responses` 和 `anthropic_messages`：

```json
{
  "id": "resp_dkyilayyyjvc",
  "request_id": "req_69d14ea8210e5f60",
  "model": "deepseek-v4-pro",
  "protocol": "openai_responses",
  "content": "……",
  "finish_reason": "stop",
  "usage": {"prompt_tokens":7,"completion_tokens":43,"total_tokens":50,"cached_tokens":1,"reasoning_tokens":4},
  "observability": {"latency_ms":0.6,"attempts":1,"retries":0,"attempt_trace":[{"index":1}]}
}
```

看上游实际收到的两份报文（协议差异取证）：

```bash
curl -s http://127.0.0.1:9090/mock/last-request | python3 -m json.tool
```

### ② 流式输出（SSE）

```bash
curl -N -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"讲讲秋天 [[MOCK:chunks=6]]"}]}'
```

```
event: meta
data: {"model":"deepseek-v4-flash","prompt_ref":"","protocol":"anthropic_messages","request_id":"req_3c0d19a9f7958da6"}

event: delta
data: {"content":"【mock:anthropi"}

event: delta
data: {"content":"c_messages】已收到"}

……

event: done
data: {"content":"……完整文本……","usage":{...},"observability":{"latency_ms":96.47,"ttft_ms":0.32,"attempts":1,"retries":0}}

data: [DONE]
```

`done` 事件里带完整文本、Token 用量和**首 Token 延迟 `ttft_ms`**。

### ③ 结构化输出

`json_schema` 模式（两种协议内部实现完全不同，对外表现一致）：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' -d '{
  "model": "deepseek-v4-flash",
  "messages": [{"role":"user","content":"分析这句话的情感：这个产品体验太糟糕了"}],
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "sentiment",
      "strict": true,
      "schema": {
        "type": "object",
        "properties": {
          "sentiment": {"type":"string","enum":["positive","negative","neutral"]},
          "score": {"type":"number"},
          "keywords": {"type":"array","items":{"type":"string"}}
        },
        "required": ["sentiment","score"],
        "additionalProperties": false
      }
    }
  }
}'
```

响应里 `content` 是 JSON 字符串，`parsed` 是网关已经解析并校验过的对象：

```json
{"content":"{\"score\":0.86,\"sentiment\":\"positive\"}","parsed":{"score":0.86,"sentiment":"positive"}}
```

`json_object` 模式只要求合法 JSON：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"用 JSON 给我三个水果"}],"response_format":{"type":"json_object"}}'
```

模型没吐出合法 JSON 时，网关会自动重试，重试耗尽后返回 `STRUCTURED_INVALID`：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"给我 JSON [[MOCK:badjson]]"}],"response_format":{"type":"json_object"}}'
```

### ④ 提示词模板：存储 / 变量替换 / 版本引用

建 v1：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/prompts -H 'Content-Type: application/json' -d '{"name":"translator","description":"翻译助手 v1","system":"你是一个专业翻译，把用户输入翻译成{{target_lang}}。","messages":[{"role":"user","content":"请翻译：{{text}}"}]}'
```

同名再 POST 一次自动变成 v2（旧版本永不覆盖）：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/prompts -H 'Content-Type: application/json' -d '{"name":"translator","description":"翻译助手 v2 增加语气","system":"你是一个专业翻译，把用户输入翻译成{{target_lang}}，保持{{tone}}语气。","messages":[{"role":"user","content":"请翻译：{{text}}"}]}'
```

变量是从模板文本里自动解析出来的，返回体里能看到 `"variables":["target_lang","text","tone"]`。

只渲染不调模型（省 Token 的调试方式）：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/prompts/translator/render -H 'Content-Type: application/json' -d '{"version":1,"variables":{"target_lang":"英文","text":"今天天气很好"}}'
```

调用时引用指定版本：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-flash","prompt":{"name":"translator","version":2,"variables":{"target_lang":"英文","tone":"正式","text":"今天天气很好"}}}'
```

响应里会带 `"prompt_ref":"translator@v2"`。省略 `version` 即引用 latest。缺变量时**不会**把 `{{text}}` 原样发给模型，而是直接 400：

```json
{"error":{"code":"PROMPT_VAR_MISSING","message":"模板 translator@v2 缺少变量: text, tone","retryable":false,"request_id":"req_f8e6bf7b93a650ac"}}
```

模板落盘在 `data/prompts.json`，网关重启不丢。

### ⑤ 可观测性

按模型聚合的指标：

```bash
curl -s http://127.0.0.1:8080/v1/metrics | python3 -m json.tool
```

```json
{
  "by_model": [{
    "model": "deepseek-v4-pro",
    "protocol": "openai_responses",
    "requests": 6, "success": 4, "failed": 2,
    "stream_requests": 1, "structured_calls": 2,
    "total_retries": 8, "rate_limited": 0,
    "tokens": {"prompt_tokens":42,"completion_tokens":154,"total_tokens":196,"cached_tokens":7,"reasoning_tokens":13},
    "latency_ms": {"count":6,"avg":600.79,"p50":49.05,"p95":1548.88,"max":1548.88},
    "ttft_ms":    {"count":1,"avg":0.47,"p50":0.47,"p95":0.47,"max":0.47},
    "error_code_counts": {"STRUCTURED_INVALID":1,"UPSTREAM_ERROR":1}
  }],
  "total": { "...": "全模型汇总" }
}
```

- **Token 分类统计**：`prompt` / `completion` / `total`，外加 `cached`（输入命中缓存的部分）和 `reasoning`（输出中属于思维链的部分）
- **延迟**：`avg` / `p50` / `p95` / `max`
- **首 Token 延迟**：`ttft_ms` 单独一组，只统计流式请求

最近调用明细（含每次重试的退避时长）：

```bash
curl -s 'http://127.0.0.1:8080/v1/traces?limit=5' | python3 -m json.tool
```

每次非流式响应的响应头里也直接带了关键值：

```bash
curl -si -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}' | head -12
```

```
X-Request-Id: req_...
X-Latency-Ms: 0.61
X-Upstream-Protocol: openai_responses
X-Retry-Count: 0
```

网关同时以 JSON 行打结构化日志，可以直接 `grep '\[metric\]'` 看单次调用。

### ⑥ 韧性：统一错误码 + 指数退避重试 + 按模型独立限流

**重试**——让上游前 2 次返回 500，第 3 次成功：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"测试重试 [[MOCK:fail=2,id=demo1]]"}]}'
```

`observability.attempt_trace` 里能看到完整退避轨迹（200ms → 400ms，含 ±20% 抖动）：

```json
{"attempts":3,"retries":2,"attempt_trace":[
  {"index":1,"error":"UPSTREAM_ERROR","delay_ms":205.38},
  {"index":2,"error":"UPSTREAM_ERROR","delay_ms":434.36},
  {"index":3}
]}
```

上游持续失败时，重试 3 次后返回统一错误码：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"一直失败 [[MOCK:status=500]]"}]}'
```

```json
{"error":{"code":"UPSTREAM_ERROR","message":"上游返回 500","retryable":true,"request_id":"req_...","upstream_status":500,"upstream_body":"..."}}
```

不可重试的错误（如 401）只会尝试 1 次，不浪费配额。

**限流**——`deepseek-v4-flash` 配置为 2 req/s、burst 2，连打 6 次：

```bash
for i in 1 2 3 4 5 6; do
  curl -s -o /dev/null -w "第${i}次 -> %{http_code}\n" -X POST http://127.0.0.1:8080/v1/chat \
    -H 'Content-Type: application/json' -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"限流测试"}]}'
done
```

```
第1次 -> 200
第2次 -> 200
第3次 -> 429
...
```

429 的响应带完整退避提示：

```
HTTP/1.1 429 Too Many Requests
Retry-After: 1
X-Retry-After-Ms: 451
X-Ratelimit-Limit: 2
X-Ratelimit-Burst: 2

{"error":{"code":"RATE_LIMITED","message":"模型 deepseek-v4-flash 触发限流（2.00 req/s, burst 2），请 450ms 后重试","retryable":false,"retry_after_ms":451}}
```

限流是**按模型独立**的：`deepseek-v4-flash` 被打满时，`deepseek-v4-pro` 完全不受影响。

> **已知局限：限流是单实例的。**
> 令牌桶是进程内内存状态（[internal/resilience/ratelimit.go](internal/resilience/ratelimit.go)），副本之间不共享。
> 部署 N 个网关副本时，上游实际承受的是 `配置速率 × N`。实测两副本轮流打，1 秒内 6 条请求放行了 4 条（单副本是 2 条）。

### 分布式限流：两个问题，别混

限流在分布式下有两个镜像问题，性质完全不同，方案也不该一样。

```
   调用方实例 × N               网关副本 × N              上游
  ┌──────────────┐           ┌──────────────┐
  │ 各有一个本地桶 │ ────────▶ │ 各有一个桶    │ ─────────▶
  └──────────────┘           └──────────────┘
       问题 A                     问题 B
   桶不准 → 撞 429             桶不准 → 真超发
   （网关会兜底）               （没有下一层兜底）
```

关键区别在**谁是权威**：

| | 桶算错的后果 | 有没有兜底 |
|---|---|---|
| 调用方（问题 A） | 浪费几个往返、吃几个 429 | ✅ 网关强制拦住 |
| 网关（问题 B） | 上游真的超载、账单真的超支 | ❌ 没有了 |

调用方的桶只是**自我约束**（少发注定失败的请求），网关的桶才是**执法**。所以两侧该用不同方案。

**问题 A · 调用方侧**——不需要引入分布式依赖：

| 方案 | 说明 |
|---|---|
| 本地令牌桶 + `Remaining` 校正 | 首选。各实例从不通信，但都从响应头拿到同一个 `Remaining`，**通过网关间接同步** |
| AIMD（撞 429 降速、平稳时加速） | 上游不告知配额时用（例如直连 DeepSeek——实测它一个限流头都不给） |
| ~~Redis~~ | 为一个「优化」引入分布式依赖不划算，桶算错了网关会兜住 |

这也是 `X-RateLimit-Remaining` 值得存在的理由：`Limit` 是配置（读一次即可，每次响应都一样），`Remaining` 是状态，且是**唯一能跨调用方实例传递的同步信号**。

#### 调用方怎么知道限流是多少

本地桶要先有 `limit` 才能建，可 `limit` 得先调一次才知道——这是个先有鸡还是先有蛋的问题。四条路，按可靠性排序：

| 路子 | 做法 | 代价 |
|---|---|---|
| ① 带外发现端点 | 启动时拉一次配置，一次拿到全部模型的配额 | 需要服务端提供这种端点 |
| ② 带内学习 | 从任意一次响应的 `X-RateLimit-Limit` 头学 | **第一次是盲发的**，冷启动并发高时可能撞一片 429 |
| ③ 文档 / 配置写死 | 按服务商文档手工配 | 文档会过期、账号等级不同、服务商临时调整都不知道 |
| ④ AIMD | 不知道也照发，撞 429 就砍半、平稳就缓慢加回 | 必然要撞几次 429 当探测成本 |

**本网关两条路都提供**：`GET /v1/models` 是路子 ①，每次响应的 `X-RateLimit-*` 头是路子 ②。推荐用 ①，一条业务请求都不用发就能建好所有桶：

```python
# 客户端启动时自举
meta = GET("/v1/models")
buckets = {
    m["model"]: TokenBucket(m["rate_limit"]["requests_per_second"],
                            m["rate_limit"]["burst"])
    for m in meta["models"]
}

# 之后每次调用，按 model 挑对应的桶
buckets[req.model].wait()
send(req)
```

注意桶是**按 model 分的**，不是全局一个——要镜像服务端 [`Limiter`](internal/resilience/ratelimit.go) 里 `map[string]*bucket` 的分桶维度，否则 `deepseek-v4-pro` 的请求会被 `deepseek-v4-flash` 的配额卡住。

现实中这四条路是**分层降级**关系：有元数据端点就用 ①，没有就退 ②，再没有退 ③，全都没有就 ④。不管哪条路建的桶，运行时都要叠加两条纠偏——响应里带 `Remaining` 就用它校正，撞到 429 就无条件降速。

> 参考：GitHub 有 `GET /rate_limit`（路子 ①）；OpenAI 和 Anthropic 只给响应头（路子 ②）；DeepSeek 实测**两者都没有**，只能靠 ③ + ④。

**问题 B · 网关侧**——必须准：

| 方案 | 代价 | 适用 |
|---|---|---|
| Redis 集中式令牌桶（Lua 脚本保证原子性） | 每请求多一次 Redis 往返；Redis 是单点，需要 fail-open 降级 | 配额贵、不能超 |
| 静态切分（每副本配 `rate/N`） | 零协调，但副本数变化要重配、负载不均时浪费配额 | 副本数固定 |
| 本地桶 + 周期同步 | 有滞后，同步周期内可能超发 | 高 QPS，多 1ms 都心疼 |
| ~~AIMD~~ | 执法者不能靠「撞墙」来学规则 | 不适用 |

Redis 方案的算法和本项目内存版**完全一致**（同样的惰性补充公式），只是把 `tokens` / `last` 两个字段挪进 Redis Hash：

```lua
-- KEYS[1]=桶key   ARGV: rate, burst, now(秒,浮点)
local rate, burst, now = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
local d = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(d[1]) or burst
local ts     = tonumber(d[2]) or now
tokens = math.min(burst, tokens + (now - ts) * rate)   -- 惰性补充，与内存版同一公式
local allowed = 0
if tokens >= 1 then tokens = tokens - 1; allowed = 1 end
redis.call('HMSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', KEYS[1], math.ceil(burst / rate) + 1)
return {allowed, tokens}
```

必须用 Lua 是因为「读—算—写」三步要原子完成，否则两个副本同时读到 `tokens=1` 会双双放行。

要落地问题 B，最小改动是把 `Limiter` 抽成接口，内存版和 Redis 版各实现一份，`internal/service` 一行不用改——和本项目的适配器模式是同一个套路。

---

## 用真实 DeepSeek Key 跑通

网关只从环境变量读 Key（配置文件里只写变量名，绝不落明文），默认读 `DEEPSEEK_API_KEY`。

```bash
export DEEPSEEK_API_KEY=sk-xxxxxxxx
./bin/gateway -config configs/gateway.real.json
```

三个模型全部直连真实上游，启动日志：

```
[boot] 注册模型 deepseek-v4-pro    协议=openai_responses    上游=https://api.deepseek.com           Key=已加载(DEEPSEEK_API_KEY, 长度 35)
[boot] 注册模型 deepseek-v4-flash  协议=anthropic_messages  上游=https://api.deepseek.com/anthropic  Key=已加载(DEEPSEEK_API_KEY, 长度 35)
[boot] 注册模型 deepseek-chat      协议=openai_chat         上游=https://api.deepseek.com           Key=已加载(DEEPSEEK_API_KEY, 长度 35)
```

curl 示例和[六大功能那一节](#六大功能的-curl-示例)完全一样，只是把 `[[MOCK:...]]` 指令去掉。以下是实测结果。

**非流式**——同一份请求体只换 `model`：

| 模型 | 协议 | 延迟 | 用量 |
|---|---|---|---|
| `deepseek-v4-pro` | `openai_responses` | 4484ms | `prompt=90 completion=152 reasoning=124` |
| `deepseek-v4-flash` | `anthropic_messages` | 1279ms | `prompt=90 completion=45` |

pro 是推理模型，`reasoning_tokens: 124` 是真实数据——这正是统一层要把 Token 拆成分类统计的意义。

**流式**：

| 模型 | delta 数 | 首 Token 延迟 | 总延迟 |
|---|---|---|---|
| `deepseek-v4-pro` | 23 | 2866ms | 3709ms |
| `deepseek-v4-flash` | 14 | 876ms | 1082ms |

**结构化输出**（`json_schema`），两种协议内部实现完全不同，对外表现一致：

```json
// deepseek-v4-pro   经 text.format
{"reason":"用户明确表达产品体验糟糕并要求退款，带有强烈的不满和负面情绪。","score":0.05,"sentiment":"negative"}
// deepseek-v4-flash 经 tools + tool_choice
{"reason":"这句话明确表达了强烈的负面情绪……","score":-0.9,"sentiment":"negative"}
```

流式 + 结构化走 Anthropic 的 `input_json_delta` 路径，实测 58 个 delta 拼接后仍是合法 JSON。

### 真实上游踩到的一个坑

DeepSeek 的 Anthropic 兼容端点在**思考模式**下会拒绝 `tool_choice: {"type":"tool"}`：

```
400 Thinking mode does not support this tool_choice
```

适配器改用 `tool_choice: {"type":"any"}`。结构化模式下只声明了一个工具，二者语义等价，但 `any` 兼容思考模式。同时 `toUnified` 显式跳过 `thinking` / `redacted_thinking` 块，避免思考内容混进最终答案。

这个坑内置 mock 是发现不了的——**真实上游才会暴露的兼容性问题，是坚持接一次真实 API 的价值所在**。

### 另一个坑：思考模型的 max_tokens

DeepSeek v4 系列会先输出思考块。`max_tokens` 给小了，token 会全部消耗在思考上，正文一个字都没有：

```bash
# max_tokens=100 → content 为空，finish_reason: "length"
# 上游原始返回：stop_reason=max_tokens, content=[{"type":"thinking", 380 字}]
```

这不是网关的问题——`finish_reason: "length"` 已经如实告诉你被截断了。真实调用时 `max_tokens` 至少给到 **500 以上**，尤其是 `deepseek-v4-pro`（实测一次简单问答就烧掉 124 个 reasoning token）。

### 切换上游地址

三个模型的上游都能用环境变量覆盖（优先级高于配置文件），不用改配置就能在真假上游之间切：

```bash
export UPSTREAM_RESPONSES_BASE_URL=http://127.0.0.1:9090   # 协议一
export UPSTREAM_MESSAGES_BASE_URL=http://127.0.0.1:9090    # 协议二
export UPSTREAM_CHAT_BASE_URL=http://127.0.0.1:9090        # 协议三
```

---

## 接口清单

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET`  | `/healthz` | 存活探针 |
| `GET`  | `/v1/models` | 模型路由表：每个模型走哪套协议、限流配置、全局重试策略。**调用方可用它自举本地限流**，见[下文](#调用方怎么知道限流是多少) |
| `POST` | `/v1/chat`<br>`/v1/chat/completions` | **统一调用入口**。`stream=true` 时返回 SSE，否则返回 JSON |
| `POST` | `/v1/prompts` | 新建模板版本（同名自动 +1，旧版本不可变） |
| `GET`  | `/v1/prompts` | 列出全部模板及最新版本 |
| `GET`  | `/v1/prompts/:name?version=N` | 取指定版本，省略或 `latest` 取最新 |
| `GET`  | `/v1/prompts/:name/versions` | 列出全部历史版本 |
| `POST` | `/v1/prompts/:name/render` | 只做变量替换预览，不调模型 |
| `GET`  | `/v1/metrics` | 按模型聚合的 Token / 延迟 / 首 Token 延迟指标 |
| `GET`  | `/v1/traces?limit=N` | 最近 N 次调用明细，含重试轨迹 |

### 统一请求体

```jsonc
{
  "model": "deepseek-v4-pro",          // 必填，决定路由到哪个适配器
  "messages": [{"role":"user","content":"..."}],
  "system": "可选的系统提示词",
  "prompt": {                          // 可选，引用提示词模板；与 messages 可叠加
    "name": "translator",
    "version": 2,                      // 省略或 <=0 表示 latest
    "variables": {"text": "...", "target_lang": "英文"}
  },
  "temperature": 0.3,
  "max_tokens": 512,
  "stream": false,
  "response_format": {                 // 可选，结构化输出约束
    "type": "json_schema",             // text | json_object | json_schema
    "json_schema": {"name":"...","strict":true,"schema":{...}}
  }
}
```

---

## 统一错误码

所有错误都是这个形状（HTTP 状态码 + `error.code`）：

```json
{"error":{"code":"UPSTREAM_ERROR","message":"上游返回 500","retryable":true,"request_id":"req_...","upstream_status":500}}
```

| Code | HTTP | 可重试 | 含义 |
|---|---|---|---|
| `INVALID_REQUEST` | 400 | 否 | 请求体不合法 |
| `MODEL_NOT_FOUND` | 404 | 否 | `model` 没命中路由表 |
| `PROMPT_NOT_FOUND` | 404 | 否 | 模板或版本不存在 |
| `PROMPT_VAR_MISSING` | 400 | 否 | 模板变量没给全 |
| `RATE_LIMITED` | 429 | 否 | 触发网关按模型的限流（本地配额，重试无意义） |
| `STRUCTURED_INVALID` | 502 | **是** | 返回不是合法 JSON 或不满足 schema，重试等于让模型重新生成 |
| `UPSTREAM_AUTH` | 401 | 否 | 上游鉴权失败 |
| `UPSTREAM_INVALID` | 400 | 否 | 上游认为请求不合法 |
| `UPSTREAM_RATE_LIMIT` | 429 | **是** | 上游限流 |
| `UPSTREAM_ERROR` | 502 | **是** | 上游 5xx |
| `UPSTREAM_TIMEOUT` | 504 | **是** | 上游超时 |
| `UPSTREAM_UNAVAILABLE` | 503 | **是** | 连不上上游 |
| `CANCELED` | 499 | 否 | 客户端断开 |
| `INTERNAL` | 500 | 否 | 网关自身异常 |

只有 `retryable=true` 的错误会进入指数退避重试。

---

## 验收脚本

```bash
./scripts/verify.sh
```

脚本会自己编译、拉起假上游（`:19090`）和网关（`:18080`）、跑完全部断言、清理进程。**178 项断言，全部离线，无需真实 Key**：

```
共 178 项断言：通过 178，失败 0

六大功能点全部通过验收。
  A. 双协议路由 · B. 流式输出 · C. 结构化输出
  D. 提示词版本管理 · E. 可观测性 · F. 重试与限流
```

覆盖范围：

- **A（46 项）**：路由表、两协议分发、**上游实际报文取证**（instructions vs system、input vs messages、input_text vs text、max_tokens 补位）、**鉴权头差异取证**、假上游对协议的强校验、消息序列归一（补位 + 归并）、未注册模型
- **B（21 项）**：两个模型的 SSE，Content-Type、meta/delta/done/[DONE] 事件、delta 块数 ≥5、拼接结果与完整文本一致、`ttft_ms > 0`、准备期错误仍返回 JSON
- **C（23 项）**：两个模型的 `json_schema` / `json_object`、`parsed` 对象、enum 命中、Anthropic 侧走 `tools`+`tool_choice`、Responses 侧走原生 `text.format`、流式结构化拼接后仍是合法 JSON、非法 JSON 触发 `STRUCTURED_INVALID`
- **D（25 项）**：版本自增、变量自动解析、按版本取用、latest 语义、渲染无残留占位符、缺变量拦截、`prompt_ref` 回传、渲染结果真的发到了上游、**重启后落盘数据完好**
- **E（26 项）**：响应头、Token 分类统计自洽（`total = prompt + completion`）、`cached`/`reasoning` 分类、延迟分位数（`p95 ≥ p50`）、首 Token 延迟、按模型分组、调用明细与重试轨迹
- **F（34 项）**：退避区间落在 `200ms±20%` / `400ms±20%` 且递增、重试上限 4 次尝试、不可重试错误只试 1 次、上游 429/401/500 各自归一、**流式在首 Token 之前可重试**、限流 429 与响应头、**按模型独立（一个被打满不牵连其它）**、令牌补充后恢复、限流与重试计入指标

脚本第 0 步会先跑 `go build` / `go vet` / `go test`，所以单元测试也在验收范围内。

单元测试有 50 个用例，覆盖纯逻辑部分：协议翻译（两个适配器的请求/响应双向映射、消息序列归一、结构化输出的两种表达）、SSE 解析、指数退避区间、令牌桶与按模型隔离、错误码分流、JSON 抽取与 Schema 校验、模板版本递增与落盘、指标聚合与分位数。

```bash
go test ./...
```

---

## 内置假上游与故障注入

`cmd/mockupstream` 同时实现三套协议，并对协议做强校验：

| 端点 | 协议 | 强校验 |
|---|---|---|
| `POST /v1/responses` | OpenAI Responses | 出现 `messages` 字段 → 400；部件类型不是 `input_text`/`output_text` → 400 |
| `POST /v1/messages` | Anthropic Messages | 缺 `anthropic-version` 头 → 400；缺 `max_tokens` → 400；首条非 user → 400；角色不交替 → 400；出现 `input`/`max_output_tokens` → 400 |
| `POST /chat/completions` | OpenAI Chat | 缺 `messages` → 400 |
| `GET /mock/last-request` | — | 取证：看上游最近一次实际收到的报文与头（Key 已掩码） |
| `POST /mock/reset` | — | 清空故障注入计数器 |

故障注入靠**写在消息文本里的魔法指令**，它会随着协议翻译一路传到上游，所以能真实地端到端触发网关的重试与错误分流：

| 指令 | 效果 |
|---|---|
| `[[MOCK:fail=2,id=abc]]` | 同一个 `id` 的前 2 次请求返回 500，第 3 次成功 |
| `[[MOCK:status=429]]` | 直接返回指定状态码 |
| `[[MOCK:delay=800]]` | 延迟 800ms 再响应（可用于触发超时） |
| `[[MOCK:badjson]]` | 结构化场景下故意返回非法 JSON |
| `[[MOCK:chunks=8]]` | 流式时切成 8 块下发 |

---

## 目录结构

```
.
├── cmd/
│   ├── gateway/            网关入口：加载配置 → 装配适配器 → 起 HTTP → 优雅退出
│   └── mockupstream/       内置假上游：三套协议 + 协议强校验 + 故障注入 + 取证端点
├── internal/
│   ├── apierr/             统一错误码体系（含上游状态码/网络异常的归一）
│   ├── config/             配置加载（环境变量 > 配置文件 > 默认值；Key 只读环境变量）
│   ├── llm/                统一抽象层
│   │   ├── types.go            协议中立的 Request/Response/Usage/StreamEvent
│   │   ├── adapter.go          Adapter 接口 + Registry 动态路由表
│   │   ├── transport.go        适配器共用的 HTTP 层
│   │   ├── sse.go              SSE 解析
│   │   ├── openai_responses.go   协议一适配器
│   │   ├── anthropic_messages.go 协议二适配器
│   │   └── openai_chat.go        协议三适配器（真实 DeepSeek 通道）
│   ├── prompt/             模板存储 + 版本管理 + 变量替换（JSON 落盘）
│   ├── resilience/         指数退避重试 + 按模型独立令牌桶限流
│   ├── observability/      调用明细环形缓冲 + 按模型聚合（Token 分类 / 延迟分位数 / TTFT）
│   ├── structured/         JSON 抽取 + JSON Schema 轻量校验
│   ├── service/            编排层：主流程 ①②③④⑤
│   └── server/             HTTP 接入层：路由、中间件、SSE 写出、错误映射
├── docs/reading-guide.md   代码阅读指南（分层顺序 + 练习）
├── docs/sequence.md        五张调用时序图（mermaid）
├── configs/
│   ├── gateway.json        模型路由表（上游=内置 mock，离线验收用）
│   └── gateway.real.json   模型路由表（上游=真实 DeepSeek）
├── scripts/verify.sh       全功能验收脚本（178 项断言）
└── Makefile
```
