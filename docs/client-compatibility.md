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
必须读取完整流，并实际向模型声明工具或开启思考；模型没有生成的字段不会凭空补上。
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

## 思考程度

| 请求档位 | 目标预算 |
| --- | ---: |
| none | 显式关闭 |
| minimal | 1024 |
| low | 2048 |
| medium | 4096 |
| high | 8192 |

Chat 使用 reasoning_effort；Responses 使用 reasoning.effort。默认不主动启用思考。
档位预算最多为 max_tokens / max_output_tokens 的一半：默认输出上限 8192 时，
medium 和 high 实际同为 4096。需要 high 的完整预算时，输出上限至少设为 16384。

显式 thinking={type:"enabled",budget_tokens:N} 优先；预算须为整数、至少 1024 且小于输出上限。
thinking={type:"disabled"} 优先关闭。
Pi 的 ZAI/DeepSeek 格式只发 enabled 开关时，网关按 effort 补预算；没有 effort 时按 medium。
ZAI 专用 clear_thinking 不会被写进 Anthropic thinking。

不支持的档位（包括 xhigh、max）、错误类型、输出上限无法容纳最低思考预算时明确返回 400，
不再静默关闭思考，也不会擅自增加 token 上限。none 与 enabled 开关同时出现会报参数冲突。
历史中的 reasoning_content / reasoning item 不会转换成缺签名的 Anthropic thinking；
原生思考签名不向 OpenAI 客户端输出，真实上游多轮行为仍需在线验收。

## Pi 配置

将 examples/pi-models.json 中 zcode2api 提供商条目合入 Pi 的 models.json，
不要覆盖其它提供商；按实际部署修改 baseUrl，并设置环境变量 ZCODE_GATEWAY_KEY。
该示例使用独立提供商名，避免更改既有代理、Z.AI 直连等配置。

关键设置：

- reasoning=true：Pi 才会按思考档位发送参数。
- thinkingFormat=openai、supportsReasoningEffort=true：发送标准 reasoning_effort。
- supportsStrictMode=false：不请求上游尚未保证的严格工具采样。
- maxTokens=16384：让 high 与 medium 的有效预算有区别。
- thinkingLevelMap 逐档同名映射；xhigh/max 设 null，从 Pi 界面隐藏不支持的档位。
- 不要把 low/medium/high 全映射为 high，否则界面切换并不会改变请求中的档位。

现有 ZAI 格式也可使用，但建议给本网关建立独立 OpenAI 格式配置。
示例只声明 GLM-5.3；需要 Flash 时可按同样设置增加 glm-5.3-flash 条目。
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
Pi 分别以 OpenAI 和 ZAI 思考格式完成：思考/文本增量 → 两个交错工具调用 → 结果回传 → 最终回答。
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
