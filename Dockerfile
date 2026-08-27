FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILT_AT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w \
  -X github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/buildinfo.Version=${VERSION} \
  -X github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/buildinfo.Commit=${COMMIT} \
  -X github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/buildinfo.BuiltAt=${BUILT_AT}" \
  -o /out/bestip-manager ./cmd/bestip

FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl tzdata tar
COPY --from=build /out/bestip-manager /usr/local/bin/bestip-manager
COPY scripts/entrypoint.sh /entrypoint.sh
COPY data/config.example.json /app/config.example.json
RUN mkdir -p /data/bin && chmod +x /entrypoint.sh
ENV TZ=Asia/Shanghai
ENV BESTIP_CONFIG=/data/config.json
ENV PATH=/data/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
