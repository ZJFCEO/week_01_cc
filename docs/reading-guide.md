# 代码阅读指南

这个项目 4500 行实现 + 900 行测试。顺序从头读到尾是最低效的方式——因为它的价值不在任何单个文件里，而在**层与层之间的边界**上。

下面按「先制造现象 → 再读契约 → 再读翻译 → 最后读编排」的顺序走，每层给几个**读之前先问自己的问题**，读完能答上来就算过。

---

## 推荐读法：从现象倒推回代码

如果你已经把网关跑起来、并且对比过两份上游报文，**不要按目录层级通读**。挑一个你亲眼见过的现象，直接跳到对应代码，往上读到函数开头。一次只追一个现象。

| 现象 | 代码位置 | 背后的思路 |
|---|---|---|
| `max_tokens: 2048` 凭空出现在 Anthropic 报文里 | `anthropic_messages.go` · `buildPayload` | 协议必填字段由适配器兜底，不污染统一层 |
| `instructions` vs 顶层 `system` | 两个适配器的 `buildPayload` | 同一语义、两种形式 |
| 两个模型的 `prompt_tokens` 完全相同 | 上面两个函数的共同结果 | 翻译只改形式、不改语义 |
| `reasoning_tokens` 一个有值一个是 0 | 两个适配器的 `toUnified` | 字段名归一，但颗粒度由上游协议决定 |
| thinking 块没混进 `content` | `anthropic_messages.go` · `toUnified` | 内容只给最终答案，用量如实计费 |
| 响应里的 `protocol` 字段 | `adapter.go` 的接口 + `Registry` | 动态路由 |
| 429 + `Retry-After` 头 | `ratelimit.go` · `Allow` | 令牌桶惰性补充，不需要后台 goroutine |
| `attempt_trace` 里退避 205ms→434ms | `retry.go` · `Do` | 指数退避 + 抖动 |
| 真实上游拒绝 `tool_choice:{type:tool}` | `anthropic_messages.go` · `buildPayload` | 兼容性问题只有真实上游能暴露 |

---

## 五个真正值得学的设计决策

**① 统一层的字段必须是语义，不是形式**

`types.go` 的 `Usage` 里有 `ReasoningTokens`——虽然只有 Responses 协议暴露它，但「输出中属于思维链的部分」是**语义**概念，两种协议都可能有，只是一个说一个不说。而 `instructions`、`input_text` 是**形式**，绝不能出现在这层。这条判据能决定任何字段该不该往上提。

**② 策略与机制分离**

`apierr/errors.go` 的 `specs` 是一张表：错误码 → (HTTP 状态, 是否可重试)。重试器从头到尾只问一句 `Retryable`，它不认识任何协议、任何 HTTP 状态码。加新错误码只改这张表，重试逻辑一行不动。

**③ 重试圈该包住什么，是个判断题**

`gateway.go` 里 `validateStructured` 在 `resilience.Do` 的闭包**里面**——等于宣布「模型吐出非法 JSON」和「上游 500」是同一类失败。这不是唯一答案，是个设计决策，你要能说出为什么。

**④ 不可逆操作要主动放弃重试**

`InvokeStream` 里的 `emitted > 0`：一旦吐出第一个字节，后续错误一律标成不可重试。已发出的内容收不回来，重试只会让客户端看到重复输出。

**⑤ 测试替身要能证伪，不只是能返回数据**

`cmd/mockupstream` 对协议做强校验（缺 `anthropic-version` 直接 400）。所以「调通了」本身就是「协议写对了」的证据。这个思路比整个网关实现更值钱。

---

## 半天的动手顺序

| 时长 | 内容 |
|---|---|
| 30 min | `types.go` + `adapter.go`（共 190 行）建立契约概念 |
| 90 min | 分屏对照两个适配器，按函数配对读 |
| 60 min | `gateway.go` 的 `Invoke()` 跟 ①②③④⑤，再只看 `InvokeStream()` 的差异 |
| 30 min | 做一个练习——改坏它，并预测怎么坏 |

---

## 附：如果还没跑过，先制造现象（15 分钟）

别急着打开代码。先让「同一份请求变成两份报文」这件事在你脑子里有画面。

**必须用 mock 配置**（`configs/gateway.json`），真实 DeepSeek 不会告诉你它收到了什么。两个终端：

```bash
./bin/mockupstream -listen :9090
```

```bash
make run
```

然后用**完全相同**的请求体，只换 `model`，打两次：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-pro","system":"你是助手","messages":[{"role":"user","content":"hi"}]}' > /dev/null

curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-flash","system":"你是助手","messages":[{"role":"user","content":"hi"}]}' > /dev/null
```

看上游实际收到了什么：

```bash
curl -s http://127.0.0.1:9090/mock/last-request | python3 -m json.tool
```

**你会看到两份连字段名都对不上的报文。** 后面读代码，就是在回答「这个转换是在哪一行发生的」。

---

## 第 1 层：契约（30 分钟 · 190 行）

| 文件 | 行数 |
|---|---|
| `internal/llm/types.go` | 116 |
| `internal/llm/adapter.go` | 73 |

这是整个项目的地基：**协议中立的类型定义**。所有适配器进出的都是这几个结构。

**读之前问自己：**

1. 如果要让调用方完全不知道底层走的是哪套协议，这一层的类型里**绝对不能**出现什么？
2. `Usage` 为什么要拆出 `CachedTokens` 和 `ReasoningTokens`，而不是只留 prompt/completion/total 三个数？
3. `Temperature` 为什么是 `*float64` 而不是 `float64`？
4. `Adapter` 接口只有 3 个方法。为什么 `Stream` 返回 `<-chan StreamEvent`，而不是接一个回调函数？

> 第 3 题的答案在「怎么区分『没传』和『传了 0』」。
> 第 4 题想想：如果是回调，调用方要在回调里做重试判断，控制流就散了。

---

## 第 2 层：翻译（核心 · 1~2 小时）

**这一层是整个项目最值得学的地方。关键是对照着读，不要顺序读。**

打开两个编辑器分屏，左边 `openai_responses.go`，右边 `anthropic_messages.go`，然后按函数配对：

| 先读（左） | 再读同名函数（右） | 盯着看什么 |
|---|---|---|
| `buildPayload` | `buildPayload` | system 去哪了？消息容器叫什么？`max_tokens` 谁必填？ |
| `headers` | `headers` | 鉴权头完全不同 |
| `toUnified` | `toUnified` | 内容从哪一层取出来？用量字段怎么改名？ |
| `Stream` | `Stream` | 两套完全不同的 SSE 事件怎么变成同一种 |

**三段重点，每段停下来想清楚再往下：**

1. **`normalizeMessages`**（anthropic_messages.go）
   Messages 协议要求「首条必须是 user、角色严格交替」。统一层不做这个约束，差异就得在这里补：相邻同角色归并、首条是 assistant 就补位。
   *问自己：为什么这个函数不放在统一层做？*

2. **结构化输出的两种表达**（两个文件的 `buildPayload` 末尾）
   Responses 有原生 `text.format`，一行搞定；Messages **根本没有** `response_format`，只能翻译成「强制调用一个入参就是目标 schema 的工具」，再把 `tool_use.input` 还原成 JSON 文本。
   *这是「协议差异被适配器吃掉」最有说服力的例子。*

3. **流式增量的两种类型**（anthropic_messages.go 的 `Stream`）
   普通文本走 `delta.text`，结构化输出走 `delta.partial_json`。适配器把两者都转发成同一种统一 delta。

**自测：** 打开 `internal/llm/adapter_test.go` 的 `TestStructuredTranslation`。它在一个测试里同时断言了两种协议的不同表达——先别看，试着自己说出它断言了什么。

---

## 第 3 层：编排（1 小时 · 427 行）

`internal/service/gateway.go`

先读 `Invoke()`，跟着代码里的 ①②③④⑤ 注释走。再读 `InvokeStream()`，但**只看它和 `Invoke` 的差异**。

**三个设计决策，每个都值得停下来想为什么：**

1. **限流失败为什么不进重试？**
   本地配额问题，重试只是把自己再撞一次墙。所以 `RATE_LIMITED` 在 `apierr` 里被标成 `retryable: false`。

2. **`validateStructured` 为什么放在 `resilience.Do` 的闭包*里面*？**
   因为「模型吐出非法 JSON」和「上游 500」一样，都是一次值得重来的失败。
   *可以实测：*
   ```bash
   curl -s -X POST http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' \
     -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"给我 JSON [[MOCK:badjson]]"}],"response_format":{"type":"json_object"}}' > /dev/null
   curl -s 'http://127.0.0.1:8080/v1/traces?limit=1' | python3 -m json.tool | grep -E 'attempts|error_code'
   ```
   会看到 `attempts: 4`（1 次首发 + 3 次重试）。

3. **流式为什么 `emitted > 0` 之后要把错误标成不可重试？**
   已经吐给客户端的内容收不回来了，重试会导致输出重复。这是写流式最容易踩的坑。

---

## 第 4 层：横切能力（可跳读，各 30 分钟）

这几个包互不依赖，挑感兴趣的看。

| 文件 | 行数 | 值得学的点 |
|---|---|---|
| `internal/apierr/errors.go` | 174 | `specs` 表把「HTTP 状态 + 是否可重试」绑定在错误码上。重试器只问 `Retryable`，完全不认识任何协议——**策略与机制分离** |
| `internal/resilience/retry.go` | 105 | `Do` 是个高阶函数，把重试逻辑和业务逻辑彻底解耦。业务只管返回 error，重试器只管看错误码 |
| `internal/resilience/ratelimit.go` | 125 | 令牌桶**惰性补充**：只在被访问时按流逝时间补令牌，不需要任何后台 goroutine |
| `internal/observability/collector.go` | 270 | 环形缓冲防止长跑内存无限增长；分位数在**读**的时候才算，写路径保持轻 |
| `internal/prompt/store.go` | 305 | 版本快照不可变，永不覆盖；变量缺失直接报错，而不是把 `{{text}}` 静默发给模型 |
| `internal/structured/validate.go` | 191 | 容错抽取：先剥 Markdown 围栏，失败再截最外层括号。**对模型的输出要有防御心** |
| `internal/llm/transport.go` | 106 | 为什么 `http.Client` 不设 `Timeout` 而用 per-call context？（设了会把流式响应从中间掐断） |
| `internal/llm/sse.go` | 76 | SSE 是行协议，空行是事件边界。注意 EOF 时对未闭合事件的兜底 |

---

## 第 5 层：怎么证明自己写对了（最值得学的一层）

| 文件 | 行数 |
|---|---|
| `cmd/mockupstream/main.go` | 745 |
| `scripts/verify.sh` | — |

**核心思路：测试替身不能只会返回数据，它得能证伪。**

内置假上游做了三件普通 mock 不做的事：

1. **协议强校验**——Messages 端点缺 `anthropic-version` 或 `max_tokens` 直接 400，首条非 user 直接 400；Responses 端点收到 `messages` 或 `max_tokens` 直接 400。所以「调通了」本身就是「协议写对了」的证据。
2. **取证端点**——`GET /mock/last-request` 让你直接看上游收到了什么，不用猜。
3. **故障注入走业务链路**——`[[MOCK:fail=2,id=xxx]]` 写在消息文本里，会**随着协议翻译一路传到上游**，所以触发的是真实的端到端重试，不是打桩出来的假象。

`scripts/verify.sh` 的 177 项断言里，最有价值的不是「返回 200」那类，而是这几类：

- 断言上游收到的报文**不含**另一种协议的字段（`assert_hasnt`）
- 断言退避时长落在 `200ms ± 20%` 区间内且递增
- 断言一个模型被限流时另一个模型**仍然可用**
- 断言所有 delta 拼接后**等于** done 事件里的完整文本

---

## 三个练习（改代码，看现象）

读懂了不算数，改一行让它坏掉、并且能预测怎么坏，才算真懂。

### 练习 1：删掉 max_tokens 兜底

`internal/llm/anthropic_messages.go` 的 `buildPayload` 里注释掉：

```go
if p.MaxTokens <= 0 {
    p.MaxTokens = defaultAnthropicMaxTokens
}
```

不带 `max_tokens` 调 `deepseek-v4-flash`。

**预测：** 上游 400 → 网关归一成 `UPSTREAM_INVALID` → 对外 400，且 `retryable: false`（不会白白重试 3 次）。

### 练习 2：把结构化校验挪出重试圈

`internal/service/gateway.go` 的 `Invoke()` 里，把 `validateStructured(...)` 从 `resilience.Do` 的闭包里挪到外面。

用 `[[MOCK:badjson]]` 调一次，看 `/v1/traces`。

**预测：** `attempts` 从 4 变成 1。

### 练习 3：让适配器「串味」

`internal/llm/openai_responses.go` 的 `oaResponsesRequest` 里加一个 Anthropic 的字段：

```go
MaxTokens int `json:"max_tokens,omitempty"`
```

并在 `buildPayload` 里给它赋值。

**预测：** 假上游立刻 400，报「Responses 协议不接受 messages/max_tokens 字段」。这就是协议强校验的价值——串味当场被抓。

---

## 一个可以自己验证的结论

适配器的边界写得对不对，有个客观判据：**编排层和 HTTP 层里，协议名应该出现 0 次。**

```bash
grep -rc 'openai_responses\|anthropic_messages\|openai_chat' internal/service/ internal/server/
```

全是 0。这意味着加第四种协议时，只需要：

1. 在 `internal/llm/` 加一个新文件
2. `cmd/gateway/main.go` 的 switch 加 2 行
3. `internal/config/config.go` 的校验加 1 个字符串

`internal/service/` 和 `internal/server/` 一行都不用改。

项目里的第三个适配器 `openai_chat.go` 就是这么加上去的——它是这个结论的活证据。

---

## 配合时序图看

[docs/sequence.md](sequence.md) 的五张图和上面的分层对应：

| 图 | 对应本指南的 |
|---|---|
| 图 3 双协议翻译 | 第 2 层 |
| 图 1 非流式主流程 | 第 3 层 |
| 图 2 流式输出 | 第 3 层（`InvokeStream` 的差异） |
| 图 4 结构化输出 | 第 2 层的重点 2 |
| 图 5 提示词版本引用 | 第 4 层 |
