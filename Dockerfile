ARG DOCKER_HUB_MIRROR=docker.m.daocloud.io

FROM ${DOCKER_HUB_MIRROR}/library/golang:1.26-alpine AS builder

ARG GOPROXY=https://goproxy.cn
ENV GOPROXY=${GOPROXY} \
    GOTOOLCHAIN=local

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/icloud-api \
    ./cmd/icloud-api

FROM ${DOCKER_HUB_MIRROR}/library/alpine:3.22 AS runtime

ARG ALPINE_MIRROR=mirrors.aliyun.com
RUN sed -i "s#dl-cdn.alpinelinux.org#${ALPINE_MIRROR}#g" /etc/apk/repositories \
    && apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 app \
    && adduser -S -D -H -u 10001 -G app app \
    && mkdir -p /app/data \
    && chown -R app:app /app

WORKDIR /app

COPY --from=builder --chown=app:app /out/icloud-api /app/icloud-api

USER app

EXPOSE 8080

ENTRYPOINT ["/app/icloud-api"]
