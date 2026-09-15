# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src

# 의존성 레이어를 분리해 소스만 바뀔 때 재다운로드를 피한다.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent  ./cmd/agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
COPY --from=build /out/agent  /agent
USER nonroot:nonroot
EXPOSE 50051
ENTRYPOINT ["/server"]
