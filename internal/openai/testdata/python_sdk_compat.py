"""官方 OpenAI SDK 本地闭环；仅访问测试提供的回环地址。"""
import json
import os
from urllib.parse import urlparse

import openai

base_url = os.environ["ZCODE_TEST_BASE_URL"]
assert urlparse(base_url).hostname in {"127.0.0.1", "localhost"}
client = openai.OpenAI(base_url=base_url, api_key="sk-test", timeout=10, max_retries=0)
function = {
    "name": "get_weather",
    "description": "查询天气",
    "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]},
    "strict": False,
}
chat_tool = {"type": "function", "function": function}
response_tool = {"type": "function", **function}
user = {"role": "user", "content": "查询杭州和上海的天气"}

def collect_chat(messages):
    calls, text, thinking, finish, usage = {}, "", "", None, None
    for chunk in client.chat.completions.create(
        model="GLM-5.3", messages=messages, tools=[chat_tool], stream=True,
        reasoning_effort="high", max_tokens=16384, stream_options={"include_usage": True},
    ):
        if chunk.usage:
            usage = chunk.usage
        if not chunk.choices:
            continue
        choice = chunk.choices[0]
        finish = choice.finish_reason or finish
        text += choice.delta.content or ""
        thinking += getattr(choice.delta, "reasoning_content", "") or ""
        for part in choice.delta.tool_calls or []:
            call = calls.setdefault(part.index, {
                "id": "", "type": "function", "function": {"name": "", "arguments": ""},
            })
            call["id"] = part.id or call["id"]
            if part.function:
                call["function"]["name"] = part.function.name or call["function"]["name"]
                call["function"]["arguments"] += part.function.arguments or ""
    assert usage is not None, "include_usage 未返回"
    return {"role": "assistant", "content": text, "reasoning_content": thinking, "tool_calls": list(calls.values())}, finish

message, finish = collect_chat([user])
assert finish == "tool_calls" and message["reasoning_content"] == "先查询两个城市。"
assert [json.loads(c["function"]["arguments"])["city"] for c in message["tool_calls"]] == ["杭州", "上海"]
history = [user, message] + [
    {"role": "tool", "tool_call_id": call["id"], "content": "晴 20 度"}
    for call in message["tool_calls"]
]
final, finish = collect_chat(history)
assert finish == "stop" and final["content"] == "回答"

# 非流式也必须保留工具和思考字段，而不是只剩 role / content。
completion = client.chat.completions.create(
    model="GLM-5.3", messages=[user], tools=[chat_tool], reasoning_effort="high", max_tokens=16384,
)
assert len(completion.choices[0].message.tool_calls) == 2
assert completion.choices[0].message.reasoning_content == "先查询两个城市。"

with client.responses.stream(
    model="GLM-5.3", input=[user], tools=[response_tool],
    reasoning={"effort": "high"}, max_output_tokens=16384,
) as stream:
    names = [event.type for event in stream]
    response = stream.get_final_response()
assert response.output_text == "正在查询天气。"
assert "response.reasoning_summary_text.done" in names
calls = [item for item in response.output if item.type == "function_call"]
assert [json.loads(c.arguments)["city"] for c in calls] == ["杭州", "上海"]
followup = [user] + [item.model_dump(exclude_none=True) for item in response.output] + [
    {"type": "function_call_output", "call_id": call.call_id, "output": "晴 20 度"}
    for call in calls
]
final = client.responses.create(
    model="GLM-5.3", input=followup, tools=[response_tool],
    reasoning={"effort": "high"}, max_output_tokens=16384, parallel_tool_calls=False,
)
assert final.status == "completed" and final.output_text == "回答"
assert final.parallel_tool_calls is False
# 验证实际 Python SDK 能在 Flash 模型上发送 max，而不是被网关白名单提前拒绝。
max_chat = client.chat.completions.create(
    model="glm-5.3-flash", messages=[user], tools=[chat_tool], reasoning_effort="max", max_tokens=8192,
)
assert len(max_chat.choices[0].message.tool_calls) == 2
max_response = client.responses.create(
    model="glm-5.3-flash", input=[user], tools=[response_tool], reasoning={"effort": "max"}, max_output_tokens=8192,
)
assert max_response.status == "completed"
try:
    client.chat.completions.create(
        model="GLM-5.3", messages=[user], reasoning_effort="extreme",
    )
except openai.BadRequestError as error:
    assert error.status_code == 400
else:
    raise AssertionError("不支持的思考档位应明确返回 400")
print(f"OpenAI Python SDK {openai.__version__}: Chat 流/非流、Responses 流、工具闭环、思考字段、参数错误均通过")
