# ============================================================
# 构建阶段
# ============================================================
FROM golang:1.24-alpine AS builder

# 安装必要的构建工具
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# 先复制 go.mod / go.sum，利用 Docker 层缓存加速依赖下载
COPY go.mod go.sum ./
RUN go mod download

# 复制全部源码
COPY . .

# 编译（禁用 CGO，生成纯静态二进制，适配 alpine/scratch）
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o gemini-web2api \
    ./cmd/server

# ============================================================
# 运行阶段（最小镜像）
# ============================================================
FROM alpine:3.20

# 安装运行时依赖：ca-certificates（HTTPS）、tzdata（时区）、wget（healthcheck）
RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app

# 从构建阶段复制二进制
COPY --from=builder /build/gemini-web2api .

# 复制示例环境变量文件（实际 .env 通过挂载或 env_file 注入）
COPY .env.example* ./

# 暴露默认端口（可通过环境变量 PORT 覆盖）
EXPOSE 8007

# 以非 root 用户运行，提升安全性
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser

ENTRYPOINT ["./gemini-web2api"]
