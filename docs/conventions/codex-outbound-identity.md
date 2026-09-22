# Codex 出站身份与路由补丁

本文件记录当前检出可核对的出站约定；此前 AGENTS 引用的文档未包含在仓库中。

- OAuth/setup-token 出站身份继续通过现有 buildUpstreamRequest / buildPassthroughUpstreamRequest、指纹投影和身份收口生成；打票复用这些构造流程。协议事实需以 openai/codex 源码为准。
- Cookie 锁定是显式开启的 HTTP 路由功能，配置键 `openai_cookie_lock`，独立于 `openai_turn_state_auto` 与手填 state。仅开启 Cookie 锁定时不替换 state；已有客户端 state 沿用既有转发守卫。
- 沿用每账号独立池，不跨账号共享。`openai_cookie_pool` 按 pod 与请求模型维护 `__oailb`、可选 `__cflb`、iat/exp、最近实际模型与失败信息；不转发 `__cf_bm`。JWT 只作路由元数据，绝不用其 host 构造目标 URL。
- 仅选择未过 JWT exp 且最近实际模型匹配的候选。模型比较沿用去除 `openai/` 前缀的规则。响应变更 host 时使旧候选失效；实际模型偏离、删除 cookie 时停止选用。迟到旧请求不得覆盖新请求的观测。
- Cookie 猎手读取 `response.created.model` 后关闭响应体，健康条件不依赖 turn-state。响应模型是实测信号，不把某个 pod 永久标成健康，也不把情报当成上游协议保证。
- 沿用代理、小时限额、间隔、空闲门槛和模型级降级暂停，目标每模型 3 个不同健康 pod；无可用 Cookie 时默认直接放行，开启暂停则换号/503。Cookie 模式不套用 state 的 7 天出口冷却。
- 到期前至少 2 个 tick（当前 2 分钟）尝试携原 Cookie 续约；只有新的 exp 才延长可用期。仍受探测限额、空闲门槛和上游签发结果约束，不保证续约成功。寻找备用 pod 的退避须为已有候选续约留出窗口。
- WebSocket 沿用既有策略，不参与此 HTTP 自动锁定功能。管理页明确标注范围；不处理两种接管同时开启的冲突。
