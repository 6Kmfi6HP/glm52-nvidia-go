# GLM-5.2 NVIDIA NIM Go Client

逆向工程 NVIDIA Playground 的 API 调用，实现 Go 语言本地调用 GLM-5.2。

## 逆向分析报告

### 抓包过程

1. 访问 https://build.nvidia.com/z-ai/glm-5.2/playground
2. 启用 agent-browser 的 HAR 网络抓包
3. 在 Playground 中发送消息
4. 抓取实际发送给 API 的 HTTP 请求

### 发现的 API 端点

| 类型 | 端点 |
|------|------|
| **预测 API** (逆向) | `POST https://api.ngc.nvidia.com/v2/predict/models/qc69jvmznzxy/glm-5.2` |
| **队列检查** | `GET https://api.ngc.nvidia.com/v2/predict/queues/models/qc69jvmznzxy/glm-5.2` |
| **API 文档** | https://docs.api.nvidia.com/nim/reference/z-ai-glm-5-2 |

### 认证机制

Playground **不**使用 API Key 认证，而是使用 **hCaptcha token** 机制：

1. 页面加载时渲染 hCaptcha 不可见 widget
2. 调用 `hcaptcha.execute(widgetId)` 生成 token
3. token 存储在 widget 的 `data-hcaptcha-response` 属性中
4. token 以 `P1_` 开头，包含 JWT 格式的加密载荷
5. 每次请求携带 `nv-captcha-token` 和 `nv-function-id` 头

#### 请求签名

```
POST https://api.ngc.nvidia.com/v2/predict/models/qc69jvmznzxy/glm-5.2
Content-Type: application/json
Accept: text/event-stream
nv-function-id: 3b9748d8-1d85-40e8-8573-0eeaa63a4b63
nv-captcha-token: P1_eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...
Origin: https://build.nvidia.com
Referer: https://build.nvidia.com/
```

**注意:** 不需要 Cookie 或 Authorization 头！认证完全依赖 `nv-captcha-token` 和 Origin 检查。

### Token 生命周期

- Token 由 hCaptcha 生成，单次有效
- 每次调用 `hcaptcha.execute(widgetId)` 可生成新 token
- Token 有效期约 2-3 分钟
- 使用后立即失效

### 请求体格式

兼容 OpenAI Chat Completions 格式：

```json
{
  "model": "z-ai/glm-5.2",
  "messages": [
    {"role": "user", "content": "Hello"}
  ],
  "temperature": 1.0,
  "top_p": 1.0,
  "max_tokens": 16384,
  "seed": 42,
  "stream": true,
  "stream_options": {
    "include_usage": true,
    "continuous_usage_stats": true
  }
}
```

### 响应体格式

标准 SSE 流：

```
data: {"id":"chatcmpl-xxx","choices":[{"index":0,"delta":{"content":"你好","role":"assistant"},"finish_reason":null}],"created":...,"model":"z-ai/glm-5.2","object":"chat.completion.chunk","usage":null}
data: {"id":"chatcmpl-xxx","choices":[{"index":0,"delta":{"content":"!","role":"assistant"},"finish_reason":null}],...}
data: [DONE]
```

支持 `reasoning_content` 字段（开启 Thinking 模式时）。

## Go 客户端使用

### 安装

```bash
go get github.com/chromedp/chromedp  # 仅自动提取 token 时需要
```

### 方式 1：Captcha Token 模式（逆向）

从浏览器获取 token：

```bash
# 1. 打开浏览器访问 Playground
# 2. 打开开发者控制台，执行：
#    hcaptcha.execute("xxx")
# 3. 获取 token：
#    document.querySelector('[data-hcaptcha-widget-id]')
#      .dataset.hcaptchaResponse

# 4. 使用 Go 客户端
go run ./cmd/example -captcha "P1_eyJ..."
```

或在代码中：

```go
client := glm52.New(glm52.WithCaptchaToken("P1_eyJ..."))
resp, err := client.Chat(ctx, messages)
```

### 方式 2：自动提取 Token（chromedp）

```go
// cmd/example/auto.go 中提供了完整实现
token, err := ExtractCaptchaToken(ctx)
client := glm52.New(glm52.WithCaptchaToken(token))
```

## 模型信息

| 属性 | 值 |
|------|-----|
| 架构 | MoE, DSA + IndexShare 稀疏注意力 |
| 参数 | 753B |
| 上下文 | 1M tokens |
| 支持 | 推理链 (thinking)、工具调用、流式输出 |
| API 兼容 | OpenAI Chat Completions 格式 |
| 部署 | Docker NIM: `nvcr.io/nim/zai-org/glm-5.2:latest` |

### 方式 3：OpenAI 兼容本地代理

上游 predict API 本身就是 Chat Completions 格式，`serve` 只做 captcha 头适配与透传。**每个 captcha token 只能用于一次上游请求。**

```bash
# 每次请求自动提取新 token
go run ./cmd/serve -auto -addr :8080

# 或启动时提供一次性 token（首次请求后即消耗）
go run ./cmd/serve -captcha "P1_..."

# 调用（与 OpenAI SDK 兼容）
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"Hi"}],"stream":true}'
```

也可在请求头携带 `nv-captcha-token` 提供一次性 token。流式响应会关闭 `continuous_usage_stats`，保证 usage 只出现一次。

## 项目结构

```
glm52-nvidia-go/
├── types.go              # 类型定义（ChatRequest、Message、Chunk 等）
├── client.go             # 客户端实现（hCaptcha token + SSE 流式）
├── internal/captcha/     # chromedp 提取一次性 captcha token
├── cmd/example/          # 命令行示例
└── cmd/serve/            # OpenAI Chat Completions 兼容代理
```
