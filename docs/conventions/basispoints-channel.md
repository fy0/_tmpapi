# Basispoints（BPS）渠道约定

参考实现：`bps_proxy.py`（独立实测过的 Basispoints→Responses 反代）。本约定描述
`klno` 分支对 sub2api 的补丁面；协议细节一律以参考实现为准，修改前先逐行对照。

## 上游与画像

- 上游端点 `https://bps.openai.com/basispoints/api/responses`（ChatGPT Excel 插件
  通道）。实测对 OAuth ChatGPT 账号放行真 `gpt-6-astra`，绕过
  `chatgpt.com/backend-api/codex` 的降智调度。
- 头画像（`basispoints.AuthHeaders`）：`chatgpt-account-id` /
  `x-openai-account-id` / `x-basispoints-auth-mode: chatgpt` /
  `x-openai-internal-basispoints-*`（Excel/Office 全套）/ `x-stainless-*`（js
  runtime 画像）/ `origin: https://bps.openai.com` / `user-agent: bps-proxy/0.1`。
  可选 `x-oai-timezone`。**不得**带 Codex 身份头（originator / turn-state 等）。

## 请求白名单（上游严格校验，多一个字段即 422）

只允许：`model` / `model_selection="explicit"` / `stream` / `store=false` /
`input` / `reasoning_effort` / `context_management` / `prompt_cache_key` /
`metadata{task_id,turn_id,agent_iteration}`。

非标准暴露面收敛（整条链路不得带任何非 stock 痕迹）：

- `context_management` 恒定 `[{"type":"compaction","compact_threshold":200000}]`，
  客户端自带值一律不透传。
- developer 消息（string 或 part 级）命中 token_budget 正则
  `</?context_window(_guidance)?>|tokens left in this context window` 即剥除；
  剥空则整条 item 丢弃。
- 估算 input tokens = 序列化 input 字节数 // 3，超 `MaxInputTokens`
  （默认 300000，extra `openai_basispoints_max_input_tokens`）本地 400
  `input_too_large` 拒绝，不打上游。
- effort 别名：`x-high/extra-high/extra_high/max/ultra/xxhigh/xx-high`→xhigh；
  `minimal/minimum/none`→low；其他非法→medium。
- 客户端 `model` 剥 `-excel`（或 `-bps`）后缀后**原样透传**（空才兜底 defaultModel）；
  `model_mapping`/`openai_basispoints_model` 只作兜底，不改写客户端模型。
- `tools`/`tool_choice`/`instructions` 不下发。工具编进 JSON catalog
  developer 消息（文案逐字对齐 `catalog_message`，含 additional_tools 来源、
  namespace 展平）；客户端工具调用经 `run_officejs` envelope 偷渡，内层
  `code` 是紧凑 JSON `{"name","arguments"}` 或 `{"name","input"}`。
- `tool_choice=="none"` 只清空 allowed 查找表（catalog 消息仍照发——参考
  实现如此，勿"修正"）。
- `task_id`/`turn_id` 为 uuid5(NAMESPACE_URL, "bps-proxy/"+conversation[+"/turn/"+fp])；
  `agent_iteration` 是字符串，按最后一条 user 消息之后的 fco/ctco 数+1。
- 会话键优先级：prompt_cache_key / promptCacheKey / session_id / sessionId /
  client_metadata.session_id(sessionId)；都没有则 hash 首条业务 input。

## 输入/输出归一化

- message part：assistant→`output_text`，其他 role→`input_text`，字段名 `text`；
  `input_image` 保留给图片阶段；未知 part 替换为 `[<type> part omitted: ...]`。
- 历史 `function_call`/`custom_tool_call`：State 命中→回放原生 run_officejs
  item；name 命中 catalog→包 envelope；name==transport→登记+透传。
- fco/ctco output：transport 命中收缩成 `{type:"function_call_output",id,call_id}`；
  list output 非文本 part 计数替换 `[N non-text part(s) omitted by proxy]`；
  空 output 补 `(tool call succeeded with no output)`；input_image 仅
  custom_tool_call_output 放行。
- reasoning 只保留 `{type,summary:[],encrypted_content}`；`item_reference` 丢弃。

## 响应

- 上游恒 SSE（整流读完，不做 token 转发）。`response.completed` 优先；内嵌
  `status=="completed"` 次之；`response.failed`/`error` → StreamFailure。
- output 里必须**恰好一个** transport 调用才回译；envelope 剥至多一层；
  name 必须在 catalog；function arguments 过 JSON-Schema 轻校验，失败即弃
  （不向下游放坏调用）。命中后原生 item 以 call_id 记入 State 供下轮回放。
- 流式下游：先提交 200+SSE 头+注释心跳（复用 `startOpenAISSEKeepalive`，
  心跳字节不计入"已写语义响应"判定）；提交后的一切失败降级为
  `response.failed` 事件+[DONE]，不再走 JSON 错误/failover。
- 客户端断开即取消上游请求。

## 图片

`input_image` 的 `data:` URL → 解 base64 → POST `{responses_url 目录}/attachments`
（multipart `file` 字段，文件名 `picture-<digest12>.<ext>`）→ 改写 `file_id` +
`detail:"auto"`。digest→file_id 缓存在每凭据账号 State。失败降级为 input_text
占位（不丢条目）。

## 账号复制（bps 影子）

- `quota_dimension` 枚举 `bps`：与 spark 影子同构——`parent_account_id` 透传母
  账号凭据（**绝不复制 OAuth token**），一母一影（部分唯一索引
  `uq_accounts_bps_shadow_per_parent`）。
- 副本 `extra`：`openai_basispoints=true` + `openai_basispoints_model` 兜底值；
  `credentials` 只含继承自母账号的 `model_mapping`（影子凭据守卫已白名单化）。
- 配额窗口：bps 影子消耗普通 ChatGPT plan 配额，`getOpenAIUsage` 按维度选
  primary 窗口（spark 才看 bengalfox）。
- `ListShadowsByParent` 已改为全维度；「一母一影」用 `ListShadowsByParentDimension`。

## 账号 extra 键

`openai_basispoints`(bool 开关) / `openai_basispoints_model` /
`openai_basispoints_url` / `openai_basispoints_timezone` /
`openai_basispoints_tools_version_id` / `openai_basispoints_timeout_seconds` /
`openai_basispoints_max_input_tokens`。
