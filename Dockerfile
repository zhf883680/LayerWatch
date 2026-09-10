FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /out/layerwatch ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates ffmpeg tzdata
WORKDIR /app
COPY --from=build /out/layerwatch /usr/local/bin/layerwatch
COPY config/config.example.yaml /app/config.example.yaml
VOLUME ["/app/data"]
EXPOSE 19091
ENTRYPOINT ["layerwatch"]
CMD ["-config", "/app/data/config.yaml", "-data", "/app/data"]
