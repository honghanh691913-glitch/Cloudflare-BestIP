# BestIP Manager Starter

这是针对 `IonRh/Cloudflare-BestIP` 使用场景重新设计的第一版骨架：不再把 IPv4 / IPv6 / DNS 域名写死在一个全局配置中，而是拆成 **Sources（IP 池）** 和 **Targets（DNS 目标）**。

## 核心模型

- Source = “我要扫什么 IP、用什么 CFST 参数、是否限定 NRT/HKG/LAX、多久扫一次”。
- Target = “这个 hostname 要引用哪些 Source，各取几个结果”。
- 同一个 Target 可以同时引用 IPv4 和 IPv6 Source：程序会在同一个 hostname 下自动创建 A + AAAA。
- 同一个 Source 可以给多个 Target 复用，避免为了不同域名重复测速。
- Source / Target 数量没有代码层面的固定上限。

示例：

```text
cf-v4 ───────┐
             ├── v4.629717.xyz  => 5 A + 5 AAAA
cf-v6 ───────┘

nrt-v4 ───────── nrtv4.629717.xyz => 5 A
```

## 默认四个 IP 池

配置样例预置了四个可删除/可重命名的池：

- `cf-v4`
- `cf-v6`
- `nrt-v4`
- `nrt-v6`

它们只是默认示例，不是硬编码分类。你可以创建 `hkg-v4`、`lax-v6`、`mobile-v4`、`cmcc-nrt-v6` 等任意池。

## Web UI

启动后访问 `http://服务器IP:8080`：

- 查看每个 Source 的运行阶段和结果数量
- 单独运行某个 Source / 全部运行
- 新增、删除、编辑 Source
- 新增、删除、编辑 Target
- Target 选择多个 Source 并设置每个池取几个 IP
- 手动同步 DNS
- 高级 JSON 配置编辑

Web 可用 `BESTIP_WEB_USER` / `BESTIP_WEB_PASS` 开启 Basic Auth。

## Docker

1. 把 `data/config.json` 中 Cloudflare Zone ID / API Token 改好。
2. 把四个示例 Source 的 `inputs` 改成你自己的 IP 段 URL / 文件 / CIDR。
3. 启动：

```bash
docker compose up -d --build
```

容器首次启动会自动下载当前最新版 XIU2/CloudflareSpeedTest (`cfst`)。

## IP 输入格式

每个 Source 的 `inputs` 支持混合：

```json
"inputs": [
  "https://example.com/my-ip-ranges.txt",
  "104.16.0.0/13",
  "2606:4700::/32",
  "/data/custom.txt"
]
```

程序按 Source 的 `family` 自动丢弃不匹配的地址族。

## NRT 等数据中心

CFST 新版支持 `-cfcolo`，Source 中写：

```json
"colo": ["NRT"]
```

即可把这个 Source 定义成 NRT 池。也可以配置多个：

```json
"colo": ["NRT", "HND"]
```

## 当前 MVP 范围

已实现骨架：

- Source/Target 多对多配置模型
- URL / 文件 / CIDR / 单 IP 输入
- 调用 CFST 并解析 CSV
- IPv4/IPv6 自动映射到 A/AAAA
- Cloudflare DNS 多记录对齐（创建/更新/删除多余记录）
- Web 管理页
- 定时调度 + 手动运行
- Docker 化

下一步建议补：

- SQLite 历史记录和趋势图
- WebSocket/SSE 实时 CFST 进度日志
- DNSPod / 阿里云 / 华为云 provider 插件
- 每 Target 独立“最低可用数量/过期阈值/健康检查”
- Source 结果缓存与失败保留上一次可用结果
- API Token 加密/环境变量引用，而不是明文落盘
- 多任务并发队列及 CPU/带宽限流

## 注意

这是“重构骨架/MVP”，不是对原项目闭源发布二进制的反编译。测速部分直接调用开源 CFST，DNS 调度和 Web 管理层重新实现，后续维护会比继续堆原 `config.json` 更清晰。
