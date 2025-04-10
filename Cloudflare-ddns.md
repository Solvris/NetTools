
# cloudflare-ddns 
Go Cloudflare DDNS Updater .
一个使用 Go 语言编写的 Cloudflare 动态 DNS (DDNS) 更新工具。

该脚本会自动检测指定网络接口的公网 IP 地址（支持 IPv4 和 IPv6），并调用 Cloudflare API v4 来更新（或创建）相应的 DNS 记录。它还包含缓存机制，以减少不必要的 API 调用。

## ✨ 功能特性

*   **自动 IP 检测:** 从指定网络接口获取当前的公网 IPv4 或 IPv6 地址（尝试使用 `ip` 命令，若失败则回退到 `ifconfig`）。
*   **Cloudflare API v4:** 使用官方 API 更新 DNS 记录。
*   **记录类型:** 支持更新 A (IPv4) 和 AAAA (IPv6) 记录。
*   **灵活配置:** 通过 `config.json` 文件进行配置，方便管理。
*   **Zone ID 自动缓存:** 自动获取 Zone ID 并在配置文件中缓存，避免重复查询。
*   **IP 地址缓存:** 在本地缓存上一次成功更新的 IP，仅当 IP 变化时才执行 Cloudflare API 更新，减少 API 请求。
*   **代理状态配置:** 可配置是否启用 Cloudflare 的代理功能 (`proxied`)。
*   **TTL 配置:** 可自定义 DNS 记录的 TTL。
*   **自定义工作目录:** 可指定 IP 缓存文件的存储目录。

## 📋 先决条件

*   **Go 环境:** 需要安装 Go 语言环境（建议 1.16+）。
*   **Cloudflare 账户与域名:** 你需要一个 Cloudflare 账户以及一个由 Cloudflare 管理的域名。
*   **Cloudflare API Token:** 需要一个 Cloudflare API Token。**强烈建议**创建具有特定区域 DNS 编辑权限的自定义 Token (`Zone:Zone:Read`, `Zone:DNS:Edit`)，而非全局 API Key。
*   **操作系统:** 推荐在 Linux/Unix-like 系统上运行 (依赖 `ip` 或 `ifconfig`)。

## 🚀 安装与设置

1.  **获取代码:**
    ```bash
    git clone https://github.com/你的用户名/你的仓库名.git
    cd 你的仓库名
    ```
    或直接下载 `ddns-cl.go` 文件。

2.  **编译 (推荐):**
    ```bash
    go build ddns-cl.go
    ```
    生成 `ddns-cl` 可执行文件。

3.  **创建配置文件 (`config.json`):**
    在项目目录（或你希望存放配置的地方）创建 `config.json`。复制以下内容并根据你的实际情况修改：

    ```json
    {
      "api_token": "YOUR_CLOUDFLARE_API_TOKEN",
      "zone": "yourdomain.com",
      "record": "subdomain",
      "ipversion": "ipv4",
      "interface": "eth0",
      "ttl": 300,
      "proxied": false,
      // "zone_id": "YOUR_ZONE_ID_WILL_BE_AUTO_ADDED_HERE_AFTER_FIRST_RUN",
      // "work_dir": "/var/cache/cloudflare-ddns"
    }
    ```
    **请务必将 `api_token` 替换为你的真实 Cloudflare API Token，并确保证书文件的安全！**

## ⚙️ 配置详解 (`config.json`)

*   `api_token` (**必需**): 你的 Cloudflare API Token。
*   `zone` (**必需**): 你在 Cloudflare 上管理的根域名 (e.g., `example.com`)。
*   `record` (**必需**): 要更新的 DNS 记录名 (e.g., `subdomain` 或 `@` 代表根域名)。
*   `ipversion` (**必需**): 获取和更新的 IP 类型 (`"ipv4"` 或 `"ipv6"`)。
*   `interface` (**必需**): 获取公网 IP 的网络接口名 (e.g., `eth0`, `ppp0`)。
*   `ttl` (**必需**): DNS 记录的 TTL (秒)。`1` 表示 "Automatic"。建议动态 IP 使用较短值 (e.g., `300`)。
*   `proxied` (**必需**): 是否启用 Cloudflare 代理 (`true` 为启用/橙色云朵, `false` 为禁用/灰色云朵)。
*   `zone_id` (*可选*): 你的域名的 Zone ID。
    *   **自动缓存:** 你可以留空或省略此字段。脚本首次成功运行时，会自动获取 Zone ID 并尝试写回到 `config.json` 文件中。
    *   **权限:** **脚本需要对 `config.json` 文件有写入权限** 才能自动保存 `zone_id`。若无权限，会打印警告且每次重新获取。
    *   **重置:** 如果你的 `zone` 域名更改，需要手动清空此字段以强制重新获取。
*   `work_dir` (*可选*): 指定 IP 缓存文件 (`.lastip` 后缀) 的存储目录。
    *   **路径:** 可以是绝对路径 (e.g., `/var/cache/cf-ddns`) 或相对路径 (e.g., `cache`)。
    *   **权限:** **指定的目录必须存在，且脚本需要对其有写入权限**。脚本不会自动创建此目录。
    *   **默认:** 如果省略或为空，缓存文件将存储在与 `config.json` 相同的目录中。

## ⚡ IP 地址缓存机制

为了避免在 IP 地址未变化时频繁调用 Cloudflare API，脚本使用了本地 IP 缓存：

*   **缓存文件:** 脚本会维护一个 IP 缓存文件。文件名基于配置文件名，后缀为 `.lastip` (e.g., `config.json.lastip`)。
*   **存储位置:** 缓存文件的位置由 `config.json` 中的 `work_dir` 字段决定。如果 `work_dir` 未指定，则存储在与 `config.json` 相同的目录。
*   **工作原理:**
    1.  脚本启动时，获取当前接口的公网 IP。
    2.  读取缓存文件中的上一次记录的 IP。
    3.  如果当前 IP 与缓存 IP **相同**，脚本会打印一条消息并直接退出，不执行任何 Cloudflare API 操作。
    4.  如果当前 IP 与缓存 IP **不同**，或者缓存文件不存在/为空，脚本会继续执行 Cloudflare 的检查和更新流程。
    5.  如果 Cloudflare 记录成功更新或确认无需更新 (API success)，脚本会将**当前 IP** 写入缓存文件。
*   **权限:** 脚本需要对缓存文件及其所在目录（如果使用 `work_dir`）有**读写权限**。
*   **强制更新:** 如果你想强制脚本执行一次 Cloudflare API 检查与更新（例如，你修改了 `proxied` 或 `ttl` 配置，但 IP 未变），只需**手动删除**对应的 `.lastip` 缓存文件即可。

## 💡 使用方法

*   **如果已编译:**
    ```bash
    ./ddns-cl -f /path/to/your/config.json
    ```
*   **如果直接运行 Go 文件:**
    ```bash
    go run ddns-cl.go -f /path/to/your/config.json
    ```
    (请将路径替换为实际路径)

## ⏳ 自动化运行 (Cron)

使用 `crontab -e` 添加类似条目（示例：每 5 分钟运行一次）：

```crontab
*/5 * * * * /path/to/your/ddns-cl -f /path/to/your/config.json >> /path/to/logfile.log 2>&1


确保使用绝对路径指向可执行文件和配置文件。

>> /path/to/logfile.log 2>&1 用于记录日志（可选）。

📜 许可证

(建议添加一个许可证，例如 MIT)

MIT License

Copyright (c) [年份] [你的名字或组织名]

Permission is hereby granted, free of charge, to any person obtaining a copy
... (完整的 MIT 许可证文本) ...
IGNORE_WHEN_COPYING_START
content_copy
download
Use code with caution.
IGNORE_WHEN_COPYING_END

希望这个工具对你有帮助！

这样应该更清晰地解释了 `zone_id` 和 `work_dir` 的配置以及 IP 缓存的工作方式和相关要求。
IGNORE_WHEN_COPYING_START
content_copy
download
Use code with caution.
IGNORE_WHEN_COPYING_END
