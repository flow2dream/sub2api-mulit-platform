# kiro.rs 协议对照与本地验证

参考项目：https://github.com/hank9999/kiro.rs

固定提交：22f36e692ad387c46581a84daf0d563347e330a3（2026-10-23 检查）

## 原理与接入范围

该项目把 Anthropic messages 转成 Kiro conversationState，通过 OAuth 刷新获得 Bearer Token，将请求发送到 Amazon Q 的 generateAssistantResponse，再解码 AWS EventStream 为 Anthropic SSE。当前提交只注册 IDE 端点；并未实现 runtime + Auto 推理。

本项目沿用已有 Go 平台模块，没有引入另一个代理服务或额外凭证副本。本次对照 src/kiro/endpoint/ide.rs、src/anthropic/converter.rs、src/kiro/model/credentials.rs，接入以下协议兼容点：

- Amazon Q OAuth 请求添加 SDK 客户端标识、vibe 模式、遥测退出与每次调用的 invocation ID。请求头不包含明文刷新令牌，客户端标识只使用其 SHA-256 派生值；重试头如实标识本次单次发送。
- IDE 会话包含 agentContinuationId、agentTaskType=vibe；保留本项目的历史、工具与推理参数支持。
- 明确版本的 Claude 名称转换为 Kiro 点号 ID，例如 claude-sonnet-4-5-20250929 → claude-sonnet-4.5。不替换模型代际、不把 Auto 偷换成其他模型。
- OAuth JSON 导入识别 endpoint=ide、authMethod=builder-id/iam。显式 endpoint_family 优先。
- OAuth 模型目录过滤 Auto，测试和三种网关协议统一拒绝旧页面传入的 Auto。此前“配置 runtime 和 Profile ARN 即可调用 Auto”的提示已删除。

API Key 继续走原有 runtime 分支，本次没有验证 kiro.rs 的 API Key IDE 调用。现有刷新、账户代理、429 冷却、计量与三种下游协议继续使用本项目实现。

## 实测与限制

导入的账号可以刷新凭证，Profile ARN 已保存，模型目录能读取。runtime + Auto 返回 HTTP 400 / INVALID_MODEL_ID；runtime + Sonnet 4.5 返回 HTTP 429。

按参考项目的 Q URL、请求头和请求体做独立对照，Sonnet 4.5 同样返回 HTTP 429 / ThrottlingException，message 为 Too many requests, please wait before trying again，reason 为 null。该结果不证明额度耗尽、不证明缺少软件激活，也不证明此次协议兼容已经打通真实推理。

初次接入时尚未收到成功的真实推理流，暂时保留了显式结束事件要求。后续已取得 MiniMax M2.5 的完整流并补充兼容，见下方更新。CRC、工具 JSON 完整性和读取错误检查仍保留。

## 部署后验证

2026-10-23：已重新构建前端、Go 嵌入服务与本地 Docker 镜像，只重建应用容器。服务 health 返回 200，容器 healthy、无重启；旧镜像保留为 sub2api-local:kiro-before-ide-20261023。

账号 test 已切换到 q（Kiro IDE），OIDC 凭证刷新返回 200，Profile ARN 与 Client ID 均与用户导入信息一致。模型接口返回 200 和 7 个具体模型，不含 Auto。部署后的 Sonnet 4.5 测试仍返回 kiro_throttled，冷却 60 秒。用户正在安装官方 Kiro，尚未提供同账号官方端对话结果。

验证通过：Kiro 协议包完整测试，service/admin 的 Kiro 相关回归，模型别名和 runtime Auto 过滤测试；前端 26 项相关测试、vue-tsc、ESLint、Vite 构建与 Go 嵌入构建。以上模拟测试通过不替代真实推理验证。

## MiniMax 真实流验证与修复（2026-10-23 后续更新）

用户提供官方 IDE 截图，当前账号可选模型为 DeepSeek V3.2、MiniMax M2.5、MiniMax M2.1、GLM 5、Qwen3 Coder Next，MiniMax 能正常回复。本项目也已收到回复，但误报 incomplete stream。

随后用同一账号实测 MiniMax M2.5，收到完整且 CRC 有效的 AWS EventStream：assistantResponseEvent（空内容）、reasoningContentEvent、assistantResponseEvent（回复内容）、contextUsageEvent、meteringEvent，然后 HTTP 正常结束；没有 metadataEvent/stopReason。

解码器现同时支持显式 stopReason，以及“最后一段输出之后的有效上下文事件和计费事件 + 正常 EOF”。仍拒绝缺失尾部事件、CRC 错误、截断帧、传输错误和未闭合工具调用；修复了完整帧头后缺失帧体被误作 EOF 的边界情况。工具调用完成时输出 tool_use，普通回复输出 end_turn。

只对当前 test 账号配置上述 5 个模型的映射范围，不全局隐藏其他账号的 Claude 模型。前后端默认测试优先 MiniMax M2.5；其他账号未提供该模型时使用其发现列表中的具体模型。

部署后逐一通过本项目账号测试 API 验证：

| 模型 ID | 收到回复 | test_complete.success |
| --- | --- | --- |
| minimax-m2.5 | 是 | true |
| deepseek-3.2 | 是 | true |
| minimax-m2.1 | 是 | true |
| glm-5 | 是 | true |
| qwen3-coder-next | 是 | true |

回归覆盖 Kiro 协议包、账号测试、网关三种协议的流式/非流式输出、13 种不完整流情形、工具流结束；前端 11 项测试、类型检查、ESLint 与前后端构建均通过。上述三种网关协议使用模拟流验证，5 个模型通过真实账号测试验证；未宣称这能证明 Claude 的 429 的具体成因。

## 测试消息一致性

两个账号测试弹窗现在明确发送 `hi`；后端空提示词的默认值也改为 `hi`。此前页面展示 `hi`，请求却传空字符串，触发了后端 `Reply with OK.` 的默认值，导致模型正常回复 OK 而界面提示不一致。现在不会额外要求固定答案，响应区域直接展示上游返回的文本。

## 参考代码许可

以下 MIT 声明适用于参考项目中用于 IDE 协议适配的实现。

MIT License

Copyright (c) 2026 hank9999

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
