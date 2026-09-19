# Optional image build. Internet is needed for the base image and apt packages.
# The source itself has no third-party Go modules and no Node build step.
ARG GO_IMAGE=golang:1-bookworm
FROM ${GO_IMAGE} AS build
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN CGO_ENABLED=1 GOPROXY=off go test ./... \
    && CGO_ENABLED=1 GOPROXY=off go build -trimpath -ldflags="-s -w" -o /out/prism-gateway ./cmd/gateway
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --create-home prism \
    && mkdir /data && chown prism:prism /data
COPY --from=build /out/prism-gateway /usr/local/bin/prism-gateway
USER prism
WORKDIR /home/prism
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["prism-gateway"]
# 容器里用 --listen 覆盖启动是有意为之：镜像需要确定性地绑 0.0.0.0，
# 不依赖卷里那份配置的内容。
CMD ["--db","/data/gateway.db","--listen","0.0.0.0:8080","--allow-remote"]
