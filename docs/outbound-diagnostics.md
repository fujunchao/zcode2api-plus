# 出站诊断字段（v2.9.18）

`[attempt] event=start` 是实际 HTTP 发送边界；对应 `headers/error` 保留同一份冻结摘要。`response_body` 描述响应读取，`[ingress] event=end http_status` 才是网关实际交付状态。`[#] status` 可能是最后一次上游状态，不能单独当作调用成功。

| 字段 | 含义 |
|---|---|
| outbound_model / outbound_max_tokens / outbound_thinking / outbound_effort | 实际整形后的参数。模型与枚举只输出白名单值，未知为 other；非法数字为 -，不改真实请求 |
| system_text_bytes / system_text_sha256 | 前16个顶层system文本块的UTF-8字节数及SHA-256；无文本的块记录空文本摘要 |
| system_summary_truncated | 超过16块时为true；总块数仍看system_blocks |
| official_prefix_match | 第一块文本是否以当前内置官方固定前缀开头 |
| official_identity_match | 前两块文本是否分别与当前内置固定前缀/身份块完全相同；不代表整个请求符合供应商规则 |
| auth_dual_equal | 实际Bearer与X-Api-Key是否同值，不记录值本身 |
| metadata_session_matches_header | metadata.user_id内会话与发送头的摘要是否匹配 |
| device_header_present / signing_header_present | 实际发送头中是否出现设备头/签名头，不记录其值 |
| account_status_before / risk_streak_before / risk_last_at | 发送前的本地账户快照；不是供应商实时状态 |
| account_state_identity_match | 当前状态快照的凭据是否仍与本次请求所用账号快照一致；false时不可把新身份状态归给旧请求 |
| risk_extra_window_s / risk_extra_active | 管理员配置的额外本地隔离及是否仍在窗口内，默认0/false |
| local_barrier / local_recheck_at | 本地限制类别和最早重新评估时间；ready不代表模型额度充足，失效/停用等不承诺自动恢复 |
| account_checked_at | 最近一次额度检查时间，检查可能失败，不能等同成功快照 |
| quota_observed_at | 当前进程确认且仍匹配账号凭据/出口的有效账单观测时间；重启或查询失败后可为- |
| quota_model_state / quota_available | 模型在本地额度快照中的状态、可用量；未知与0区分，不保证上游实际准入 |
| history_blocked_candidates | risk-proxy换线时，被近期失败历史排除的候选代理条数；不是账号被封次数 |

`history_blocked` 是上述候选数的兼容别名，`history_scope=account_route_candidates` 明确统计口径。历史失败、阶梯连击、当前额外隔离是三个不同概念。

日志不保存用户正文、工具名称/参数/schema、JWT、API Key、认证头、metadata设备或会话标识原文。原有账号/线路关联字段仍用于排障，日志应作为内部运维资料保管。不同层的请求数、账号尝试数和上游响应数必须分开统计。
