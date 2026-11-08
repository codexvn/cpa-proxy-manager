# cpa-proxy-manager · 代理批量设置

CLIProxyAPI（CPA）插件：在管理面板内批量设置与清空凭据（OAuth 认证文件）的出口代理地址 `proxy_url`。面向千级凭据规模，支持命名模板、点路径自定义占位符、只读预览、可停止的批量执行与失败明细。

## 功能

- **搜索与筛选**：按账号 / 文件名搜索，按凭据类型筛选，按是否已绑定代理筛选，勾选跨页保留
- **命名模板**：代理地址模板库，支持 http / https / socks5 / socks5h 四种协议
- **占位符**：内置 `{account}`（邮箱前缀）、`{email}`、`{provider}`，自定义占位符按点路径（如 `$.organization_uuid`）从认证文件 JSON 取标量值（自定义优先、内置兜底）；支持光标插入
- **预览**：只读对照表（当前值 vs 设置后），作用于筛选结果
- **批量设置 / 批量清空**：确认弹层显示数量与跳过统计，后端异步执行、可停止，失败原因仅界面显示
- **写入并发可配置**（默认 8，范围 1–64）

## 安装

### 在线安装（推荐）

CPA 插件商店添加本仓库作为第三方源，之后在管理面板的插件商店中直接安装：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/codexvn/cpa-proxy-manager/main/registry.json"
```

支持 windows/amd64、linux/amd64、linux/arm64、darwin/amd64、darwin/arm64 五个平台，安装包带 sha256 校验。

### 手动安装

构建产物为 CGO 动态库，要求本机装有 Go 1.22+ 与 C 编译器：

```bash
./build.sh          # Linux / macOS / Windows（Git Bash）
# 或 Windows PowerShell 见 build.ps1
```

把 `dist/cpa-proxy-manager.dll`（Windows）/ `cpa-proxy-manager.so`（Linux）放入 CPA 插件目录，并在 CPA `config.yaml` 启用：

```yaml
plugins:
  enabled: true
  configs:
    cpa-proxy-manager:
      enabled: true
      priority: 1
```

重启 CPA 后，管理页面出现「代理批量设置」入口。写操作所需管理密钥从面板转发的请求头自动提取，无需手动输入。

## 配置

插件配置保存在 `data/cpa-proxy-manager/config.json`，只含配置项（模板、选中模板、占位符、写入并发数），不含任何运行状态与密钥。

环境变量（均可选）：

| 变量 | 作用 | 默认 |
|------|------|------|
| `CPA_PROXY_MANAGER_MGMT_BASE_URL` | CPA 管理 API 基址（http / https 均可） | `https://127.0.0.1:8317` |
| `CPA_PROXY_MANAGER_DATA_DIR` | 配置文件目录 | `data/cpa-proxy-manager` |
| `MANAGEMENT_PASSWORD` / `CPA_MANAGEMENT_KEY` | 无头部署的密钥兜底 | — |

回环地址上的 https 跳过证书校验（兼容自签证书部署），非回环 https 正常校验。

## 设计文档

见 [docs/design/2026-08-16-cpa-proxy-manager-design.md](docs/design/2026-08-16-cpa-proxy-manager-design.md)。UI 原型为 Penpot 项目 `cpa-proxy-manager`（「界面设计」页面）。

## 注意事项

- 清空代理提交空字符串而非 `null`：CPA 对 metadata 值做字符串类型断言，`null` 会被静默忽略、旧值残留
- 渲染逻辑只在插件后端实现（`render.go`），预览与执行使用同一函数，前端不复制渲染规则
- 认证文件 JSON 不离开插件进程：页面只收到预渲染结果与当前代理值
