# syntax=docker/dockerfile:1.7
FROM golang:1.25-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG GIT_COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/nabePi/kotoki-backend/internal/platform/buildinfo.Version=${VERSION} \
        -X github.com/nabePi/kotoki-backend/internal/platform/buildinfo.GitCommit=${GIT_COMMIT}" \
      -o /out/worker ./cmd/worker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/worker /usr/local/bin/worker
USER 65532:65532
EXPOSE 8081 9090
ENTRYPOINT ["/usr/local/bin/worker"]
