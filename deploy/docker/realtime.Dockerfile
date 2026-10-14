# syntax=docker/dockerfile:1.7
FROM golang:1.25-trixie AS build
RUN apt-get update \
 && apt-get install -y --no-install-recommends pkg-config libopus-dev libopusfile-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG GIT_COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/nabePi/kotoki-backend/internal/platform/buildinfo.Version=${VERSION} \
        -X github.com/nabePi/kotoki-backend/internal/platform/buildinfo.GitCommit=${GIT_COMMIT}" \
      -o /out/realtime ./cmd/realtime

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends libopus0 libopusfile0 ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/realtime /usr/local/bin/realtime
USER 65532:65532
EXPOSE 8081 9090
ENTRYPOINT ["/usr/local/bin/realtime"]
