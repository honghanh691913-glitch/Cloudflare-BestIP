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

# Pin a known sing-box core in the image so real-link tests do not depend on
# the NAS being able to download GitHub releases at runtime.
FROM alpine:3.21 AS singbox
ARG SING_BOX_VERSION=1.13.12
RUN apk add --no-cache ca-certificates curl tar \
 && curl -fL --retry 5 --retry-delay 2 \
      "https://github.com/SagerNet/sing-box/releases/download/v${SING_BOX_VERSION}/sing-box-${SING_BOX_VERSION}-linux-amd64-musl.tar.gz" \
      -o /tmp/sing-box.tgz \
 && tar -xzf /tmp/sing-box.tgz -C /tmp \
 && cp "/tmp/sing-box-${SING_BOX_VERSION}-linux-amd64-musl/sing-box" /sing-box \
 && chmod +x /sing-box

FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl tzdata tar
COPY --from=build /out/bestip-manager /usr/local/bin/bestip-manager
COPY --from=singbox /sing-box /usr/local/bin/sing-box
COPY scripts/entrypoint.sh /entrypoint.sh
COPY data/config.example.json /app/config.example.json
RUN mkdir -p /data/bin && chmod +x /entrypoint.sh
ENV TZ=Asia/Shanghai
ENV BESTIP_CONFIG=/data/config.json
ENV BESTIP_SINGBOX=/usr/local/bin/sing-box
ENV PATH=/data/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
