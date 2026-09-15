// 直接调用 Pi 的 Chat Completions 适配器，不启动真实编码代理、不执行系统工具。
import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";

const baseUrl = process.env.ZCODE_TEST_BASE_URL;
assert.ok(["127.0.0.1", "localhost"].includes(new URL(baseUrl).hostname));
const modulePath = process.env.ZCODE_PI_AI_MODULE;
const { stream } = await import(modulePath
  ? pathToFileURL(modulePath).href
  : "@earendil-works/pi-ai/api/openai-completions");
const model = {
  id: "GLM-5.3", name: "ZCode test", api: "openai-completions",
  provider: "zcode-test", baseUrl, reasoning: true, input: ["text"],
  contextWindow: 128000, maxTokens: 16384,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  compat: {
    thinkingFormat: "openai", supportsReasoningEffort: true,
    supportsStrictMode: false, supportsStore: false, maxTokensField: "max_tokens",
  },
};
const context = {
  messages: [{ role: "user", content: "查询杭州和上海的天气", timestamp: Date.now() }],
  tools: [{
    name: "get_weather", description: "查询天气", constrainedSampling: false,
    parameters: {
      type: "object", properties: { city: { type: "string" } }, required: ["city"],
    },
  }],
};
async function run(effort) {
  const response = stream(model, context, {
    apiKey: "sk-test", maxTokens: 16384, reasoningEffort: effort, toolChoice: "auto",
  });
  const events = [];
  for await (const event of response) {
    events.push(event.type);
    if (event.type === "error") throw new Error(event.error.errorMessage);
  }
  return { message: await response.result(), events };
}
for (const modelId of ["GLM-5.3", "glm-5.3-flash"]) {
  for (const effort of ["high", "max"]) {
    for (const format of ["openai", "zai"]) {
      model.id = modelId;
      model.compat.thinkingFormat = format;
      context.messages = context.messages.slice(0, 1);
      const first = await run(effort);
      assert.equal(first.message.stopReason, "toolUse");
      assert.ok(first.events.includes("thinking_delta"));
      assert.ok(first.events.includes("toolcall_delta"));
      assert.equal(first.message.content.find(x => x.type === "thinking").thinking, "先查询两个城市。");
      const calls = first.message.content.filter(x => x.type === "toolCall");
      assert.deepEqual(calls.map(x => x.arguments.city), ["杭州", "上海"]);
      assert.deepEqual(calls.map(x => x.id), ["call_a", "call_b"]);
      context.messages.push(first.message);
      for (const call of calls) {
        context.messages.push({
          role: "toolResult", toolCallId: call.id, toolName: call.name,
          content: [{ type: "text", text: "晴 20 度" }], isError: false, timestamp: Date.now(),
        });
      }
      const second = await run(effort);
      assert.equal(second.message.stopReason, "stop");
      assert.equal(second.message.content.find(x => x.type === "text").text, "回答");
    }
  }
}
console.log("Pi Chat Completions 适配器：GLM-5.3 / Flash，high / max，OpenAI / ZAI 格式与工具闭环均通过");
