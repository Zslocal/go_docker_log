# 多阶段构建：编译阶段用宿主机架构，产物按目标架构交叉编译，
# 这样在 x86 机器上也能构建出 arm64 镜像。
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# 零第三方依赖，这一层几乎瞬间完成
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/dlog ./cmd/dlog

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /out/dlog /usr/local/bin/dlog

ENV TZ=Asia/Shanghai
EXPOSE 8080

# 刻意以 root 运行：Docker 的 containers/<id>/ 目录权限是 710 且属主为 root，
# 非 root 用户连目录都进不去。真要降权，得先在宿主机上
#   chgrp -R <组> /app/docker/data/containers && chmod -R g+rX ...
# 再配合 user: 指定 uid，代价是宿主机权限被放宽，反而更不划算。
ENTRYPOINT ["dlog"]
CMD ["-listen", ":8080"]
