# rclone `115` backend (read-only)

[rclone](https://rclone.org) 的 **115 网盘（115.com）只读后端**，基于
[SheltonZhu/115driver](https://github.com/SheltonZhu/115driver) 实现。

## 特性

- **只读**：列目录、读取文件、Range 随机读；`Put`/`Mkdir`/`Rmdir`/`Remove`/`Update` 全部返回只读错误
- 可用于 `rclone mount`、`rclone copy`、`rclone cat`、WebDAV 等所有读取路径
- SHA1 校验（115 自带文件哈希）
- 进程级共享 client（cookie 校验只做一次）+ 跨 Fs 的路径/目录缓存
- 全局 API 串行 + 最小间隔限流，识别 115 的 WAF 拦截后全局冷却
- 实现 `About()`，`rclone about` / `df` 可显示 115 容量

## 为什么是一个 fork

rclone 的 backend 在 `backend/all/all.go` 里是**编译期注册**的，不能动态加载，
因此新增 backend 必须 fork rclone 后重新编译。本仓库就是这样一个 fork，
新增内容集中在一个目录：`backend/115/`。

## 构建

```bash
go build -o rclone-115 .
```

- 需要 Go 1.26+（rclone 当前要求）；`GOTOOLCHAIN=auto` 会自动下载对应 toolchain。
- 产物是完整 rclone，含本 backend，用法与官方 rclone 相同。

## 配置

需要 115 cookie 中的四个值：`UID` / `CID` / `SEID` / `KID`。

### 方式一：命令行

```bash
rclone-115 config create my115 115 \
  uid='<UID>' cid='<CID>' seid='<SEID>' kid='<KID>'
```

### 方式二：直接编辑 `~/.config/rclone/rclone.conf`

```ini
[my115]
type = 115
uid = <UID>
cid = <CID>
seid = <SEID>
kid = <KID>
qps = 2
```

### 后端选项

| 选项 | 默认 | 说明 |
|---|---|---|
| `qps` | `2` | 115 API 每秒最大请求数。**最小为 1.5**：配置低于 1.5 会被强制为 1.5 |
| `api_timeout` | `1m` | 115 API 请求的 HTTP 超时。不设的话，被 WAF 拦住的请求可能无限挂起、拖死挂载 |
| `list_cache_time` | `1h` | 路径解析与目录列表的缓存时长（`0` 关闭） |
| `download_cache_time` | `5m` | 下载地址（pickCode→签名URL）的复用时长。同文件短时间重复打开（双读流、重试、扫描）直接复用，**减少向 115 取下载地址的请求**（`0` 关闭） |
| `page_size` | `1000` | 每个列表请求的条数（上限 1150） |
| `list_api_urls` | 3 个端点 | 目录列表端点，按顺序回落，用于绕开单个端点被 WAF 拦截 |

## 使用

```bash
rclone-115 lsd my115:                    # 列目录
rclone-115 ls  my115:some/dir            # 递归列文件
rclone-115 lsl my115:some/dir            # 列文件 + 大小
rclone-115 cat my115:some/file > out     # 读取/下载单文件
rclone-115 copy my115:some/dir /local    # 批量下载
rclone-115 about my115:                  # 容量
```

### 挂载成本地盘

```bash
mkdir -p /mnt/115
rclone-115 mount my115: /mnt/115 --read-only --allow-other \
  --vfs-read-chunk-size 4M --vfs-read-chunk-streams 2 \
  --buffer-size 32M --dir-cache-time 1h
```

> `--allow-other` 需要 `/etc/fuse.conf` 里开启 `user_allow_other`。

### 开机自启（systemd）

见 [`rclone-115.service.example`](rclone-115.service.example)。其中几项不是可有可无的：

| 参数 | 为什么必须 |
|---|---|
| `--timeout 2m` | CDN 下载走 rclone 的 `fshttp`，它的空闲超时就是 `--timeout`（默认 5m）。卡住时进程要在不可中断睡眠里待满这个时间才拿到错误，调小能让 Plex 更快拿到 EIO 而不是干等 |
| `--115-api-timeout 1m` | 115 API 请求的硬超时，防止被 WAF 拦住的请求无限挂起（后端默认值即 1m，写出来是为了显式可见） |
| `--115-qps 2` | 115 API 请求速率上限（最低会被抬到 1.5），避免触发风控 |
| `ExecStop=fusermount -uz` | **懒卸载**。挂载点卡死时 `fusermount -u` 会一直阻塞到 `TimeoutStopSec` 超时，让 unit 处于半死状态 |

### 卡死自愈（看门狗，强烈建议）

FUSE 内核驱动**没有请求超时**：后端没应答某个请求时，调用者会一直睡在
`request_wait_answer()`（不可中断睡眠，连 `SIGKILL` 都无效），整个挂载点停止服务。
**systemd 也救不了** —— 进程还活着，`Restart=on-failure` 根本不会触发，
所以一次卡死可以持续好几天。

[`rclone-115-watchdog.sh`](rclone-115-watchdog.sh) 轮询内核的每连接 `waiting` 计数，
连续多次不为 0 就 `abort` 该 FUSE 连接（把卡住的任务以 EIO 释放）并重启挂载：

```bash
sudo install -m 755 rclone-115-watchdog.sh      /usr/local/bin/rclone-115-watchdog
sudo install -m 644 rclone-115-watchdog.service /etc/systemd/system/
sudo install -m 644 rclone-115-watchdog.timer   /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now rclone-115-watchdog.timer
```

默认每分钟检查一次、连续 3 次命中才动手，即**卡死约 3 分钟内自动恢复**，
并把现场（`waiting` 数、连接 id、rclone 进程、`dmesg` 尾部）记进 journal。

### 另一个陷阱：core dump 写进 FUSE

`kernel.core_pattern` 是**相对路径**（Debian 默认 `core`）时，内核会把 core 文件写到
**崩溃进程的 cwd**。如果那个 cwd 落在 FUSE 挂载点上，coredump 的写操作本身就会卡住，
并且 `PF_POSTCOREDUMP` 会让这些线程**不会被 `SIGKILL` 清理**，形成永久 D 状态。
本 backend 只读，所以写 core 会 `EROFS` 快速失败，但**一旦启用写支持、或 cwd 落在
别的可写 FUSE 上，这就是真死锁**。建议改成绝对路径：

```ini
# /etc/sysctl.d/99-coredump.conf
kernel.core_pattern = /var/crash/core.%e.%p.%t
```

## 限制与注意

- **只读**：不支持上传、改名、移动、删除。
- **速度取决于账号等级**：115 对**非会员**第三方下载限速约 100 kB/s；单文件最多 2 线程
  （用 `--vfs-read-chunk-streams 2` 可略微提速）。要流畅看在线的视频需要会员账号。
- **cookie 会过期**：失效时报 `990001 登录超时，请重新登录`。重新登录后更新配置：
  ```bash
  rclone-115 config update my115 uid='...' cid='...' seid='...' kid='...'
  ```
- 走 115 的**私有 API（cookie）**，不是官方 Open API，存在被风控的可能。
  请勿高频调用，用 `qps` 控制请求速率（已设最小 1.5，防过慢）。
- **FUSE 挂载点会卡死且不可自愈**：内核不给 FUSE 请求设超时，后端一旦不应答，
  相关进程就永久停在不可中断睡眠，且 systemd 看不出来。**必须配看门狗**，
  见上文《卡死自愈》。

## 设计说明

- **路径解析**：优先用 `files/getid`（`DirName2CID`）一次请求解析整条路径；
  失败回落到逐级列目录。列目录时把子目录的 cid 回填缓存，后续访问无需再解析。
- **列目录**：用低层 `GetFiles`（可拿到总数 `count`，精确翻页、逐页限流），
  默认每页 1000 条，避免大目录产生大量连续请求。
- **共享状态**：整个进程共用一个 `115driver` client 与缓存，`CookieCheck` 只执行一次。
- **限流**：所有 API 调用经过一个全局锁 + 最小间隔；CDN 下载直连、不参与串行。
- **下载签名**：115 的下载 URL 签名与请求时的 `User-Agent` 绑定，因此用
  **rclone 实际会发送的 UA** 去取 URL（rclone 的 `fshttp` 会强制覆盖 UA）。
  这一点参考了 OpenList 的做法（`DownloadWithUA(pickCode, userAgent)`）。

## License

MIT（沿用 rclone）。本 backend 依赖同样为 MIT 的
[115driver](https://github.com/SheltonZhu/115driver)。
