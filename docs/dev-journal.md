# dev-journal

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
