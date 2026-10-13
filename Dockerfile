# check=error=true

# =============================================================================
# BUILD

FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine AS build-stage

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
RUN go mod verify

COPY . .

ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-w -s" -o ./tmp/thub-server ./cmd/thub-server/
RUN mkdir -p ./tmp/data

# =============================================================================
# FINAL-IMAGE

FROM gcr.io/distroless/static-debian13:nonroot AS final-image

LABEL org.opencontainers.image.source=https://github.com/Felipe-CdS/things-hub

COPY --from=build-stage /app/tmp/thub-server /usr/local/bin/thub-server
COPY --from=build-stage --chown=65532:65532 /app/tmp/data /data

ENV THUB_DB=/data/thub.db
ENV THUB_LISTEN=:8787
ENV THUB_RETENTION_DAYS=90

USER nonroot:nonroot
VOLUME /data
EXPOSE 8787

HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/thub-server", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/thub-server"]
CMD ["serve"]
