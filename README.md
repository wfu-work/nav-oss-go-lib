<h1 align="center">nav-oss-go-lib</h1>

<p align="center">
  面向 Go 应用的统一对象存储抽象，让本地文件系统与 S3 兼容存储共享同一套 API。
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/wfu-work/nav-oss-go-lib"><img src="https://pkg.go.dev/badge/github.com/wfu-work/nav-oss-go-lib.svg" alt="Go Reference"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.26+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-green.svg" alt="MIT License"></a>
  <img src="https://img.shields.io/badge/Provider-Local%20%7C%20S3-2563eb" alt="Local and S3">
</p>

---

`nav-oss-go-lib` 为 Go 服务提供简洁、稳定的对象存储接口。应用只需面向统一契约编程，即可在本地文件系统、Amazon S3、MinIO、RustFS、Ceph RGW、Cloudflare R2 等存储之间切换。

它专注于对象字节和元数据的读写，不侵入用户、权限、目录树、版本、配额等业务模型，适合作为网盘、文档中心、媒体服务和备份系统的底层存储组件。

> [!NOTE]
> 当前版本为 `v0.1.0`。在 `v1.0.0` 发布前，公共 API 可能根据实际使用反馈进行调整。

## 特性

- **统一存储接口**：使用同一套 API 完成上传、下载、查询、删除与列举。
- **多后端支持**：内置 Local 与 S3 Provider，开发和生产环境可灵活切换。
- **大文件友好**：支持 Range 读取、流式传输和可恢复的分片上传。
- **对象操作完整**：支持分页列举、复制、移动、元数据与 Content-Type。
- **临时访问地址**：S3 Provider 支持 Presigned GET 与 PUT URL。
- **稳定错误语义**：使用 `errors.Is` 统一判断不存在、无权限、冲突和临时故障。
- **安全的本地存储**：防止绝对路径、目录穿越、符号链接逃逸和内部目录访问。
- **内置展示页面**：通过可选的 `webs` 包直接提供项目主页、能力说明和接入文档。
- **对象存储软件安装**：可从管理端下载、校验并启动 MinIO 或 RustFS，自动创建 Bucket 和注册存储源。
- **上下文控制**：所有耗时操作均接受 `context.Context`，支持取消与超时。

## 功能支持

| 能力 | Local | S3 |
| --- | :---: | :---: |
| 上传、下载、查询、删除 | ✅ | ✅ |
| Range 读取 | ✅ | ✅ |
| 前缀列举与游标分页 | ✅ | ✅ |
| 复制与移动 | ✅ | ✅ |
| Multipart 分片上传 | ✅ | ✅ |
| 自定义元数据 | ✅ | ✅ |
| Presigned GET / PUT | — | ✅ |

S3 Provider 基于 [`minio-go/v7`](https://github.com/minio/minio-go) 实现。不同 S3 兼容服务可能存在行为差异，接入前请确认服务商对相关 API 的支持情况。

## 安装

```bash
go get github.com/wfu-work/nav-oss-go-lib@latest
```

项目要求 Go `1.26` 或更高版本。

## Go SDK 接入

### 本地文件系统

下面的示例会把对象保存到 `./data/objects`：

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"

	oss "github.com/wfu-work/nav-oss-go-lib"
)

func main() {
	ctx := context.Background()

	store, err := oss.NewLocal(oss.LocalConfig{
		RootDir: "./data/objects",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	content := []byte("你好，nav-oss-go-lib！")
	info, err := store.Put(
		ctx,
		"documents/hello.txt",
		bytes.NewReader(content),
		int64(len(content)),
		oss.PutOptions{ContentType: "text/plain; charset=utf-8"},
	)
	if err != nil {
		log.Fatal(err)
	}

	reader, err := store.Open(ctx, info.Key, oss.OpenOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(string(data))
}
```

Local Provider 会在 `RootDir/.nav-oss/` 中保存元数据和分片上传状态。`RootDir` 应由当前应用独占，避免被不受信任的进程修改。

### S3 兼容存储

生产环境建议通过环境变量或 IAM 获取凭证，避免将密钥写入代码：

```go
store, err := oss.NewS3(ctx, oss.S3Config{
	Endpoint:       "https://s3.example.com",
	Region:         "us-east-1",
	Bucket:         "my-bucket",
	Prefix:         "production/objects",
	CredentialMode: oss.CredentialEnv,
	BucketLookup:   oss.BucketLookupAuto,
})
if err != nil {
	return err
}
defer store.Close()
```

环境变量模式会读取：

```bash
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_SESSION_TOKEN="your-session-token" # 可选
```

支持的凭证模式：

| 模式 | 说明 |
| --- | --- |
| `static` | 使用配置中的 Access Key、Secret Key 和可选 Session Token |
| `env` | 从 AWS 标准环境变量读取凭证 |
| `iam` | 使用 EC2、ECS 或 EKS IAM 元数据凭证 |
| `anonymous` | 匿名访问，仅适用于公开 Bucket |

## 内置控制台

项目已将 `nav-oss-lib-web` 的生产制品嵌入可选的 `webs` 包，并在 `server` 包中提供独立的控制台内部管理 API。内置启动命令使用两个端口：

- `127.0.0.1:9000`：nav-oss 管理 REST API；
- `127.0.0.1:9001`：项目展示站与管理控制台。

Console 端口会把同源 `/api/` 请求反向代理到 API 端口。API 端口不是 S3 服务端端口；真正的 S3、MinIO 或 RustFS 地址由 Provider 配置。

未配置管理员凭证时，首次启动会使用以下默认账号：

- 用户名：`admin`
- 密码：`navfirst.com`

直接启动：

```bash
go run ./cmd/nav-oss-web
```

随后访问 `http://127.0.0.1:9001`，并在登录后通过“个人中心”立即修改默认密码。修改后的密码会以 bcrypt 哈希写入权限为 `0600` 的状态文件，重启服务不会恢复成默认密码，浏览器也不会保存密码。

生产环境建议通过环境变量显式覆盖默认凭证：

```bash
NAV_OSS_ADMIN_USERNAME=admin \
NAV_OSS_ADMIN_PASSWORD='replace-with-a-strong-password' \
go run ./cmd/nav-oss-web
```

### 配置文件

复制 [`nav-oss.example.json`](nav-oss.example.json) 并创建只包含密码的文件：

```bash
mkdir -p secrets
printf '%s' 'replace-with-a-strong-password' > secrets/admin-password
chmod 600 secrets/admin-password
go run ./cmd/nav-oss-web -config ./nav-oss.json
```

配置格式：

```json
{
  "apiAddr": "127.0.0.1:9000",
  "consoleAddr": "127.0.0.1:9001",
  "stateFile": "./data/nav-oss-state.json",
  "runtimeDir": "./data/runtimes",
  "auth": {
    "username": "admin",
    "passwordFile": "./secrets/admin-password",
    "sessionTTLMinutes": 1440
  }
}
```

也支持 `NAV_OSS_API_ADDR`、`NAV_OSS_CONSOLE_ADDR`、`NAV_OSS_STATE_FILE`、`NAV_OSS_RUNTIME_DIR`、`NAV_OSS_ADMIN_USERNAME`、`NAV_OSS_ADMIN_PASSWORD`、`NAV_OSS_ADMIN_PASSWORD_FILE` 和 `NAV_OSS_ADMIN_PASSWORD_HASH` 环境变量。命令行可使用 `-runtime-dir` 覆盖对象存储软件的程序、凭证、日志与数据目录。

### 一键安装对象存储软件

管理端“对象存储软件”页面可以在 nav-oss 所在主机直接安装 S3 兼容服务，不依赖 Docker。当前支持范围：

| 软件 | Linux amd64 | Linux arm64 | macOS amd64 | macOS arm64 | 下载校验 |
| --- | :---: | :---: | :---: | :---: | --- |
| MinIO | ✅ | ✅ | ✅ | ✅ | 官方 SHA256 |
| RustFS | ✅ | ✅ | — | — | 官方 HTTPS 下载并记录本地 SHA256 |

一次安装任务会依次完成：

1. 从服务端内置白名单确定官方下载地址；
2. 下载并校验可执行程序；
3. 生成随机 Access Key 与 Secret Key，并以 `0600` 权限保存；
4. 默认在 `127.0.0.1` 启动 S3 API 与软件控制台；
5. 等待健康检查通过，创建 Bucket 并注册同名存储源。

管理 API 不接受任意下载 URL、可执行路径或启动参数，避免管理端被用作远程命令执行入口。运行目录默认位于状态文件旁的 `runtimes/`，可通过 `runtimeDir`、`NAV_OSS_RUNTIME_DIR` 或 `-runtime-dir` 修改。若需要让其他主机直接访问新安装的 S3 服务，应在受控的反向代理或防火墙策略下显式开放端口；一键安装默认不会监听所有网卡。

### 控制台内部 API

`/api/v1` 用于内置控制台的管理操作，不作为业务项目的公共集成契约，也不承诺独立的客户端兼容性。业务项目应直接使用上面的 Go SDK；如果需要向其他语言或外部系统提供能力，应由宿主应用基于自身认证与权限模型封装接口。

管理接口统一位于 `/api/v1`：

- `/api/v1/auth/*`：登录、当前账号、注销和修改密码；
- `/api/v1/providers`：存储源配置、启停和健康检查；
- `/api/v1/runtimes/catalog`：当前操作系统与架构可安装的软件目录；
- `/api/v1/installations`：创建和查询软件安装任务；
- `/api/v1/installations/{id}/{start|stop|restart|retry}`：控制已管理实例的生命周期；
- `/api/v1/objects`：对象查询、详情、上传、复制、移动、删除和临时签名；
- `/api/v1/dashboard`：存储规模、健康状态和近期操作概览；
- `/api/v1/multipart`：当前 Provider 可枚举的 Multipart 会话；
- `/api/v1/operations`：最近的管理操作；
- `/api/v1/system`：服务版本、系统架构、CPU、内存、应用数据磁盘和运行配置等脱敏信息；
- `/api/v1/settings`：默认 Provider、分页和临时签名策略。

底层 `Store` 接口暂时没有枚举全部 Multipart 会话的能力，因此 `/api/v1/multipart` 当前返回真实空集合，不使用演示数据。

`webs` 与 `server` 都是独立可选包。只导入根包、`core`、`local` 或 `s3` 的程序不会启动 HTTP 服务，也不会加载控制台资源。

前端重新构建后同步制品：

```bash
make web-sync
make web-verify
```

也可以通过 `WEB_ZIP=/path/to/nav-oss-lib-web.zip` 指定制品位置。

## 常用操作

### Range 读取

```go
reader, err := store.Open(ctx, "videos/demo.mp4", oss.OpenOptions{
	Offset: 1024,
	Length: 4096,
})
if err != nil {
	return err
}
defer reader.Close()
```

`Length` 为 `0` 或 `-1` 时，将从 `Offset` 一直读取到对象末尾。

### 分页列举

```go
result, err := store.List(ctx, oss.ListOptions{
	Prefix:    "documents/",
	Limit:     100,
	Recursive: true,
})
if err != nil {
	return err
}

for _, object := range result.Items {
	fmt.Println(object.Key, object.Size)
}
```

继续读取下一页时，将 `result.NextCursor` 传给下一次请求的 `Cursor`。

### 复制与移动

```go
copier, ok := store.(oss.Copier)
if !ok {
	return oss.ErrUnsupported
}

_, err = copier.Copy(
	ctx,
	"inbox/report.pdf",
	"archive/report.pdf",
	oss.CopyOptions{},
)

_, err = oss.Move(
	ctx,
	store,
	"inbox/photo.jpg",
	"archive/photo.jpg",
	oss.CopyOptions{},
)
```

`Move` 按照“先复制、再删除源对象”的顺序执行，并非原子操作。如果目标对象已创建但源对象删除失败，会返回 `ErrPartialFailure`。

### 分片上传

```go
multipart, ok := store.(oss.MultipartStore)
if !ok {
	return oss.ErrUnsupported
}

upload, err := multipart.CreateMultipart(
	ctx,
	"videos/demo.mp4",
	oss.PutOptions{ContentType: "video/mp4"},
)
if err != nil {
	return err
}

part, err := multipart.UploadPart(ctx, upload, 1, reader, partSize)
if err != nil {
	_ = multipart.AbortMultipart(ctx, upload)
	return err
}

info, err := multipart.CompleteMultipart(
	ctx,
	upload,
	[]oss.CompletedPart{part},
)
```

调用方应持久化 `MultipartUpload` 和每个 `CompletedPart`，以便在进程重启后恢复上传。

### Presigned URL

```go
presigner, ok := store.(oss.Presigner)
if !ok {
	return oss.ErrUnsupported
}

downloadURL, err := presigner.PresignGet(
	ctx,
	"documents/report.pdf",
	oss.PresignGetOptions{Expiry: 15 * time.Minute},
)
```

Presigned URL 仅由 S3 Provider 提供，有效期最长为 7 天。它等同于短期访问凭证，请避免写入日志或长期暴露。

## 统一创建 Provider

需要通过配置动态选择后端时，可以使用 `oss.New`：

```go
store, err := oss.New(ctx, oss.Config{
	Provider: oss.ProviderLocal,
	Local: oss.LocalConfig{
		RootDir: "./data/objects",
	},
})
```

也可以直接导入 `core`、`local`、`s3` 和 `factory` 子包。根包保留了常用类型别名与便捷构造函数，适合大多数项目使用。

## 配置说明

### LocalConfig

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `RootDir` | 必填 | 对象和内部状态的存储根目录 |
| `DirMode` | `0750` | 新建目录的权限位 |
| `FileMode` | `0640` | 新建文件的权限位 |

### S3Config

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `Endpoint` | 必填 | S3 服务地址，可使用完整 URL 或 `host:port` |
| `Secure` | `false` | Endpoint 未包含协议时是否启用 HTTPS |
| `Region` | 自动发现 | Bucket 所在区域 |
| `Bucket` | 必填 | Bucket 名称 |
| `Prefix` | 空 | 应用于所有逻辑对象键的命名空间前缀 |
| `CredentialMode` | 自动推断 | `static`、`env`、`iam` 或 `anonymous` |
| `BucketLookup` | `auto` | `auto`、`dns` 或 `path` |
| `AutoCreateBucket` | `false` | Bucket 不存在时是否自动创建 |
| `SkipBucketCheck` | `false` | 初始化时是否跳过 Bucket 检查 |
| `PartSize` | SDK 默认值 | 自动分片上传使用的分片大小 |
| `MaxRetries` | SDK 默认值 | S3 请求最大重试次数 |
| `Transport` | SDK 默认值 | 自定义 HTTP Transport |

`Prefix` 可用于隔离环境或业务命名空间。例如配置为 `production/objects` 后，逻辑键 `docs/a.pdf` 会映射到 `production/objects/docs/a.pdf`，API 返回的仍是逻辑键。

## 错误处理

所有 Provider 都会将底层错误转换为稳定的错误分类：

```go
info, err := store.Stat(ctx, "documents/report.pdf")
switch {
case err == nil:
	fmt.Println(info.Size)
case errors.Is(err, oss.ErrNotFound):
	// 对象不存在
case errors.Is(err, oss.ErrAccessDenied):
	// 凭证无权限
case errors.Is(err, oss.ErrTemporary):
	// 可根据业务幂等性决定是否重试
default:
	return err
}
```

可判断的错误包括：

- `ErrNotFound`
- `ErrAccessDenied`
- `ErrConflict`
- `ErrInvalidConfig`
- `ErrInvalidKey`
- `ErrInvalidRange`
- `ErrInvalidPart`
- `ErrUnsupported`
- `ErrTemporary`
- `ErrPartialFailure`

错误值通常包含操作名、Provider、对象键和服务端错误码，便于日志记录和问题定位。业务逻辑应使用 `errors.Is`，不要依赖错误文本。

## 项目结构

```text
nav-oss-go-lib/
├── cmd/nav-oss-web/      # API 与内置控制台双端口启动命令
├── core/                 # 公共接口、配置、值对象和错误定义
├── factory/              # 根据配置创建 Provider
├── local/                # 本地文件系统 Provider
├── s3/                   # S3 兼容 Provider
├── server/               # 可选认证与管理 REST API
├── webs/                 # 内嵌前端制品与标准 HTTP Handler
├── internal/ossutil/     # Provider 共享的内部工具
├── exports.go            # 根包公共类型与常量
└── factory.go            # 根包便捷构造函数
```

## 设计边界

核心存储库只负责对象存储；可选 `server` 包仅提供单管理员本地控制台，不处理以下业务能力：

- 多用户、复杂角色与租户权限体系；
- 目录树、文件版本与回收站；
- 配额、分享、审计与数据库事务；
- S3 服务端、分布式副本与集群调度。

建议使用不可变的业务 ID 生成对象键，并在业务数据库中维护用户可见路径。对象存储操作与数据库写入不属于同一个事务，调用方应根据业务需要设计幂等、补偿和审计机制。

## 安全建议

- 不要将 Access Key、Secret Key、Session Token 或 Presigned URL 提交到仓库和 Issue。
- 生产环境优先使用 IAM 临时凭证、最小权限策略和 HTTPS。
- 不要向不受信任的输入直接暴露对象键，业务层应先完成身份与权限校验。
- Local Provider 的 `RootDir` 应由应用独占，并设置合适的操作系统权限。
- `SkipBucketCheck` 只应在明确了解目标服务权限模型时启用。
- 如果发现安全问题，请通过私密渠道联系维护者，避免公开真实凭证和生产数据。

## 路线图

- [x] Local 与 S3 Provider
- [x] Range、List、Copy 与 Move
- [x] 可恢复的 Multipart 上传
- [x] Presigned GET 与 PUT
- [x] Provider 无关的错误分类
- [ ] 条件写入与乐观并发控制
- [ ] SSE-S3、SSE-KMS 与客户密钥配置
- [ ] 对象版本、生命周期和对象锁接口
- [ ] Metrics、Tracing 与请求审计 Hook

路线图用于表达演进方向，不代表具体发布时间。

## 参与贡献

欢迎通过 Issue 和 Pull Request 参与项目：

1. Fork 本仓库并创建功能分支；
2. 保持改动聚焦，一个 Pull Request 解决一个明确问题；
3. 使用 `gofmt` 格式化代码，并执行 `go vet ./...`；
4. 同步更新相关文档；
5. 在 Pull Request 中说明问题背景、实现方案和兼容性影响。

新增 Provider 时，请实现 `core.Store`，准确声明 `Capabilities()`，并将厂商错误映射到 `core` 中的统一错误类型。

## 致谢

感谢 [`minio-go`](https://github.com/minio/minio-go) 及 Go 开源社区提供的优秀基础设施。

## 许可证

本项目基于 [MIT License](LICENSE) 开源，可自由用于个人与商业项目。
