FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bestip-manager ./cmd/bestip

FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl tzdata tar && addgroup -S app && adduser -S -G app app
COPY --from=build /out/bestip-manager /usr/local/bin/bestip-manager
COPY scripts/entrypoint.sh /entrypoint.sh
COPY data/config.example.json /app/config.example.json
RUN mkdir -p /data && chmod +x /entrypoint.sh
ENV TZ=Asia/Shanghai BESTIP_CONFIG=/data/config.json
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
