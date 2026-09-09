# 调用时序图

五张图覆盖六大功能点的全部流转。图里的编号与 `internal/service/gateway.go` 里的 ①②③④⑤ 注释一一对应。

- [1. 非流式主流程](#1-非流式主流程)——统一抽象层、限流、重试、统一错误码、可观测
- [2. 流式输出](#2-流式输出sse)——SSE 逐块下发、首 Token 延迟打点、首块之前可重试
- [3. 双协议翻译](#3-双协议翻译)——同一份统一请求落到上游是两份完全不同的报文
- [4. 结构化输出](#4-结构化输出)——两种协议、两条实现路径、一套兜底校验
- [5. 提示词版本引用](#5-提示词版本引用)——存储、变量替换、按版本引用

---

## 1. 非流式主流程

`internal/service/gateway.go` · `Invoke()`

调用方只递一份统一请求。校验与动态路由决定了走哪套协议，限流按模型独立扣令牌，重试把「调用 + 结构化校验」整体包住——所以模型吐出非法 JSON 也会被当成一次可重试的失败。无论成功失败，最后都会落一条可观测记录。

```mermaid
sequenceDiagram
    autonumber
    actor C as 调用方
    participant S as HTTP 接入层
    participant G as 编排层
    participant L as 限流器
    participant R as 重试器
    participant A as 协议适配器
    participant U as 上游 API
    participant M as 指标收集

    C->>S: POST /v1/chat
    S->>S: 分配 X-Request-Id
    S->>G: Invoke(ctx, requestID, req)

    Note over G,L: ① 准备：校验 → 动态路由 → 模板渲染
    G->>G: 校验 model / response_format
    G->>G: Registry.Resolve(model)<br/>命中协议与适配器实例
    opt 请求里带 prompt 引用
        G->>G: 取模板版本 + 变量替换（详见图 5）
    end

    Note over G,L: ② 限流：每个模型一个独立令牌桶
    G->>L: Allow(model)
    alt 桶里没有令牌
        L-->>G: 拒绝 + Retry-After
        G->>M: Record(error, RATE_LIMITED)
        G-->>S: RATE_LIMITED（不进重试）
        S-->>C: 429 + Retry-After / X-RateLimit-*
    else 放行
        L-->>G: 扣掉 1 个令牌
    end

    Note over G,U: ③④ 重试包住「调用 + 结构化校验」
    G->>R: Do(policy, fn)
    loop 1 次首发 + 最多 3 次重试
        R->>A: Invoke(ctx, 统一请求)
        A->>A: 翻译：鉴权头 · 请求体结构 · 字段名
        A->>U: POST /v1/responses 或 /v1/messages
        alt 上游 2xx
            U-->>A: 该协议原生的响应体
            A->>A: 翻译回统一 Response<br/>内容 · 用量分类 · finish_reason
            A-->>R: llm.Response
            opt 要求结构化输出
                R->>R: 抽出 JSON + Schema 校验
                Note right of R: 非法 JSON 或不满足 schema<br/>→ STRUCTURED_INVALID（可重试）
            end
        else 上游 4xx / 5xx / 超时 / 连不上
            U-->>A: 各协议自己的错误结构
            A->>A: 按状态码归一成统一错误码
            A-->>R: apierr.Error
        end
        alt 本次成功
            R-->>G: 结果 + attempt_trace
        else 可重试且还有额度
            R->>R: 退避 200ms → 400ms → 800ms（±20% 抖动）
        else 不可重试 或 额度耗尽
            R-->>G: 末次错误 + attempt_trace
        end
    end

    Note over G,M: ⑤ 落可观测数据（成功失败都记）
    G->>M: latency · usage 分类 · attempts · error_code
    G-->>S: ChatResponse
    S-->>C: 200 + X-Latency-Ms / X-Upstream-Protocol / X-Retry-Count
```

**读图要点**

- 限流失败**不进重试**：本地配额问题重试没有意义，直接 429 抛给调用方。
- 结构化校验在重试圈**里面**：模型偶尔吐出带解释文字的 JSON，重试一次通常就好了。
- `attempt_trace` 会随响应返回，每次失败的错误码和实际退避时长都能看到。
- **用量按尝试累计**：只要上游返回了响应，Token 就已经计费，哪怕结果因为 JSON 不合法被判废。指标里记的是真实花销，不是最后一次的用量。

---

## 2. 流式输出（SSE）

`internal/service/gateway.go` · `InvokeStream()`

SSE 写出器采用「懒写头」：在真正吐出第一个字节之前，准备阶段的错误仍然能以标准 JSON + 正确状态码返回。首块内容到达时打点 `ttft_ms`。一旦开始输出就不再重试——否则客户端会看到重复内容。

```mermaid
sequenceDiagram
    autonumber
    actor C as 调用方
    participant S as SSE 写出器
    participant G as 编排层
    participant A as 协议适配器
    participant U as 上游 API

    C->>S: POST /v1/chat  stream=true
    S->>G: InvokeStream(ctx, requestID, req, sink)
    Note over S,G: 懒写头：还没写出字节之前，<br/>准备期错误仍返回标准 JSON + 正确状态码

    G->>G: ① 准备（校验 · 路由 · 模板渲染）
    G->>G: ② 限流（配额写进响应头，此时头还没提交）

    Note over G,U: ③ 建流：一个字节都还没写出去，失败可以重来<br/>而且能返回正确的 HTTP 状态码
    loop 首 Token 之前最多重试 3 次
        G->>A: Stream(ctx, 统一请求)
        A->>U: stream=true
        alt 建流失败
            U-->>A: 4xx / 5xx / 超时
            A-->>G: apierr.Error（retryable 则退避后重来）
            Note over C,G: 重试耗尽 → 标准 JSON + 502/401/429<br/>不是 200 套 SSE error
        else 建流成功
            U-->>A: SSE 事件流
            G->>S: Meta(requestID, model, protocol)
            S-->>C: event: meta（首个字节，响应头在此定型）
        end
    end

    Note over A,U: ④ 逐块转发：两种协议的事件名完全不同，<br/>在适配器里归一成同一种 delta
    U-->>A: response.output_text.delta / content_block_delta
    A-->>G: StreamEvent{Delta}
    G->>G: 首块到达 → 打点 ttft_ms
    G->>S: Delta(text)
    S-->>C: event: delta（立即 flush）

    loop 后续每一块
        U-->>A: 增量事件
        A-->>G: StreamEvent{Delta}
        G->>S: Delta(text)
        S-->>C: event: delta
    end
    Note over G: 已经吐过内容之后再出错，<br/>标记为不可重试，避免客户端看到重复输出

    U-->>A: response.completed / message_stop
    A->>A: 汇总用量：Messages 协议的输入用量在 message_start，<br/>输出用量在 message_delta，要分段累积
    A-->>G: StreamEvent{Done, Response}
    G->>G: ⑤ 结构化校验（如有）+ 落指标
    G->>S: Done(ChatResponse)
    S-->>C: event: done（完整文本 · usage · ttft_ms · latency_ms）
    S-->>C: data: [DONE]
```

**读图要点**

- `meta` 事件把本次实际命中的协议回传给客户端，方便排查。
- 所有 `delta` 拼接起来必须等于 `done` 里的完整文本，验收脚本对这一条有断言。
- `[DONE]` 哨兵是为了兼容按 OpenAI 习惯写的客户端。
- **断流不算成功**：上游没发终止事件就断开，一律报 `UPSTREAM_ERROR`，绝不拿半段文字冒充完整回复。
- **meta 发在上游握手之后**：这样建流阶段的失败还能返回正确的 HTTP 状态码（502/401/429），而不是先写 200 再用 SSE `error` 事件找补。成功路径的事件顺序完全不变，meta 仍是客户端收到的第一个事件。

---

## 3. 双协议翻译

`internal/llm/openai_responses.go` · `internal/llm/anthropic_messages.go`

同一份统一请求，经过两个适配器之后，落到上游是两份连字段名都对不上的报文；两份原生响应回来，又被还原成同一种结构。这就是「协议差异被适配器吃掉」的全部含义。

```mermaid
sequenceDiagram
    autonumber
    participant G as 编排层（协议中立）
    box rgb(246,237,220) 协议一 · OpenAI Responses
    participant AR as Responses 适配器
    participant UR as POST /v1/responses
    end
    box rgb(228,230,248) 协议二 · Anthropic Messages
    participant AM as Messages 适配器
    participant UM as POST /v1/messages
    end

    Note over G: 同一份统一请求<br/>system = 你是助手<br/>messages = [ user: 解释适配器模式 ]<br/>max_tokens 未设置

    G->>AR: Invoke(req)
    AR->>UR: Authorization: Bearer sk-***<br/>{ instructions: 你是助手,<br/>  input: [ { role: user,<br/>    content: [ { type: input_text } ] } ] }
    UR-->>AR: { output: [ { content: [ { type: output_text } ] } ],<br/>  usage: { input_tokens, output_tokens,<br/>    input_tokens_details.cached_tokens } }
    AR-->>G: llm.Response{ Content, Usage, FinishReason }

    G->>AM: Invoke(req)
    AM->>AM: max_tokens 未设置 → 兜底 2048<br/>相邻同角色归并 · 首条非 user 则补位
    AM->>UM: x-api-key: sk-***<br/>anthropic-version: 2023-06-01<br/>{ system: 你是助手,<br/>  messages: [ { role: user,<br/>    content: [ { type: text } ] } ],<br/>  max_tokens: 2048 }
    UM-->>AM: { content: [ { type: text } ],<br/>  stop_reason: end_turn,<br/>  usage: { input_tokens,<br/>    cache_read_input_tokens } }
    AM-->>G: llm.Response{ Content, Usage, FinishReason }

    Note over G: 两条路径回到编排层时结构完全一致<br/>调用方永远不知道下面走的是哪套协议
```

**读图要点**

- 鉴权就不一样：`Authorization: Bearer` 对 `x-api-key` + `anthropic-version`。
- `system` 一个叫 `instructions` 放顶层，一个叫 `system` 放顶层但**不允许**出现在消息数组里。
- `max_tokens` 在 Messages 协议里是必填，统一层没给就由适配器兜底。
- 用量字段名也不同：`input_tokens_details.cached_tokens` 对 `cache_read_input_tokens`，归一到同一个 `Usage`。

内置假上游 `cmd/mockupstream` 对这些差异做了强校验（缺 `anthropic-version` 或 `max_tokens` 直接 400），并提供 `GET /mock/last-request` 取证端点，可以直接看到上游实际收到了什么。

---

## 4. 结构化输出

`internal/llm` 适配器 + `internal/structured`

Responses 协议原生支持 `text.format`；Messages 协议根本没有 `response_format`，只能翻译成「强制调用一个入参就是目标 schema 的工具」，再把 `tool_use.input` 还原回一段 JSON 文本。两条路径殊途同归，最后都过一遍网关侧的兜底校验。

```mermaid
sequenceDiagram
    autonumber
    participant G as 编排层
    participant AR as Responses 适配器
    participant AM as Messages 适配器
    participant V as 结构化校验

    Note over G: response_format = json_schema<br/>schema: sentiment 是 enum，score 是 number

    G->>AR: 本协议原生支持结构化输出
    AR->>AR: 翻译成 text.format<br/>{ type: json_schema, name, strict, schema }
    AR-->>G: content 就是一段 JSON 文本

    G->>AM: 本协议没有 response_format
    AM->>AM: 翻译成 tools + tool_choice<br/>强制调用一个入参就是该 schema 的工具
    AM->>AM: 上游回 content[].type = tool_use
    AM->>AM: 把 tool_use.input 原样序列化<br/>还原成统一层的一段 JSON 文本
    AM-->>G: content 就是一段 JSON 文本
    Note over AM: 流式下增量走 delta.partial_json 而不是 delta.text，<br/>适配器把两者都转发成同一种 delta

    G->>V: Extract(content) + ValidateSchema
    alt 合法且满足 schema
        V-->>G: parsed 对象，随响应一起返回
    else 非法 JSON / 缺必填字段 / enum 不匹配
        V-->>G: STRUCTURED_INVALID（可重试，等于让模型重新生成）
    end
```

**读图要点**

- `Extract` 容忍模型加的 Markdown 代码围栏和前后解释文字，会截出最外层的 JSON 主体。
- 网关侧再校验一遍的意义：三种协议对结构化输出的支持强度不同，只有在这一层统一校验，调用方才能拿到一致的保证。
- 第三条真实通道（DeepSeek 的 Chat Completions）只支持 `json_object`，适配器会把 `json_schema` 降级成 `json_object` 并把 schema 作为约束注入 system。

---

## 5. 提示词版本引用

`internal/prompt/store.go`

同名模板每次提交都追加一个新版本，历史版本不可变、永远可以被引用。变量从模板文本里自动解析；调用时缺变量会直接报错，而不是把占位符原样发给模型。

```mermaid
sequenceDiagram
    autonumber
    actor C as 调用方
    participant S as HTTP 接入层
    participant P as 模板仓库
    participant D as data/prompts.json
    participant G as 编排层
    participant A as 协议适配器

    C->>S: POST /v1/prompts  name=translator
    S->>P: Create(name, system, messages)
    P->>P: 扫描模板文本，自动解析出变量名
    P->>P: 版本号 = 当前最大值 + 1
    P->>D: 全量落盘（重启不丢）
    P-->>C: v1  variables: [ target_lang, text ]

    C->>S: POST /v1/prompts  同名再提交一次
    S->>P: Create(...)
    P-->>C: v2（v1 原样保留，永不覆盖）

    Note over C,A: 调用时按版本引用
    C->>S: POST /v1/chat<br/>prompt: { name: translator, version: 2,<br/>  variables: { target_lang, tone, text } }
    S->>G: Invoke(...)
    G->>P: Get(translator, 2)<br/>version 省略或 <=0 则取 latest
    P-->>G: 不可变的 v2 快照
    G->>P: Render(tmpl, variables)
    alt 变量齐全
        P-->>G: 替换后的 system + messages
        G->>A: 渲染结果作为本次调用的消息
        A-->>G: 模型响应
        G-->>C: 200 + prompt_ref: translator@v2
    else 缺变量
        P-->>G: PROMPT_VAR_MISSING
        G-->>C: 400（不会把占位符原样发给模型）
    end
```

**读图要点**

- `prompt_ref` 会随响应返回并写进可观测记录，能追出「这次调用用的是哪个模板的哪一版」。
- 模板的 `system` 排在调用方 `system` 之前，模板消息排在显式 `messages` 之前，可以叠加使用。
- `POST /v1/prompts/:name/render` 只做渲染预览，不调模型，用来在花 Token 之前确认替换结果。
