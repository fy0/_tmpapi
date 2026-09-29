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
- remote compaction v2（裸 `/responses` + input 末尾 `compaction_trigger`）
  **不走 BPS**：上游无服务端压缩实现、永不产出 `compaction` output item，
  触发回合经它注定失败。网关按 `HasCompactionTriggerInInput` 在路由层排除，
  回 Codex 通道（真上游支持 v2；该通道 sanitize 顺带剥除带病 item id）。
<<<<<<< HEAD
- 上游 429 **不触发账号冻结/failover**：插件端点限流是会话内配额（几十秒
  自愈），与账号级配额无关。`forwardOpenAIBasisPoints` 按 Retry-After
  （缺省 30s、单次上限 90s）原地重发最多 2 次；耗尽仍 429 则透传上游错误
  体给客户端退避。`handle429`/`markOpenAIOAuth429RateLimited` 对
  `openai_basispoints` 账号一律早退，不写 `SetRateLimited`、不运行时熔断、
  不计入全局 429 storm。
=======
>>>>>>> 401cfa985 (fix(basispoints): route compaction_trigger off BPS, retype tool-call item ids)

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
- item id 前缀契约（上游按类型校验，违规 400 "Expected an ID that begins
  with 'ctc'"）：`function_call`→`fc_`、`custom_tool_call`→`ctc_`、
  `tool_search_call`→`tsc_`；output item 统一按 `fc` 命名空间
  （`fco_`/`ctco_` 里只有 fc 前缀放行）。发射端把 run_officejs 翻回
  `custom_tool_call` 时必须把 `fc_` id retype 成 `ctc_`（保 suffix），否则
  客户端历史带毒；回放侧 `translateInputItems` 对 call item 按类型重冠
  （fc_↔ctc_）自愈存量污染，对 output item 剥除非 `fc` 前缀 id
  （配对靠 call_id，id 可省）。

## 响应

- 上游恒 SSE（整流读完，不做 token 转发）。`response.completed` 优先；内嵌
  `status=="completed"` 次之；`response.failed`/`error` → StreamFailure。
- output 中的 transport 调用按原顺序全部回译（支持同一响应中的并行调用）；每个
  envelope 至多剥一层，name 必须在 catalog；function arguments 须过 JSON-Schema
  轻校验。任何一个封装无法完整解析时，整批返回 `invalid_tool_envelope`，不向下游
  放坏调用，也不写入部分 State 缓存。命中后原生 item 以 call_id 记入 State 供下轮
  回放。
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
