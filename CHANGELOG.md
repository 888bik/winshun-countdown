# Changelog

## 1.3.0

- 新增 `auto_close_seconds` 设置（0–600，默认 0）：悬浮窗闲置 N 秒自动隐藏；鼠标进入、点击、按键重置计时；自动关闭不确认，下一次重复提醒会再弹。
- 新增 `snooze_minutes` 设置（1–120，默认 5）和稍后提醒：窗口每行加“稍后提醒”，底部加“全部稍后提醒”；列表待确认行增加第二个 snooze 动作。稍后提醒把计时器改回 running，EndsAt = now + N 分钟，reminderCount/NextReminderAt 清零，然后重新走完整提醒流程。
- 新增 `repeat_first_seconds`（5–300，默认 15）和 `repeat_every_seconds`（10–3600，默认 60），运行时读取并 clamp；默认仍是“立即 → 15 秒 → 每 60 秒”。
- 修复滚动：`scrollBy` 后重新布局/重建按钮，>6 条滚动时按钮和文字保持对应。
- 修复 manifest：`build.ps1` 用 Windows SDK `mt.exe` 把 `countdown.exe.manifest` 嵌入 exe，并自查 `Microsoft.Windows.Common-Controls`；原生 BUTTON 使用系统视觉样式。
- 窗口背景改用 `GetSysColor(COLOR_WINDOW)`，文字使用系统 `COLOR_WINDOWTEXT`/`COLOR_GRAYTEXT`，不再硬编码纯白。
- 通知/悬浮窗继续互斥：开窗时不发到期/重复通知，关窗时按 1.1.0 通知；窗口创建失败发通知兜底。
- 版本升到 1.3.0；`minWinShun` 保持 0.22.0，权限仍为 notifications + ui。

## 1.2.0

- 新增独立 Win32 悬浮确认窗：有 pending_ack 时在右下角任务栏上方弹出，标题“倒计时”，每行标题 + “已结束” + “已知晓”，底部“全部已知晓”。
- 悬浮窗与系统通知互斥：`floating_window` 开启时，到期、重复提醒、启动恢复只弹窗不发通知；关闭时回到 1.1.0 的仅通知 + 列表。窗口创建失败会发通知兜底。
- 窗口布局用 `AdjustWindowRectEx` 把 340×内容高度的客户区换算成窗口尺寸，修复“全部已知晓”压住最后一行、“已结束”被截断的问题；每行 52 逻辑像素。
- 行内“已知晓”和底部“全部已知晓”改为真正的 Win32 `BUTTON` 子控件，带系统主题、hover 和按下状态，通过 `WM_COMMAND/BN_CLICKED` 回调；不再手画假按钮。
- 点单条只确认该计时器；点全部确认全部；待确认清空自动收起。标题栏 × 只隐藏，不确认、不改 pending_ack，下一次重复提醒重新显示同一个窗口。
- 搜索列表里的 Enter 确认会立即同步到悬浮窗；悬浮窗确认也会更新列表和持久化。
- 列表可见性用 query 心跳推断：最后一次 countdown 查询后 2.5 秒内不抢前台；query/action 执行期间不弹，关闭列表后再补弹。
- 新增 `floating_window` bool 设置（默认 true），关闭立即隐藏，行为回到 1.1.0 的通知 + 列表。
- 新增 `ui` 权限；升级用户需要重新确认新增权限。`minWinShun` 仍为 `0.22.0`。
- 悬浮窗使用独立 goroutine、`runtime.LockOSThread` 和 Win32 消息循环；Win32 实现在 `window_windows.go`，非 Windows 用 `window_stub.go`，核心只依赖 fake-able 接口。
- DPI 使用 Per-Monitor Aware V2，窗口宽 340 逻辑像素、最大高 480 逻辑像素，按 DPI 缩放并处理 `WM_DPICHANGED`；>6 条可滚动。
- 窗口状态只在内存中，不写 v2 数据文件；重复创建窗口、重复确认和 GUI/协议锁互相等待均已避免。

## 1.1.0

- 到期不再直接删除：`running` 转为 `pending_ack`，用户按 Enter 确认后才移除；`pending_ack` 不占 20 个运行名额。
- 重复提醒：首次到期立即通知，15 秒后首次重复，之后每 60 秒一次；绝对时间 `nextReminderAt` 持久化，确认后停止；多条待确认合并通知。
- 待确认列表排在运行中项目之前，每项使用稳定 ID；新增 `acknowledge` action，只在 Enter 后执行并再次校验状态。
- 通知 click 使用 `setQuery` 打开 `倒计时 `，不会自动确认；文案明确提示“点我打开列表，按 Enter 确认”两步流程。
- 数据格式升级到 v2，新增 `status`、`endedAt`、`nextReminderAt`、`reminderCount`；v1 自动迁移，已到期旧记录转为 pending，不直接清除，迁移原子写回。
- 确认与后台提醒使用同一把锁串行化，确认成功后不再发送该计时器提醒。
- 后台 ticker 保持 1 秒；测试全部使用可注入时间，不依赖真实 sleep。

## 1.0.0

- 初始版本：原生 Go 常驻插件，标准库实现 JSON-RPC 2.0 一行一条协议。
- 输入标题和时长创建倒计时，支持纯数字、秒/分/小时、组合时长和 `mm:ss`；最多同时运行 20 个。
- 独立后台 ticker 计时，关掉搜索窗口后继续；到期批量通知并刷新显示中的列表。
- `countdown.json` 原子持久化，重启按截止时间恢复；恢复时先清除过期记录再发一次汇总通知。
- 支持取消、队列刷新、`settingsChanged`、`shutdown`。
- 只申请 `notifications` 权限；系统通知使用 `winshun/notify` 的 toast。
- `minWinShun` 设为 `0.22.0`：未审核原生插件的宿主最低要求；`eager` 在该版本也可用。`memoryMB` 使用 Schema 允许的最小值 64（任务说明的 32 低于宿主/Schema 下限，无法合法写入）。
- `build.ps1` 重新保存为 UTF-8 with BOM，保证 Windows PowerShell 5.1 下中文不乱码。
- 后台到期检查从 250ms 调整为 1 秒，与列表每秒刷新一致，减少空闲唤醒。
- 纯数字时长在乘以 60 前先做上限检查，避免极大整数乘法溢出绕过 365 天上限；新增非法大数测试。
- 提供 `build.ps1`：Go 1.22+ 检查、vet、gofmt、测试、Windows x64 构建和真实二进制协议冒烟测试。

### 已知边界

- 通知协议没有送达确认；采用先持久化清除、再通知的 at-most-once 策略，不能承诺崩溃场景的 exactly-once。
- 数据文件损坏时明确报告并拒绝覆盖；需要用户修复或删除文件后才能继续创建。
