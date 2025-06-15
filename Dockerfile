# 使用多阶段构建
# 阶段1: 构建
FROM golang:1.18-alpine AS builder

WORKDIR /app

# 安装依赖 (bbolt需要)
RUN apk add --no-cache build-base

# 拷贝代码
COPY . .

# 构建
RUN go build -o javdb-crawler .

# 阶段2: 运行
FROM alpine:3.16

# 安装必要的库
RUN apk --no-cache add ca-certificates

WORKDIR /app

# 拷贝构建结果
COPY --from=builder /app/javdb-crawler .

# 设置环境变量默认值
ENV BASE_URL=https://javdb.com
ENV QB_URL=http://127.0.0.1:8080
ENV QB_USERNAME=admin
ENV QB_PASSWORD=adminadmin
ENV USER_AGENT="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
ENV WORKER_COUNT=1
ENV MAX_DEPTH=3
 # 为空则立即运行一次
ENV RUN_SCHEDULE=""
ENV START_URL="/rankings/movies?p=daily&t=censored"

# 设置入口点
ENTRYPOINT ["/app/javdb-crawler"]