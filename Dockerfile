# syntax=docker/dockerfile:1

# ---- web UI ----
FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- Go binary ----
FROM golang:1.26-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/clockwork ./cmd/clockwork

# ---- runtime ----
FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
 && adduser -D -H -u 10001 clockwork
COPY --from=server /out/clockwork /usr/local/bin/clockwork
COPY --from=web /web/dist /srv/web
ENV CLOCKWORK_HTTP_ADDR=:8080 \
    CLOCKWORK_WEB_DIR=/srv/web
# Never run as root: shell tasks (when enabled) inherit this user.
USER clockwork
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["clockwork"]
