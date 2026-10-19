# syntax=docker/dockerfile:1.7
# One image for every backend process: api, scheduler, worker, migrate, seed.

FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/taskflow ./cmd/taskflow

# Distroless: no shell or package manager, runs as an unprivileged user.
# The IANA timezone database is embedded in the binary (time/tzdata).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/taskflow /taskflow
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/taskflow"]
CMD ["api"]
