# Kiro 原生接入

实施记录：2026-10-23。代码已接入本地项目；当前验证使用合成协议样本与隔离的 PostgreSQL，尚未使用真实 Kiro 凭证调用上游。部署前仍需完成本文末尾的账号验收。

## 实施进度

| 阶段 | 当前结果 |
| --- | --- |
| M0 协议确认 | 已核对固定版本开源源码并建立严格离线样本；真实账号的认证、区域、端点、完成信号与计量字段待确认。 |
| M1 账号生命周期 | 独立 `kiro` 平台；AWS Builder ID / IAM Identity Center 手动设备授权；Social / AWS OIDC 凭证导入、编辑、重新授权；原生 API Key 配置；统一后台、手动及请求刷新；令牌轮转条件写入。 |
| M2 Messages | 原生请求转换、严格 AWS EventStream、流式与非流式文本及函数工具、部分用量与独立 credits 保存。 |
| M3 OpenAI 桥接 | Chat Completions 与 Responses 共用原生调用；工具参数交错解码；Responses 客户端 custom / namespace 工具回写。 |
| M4 运营接入 | 原生连接测试、模型分页同步、OAuth 额度缓存、HTTP 冷却、日志来源展示、Composite 显式路由及相关回归。自动按套餐阈值停调仍未启用。 |

后三个阶段的结论是代码实现和本地验证完成，不代表某种真实账号或端点组合已经获得可用性验收。

## 配置步骤

1. 使用包含此次变更的前后端版本。后端启动时须成功应用 `backend/migrations/247_kiro_upstream_usage.sql`。该表使用专用 SQL repository，不修改现有 Ent 用量表，也不需要重新生成 Ent。
2. 在账号管理中新增账号，选择 **Kiro**。OAuth 账号先填写基本信息、代理和分组，点击“下一步”，选择“手动授权”“填写 Token”或“导入 JSON”，最后创建；点击“返回”修改基本信息时保留已填凭证。API Key 账号与其他平台一样，在基本信息页填写密钥后直接创建。导入支持 camelCase 和 snake_case；导入 JSON 优先于表单初始默认值。普通编辑时敏感输入留空保留服务器原值；新增或重新授权时必须自行提供新凭证，或完成一次新的登录授权。Token 输入框及 JSON 输入框不会预填已有凭证，也不会读取本机 Kiro 缓存。
3. 分别设置认证区域和推理区域。`profile_arn` 中的区域须与推理区域一致。仅在用户确认的账号/端点组合中使用 Profile ARN；当前不会自动发现 Profile。
4. OAuth 账号打开连接测试窗口时自动查询原生模型目录，显示真实模型名称并优先选中 Sonnet；获取失败会显示错误，可点击“重新获取可用模型”。无需为了测试填写模型映射；配置了映射时，只展示上游确实存在的目标对应的账号别名。未指定测试模型的后端调用也会自动查询。原生 API Key 的管理接口尚未验证，仍要求手工配置模型映射。这里不提供猜测的 Claude 默认模型表。
5. 把账号加入 `kiro` 分组，或在 Composite 分组中设置 `target_platform=kiro` 的显式模型路由。裸 `claude-*`、`gpt-*` 的默认原厂归属保持原有含义；当前没有自动去除 `kiro/` 前缀的逻辑。
6. 配置该模型的价格后，先运行账号连接测试，再验证文本、工具调用和工具结果回传。连接测试必须选择或映射该账号实际可用的上游模型 ID。

示例只展示字段结构，以下字符串均为占位符，不能用于真实调用：

```json
{
  "auth_method": "social",
  "refresh_token": "REPLACE_WITH_AUTHORIZED_REFRESH_TOKEN",
  "auth_region": "us-east-1",
  "api_region": "us-east-1",
  "endpoint_family": "q",
  "model_mapping": {"my-kiro-model": "REPLACE_WITH_AVAILABLE_MODEL_ID"}
}
```

AWS OIDC 使用 `auth_method=idc`，并补充 `client_id`、`client_secret`。Kiro API Key 使用 `auth_method=api_key`、`api_key`、`endpoint_family=runtime`，账号类型为 `apikey`。API Key 不走 OAuth 刷新，不走 OpenAI 出站接口。

### 手动登录授权

手动授权接入 AWS OIDC 设备授权流程，支持 AWS Builder ID 与 IAM Identity Center。后者需填写 `https://your-portal.awsapps.com/start`，并选择所属认证区域。Google / GitHub Social 登录暂不提供浏览器授权，使用“填写 Token”或“导入 JSON”。

1. 确认认证区域和代理，点击“生成授权链接”。
2. 点击“打开授权页”，核对页面设备验证码，由用户本人登录并批准。
3. 回到本系统等待自动检查，也可点击“检查授权结果”。页面按服务器返回的间隔检查，成功、拒绝、过期或关闭表单时停止；创建或保存时会检查或等待当前正在进行的检查。尚未取得凭证时提示等待确认，不要求重新生成链接。设备验证码只用于 AWS 页面，不要填入模型映射等其他字段。

后端管理端点为 `POST /api/v1/admin/kiro/oauth/auth-url` 与 `POST /api/v1/admin/kiro/oauth/check`。服务依次调用区域 OIDC `/client/register`、`/device_authorization`、`/token`。Session ID 绑定当前管理员；设备码和注册密钥只存于有 TTL 的 Redis 会话，不从生成链接接口返回。查询采用 Redis 锁，遵守服务端 interval 与 slow_down；成功消费后删除会话。后台重启后会话仍可从 Redis 读取；Redis 不可用时明确失败。

用户切换认证区域、代理或授权模式后，界面会丢弃旧会话和迟到响应。已完成的登录凭证只用于当前提交，不写入 Token 手填框。已验证合成响应下的等待、限速、拒绝、过期和一次性消费；2026-10-23 还通过真实 AWS `us-east-1` 接口验证了客户端注册与设备授权链接生成（返回 5 秒查询间隔和 600 秒有效期），未执行用户登录或获取真人账号 Token。完整真人登录仍须通过实际账号验收。

可选 OAuth 字段：`access_token`、`expires_at`（Unix 秒或 RFC3339 字符串）、`profile_arn`。替换刷新身份但未提供新 access token 时，旧 access token 和旧有效期会清除，下一次操作走统一刷新。

## 原生端点与认证

| 用途 | 当前代码 |
| --- | --- |
| Social 刷新 | `POST https://prod.{auth_region}.auth.desktop.kiro.dev/refreshToken` |
| AWS OIDC 刷新 | `POST https://oidc.{auth_region}.amazonaws.com/token` |
| Q 推理 | `POST https://q.{api_region}.amazonaws.com/generateAssistantResponse` |
| CodeWhisperer 推理 | `POST https://codewhisperer.{api_region}.amazonaws.com/generateAssistantResponse` |
| Kiro runtime 推理 | OAuth 使用 `/generateAssistantResponse`；API Key 使用 `/` 和原生 `tokentype: API_KEY`，主机为 `runtime.{api_region}.kiro.dev` |
| OAuth 模型目录 | `GET https://codewhisperer.{api_region}.amazonaws.com/ListAvailableModels`，有界分页 |
| OAuth 额度 | `GET https://codewhisperer.{api_region}.amazonaws.com/getUsageLimits` |

这些端点来自已核对的参考源码。Social、OIDC、API Key 以及不同区域和端点族必须分别实测，不能互相代替验收。刷新、推理、模型同步、额度和连接测试均使用账号的代理配置。

## 请求和响应边界

- 入站支持 `/v1/messages`、`/v1/chat/completions`、`/v1/responses` 的流式与非流式文本、函数工具及工具结果。
- 每次请求携带完整历史，分配独立会话 UUID；同一次 401 重试保留 UUID，并在刷新后重建请求。没有跨用户共享的会话缓存。
- 统一帧解码器检查长度、头边界、prelude CRC 和 message CRC；按工具 ID 独立聚合交错的参数分片，结束时校验 JSON。
- 完成判定接受已识别的 `metadataEvent.stopReason`，或真实 IDE 流中已验证的“最后输出后的有效 contextUsageEvent + meteringEvent + 正常 EOF”；始终要求完整帧及完整工具参数。只有内容片段或异常断流不会判定为成功。
- 显式 reasoning/thinking 控制、强制工具选择、关闭并行工具、stop sequences、结构化输出、图片/文件/音频输入、服务器托管工具暂不支持。Responses `custom` 工具通过现有函数封装适配，不表示支持任意服务端工具。
- 原生可见 reasoning 输出会转换；历史 reasoning 只保留可见文字，纯加密历史明确拒绝。模型目录仅宣告文本输入和无可配置 reasoning effort。
- 不支持 `previous_response_id`、存储型/后台 Responses、compact、WebSocket、旧式 `function_call` 历史。要求提供完整消息和完整工具结果；不通过静默删除历史修复请求。
- Kiro 的 `/v1/messages/count_tokens` 返回明确的 404，供客户端采用本地估算；不会将原生凭证发到 Anthropic。
- 只在输出前允许现有调度器换号；已经输出或观测到消耗的失败返回部分用量及错误。中断、坏帧、未完成工具和计量写入失败均不会发送正常完成标志。

## 用量、冷却与日志

`usage_source` 区分 `reported`、`mixed`、`estimated`。缺失 token 时采用 ASCII 字符数/4 向上取整加非 ASCII 字符数的本地估算；输入估算基于已编码的原生请求。该估算不是模型官方 tokenizer。

credits 与上下文占用比例保存在 `kiro_upstream_usage.upstream_usage` 中，分别保留，不写入 token 或 USD 成本。按现有模型价格及倍率结算；计量表通过结算 request ID、账号 ID 和调用方 API Key ID 关联用量日志。数据库对这个三元组执行幂等插入。

用户用量日志显示 token 估算来源；管理员还可看到原生计量、credits 和完成状态。流式 HTTP 头使用 `X-Sub2API-Usage-Source: estimated-unless-reported`；非流式头可返回最终来源。以持久化日志为最终来源依据。

OAuth 额度查询使用一分钟正缓存、三十秒错误缓存和 singleflight；缓存隔离凭证与代理变更。分别展示基础、试用和奖励额度。查询失败表示未知，不等于耗尽。

429 尊重有界 `Retry-After`，默认冷却一分钟；402 默认冷却两分钟，等待重新判断。403 不自动视为 token 过期或整个平台禁用。冷却与令牌写入均比较原凭证和代理，避免覆盖管理员的并发替换。

当前 HTTP 错误仅暴露状态码，不透传上游正文；没有按未验证的上游错误正文自动永久封禁或启用套餐阈值停调。持续 401 / 刷新失败需要管理员检查区域、Profile、权限并重新授权。原生计量表目前独立保留，尚未增加随普通日志清理的联动删除。

## 本地验证

后端使用项目要求的 Go 版本（本次为 Windows amd64 Go 1.27.3）。代表命令：

```sh
cd backend
go test ./internal/pkg/kiro
go test -tags unit ./internal/service ./internal/repository ./internal/handler/dto -run 'TestKiro|TestFrontendBuiltinPlatformCatalogInSync|TestUsageLogFromService' -count=1
SUB2API_TEST_POSTGRES_IMAGE=postgres:15-alpine go test -tags integration ./internal/repository -run '^TestKiroPersistenceIntegration$' -count=1
go build ./cmd/server
```

集成测试使用 testcontainers 启动独立 PostgreSQL/Redis，并应用项目迁移，不连接现有业务数据库。另已运行平台清单、Composite、OAuth 刷新、上游模型同步、网关计费及相关 DTO 回归。

前端使用锁文件对应的 pnpm 安装依赖后，运行 `vue-tsc --noEmit`、变更文件 ESLint、`vite build`，以及 Kiro 凭证/重新授权、创建账号、平台清单、用量表和翻译测试。测试中所有 token 均为合成字符串。

2026-10-23 使用本地已授权的 OAuth 账号实际查询模型目录，返回 8 个模型 ID。该检查仅验证目录发现，未发起推理，不代表这些模型均完成了推理验收。

后续已完成 MiniMax M2.5、MiniMax M2.1、DeepSeek V3.2、GLM 5、Qwen3 Coder Next 的真实账号测试，并修复 IDE 流结束识别。默认测试发送 `hi` 并显示真实回复。协议接入和验证边界详见 [KIRO_RS_INTEGRATION.md](../../../../docs/KIRO_RS_INTEGRATION.md)。

## 真实账号验收清单

1. 每种准备启用的认证方式记录实际区域、端点、套餐及可用模型；确认能刷新并保存旋转令牌。
2. 三种入站协议分别验证流式/非流式文本，至少一次完整函数工具往返；确认响应终止信号与 decoder 假设相符。
3. 验证原生 reasoning、credits、是否存在精确 token 字段，以及模型和额度 API 的实际结构。
4. 验证 401 刷新重试、真实 429、代理、取消请求和多账号切换；确认客户端不会收到拼接答案或伪完成。
5. API Key、Social、OIDC 分开记录结果；API Key 的模型与额度查询仍须单独实现并验收后才能宣告支持。

## 参考与实现来源

主体使用 Go 实现，IDE 协议适配参考 kiro.rs，相关 MIT 声明保留于上述接入文档。协议行为主要核对：

- [hank9999/kiro.rs](https://github.com/hank9999/kiro.rs/tree/22f36e692ad387c46581a84daf0d563347e330a3)（MIT）。

- [Quorinex/Kiro-Go](https://github.com/Quorinex/Kiro-Go/tree/f8f6071c9298a4266ad3e0c7e483d4a2510cbcaf)（MIT）。
- [caidaoli/kiro2api](https://github.com/caidaoli/kiro2api/tree/a2837e91d0f93d1f340ce43911c31b04c257c0d8)（MIT）。
- [jwadow/kiro-gateway](https://github.com/jwadow/kiro-gateway/tree/a5292ca04c7c6231e0b47673ac3f981f5a706e1e)（AGPL-3.0，仅用于语义交叉核对）。
- [Kiro CLI Headless](https://kiro.dev/docs/cli/headless/)、[API Key 管理](https://kiro.dev/docs/enterprise/governance/api-keys/)、[可用模型](https://kiro.dev/docs/models/available-models/)。
