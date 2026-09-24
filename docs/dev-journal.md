# 开发日志

## 2026-09-25 — klno 归档为 klno-legacy-2026-09-23

- 原 klno（截至 0ac464828，基于上游 v0.2.7 的 klno.4 标签线）整体改名为 klno-legacy-2026-09-23，日期取最后一次补丁提交。
- 新 klno 从 upstream/main（a3eb7ef30，v0.2.8 之后）重建，只携带 sync-upstream.yml 一份文件，保证 cron 的 main 重同步步骤能 checkout 到它。
- 补丁尚未移植到新 klno；续接工作时以本分支为补丁来源。

## 2026-09-23 — 独立 Cookie 锁定打票

- 增加每账号 `openai_cookie_lock` 开关和按 pod/请求模型分桶的 `openai_cookie_pool`。保留 state 路径，可以只锁定 Cookie。
- HTTP 标准、透传、兼容桥共用出站及响应体观测；采集 __oailb / __cflb，以实际响应模型和 JWT exp 判定候选。支持 host 迁移、模型降级、cookie 删除、过期和迟到响应保护。
- 猎手在 Cookie 模式读取 response.created，补足不同 pod，提前携原 Cookie 续约，保留用量记账、限额与降级暂停；取消该模式的七天出口冷却。
- 管理页增加独立开关、候选数量、pod/实际模型/到期状态与探测结果；Cookie 不在界面明文展示。
- 新增后端回归覆盖独立注入、账号隔离、失效/恢复、过期、原样传流、无 state 探测、续约和新鲜池读取；补前端开关和展示测试。
- 按用户要求不在本地编译或构建：本地只做 gofmt 和 diff 检查，测试、类型检查与 Docker 发布交由 push 后 GitHub Actions。最终结果见对应提交的 CI/GHCR 记录。
