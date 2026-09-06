# Follow the latest stable Go release. CI and Compose builds use --pull.
FROM --platform=$BUILDPLATFORM golang:alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go version \
  && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/webhook ./cmd/server

# alpine:3.22 index digest (multi-arch); pin to avoid floating tag drift
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
ARG WEBHOOK_UID=10001
ARG WEBHOOK_GID=10001
ARG VCS_REF=unknown
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
    GOGC=50 \
    GENERAL_WEBHOOK_GIT_COMMIT=${VCS_REF}
USER webhook
# EXPOSE is image metadata only. Runtime deployment intentionally has no host port mapping.
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/app/webhook", "-config", "/app/configs/webhooks.yaml"]
