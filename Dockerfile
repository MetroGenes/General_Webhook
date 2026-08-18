# golang:1.26.6-alpine index digest (multi-arch); pin the build toolchain too.
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webhook ./cmd/server

# alpine:3.22 index digest (multi-arch); pin to avoid floating tag drift
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
ARG WEBHOOK_UID=10001
ARG WEBHOOK_GID=10001
RUN apk add --no-cache ca-certificates tzdata \
  && addgroup -S -g "${WEBHOOK_GID}" webhook \
  && adduser -S -D -H -u "${WEBHOOK_UID}" -G webhook webhook
WORKDIR /app
COPY --from=build --chown=webhook:webhook /out/webhook /app/webhook
COPY --chown=webhook:webhook configs /app/configs
COPY --chown=webhook:webhook scripts /opt/general-webhook/scripts
RUN mkdir -p /app/data /opt/general-webhook/scripts \
  && chown -R webhook:webhook /app /opt/general-webhook
ENV GOMEMLIMIT=192MiB \
    GOGC=50
USER webhook
# EXPOSE is image metadata only. Runtime deployment intentionally has no host port mapping.
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/webhook", "-config", "/app/configs/webhooks.yaml"]
