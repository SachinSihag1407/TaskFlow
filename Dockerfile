# Single-image build for one-service hosting (e.g. Render's free plan):
# the Go binary runs migrations, API, scheduler and a worker in one process
# and serves the built frontend itself. For the multi-service deployment use
# docker-compose.yml, which builds backend/ and frontend/ separately.
# No BuildKit-only syntax, so any Docker builder (including Render's) works.

FROM node:22-alpine AS web
WORKDIR /web
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

FROM golang:1.26-alpine AS api
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
ARG VERSION=demo
RUN go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/taskflow ./cmd/taskflow

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=api /out/taskflow /taskflow
COPY --from=web /web/dist /web
ENV STATIC_DIR=/web \
    TASKFLOW_ENV=production \
    TRUST_PROXY_HEADERS=true
USER nonroot:nonroot
EXPOSE 10000
ENTRYPOINT ["/taskflow"]
CMD ["all-in-one"]
