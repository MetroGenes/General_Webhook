FROM golang:1.26.5-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webhook ./cmd/server

FROM alpine:3.22
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
