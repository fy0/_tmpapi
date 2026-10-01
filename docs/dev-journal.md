# dev-journal

## 2026-10-01 同步上游 v0.2.11-klno.2（含 gpt-6.1-sol）

- 上游 KlN-4096/sub2api klno 推进到 v0.2.11-klno.2，已内置 gpt-6.1-sol
  （DefaultModels、codexModelMap、专属 instructions codex_gpt61_sol.json、
  effort 校验），且把旧补丁栈整体吸收为 squash 演进版（149 个新提交）。
- 同步方式：本地 55 个提交分两段 rebase——旧栈段
  `rebase -X ours --empty=drop --onto upstream-klno <mb> 3fd1b3de6`，git 自动
  识别「patch contents already upstream」丢弃大半；BPS 尾段手动解冲突。
- 坑：`-X ours` 的 add/add 冲突把 fork 自有的 `sync-upstream.yml`（我们对
  KlN 的版本，上游那份是 KlN→Wei-Shaw 的）和 docs/ 独有文件换成了上游版/
  丢掉，用单独的恢复提交拉回（`dae4ab26d`）；`docs/conventions/` 被上游
  .gitignore 覆盖，`git add` 需 `-f`。
- 另一坑：被部分吸收的提交仍把上游已有内容按旧上下文重复插入（
  CreateCodexBackendReqClient 重定义、buildCodexQuotaHeaders 接管块重复、
  TurnStateCell 接口重复声明、hunter 旧废弃注释、turnStateRecovery 旧
  措辞），单独清理提交回退到上游版本（`28ba7e536`）。这类残留只能靠
  `git diff upstream-klno HEAD` 审计抓出来。
- 验证：`go build ./...` 全量通过；定向测试
  CodexFingerprint*/CPR/BasisPoints 等 ok。
- 剩余本地提交：BPS 系列 + fork CI（ghcr/sync-upstream）+ 恢复/清理提交；
  待 push --force-with-lease。

## 2026-09-30 BPS 429 消化策略：原地等待重发，不冻结账号

- 背景：BPS 上游 429 是 Excel 插件端点的会话内限流，通常几十秒自愈；但
  默认链路 `shouldFailoverUpstreamError(429)=true` → `handle429` →
  `SetRateLimited`/`BlockAccountScheduling`，账号被摘除到 resetAt，代价
  远大于限流本身。
- 改动：
  - `forwardOpenAIBasisPoints` 把上游发送重构成可重入的 `sendUpstream`
    闭包；命中 429 按 Retry-After（缺省 30s、单次上限 90s）原地等待重发
    （≤2 次），keepalive/客户端断开语义不变。
  - 重试耗尽仍 429：透传上游错误体给客户端（`writeOpenAIUpstreamClientError`），
    不 failover、不写账号限流状态——客户端（codex）自带退避。
  - 兜底：`handle429` 与 `markOpenAIOAuth429RateLimited` 对
    `openai_basispoints` 账号早退，覆盖任何绕过 BPS 转发层到达的 429。
- 测试：`openai_basispoints_test.go` 覆盖 429→200 原地重试（2 次上游请求、
  账号无冻结/熔断）与 429 耗尽透传（4 次请求、`rec.Code==429`）。
- 构建：走 GitHub Actions，CI 绿了再说。

## 2026-09-30 BPS 压缩回合：compaction_trigger 路由排除 + item id 前缀契约

- 背景：BPS 渠道首个压缩周期 400 `Invalid 'input[9].id': 'fc_...'. Expected an
  ID that begins with 'ctc'`。根因：`convertNativeToolCall` 把上游
  run_officejs（function_call，`fc_` id）回译成 `custom_tool_call` 时原样
  保留 `fc_` id；上游按类型校验前缀，客户端把带毒 item 固化进会话历史后每
  回合原样回放。日常回合靠 State 回放 + catalog envelope 双兜底；压缩触发
  回合恰好双 miss 落透传分支，坏 id 直送上流。
- 改动：
  - 路由：`forwardOpenAIResponses` 的 BPS 拦截加
    `!HasCompactionTriggerInInput(body)`——remote compaction v2（裸
    /responses + compaction_trigger）回 Codex 通道。BPS 端点无服务端压缩
    实现、永不产出 compaction output item；Codex 通道 sanitizer 顺带剥除
    带病 id，存量会话自愈。
  - 发射端：`convertNativeToolCall` 翻 custom 时 `fc_`→`ctc_` retype（保
    suffix）；新增 `retypeToolCallItemID`（fc_/ctc_/tsc_ 已知前缀互换，
    未知形态不动）。
  - 回放侧：`translateInputItems` 对 call item 透传按类型重冠 id（存量
    自愈）；fco/ctco output 透传剥除非 `fc` 前缀 id（上游 output item
    按通用 fc 命名空间校验，配对靠 call_id）。
- 测试：protocol_test 覆盖 custom 工具 retype + 存量自愈 + output id 剥除；
  openai_basispoints_test 覆盖 compaction_trigger 不走 BPS。
- 残留：v2 压缩成功后 codex 回放 `{type:"compaction",encrypted_content}`，
  过 BPS 透传——上游是否接受待实测；`tool_search_call` 等新 item type 的
  BPS 行为未验证。
- 构建：走 GitHub Actions（不本地编译）。顺手清掉本包存量 errcheck
  （Builder 写 `_ =`、测试断言 comma-ok helper）——`3052aa173` 起 CI 全绿
  （lint/test/shell/frontend/release-helpers + GHCR 镜像构建）。

## 2026-XX-XX basispoints 渠道对齐复刻 + bps 影子副本

- 需求：把已实测的 Python 参考实现 `bps_proxy.py` 对齐移植进网关；旧 Go BPS
  实现未实测，整体视为可移除重写。另加"账号复制"——快速复制一个号专挂 bps。
- 方案：
  - `internal/pkg/basispoints` 逐行对齐参考实现重写（catalog 长文案、
    additional_tools、namespace 展平、run_officejs envelope 内层
    `{name,arguments|input}`、message part `text` 字段、output 归一化、
    uuid5 task/turn、reasoning_effort 别名、data: 图片→/attachments→file_id）。
  - `openai_gateway_basispoints.go` 重写：整流上游 SSE、复用 compact
    keepalive（心跳字节不进 failover 判定）、断线取消上游、已提交后失败降级
    response.failed、State 按凭据账号 sync.Map。
  - "复制账号"落地为凭据影子而非深拷贝（refresh token 轮换会杀副本）：
    `quota_dimension` 新增 `bps`（ent+CHECK 放宽+`uq_accounts_bps_shadow_per_parent`
    部分唯一索引），`CreateShadow` 泛化 `Dimension` 参数（默认 spark），bps 副本
    写 `extra.openai_basispoints=true`、继承母账号 model_mapping/分组/代理/并发/
    优先级，`ListShadowsByParent` 改全维度（级联/守卫需要），一母一影按维度隔离。
  - 前端：账号菜单"创建 BPS 渠道副本"→ `POST /accounts/:id/shadow {dimension:"bps"}`。
  - 用量：bps 影子看 primary 窗口（普通 plan 配额），非 bengalfox。
- 测试：`openai_basispoints_test.go` 网关级断言（白名单 schema/头/profile/
  envelope 回译/回放/确定性 fc_id/422 直写）；`admin_service_spark_shadow_test.go`
  加 `TestCreateShadow_BpsDimension`（extra/mapping 继承/一母一影/维度隔离/非法维度）。
- 约定：新增 `docs/conventions/basispoints-channel.md`。
- 构建：全部走 GitHub Actions（用户明确要求不本地编译），CI 未绿前继续修。
- 跟进参考实现"非标准暴露面"更新：context_management 恒定 stock 值
  （不透传客户端的）；developer 消息剥除 token_budget 泄露片段
  （context_window 标签/"tokens left"句式，整条或 part 级命中即丢）；
  input 序列化 bytes/3 超 300k（`openai_basispoints_max_input_tokens` 可调）
  本地 400 input_too_large 不打上游；effort 别名扩 max/ultra/xxhigh→xhigh、
  minimal/minimum/none→low；model 额外剥 `-bps` 后缀。
