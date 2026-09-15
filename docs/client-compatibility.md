# 客户端工具调用与思考兼容

## 当前能力与边界

| 协议 | 工具调用 | 思考输出 |
| --- | --- | --- |
| Anthropic Messages | 原生 tools / tool_use / tool_result 透传 | 原生 thinking 内容块 |
| OpenAI Chat Completions | tools / tool_choice；message.tool_calls 或 delta.tool_calls | message.reasoning_content 或 delta.reasoning_content |
| OpenAI Responses | function 工具；function_call / function_call_output | reasoning 输出项及 reasoning_summary_text 事件 |

网关只适配协议、选择账号、转发模型响应。函数由 Pi、SDK 使用方等客户端执行；
本服务不提供服务器端 bash、文件访问或内建搜索工具运行器。

只看到 role、content 不一定是丢字段：普通回复和首个流分片就可能只有这两项。
必须读取完整流，并实际向模型声明工具；思考输出由模型生成，网关不会凭空补出字段。
发送给 Anthropic 的消息外层通常仍只有 role/content，工具和结果位于 content 数组内。

## 工具控制

- auto：由模型选择；required：要求使用工具；none：禁止；也可指定已声明的函数。
- Chat 指定函数：{"type":"function","function":{"name":"get_weather"}}。
- Responses 指定函数：{"type":"function","name":"get_weather"}。
- parallel_tool_calls=false 转换为 Anthropic 的 disable_parallel_tool_use=true。
- 无工具声明却要求 required、指定不存在的函数、重复名称、格式错误：HTTP 400。
- 仅支持 function 类型。内建 web_search / shell / custom / MCP 等类型不模拟支持，应由客户端封装成函数。
- strict=true 当前返回 400；strict=false 或省略时将 JSON Schema 传给上游，但不承诺约束解码。
- 多个函数调用及多个工具结果按同角色合并，不丢失 call_id 或 tool_use_id。
- Responses 是无状态模式；每轮携带完整历史，previous_response_id 仍不支持。

## 思考程度（v2.0.4 修正）

GLM-5.3 和 GLM-5.3-Flash 的原生档位是 low、high、max，默认且推荐 max。
两者均强制思考，不能关闭。v2.0.3 把自定义 token 预算映射误当成模型能力白名单，
错误拒绝了 max；该限制已移除。

按官方 Coding Plan 的兼容规则归一化：

| 客户端档位 | 原生 effort | 说明 |
| --- | --- | --- |
| none / minimal / low | low | 轻度推理，none 在这里不表示关闭 |
| medium / high | high | 增强推理 |
| xhigh / max | max | 深度推理 |

Chat 使用 reasoning_effort，Responses 使用 reasoning.effort。
内部统一调用 Anthropic 兼容端点，因此转换成 output_config.effort，
不把 OpenAI 顶层参数直接塞给不同协议，也不再用虚构的 budget_tokens 代替原生 effort。
未指定档位时保留上游默认值。

max 与 max_tokens / max_output_tokens 是两个独立控制：前者是推理程度，
后者是本次输出总上限。网关保持调用方的输出上限，不要求它必须达到 16384，
也不再把一半固定划作思考预算。小上限可能导致正常的 length / incomplete 截断，
应根据任务需要调整。

显式 Anthropic thinking.budget_tokens 仍校验并保留（整数、至少 1024 且小于总上限），
但不会替代或覆盖同时请求的原生 effort。
Pi ZAI 的无预算 enabled 开关等同于模型的强制思考默认行为，不会被换成猜测的预算；
clear_thinking 不是 Anthropic thinking 参数，不上行。
thinking.type=disabled 明确返回 400，建议用 low 降低开销；真正未知的档位仍报错。

历史中的 reasoning_content / reasoning item 不会转换成缺签名的 Anthropic thinking；
原生思考签名不向 OpenAI 客户端输出，真实上游多轮保留式思考行为仍需在线验收。

能力依据：
[GLM-5.3-Flash 推荐参数](https://docs.z.ai/guides/vlm/glm-5.3-flash)、
[GLM-5.3 原生档位](https://docs.z.ai/guides/llm/glm-5.3)、
[Coding Plan 兼容映射](https://docs.z.ai/guides/capabilities/thinking)、
[Anthropic effort 参数](https://platform.claude.com/docs/en/build-with-claude/effort)。

## Pi 配置

将 examples/pi-models.json 中 zcode2api 提供商条目合入 Pi 的 models.json，
不要覆盖其它提供商；按实际部署修改 baseUrl，并设置环境变量 ZCODE_GATEWAY_KEY。
示例同时包含 GLM-5.3 和 glm-5.3-flash，使用独立提供商名，不修改其它代理配置。

关键设置：

- reasoning=true：让 Pi 显示思考档位并发送参数；不代表底层模型可以关闭思考。
- thinkingFormat=openai、supportsReasoningEffort=true：发送标准 reasoning_effort；ZAI 格式也兼容。
- supportsStrictMode=false：不请求上游尚未保证的严格工具采样。
- maxTokens=16384 仅是示例输出上限，可根据任务调整，不是 max 档位的启用门槛。
- thinkingLevelMap 暴露原生 low/high/max；off/minimal/medium/xhigh 设 null，避免界面暗示不存在的独立档位。
- 其它客户端仍可使用上表 Coding Plan 别名；例如 xhigh 会转为 max，medium 会转为 high。

模型白名单仍只有这两个，glm-5.2 等不会被路由。

## 流事件与错误

Responses 的每个输出项都有连续 output_index，并发出 output_item.added/done。
文本有 content_part.added/done、output_text.delta/done；思考有 summary_part 与
summary_text 的 added/delta/done；工具有 function_call_arguments.delta/done。
所有事件带 sequence_number；增量附 item_id、output_index 及对应内容/摘要索引。

工具参数按上游块索引分别累积，交错工具不会串线。纯工具轮次不插入空的 message。
SSE 的项目 done 统一在最终 response 之前发送；文本与思考不会相互混入。

- 正常：response.completed；Chat 以 [DONE] 结束。
- token 上限截断：Responses 为 response.incomplete，原因 max_output_tokens；Chat 的 finish_reason 为 length。
- 上游 error、非法 JSON、message_stop 前断流：Responses 为 response.failed；
  Chat 为携带 upstream_stream_error 的错误分片，不发送正常 [DONE]。

## 回归测试

普通 Go 测试不要求安装 SDK：

    go test ./internal/openai
    go build ./...
    go vet ./...
    go test ./...

实际客户端回归仍只访问本地 httptest 模型替身，不使用真实账号：

    # 先安装 openai==2.30.0，以及 @earendil-works/pi-ai@0.85.1
    # ZCODE_PI_AI_MODULE 指向 pi-ai/dist/api/openai-completions.js
    ZCODE_TEST_CLIENTS=python,pi ZCODE_PI_AI_MODULE=/path/to/pi-ai/dist/api/openai-completions.js \
      go test -count=1 -timeout=180s ./internal/openai -run '^TestClientSDKCompatibility$' -v

Windows PowerShell 示例：

    $env:ZCODE_TEST_CLIENTS = "python,pi"
    $env:ZCODE_PI_AI_MODULE = "C:\path\to\pi-ai\dist\api\openai-completions.js"
    go test -count=1 -timeout=180s ./internal/openai -run '^TestClientSDKCompatibility$' -v

本地已验证 OpenAI Python SDK 2.30.0、Pi 0.85.1 的真实协议适配器。
Pi 在两种模型、high/max 两档、OpenAI/ZAI 两种格式下验证：思考/文本增量 → 两个交错工具调用 → 结果回传 → 最终回答。
CI 增加相同 SDK 回归并开启竞态检测。Windows 无符号链接权限时只跳过对应环境能力用例，
Linux CI 继续执行该用例。离线回归证明网关与客户端协议兼容，不等于真实 Z.AI 模型在线验收完成。

本机全量复验还观察到既有 SQLite 相关测试的 Windows TempDir 清理偶发失败
（directory is not empty，出现用例不固定）；对应失败用例定向复跑通过。
协议包及两种真实 SDK 回归均通过，此环境清理问题不应写成“全量测试稳定全绿”。

## 协议参考

- [OpenAI 官方工具调用](https://developers.openai.com/api/docs/guides/function-calling)
- [OpenAI 官方流式响应](https://developers.openai.com/api/docs/guides/streaming-responses)
- [Responses 事件字段](https://developers.openai.com/api/reference/resources/responses)
- Pi 模型配置以安装版本附带的 docs/models.md 为准。
