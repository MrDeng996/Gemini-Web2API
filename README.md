# Gemini-Web2API (Go Version)

将 Google Gemini Web 网页版转换为 OpenAI/Claude/Gemini 兼容的 API 格式。

## 特性

- **OpenAI 兼容**: `/v1/chat/completions`, `/v1/models`, `/v1/images/generations`
- **Claude 兼容**: `/v1/messages`, `/v1/messages/count_tokens`
- **Gemini 原生协议**: `/v1beta/models/{model}:generateContent`, `:streamGenerateContent`
- **流式输出**: SSE (Server-Sent Events) 打字机效果
- **思考过程**: 支持提取模型思考过程 (`reasoning_content`)
- **图片生成**: 支持 Nano Banana / Nano Banana Pro 生图
- **图片上传**: 支持多模态图片输入
- **多账户负载均衡**: 支持配置多个 Google 账户
- **HTTP 代理**: 支持全局代理和每账号独立代理 (HTTP/SOCKS5)
- **模型映射**: 支持将 Claude/OpenAI 模型名映射到 Gemini 模型
- **403 自动重试**: Cookie 过期时自动重新初始化并重试

## 支持的模型

| 模型名 | 说明 |
|--------|------|
| `gemini-2.5-flash` | 快速模型 |
| `gemini-3.1-pro-preview` | Pro 预览版 |
| `gemini-3-flash-preview` | Flash 预览版 |
| `gemini-3-flash-preview-no-thinking` | Flash 无思考模式 |
| `gemini-2.5-flash-image` | Nano Banana 生图 |
| `gemini-3-pro-image-preview` | Nano Banana Pro 生图 |

## 快速开始

### 1. 运行
```bash
# 编译
go build -o Gemini-Web2API.exe ./cmd/server

# 运行
./Gemini-Web2API.exe
```

### 2. 配置 Cookie

**方式一：自动获取 (Firefox)**

程序会自动从 Firefox 读取 Google Cookies（默认账户）。

**方式二：Chrome 批量获取（推荐）**
```bash
# 1. 关闭 Chrome 浏览器
# 2. 运行命令
./Gemini-Web2API.exe --fetch-cookies

# 3. 选择配置文件（输入 1,2,3 或 ALL）
```
详见 [internal/browser/README.md](internal/browser/README.md)
新版Chrome可能不适用此方法，或者说绝大部分Chrome。

**方式三：手动配置**
```bash
cp .env.example .env
# 编辑 .env 填入 Cookie
```

多账户配置（带后缀）：
```
__Secure-1PSID_Account1=xxx
__Secure-1PSIDTS_Account1=yyy
__Secure-1PSID_Account2=xxx
__Secure-1PSIDTS_Account2=yyy
```

### 3. 模型映射（可选）
将外部模型名映射到 Gemini 模型：
```
MODEL_MAPPING=claude-haiku-4-5-20251001:gemini-3-flash-preview-no-thinking
```

## API 端点

### OpenAI 兼容
```
POST /v1/chat/completions
POST /v1/images/generations
GET  /v1/models
```

### Claude 兼容
```
POST /v1/messages
POST /v1/messages/count_tokens
GET  /v1/models/claude
```

### Gemini 原生协议
```
POST /v1beta/models/{model}:generateContent
POST /v1beta/models/{model}:streamGenerateContent
GET  /v1beta/models
```
认证支持 `Authorization: Bearer xxx`、`?key=xxx`、`x-goog-api-key` 三种方式。

## 使用示例

### 聊天
```bash
curl http://127.0.0.1:8007/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3-flash-preview",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

### 图片生成
```bash
curl http://127.0.0.1:8007/v1/images/generations \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.5-flash-image",
    "prompt": "a cat wearing a hat",
    "n": 1,
    "size": "1024x1024",
    "response_format": "b64_json"
  }'
```
或者直接在 `v1/chat/completions` 端点使用，回复将自动格式化为 `![Generated Image 1](data:image/png;base64,xxx)`

## 目录结构

```
cmd/server/         # 程序入口
internal/
  adapter/          # OpenAI/Claude/Gemini 协议适配
  balancer/         # 多账户负载均衡
  browser/          # Cookie 获取
  claude/           # Claude 协议类型
  config/           # 配置（模型映射）
  gemini/           # Gemini Web API 客户端
```

## 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `PORT` | 服务端口 | 8007 |
| `PROXY_API_KEY` | API 密钥 | (空=无认证) |
| `PROXY` | 全局代理 (http/socks5) | (空) |
| `PROXY_{id}` | 单账号代理，覆盖全局 | (空) |
| `MODEL_MAPPING` | 模型映射 | (空) |
| `LANGUAGE` | 语言（Accept-Language / payload） | en |
| `SNAPSHOT_STREAMING` | 启用快照流式（实验性） | 0 |

## Docker 部署

### 方式一：docker compose（推荐）

```bash
# 1. 复制并编辑环境变量
cp .env.example .env
# 编辑 .env，填入 Cookie 及其他配置

# 2. 构建并启动
docker compose up -d --build

# 3. 查看日志
docker compose logs -f

# 4. 停止
docker compose down
```

服务默认监听 `http://localhost:8007`，可在 `.env` 中设置 `PORT` 覆盖端口。

`.env` 文件支持**热重载**：修改后容器会自动重新加载账号，无需重启。

### 方式二：纯 docker 命令

```bash
# 构建镜像
docker build -t gemini-web2api .

# 运行容器（将本地 .env 挂载进容器）
docker run -d \
  --name gemini-web2api \
  --restart unless-stopped \
  -p 8007:8007 \
  --env-file .env \
  -v "$(pwd)/.env:/app/.env:rw" \
  gemini-web2api
```

### 常用管理命令

```bash
# 查看运行状态
docker compose ps

# 重启服务
docker compose restart

# 拉取最新代码后重新构建
git pull && docker compose up -d --build

# 查看实时日志
docker compose logs -f gemini-web2api
```

### 注意事项

- Docker 环境下**不支持** `--fetch-cookies` 自动从浏览器读取 Cookie，请手动配置 `.env`。
- `.env` 通过卷挂载到容器内，修改宿主机 `.env` 后配置即时生效（无需重建镜像）。
- 若需修改端口，同时更新 `.env` 中的 `PORT` 和 `docker-compose.yml` 的 `ports` 映射，或直接设置环境变量 `PORT=xxxx docker compose up -d`。

## 注意

不适用于生产安全级。欢迎提Issue提PR。