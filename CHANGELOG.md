## 1.8.0

- 新增 `island_mode`（默认关闭）：在主显示器顶部中央显示深色灵动岛，收起态固定宽度并显示最近到期计时器剩余时间，悬停展开计时器列表。
- 展开面板支持运行中暂停/取消、已暂停继续/取消、待确认已知晓/稍后提醒，以及打开列表和全部已知晓；最多显示 6 行并支持滚轮浏览。
- 待确认时药丸切换暖橙色并呼吸脉冲；到期声音和 15 秒/60 秒重复提醒保持不变。岛模式开启时到期提醒改由灵动岛承载，不再弹出原悬浮窗或发送到期 toast；开始通知与到期前 1 分钟提醒仍走 Win顺 toast。
- 关闭灵动岛模式后恢复原悬浮窗/系统通知行为；托盘、预设、结束时间、暂停/继续/重新开始和自定义声音保持不变。

## 1.7.0

- 空查询列表新增 9 个一键时长预设：5、10、15、25、30、45、60 分钟，以及“番茄专注”（25 分钟）和“休息”（5 分钟）；按 Enter 直接开始，预设不自动串联。已有计时器时预设排在列表末尾，手动非空查询不显示预设。
- 正在运行的计时器副标题新增本地预计结束时间，格式为“剩余 mm:ss · HH:MM 结束 · Enter 取消”；暂停和待确认项目保持原样。
- 托盘图标在有 running 计时器时显示最先到期项目的剩余整分钟数（向上取整，最大显示 99+）；没有 running 项目时恢复纯时钟。显示值不变时复用原 HICON，变化时才重建并仅销毁插件拥有的旧图标。
- 托盘图标和 tooltip 在调度器每次最长 30 秒的唤醒后刷新；分钟数字最多可能晚约 30 秒跳变。
- 新增到期前 1 分钟 toast：原始总时长超过 60 秒、且当前一轮尚未提醒的 running 计时器，在结束前 1 分钟提醒一次；不播放声音、不弹悬浮窗、不改变计时状态。`preReminded` 写入 v2 数据，旧记录缺失时默认为 false；重新开始重置标记，暂停/继续保留标记。
- 插件版本及 manifest assembly version 升至 1.7.0 / 1.7.0.0；不新增设置或权限，数据版本继续为 v2。

# Changelog

## 1.6.1

- 优化悬浮窗滑入/淡出：入场 220ms、退场 160ms，动画帧间隔调整为约 10ms；仅在动画活动期间请求 1ms 定时器分辨率，并在结束、隐藏或动画设置关闭时配对释放，降低卡顿且不长期增加系统唤醒。
- 自定义提示音升级为 WAV 内容校验：不仅检查 `.wav` 扩展名，还验证 RIFF/WAVE 标识、RIFF 长度、`fmt ` 格式块和非空 `data` 块；选择无效文件时提示，播放前再次检查并回退系统默认音。
- `plugin.json` 的 `sound_file` 提示说明更新；由于当前 Win顺 `file` 设置 schema 没有扩展名过滤字段，文件选择器仍可能显示其他格式，由插件选择后验证。
- 插件版本及 manifest assembly version 升至 1.6.1 / 1.6.1.0。

## 1.6.0

- 新增 Windows 任务栏通知区域托盘图标（`tray_icon` 默认开启）：由插件进程自己的隐藏消息窗口和 GUI 线程管理，与到期悬浮确认窗相互独立。
- 左键或右键点击托盘图标都会在点击时读取最新计时器快照并动态生成菜单；运行中项目可暂停/取消/重新开始，已暂停项目可继续/取消，待确认项目可确认/稍后提醒/重新开始。每类项目按列表对应的到期时间或剩余时间排序，菜单最多展示 20 项并提示剩余数量。
- 托盘悬停提示随计时器状态变化更新，显示运行中数量、最近到期时间及待确认数量；图标由代码绘制蓝底白针时钟，不依赖外部 `.ico` 文件。
- 新增 `tray_icon` bool 设置（默认 true），设置变化立即添加或删除图标；收到 Explorer 的 `TaskbarCreated` 消息后自动重新添加。正常 shutdown 时通过 `NIM_DELETE` 删除图标。
- 菜单“打开倒计时列表…”通过 Win顺 系统通知的 click/setQuery 中转，用户需先点菜单项，再点通知打开列表；进程被宿主直接杀掉时可能留下短暂幽灵图标，鼠标划过后由 Windows 清理。
- 插件版本及 manifest assembly version 升至 1.6.0 / 1.6.0.0；权限与 `minWinShun: 0.22.0` 保持不变。

## 1.5.0

- 新增暂停/继续：运行中计时器可暂停并持久化剩余秒数；暂停项排在列表末尾，Enter 继续，第二个动作取消。暂停项不占 20 个运行名额，也不会参与后台到期调度。
- 新增重新开始：运行中和待确认计时器可从原始总时长重新计时；重置结束/重复提醒字段，待确认项目重新开始时停止当前提示音。
- 新增到期声音提示：`sound_enabled` 默认开启；支持 `sound_file` 文件设置，仅接受 WAV。首次到期和每次重复提醒播放异步声音，确认/稍后提醒时停止；路径无效或播放失败时记录日志并回退系统默认音。默认声音使用 Windows `PlaySoundW` 的 `SND_ALIAS` / `SystemAsterisk`。
- 新增 `popup_center` 设置（默认关闭）：可将悬浮窗放到当前最近显示器工作区中央；关闭时保持原有右下角位置，设置变化立即生效。
- 数据格式版本保持 v2；新增可选 `originalSeconds` 和 `remainingSeconds` 字段。旧记录缺少 `originalSeconds` 时自动按 `endsAt - createdAt` 回填，无需手动迁移；暂停记录以 `status: paused` 和 `remainingSeconds` 持久化。
- 插件版本及 manifest assembly version 升至 1.5.0 / 1.5.0.0。

## 1.4.0

- Deferred floating window/notification wakeups are capped at 2.5 seconds even when a pending timer's next repeat is far away. Without `deferredWindow`, the nearest deadline and 30-second cap are unchanged.
- Scheduler: replaced the permanent 1-second ticker with one `time.Timer` reset to the nearest `EndsAt`/`NextReminderAt`; no timers means the scheduler blocks. While timers are active the maximum sleep is 30 seconds as a sleep/clock-change safeguard; window `WM_POWERBROADCAST`/`WM_TIMECHANGE` wakes the scheduler. Injected `now()` remains the test clock.
- Window loop: replaced the 10ms `PeekMessage`/`Sleep` poll with a blocking `GetMessageW` loop. Commands are posted with `WM_APP+1`; auto-close uses `SetTimer`; activity resets from mouse/keyboard messages. Hidden and non-animating windows make no polling wakeups.
- `processDue` now scans for due work before allocating a rollback snapshot.
- Floating window: layered fade/slide entry (180ms) and fade exit (120ms), a `animations` setting (default true), and a system `SPI_GETCLIENTAREAANIMATION` check. Entry/exit uses `SetLayeredWindowAttributes` + `SetWindowPos`; animation state/easing lives in `anim.go` with injected-clock tests.
- Visible reminder refresh no longer resets the scroll position; repeat reminders update the existing window instead of bringing it to the foreground.
- Version bumped to 1.4.0; manifest assembly version corrected to 1.4.0.0.
- Not yet implemented in this pass: appearance/locale propagation, borderless custom title bar, DWM corners/backdrop, row-collapse animation, pulse animation, async off-lock persistence, button reuse, double-buffered paint/system message font, and multi-monitor placement. See README limitations.

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
