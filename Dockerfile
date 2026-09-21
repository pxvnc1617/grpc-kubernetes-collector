# syntax=docker/dockerfile:1

# ── 1단계: 프론트엔드 빌드 ───────────────────────────────────
FROM node:20-alpine AS web
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ── 2단계: Go 빌드 ───────────────────────────────────────────
FROM golang:1.24-alpine AS build
WORKDIR /src

# 의존성 레이어를 분리해 소스만 바뀔 때 재다운로드를 피한다.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent  ./cmd/agent

# ── 3단계: 실행 이미지 ───────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
COPY --from=build /out/agent  /agent
COPY --from=web   /web/dist   /web/dist
USER nonroot:nonroot
EXPOSE 50051 8080
ENTRYPOINT ["/server"]
