package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	appName         = "me.countdown"
	displayName     = "倒计时"
	dataFileName    = "countdown.json"
	maxTimers       = 20
	maxDuration     = int64(365 * 24 * 60 * 60)
	tickInterval    = 1 * time.Second
	repeatFirst     = 15 * time.Second
	repeatEvery     = 60 * time.Second
	dataVersion     = 2
	statusRunning   = "running"
	statusPending   = "pending_ack"
	statusPaused    = "paused"
	acknowledged    = "acknowledged"
	preReminderLead = time.Minute
	parseErrorText  = "时长看不懂，试试 25、25m、90s、1h30m、25:00。"
	emptyTitle      = "目前没有正在运行的倒计时。"
	emptyHint       = "输入 泡面 3 即可开始。"
)

type timer struct {
	ID               string
	Title            string
	CreatedAt        time.Time
	EndsAt           time.Time
	Status           string
	EndedAt          time.Time
	NextReminderAt   time.Time
	ReminderCount    int
	OriginalSeconds  int64
	RemainingSeconds int64
	PreReminded      bool
}

type storedTimer struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	CreatedAt        string `json:"createdAt"`
	EndsAt           string `json:"endsAt"`
	Status           string `json:"status,omitempty"`
	EndedAt          string `json:"endedAt,omitempty"`
	NextReminderAt   string `json:"nextReminderAt,omitempty"`
	ReminderCount    int    `json:"reminderCount,omitempty"`
	OriginalSeconds  int64  `json:"originalSeconds,omitempty"`
	RemainingSeconds int64  `json:"remainingSeconds,omitempty"`
	PreReminded      bool   `json:"preReminded,omitempty"`
}

type storedFile struct {
	Version int           `json:"version"`
	Timers  []storedTimer `json:"timers"`
}

type windowCallbacks struct {
	Ack       func(id string)
	AckAll    func()
	Snooze    func(id string)
	SnoozeAll func()
	Failed    func()
	Wake      func()
}

type pendingItem struct {
	ID    string
	Title string
}

type windowBackend interface {
	Show(items []pendingItem) bool
	Refresh(items []pendingItem)
	Hide()
	Visible() bool
	SetAutoClose(seconds int)
	SetAnimations(enabled bool)
	SetPopupCenter(enabled bool)
}

type soundBackend interface {
	PlayFile(path string) error
	PlayDefault() error
	Stop()
}

type trayTimerSnapshot struct {
	ID               string
	Title            string
	Status           string
	RemainingSeconds int64
	EndsAt           time.Time
	EndedAt          time.Time
}

type trayActionBinding struct {
	Action   string
	TimerID  string
	OpenList bool
}

type trayMenuItem struct {
	Label     string
	Disabled  bool
	Separator bool
	CommandID uint32
	Children  []trayMenuItem
}

type trayMenuModel struct {
	Items    []trayMenuItem
	Commands map[uint32]trayActionBinding
}

type trayCallbacks struct {
	Snapshot func() []trayTimerSnapshot
	Action   func(timerID, action string)
	OpenList func()
}

type trayBackend interface {
	SetEnabled(enabled bool)
	Update(items []trayTimerSnapshot)
	TaskbarCreated()
	Close()
}

const (
	trayMaxMenuTimers  = 20
	trayFirstCommandID = uint32(5000)
)

func normalizedTrayStatus(status string) string {
	if status == "" {
		return statusRunning
	}
	return status
}

func truncateTrayTitle(title string, limit int) string {
	if limit < 1 {
		return ""
	}
	runes := []rune(title)
	if len(runes) <= limit {
		return title
	}
	return string(runes[:limit]) + "…"
}

func sortedTraySnapshots(items []trayTimerSnapshot) (running, paused, pending []trayTimerSnapshot) {
	for _, item := range items {
		switch normalizedTrayStatus(item.Status) {
		case statusRunning:
			running = append(running, item)
		case statusPaused:
			paused = append(paused, item)
		case statusPending:
			pending = append(pending, item)
		}
	}
	sort.Slice(running, func(i, j int) bool {
		if running[i].EndsAt.Equal(running[j].EndsAt) {
			return running[i].ID < running[j].ID
		}
		if running[i].EndsAt.IsZero() || running[j].EndsAt.IsZero() {
			if running[i].RemainingSeconds == running[j].RemainingSeconds {
				return running[i].ID < running[j].ID
			}
			return running[i].RemainingSeconds < running[j].RemainingSeconds
		}
		return running[i].EndsAt.Before(running[j].EndsAt)
	})
	sort.Slice(paused, func(i, j int) bool {
		if paused[i].RemainingSeconds == paused[j].RemainingSeconds {
			return paused[i].ID < paused[j].ID
		}
		return paused[i].RemainingSeconds < paused[j].RemainingSeconds
	})
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].EndedAt.Equal(pending[j].EndedAt) {
			return pending[i].ID < pending[j].ID
		}
		return pending[i].EndedAt.Before(pending[j].EndedAt)
	})
	return running, paused, pending
}

func buildTrayMenuModel(items []trayTimerSnapshot) trayMenuModel {
	running, paused, pending := sortedTraySnapshots(items)
	header := "倒计时 · 没有正在运行的计时器"
	if len(running) > 0 {
		header = fmt.Sprintf("倒计时 · %d 个在跑", len(running))
	}
	if len(pending) > 0 {
		header += fmt.Sprintf(" · %d 个待确认", len(pending))
	}

	model := trayMenuModel{
		Items:    []trayMenuItem{{Label: header, Disabled: true}},
		Commands: make(map[uint32]trayActionBinding),
	}
	nextID := trayFirstCommandID
	addTimer := func(item trayTimerSnapshot, label string, actions ...string) {
		entry := trayMenuItem{Label: truncateTrayTitle(label, 100)}
		for _, action := range actions {
			commandID := nextID
			nextID++
			model.Commands[commandID] = trayActionBinding{Action: action, TimerID: item.ID}
			entry.Children = append(entry.Children, trayMenuItem{
				Label: actionTitle(action), CommandID: commandID,
			})
		}
		model.Items = append(model.Items, entry)
	}

	ordered := make([]trayTimerSnapshot, 0, len(items))
	ordered = append(ordered, running...)
	ordered = append(ordered, paused...)
	ordered = append(ordered, pending...)
	visible := len(ordered)
	if visible > trayMaxMenuTimers {
		visible = trayMaxMenuTimers
	}
	for _, item := range ordered[:visible] {
		switch normalizedTrayStatus(item.Status) {
		case statusRunning:
			addTimer(item, truncateTrayTitle(item.Title, 40)+" · 剩余 "+formatSeconds(item.RemainingSeconds), "pause", "cancel", "restart")
		case statusPaused:
			addTimer(item, truncateTrayTitle(item.Title, 40)+" · 已暂停，剩余 "+formatSeconds(item.RemainingSeconds), "resume", "cancel")
		case statusPending:
			addTimer(item, truncateTrayTitle(item.Title, 40)+" · 已到期，等待确认", "acknowledge", "snooze", "restart")
		}
	}
	if len(ordered) > visible {
		model.Items = append(model.Items, trayMenuItem{
			Label:    fmt.Sprintf("还有 %d 个，去列表里看", len(ordered)-visible),
			Disabled: true,
		})
	}
	model.Items = append(model.Items, trayMenuItem{Separator: true})
	openID := nextID
	model.Commands[openID] = trayActionBinding{OpenList: true}
	model.Items = append(model.Items, trayMenuItem{Label: "打开倒计时列表…", CommandID: openID})
	return model
}

func buildTrayTooltip(items []trayTimerSnapshot) string {
	running, _, pending := sortedTraySnapshots(items)
	text := "倒计时：没有正在运行的计时器"
	if len(running) > 0 {
		text = fmt.Sprintf("倒计时：%d 个在跑，最近 %s 后到期", len(running), formatSeconds(running[0].RemainingSeconds))
		if len(pending) > 0 {
			text += fmt.Sprintf(" · %d 个待确认", len(pending))
		}
	} else if len(pending) > 0 {
		text = fmt.Sprintf("倒计时：没有正在运行的计时器 · %d 个待确认", len(pending))
	}
	return truncateTrayTitle(text, 127)
}

// trayIconMinuteValue returns the rounded-up minute label for the running
// timer that expires first. An empty string means the plain clock icon.
func trayIconMinuteValue(items []trayTimerSnapshot) string {
	running, _, _ := sortedTraySnapshots(items)
	if len(running) == 0 {
		return ""
	}
	seconds := running[0].RemainingSeconds
	if seconds < 1 {
		seconds = 1
	}
	minutes := (seconds + 59) / 60
	if minutes > 99 {
		return "99+"
	}
	return strconv.FormatInt(minutes, 10)
}

func trayIconValueChanged(previous string, initialized bool, next string) bool {
	return !initialized || previous != next
}

func shouldDestroyOwnedTrayIcon(oldIcon uintptr, oldOwned bool, newIcon uintptr) bool {
	return oldIcon != 0 && oldOwned && oldIcon != newIcon
}

type Plugin struct {
	outMu sync.Mutex
	out   io.Writer

	mu              sync.Mutex
	dataDir         string
	settings        map[string]any
	timers          map[string]*timer
	listVisible     bool
	lastQueryAt     time.Time
	interactionBusy int
	deferredWindow  bool
	windowBroken    bool
	window          windowBackend
	sound           soundBackend
	tray            trayBackend
	corrupt         bool
	started         bool
	stopped         bool
	stopCh          chan struct{}
	doneCh          chan struct{}
	wakeCh          chan struct{}

	now   func() time.Time
	newID func() string
}

func newPlugin(out io.Writer, dataDir string, now func() time.Time, newID func() string) *Plugin {
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = randomID
	}
	p := &Plugin{
		out:      out,
		dataDir:  dataDir,
		settings: map[string]any{},
		timers:   map[string]*timer{},
		now:      now,
		newID:    newID,
		wakeCh:   make(chan struct{}, 1),
		sound:    newSoundBackend(),
	}
	p.window = newWindowBackend(windowCallbacks{
		Ack:       p.acknowledgeFromWindow,
		AckAll:    p.acknowledgeAllFromWindow,
		Snooze:    p.snoozeFromWindow,
		SnoozeAll: p.snoozeAllFromWindow,
		Failed:    p.windowFailed,
		Wake:      p.wakeScheduler,
	})
	p.tray = newTrayBackend(trayCallbacks{
		Snapshot: p.traySnapshot,
		Action:   p.handleTrayAction,
		OpenList: p.openListFromTray,
	})
	return p
}

func (p *Plugin) floatingEnabledLocked() bool {
	value, ok := p.settings["floating_window"]
	if !ok {
		return true
	}
	enabled, ok := value.(bool)
	if !ok {
		return true
	}
	return enabled
}

func (p *Plugin) intSettingLocked(key string, fallback, minimum, maximum int) int {
	value, ok := p.settings[key]
	if !ok {
		return fallback
	}
	number := 0
	switch typed := value.(type) {
	case float64:
		number = int(typed)
	case int:
		number = typed
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return fallback
		}
		number = int(parsed)
	default:
		return fallback
	}
	if number < minimum {
		return minimum
	}
	if number > maximum {
		return maximum
	}
	return number
}

func (p *Plugin) boolSettingLocked(key string, fallback bool) bool {
	value, ok := p.settings[key]
	if !ok {
		return fallback
	}
	enabled, ok := value.(bool)
	if !ok {
		return fallback
	}
	return enabled
}

func (p *Plugin) stringSettingLocked(key, fallback string) string {
	value, ok := p.settings[key]
	if !ok {
		return fallback
	}
	text, ok := value.(string)
	if !ok {
		return fallback
	}
	return text
}

func (p *Plugin) animationsEnabledLocked() bool {
	return p.boolSettingLocked("animations", true)
}

func (p *Plugin) soundEnabledLocked() bool {
	return p.boolSettingLocked("sound_enabled", true)
}

func (p *Plugin) autoCloseSecondsLocked() int {
	return p.intSettingLocked("auto_close_seconds", 0, 0, 600)
}

func (p *Plugin) snoozeMinutesLocked() int {
	return p.intSettingLocked("snooze_minutes", 5, 1, 120)
}

func (p *Plugin) repeatFirstSecondsLocked() int {
	return p.intSettingLocked("repeat_first_seconds", 15, 5, 300)
}

func (p *Plugin) repeatEverySecondsLocked() int {
	return p.intSettingLocked("repeat_every_seconds", 60, 10, 3600)
}

func autoCloseDue(visible bool, seconds int, idle time.Duration) bool {
	return visible && seconds > 0 && idle >= time.Duration(seconds)*time.Second
}

func pageIndices(total, scroll, page int) []int {
	if total <= 0 || page <= 0 {
		return nil
	}
	if scroll < 0 {
		scroll = 0
	}
	if scroll > total-1 {
		scroll = total - 1
	}
	limit := scroll + page
	if limit > total {
		limit = total
	}
	out := make([]int, 0, limit-scroll)
	for index := scroll; index < limit; index++ {
		out = append(out, index)
	}
	return out
}

func (p *Plugin) trayEnabledLocked() bool {
	return p.boolSettingLocked("tray_icon", true)
}

func (p *Plugin) traySnapshotLocked(now time.Time) []trayTimerSnapshot {
	items := make([]trayTimerSnapshot, 0, len(p.timers))
	for _, item := range p.timers {
		snapshot := trayTimerSnapshot{
			ID:      item.ID,
			Title:   item.Title,
			Status:  normalizedTrayStatus(item.Status),
			EndsAt:  item.EndsAt,
			EndedAt: item.EndedAt,
		}
		switch snapshot.Status {
		case statusRunning:
			remaining := int64(math.Ceil(item.EndsAt.Sub(now).Seconds()))
			if remaining < 1 {
				remaining = 1
			}
			snapshot.RemainingSeconds = remaining
		case statusPaused:
			snapshot.RemainingSeconds = item.RemainingSeconds
		}
		items = append(items, snapshot)
	}
	return items
}

func (p *Plugin) traySnapshot() []trayTimerSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.traySnapshotLocked(p.now())
}

func (p *Plugin) refreshTray() {
	if p.tray == nil {
		return
	}
	p.tray.Update(p.traySnapshot())
}

func (p *Plugin) applyTraySettings() {
	if p.tray == nil {
		return
	}
	p.mu.Lock()
	enabled := p.trayEnabledLocked()
	items := p.traySnapshotLocked(p.now())
	p.mu.Unlock()
	p.tray.SetEnabled(enabled)
	p.tray.Update(items)
}

func (p *Plugin) handleTrayAction(timerID, action string) {
	p.beginInteraction()
	defer p.endInteraction()
	_ = p.doActionNamed(actionData{TimerID: timerID}, action)
}

func (p *Plugin) openListFromTray() {
	p.notifyToast("点击此通知打开倒计时列表。")
}

func (p *Plugin) popupCenterLocked() bool {
	return p.boolSettingLocked("popup_center", false)
}

func (p *Plugin) applyWindowSettings() {
	p.mu.Lock()
	seconds := p.autoCloseSecondsLocked()
	animations := p.animationsEnabledLocked()
	center := p.popupCenterLocked()
	p.mu.Unlock()
	p.window.SetAutoClose(seconds)
	p.window.SetAnimations(animations)
	p.window.SetPopupCenter(center)
}

func (p *Plugin) playReminderSoundLocked() {
	if !p.soundEnabledLocked() || p.sound == nil {
		return
	}
	path := strings.TrimSpace(p.stringSettingLocked("sound_file", ""))
	if path == "" {
		if err := p.sound.PlayDefault(); err != nil {
			p.logf("default reminder sound failed: %v", err)
		}
		return
	}
	if err := validateWAVFile(path); err != nil {
		p.logf("sound_file is not a valid WAV file (%v); using the default system sound", err)
		if defaultErr := p.sound.PlayDefault(); defaultErr != nil {
			p.logf("default reminder sound failed: %v", defaultErr)
		}
		return
	}
	if err := p.sound.PlayFile(path); err != nil {
		p.logf("custom reminder sound failed (%v); using the default system sound", err)
		if defaultErr := p.sound.PlayDefault(); defaultErr != nil {
			p.logf("default reminder sound failed: %v", defaultErr)
		}
	}
}

func (p *Plugin) warnInvalidSoundFile(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if err := validateWAVFile(path); err != nil {
		p.logf("selected sound_file is invalid: %v", err)
		p.notifyToast("自定义提示音不是有效的 WAV 文件；到期时将使用系统默认提示音。请重新选择 WAV 文件。")
	}
}

func (p *Plugin) stopReminderSoundLocked() {
	if p.sound != nil {
		p.sound.Stop()
	}
}

func runningCountLocked(timers map[string]*timer) int {
	running := 0
	for _, item := range timers {
		if item.Status == "" || item.Status == statusRunning {
			running++
		}
	}
	return running
}

func (p *Plugin) pendingItemsLocked() []pendingItem {
	items := make([]*timer, 0)
	for _, item := range p.timers {
		if item.Status == statusPending {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].EndedAt.Equal(items[j].EndedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].EndedAt.Before(items[j].EndedAt)
	})
	out := make([]pendingItem, 0, len(items))
	for _, item := range items {
		out = append(out, pendingItem{ID: item.ID, Title: item.Title})
	}
	return out
}

func (p *Plugin) beginInteraction() {
	p.mu.Lock()
	p.interactionBusy++
	p.mu.Unlock()
}

func (p *Plugin) endInteraction() {
	p.mu.Lock()
	if p.interactionBusy > 0 {
		p.interactionBusy--
	}
	p.mu.Unlock()
}

func (p *Plugin) noteQuery() {
	p.mu.Lock()
	p.lastQueryAt = p.now()
	p.mu.Unlock()
}

func (p *Plugin) listRecentlyVisibleLocked(now time.Time) bool {
	return !p.lastQueryAt.IsZero() && now.Sub(p.lastQueryAt) < 2500*time.Millisecond
}

func (p *Plugin) notifyItems(items []pendingItem) {
	titles := make([]string, 0, len(items))
	for _, item := range items {
		titles = append(titles, item.Title)
	}
	p.notifyPendingTitles(titles)
}

func (p *Plugin) windowFailed() {
	p.mu.Lock()
	p.windowBroken = true
	enabled := p.floatingEnabledLocked()
	items := p.pendingItemsLocked()
	p.mu.Unlock()
	if enabled && len(items) > 0 {
		p.notifyItems(items)
	}
}

func (p *Plugin) syncWindow(force bool) {
	p.mu.Lock()
	items := p.pendingItemsLocked()
	enabled := p.floatingEnabledLocked()
	busy := p.interactionBusy > 0
	listed := p.listRecentlyVisibleLocked(p.now())
	broken := p.windowBroken
	if !enabled || len(items) == 0 {
		p.deferredWindow = false
		p.mu.Unlock()
		p.window.Hide()
		return
	}
	if force {
		if busy || listed {
			p.deferredWindow = true
			p.mu.Unlock()
			p.wakeScheduler()
			p.window.Refresh(items)
			return
		}
		p.deferredWindow = false
		p.mu.Unlock()
		if broken || !p.window.Show(items) {
			p.mu.Lock()
			p.windowBroken = true
			p.mu.Unlock()
			p.notifyItems(items)
		}
		return
	}
	p.mu.Unlock()
	p.window.Refresh(items)
}

func (p *Plugin) flushDeferredWindow() {
	p.mu.Lock()
	deferred := p.deferredWindow
	enabled := p.floatingEnabledLocked()
	items := p.pendingItemsLocked()
	busy := p.interactionBusy > 0
	listed := p.listRecentlyVisibleLocked(p.now())
	broken := p.windowBroken
	if deferred && enabled && len(items) > 0 && !busy && !listed {
		p.deferredWindow = false
		p.mu.Unlock()
		if broken || !p.window.Show(items) {
			p.mu.Lock()
			p.windowBroken = true
			p.mu.Unlock()
			p.notifyItems(items)
		}
		return
	}
	p.mu.Unlock()
}

func (p *Plugin) acknowledgeFromWindow(id string) {
	p.confirmPending(id)
	p.syncWindow(false)
	p.refreshList()
}

func (p *Plugin) acknowledgeAllFromWindow() {
	p.mu.Lock()
	ids := make([]string, 0)
	for id, item := range p.timers {
		if item.Status == statusPending {
			ids = append(ids, id)
		}
	}
	p.mu.Unlock()
	for _, id := range ids {
		p.confirmPending(id)
	}
	p.syncWindow(false)
	p.refreshList()
}

func (p *Plugin) snoozeIDs(ids []string) ([]string, int, bool) {
	p.mu.Lock()
	minutes := p.snoozeMinutesLocked()
	now := p.now()
	before := make(map[string]timer, len(p.timers))
	for id, item := range p.timers {
		before[id] = *item
	}
	titles := make([]string, 0, len(ids))
	for _, id := range ids {
		item := p.timers[id]
		if item == nil || item.Status != statusPending {
			continue
		}
		item.Status = statusRunning
		item.EndsAt = now.Add(time.Duration(minutes) * time.Minute)
		item.EndedAt = time.Time{}
		item.NextReminderAt = time.Time{}
		item.ReminderCount = 0
		titles = append(titles, item.Title)
	}
	if len(titles) == 0 {
		p.mu.Unlock()
		return nil, minutes, false
	}
	if err := p.saveLocked(); err != nil {
		p.timers = make(map[string]*timer, len(before))
		for id := range before {
			item := before[id]
			p.timers[id] = &item
		}
		p.mu.Unlock()
		return nil, minutes, false
	}
	p.listVisible = false
	p.stopReminderSoundLocked()
	p.mu.Unlock()
	p.wakeScheduler()
	p.refreshTray()
	return titles, minutes, true
}

func (p *Plugin) snoozeTimer(id string) map[string]any {
	titles, minutes, ok := p.snoozeIDs([]string{strings.TrimSpace(id)})
	if !ok {
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "该计时器无法稍后提醒。"}}}
	}
	p.syncWindow(false)
	p.refreshList()
	text := "已稍后提醒：" + strings.Join(titles, "、") + fmt.Sprintf("，%d 分钟后再次提醒", minutes)
	return map[string]any{"effects": []map[string]any{
		{"type": "notify", "text": text},
		{"type": "hide"},
	}}
}

func (p *Plugin) snoozeFromWindow(id string) {
	if _, _, ok := p.snoozeIDs([]string{id}); ok {
		p.syncWindow(false)
		p.refreshList()
	}
}

func (p *Plugin) snoozeAllFromWindow() {
	p.mu.Lock()
	ids := make([]string, 0)
	for id, item := range p.timers {
		if item.Status == statusPending {
			ids = append(ids, id)
		}
	}
	p.mu.Unlock()
	if _, _, ok := p.snoozeIDs(ids); ok {
		p.syncWindow(false)
		p.refreshList()
	}
}

func (p *Plugin) confirmPending(id string) bool {
	p.mu.Lock()
	item := p.timers[id]
	if item == nil || item.Status != statusPending {
		p.mu.Unlock()
		return false
	}
	delete(p.timers, id)
	if err := p.saveLocked(); err != nil {
		p.timers[id] = item
		p.mu.Unlock()
		p.logf("window acknowledge save failed: %v", err)
		return false
	}
	p.listVisible = false
	p.stopReminderSoundLocked()
	p.mu.Unlock()
	p.wakeScheduler()
	p.refreshTray()
	return true
}

func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func (p *Plugin) emit(value any) {
	if p.out == nil {
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	p.outMu.Lock()
	defer p.outMu.Unlock()
	_, _ = p.out.Write(append(data, '\n'))
	if f, ok := p.out.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
}

func (p *Plugin) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "me.countdown: "+format+"\n", args...)
}

func (p *Plugin) notifyToast(text string) {
	p.emit(map[string]any{
		"jsonrpc": "2.0", "method": "winshun/notify",
		"params": map[string]any{
			"toast": true, "title": displayName, "text": text,
			"click": []map[string]any{{"type": "setQuery", "text": "倒计时 "}},
		},
	})
}

func (p *Plugin) notifyPendingTitles(titles []string) {
	if len(titles) == 0 {
		return
	}
	p.notifyToast(strings.Join(titles, "、") + "倒计时已结束。点我打开列表，按 Enter 确认。")
}

func (p *Plugin) refreshList() {
	p.emit(map[string]any{"jsonrpc": "2.0", "method": "winshun/refresh", "params": map[string]any{}})
}

func (p *Plugin) start() {
	p.mu.Lock()
	if p.started || p.stopped {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.mu.Unlock()
	go p.run()
}

func (p *Plugin) stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	started := p.started
	var done chan struct{}
	if started {
		close(p.stopCh)
		done = p.doneCh
	}
	p.mu.Unlock()
	if started {
		<-done
	}
	if p.sound != nil {
		p.sound.Stop()
	}
	if p.tray != nil {
		p.tray.Close()
	}
}

func (p *Plugin) run() {
	defer close(p.doneCh)
	p.load()
	p.refreshTray()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		p.mu.Lock()
		delay, hasDeadline := p.nextWakeDelayLocked(p.now())
		p.mu.Unlock()
		if !hasDeadline {
			select {
			case <-p.stopCh:
				return
			case <-p.wakeCh:
				continue
			}
		}
		timer.Reset(delay)
		select {
		case <-p.stopCh:
			return
		case <-p.wakeCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			continue
		case <-timer.C:
			p.handleSchedulerWake(p.now())
		}
	}
}

func (p *Plugin) wakeScheduler() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

func (p *Plugin) deferredWakeDelayLocked(now time.Time) time.Duration {
	delay := 2500 * time.Millisecond
	if !p.lastQueryAt.IsZero() {
		remaining := p.lastQueryAt.Add(2500 * time.Millisecond).Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		if remaining < delay {
			delay = remaining
		}
	}
	return delay
}

func (p *Plugin) nextWakeDelayLocked(now time.Time) (time.Duration, bool) {
	var next time.Time
	consider := func(candidate time.Time) {
		if candidate.IsZero() {
			return
		}
		if next.IsZero() || candidate.Before(next) {
			next = candidate
		}
	}
	for _, item := range p.timers {
		switch item.Status {
		case statusPending:
			consider(item.NextReminderAt)
		case "", statusRunning:
			consider(item.EndsAt)
			if item.OriginalSeconds > 60 && !item.PreReminded && !item.EndsAt.IsZero() {
				consider(item.EndsAt.Add(-preReminderLead))
			}
		case statusPaused:
			// Paused timers have no deadline and must not wake the scheduler.
			continue
		default:
			continue
		}
	}
	if next.IsZero() {
		if p.deferredWindow {
			return p.deferredWakeDelayLocked(now), true
		}
		return 0, false
	}
	delay := next.Sub(now)
	if delay < 0 {
		delay = 0
	}
	// A deferred window must be reconsidered promptly even when a pending
	// timer's next repeat is still far away. This bounds only deferred work;
	// non-deferred schedules keep their normal deadline and 30s cap.
	if p.deferredWindow {
		if deferredDelay := p.deferredWakeDelayLocked(now); delay > deferredDelay {
			delay = deferredDelay
		}
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay, true
}

func (p *Plugin) handleSchedulerWake(now time.Time) {
	if !p.processDue(now) {
		// Even when nothing is due, refresh remaining-minute icon text and the
		// tooltip. The scheduler's existing 30-second cap bounds this staleness.
		p.refreshTray()
	}
	p.flushDeferredWindow()
}

func (p *Plugin) timerListLocked() []*timer {
	list := make([]*timer, 0, len(p.timers))
	for _, item := range p.timers {
		list = append(list, item)
	}
	priority := func(status string) int {
		switch status {
		case statusPending:
			return 0
		case "", statusRunning:
			return 1
		case statusPaused:
			return 2
		default:
			return 3
		}
	}
	sort.Slice(list, func(i, j int) bool {
		left, right := priority(list[i].Status), priority(list[j].Status)
		if left != right {
			return left < right
		}
		if list[i].Status == statusPaused {
			if list[i].RemainingSeconds == list[j].RemainingSeconds {
				return list[i].ID < list[j].ID
			}
			return list[i].RemainingSeconds < list[j].RemainingSeconds
		}
		leftTime, rightTime := list[i].EndsAt, list[j].EndsAt
		if list[i].Status == statusPending {
			leftTime, rightTime = list[i].EndedAt, list[j].EndedAt
		}
		if leftTime.Equal(rightTime) {
			return list[i].ID < list[j].ID
		}
		return leftTime.Before(rightTime)
	})
	return list
}

func (p *Plugin) saveLocked() error {
	if p.dataDir == "" {
		return errors.New("数据目录为空")
	}
	if p.corrupt {
		return errors.New("countdown.json 已损坏，未覆盖原文件")
	}
	if err := os.MkdirAll(p.dataDir, 0700); err != nil {
		return err
	}
	doc := storedFile{Version: dataVersion}
	for _, item := range p.timerListLocked() {
		status := item.Status
		if status == "" {
			status = statusRunning
		}
		stored := storedTimer{
			ID: item.ID, Title: item.Title,
			CreatedAt: item.CreatedAt.Format(time.RFC3339Nano),
			Status:    status, OriginalSeconds: item.OriginalSeconds,
			PreReminded: item.PreReminded,
		}
		if status != statusPaused {
			stored.EndsAt = item.EndsAt.Format(time.RFC3339Nano)
		}
		if status == statusPaused {
			stored.RemainingSeconds = item.RemainingSeconds
		}
		if status == statusPending {
			stored.EndedAt = item.EndedAt.Format(time.RFC3339Nano)
			stored.NextReminderAt = item.NextReminderAt.Format(time.RFC3339Nano)
			stored.ReminderCount = item.ReminderCount
		}
		doc.Timers = append(doc.Timers, stored)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(p.dataDir, "countdown-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(p.dataDir, dataFileName))
}

func (p *Plugin) load() {
	p.mu.Lock()
	dir := p.dataDir
	p.mu.Unlock()
	if dir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		p.failCorrupt(fmt.Sprintf("读取 countdown.json 失败：%v", err))
		return
	}
	var doc storedFile
	if err := json.Unmarshal(data, &doc); err != nil {
		p.failCorrupt("countdown.json 格式损坏，原文件未覆盖")
		return
	}
	if doc.Version > dataVersion || doc.Version < 0 {
		p.failCorrupt(fmt.Sprintf("countdown.json 版本不支持：%d", doc.Version))
		return
	}
	now := p.now()
	restored := map[string]*timer{}
	newlyPending := make([]string, 0)
	changed := doc.Version < dataVersion
	for _, raw := range doc.Timers {
		created, err := time.Parse(time.RFC3339Nano, raw.CreatedAt)
		if err != nil || strings.TrimSpace(raw.ID) == "" || strings.TrimSpace(raw.Title) == "" {
			p.failCorrupt("countdown.json 含无效计时记录，原文件未覆盖")
			return
		}
		if restored[raw.ID] != nil {
			p.failCorrupt("countdown.json 含重复计时 ID，原文件未覆盖")
			return
		}
		status := strings.TrimSpace(raw.Status)
		if status == "" {
			status = statusRunning
			changed = true
		}
		if status == acknowledged {
			changed = true
			continue
		}
		var ends time.Time
		if status != statusPaused {
			if raw.EndsAt == "" {
				p.failCorrupt("countdown.json 的 endsAt 缺失，原文件未覆盖")
				return
			}
			parsed, parseErr := time.Parse(time.RFC3339Nano, raw.EndsAt)
			if parseErr != nil {
				p.failCorrupt("countdown.json 的 endsAt 无效，原文件未覆盖")
				return
			}
			ends = parsed
		}
		originalSeconds := raw.OriginalSeconds
		if originalSeconds <= 0 {
			if !ends.IsZero() {
				originalSeconds = int64(ends.Sub(created) / time.Second)
			} else if status == statusPaused && raw.RemainingSeconds > 0 {
				originalSeconds = raw.RemainingSeconds
			}
			if originalSeconds < 1 {
				originalSeconds = 1
			}
			changed = true
		}
		if originalSeconds > maxDuration {
			p.failCorrupt("countdown.json 的 originalSeconds 超出范围，原文件未覆盖")
			return
		}
		item := &timer{
			ID: raw.ID, Title: raw.Title, CreatedAt: created,
			EndsAt: ends, Status: status, OriginalSeconds: originalSeconds,
			PreReminded: raw.PreReminded,
		}
		switch status {
		case statusRunning:
			if ends.After(now) {
				restored[item.ID] = item
			} else {
				item.Status = statusPending
				item.EndedAt = ends
				item.NextReminderAt = now.Add(time.Duration(p.repeatFirstSecondsLocked()) * time.Second)
				item.ReminderCount = 0
				restored[item.ID] = item
				newlyPending = append(newlyPending, item.Title)
				changed = true
			}
		case statusPending:
			item.EndedAt = ends
			if raw.EndedAt != "" {
				parsed, parseErr := time.Parse(time.RFC3339Nano, raw.EndedAt)
				if parseErr != nil {
					p.failCorrupt("countdown.json 的 endedAt 无效，原文件未覆盖")
					return
				}
				item.EndedAt = parsed
			}
			if raw.NextReminderAt != "" {
				parsed, parseErr := time.Parse(time.RFC3339Nano, raw.NextReminderAt)
				if parseErr != nil {
					p.failCorrupt("countdown.json 的 nextReminderAt 无效，原文件未覆盖")
					return
				}
				item.NextReminderAt = parsed
			} else {
				item.NextReminderAt = now.Add(time.Duration(p.repeatFirstSecondsLocked()) * time.Second)
				changed = true
			}
			item.ReminderCount = raw.ReminderCount
			restored[item.ID] = item
		case statusPaused:
			if raw.RemainingSeconds < 1 || raw.RemainingSeconds > maxDuration {
				p.failCorrupt("countdown.json 的 remainingSeconds 无效，原文件未覆盖")
				return
			}
			item.EndsAt = time.Time{}
			item.RemainingSeconds = raw.RemainingSeconds
			restored[item.ID] = item
		case acknowledged:
		default:
			p.failCorrupt("countdown.json 含未知状态，原文件未覆盖")
			return
		}
	}
	p.mu.Lock()
	p.timers = restored
	if changed {
		if saveErr := p.saveLocked(); saveErr != nil {
			p.mu.Unlock()
			p.logf("saving migrated countdown.json failed: %v", saveErr)
			return
		}
	}
	floating := p.floatingEnabledLocked()
	hasPending := len(p.pendingItemsLocked()) > 0
	if !floating && len(newlyPending) > 0 {
		p.notifyPendingTitles(newlyPending)
	}
	p.mu.Unlock()
	if floating && hasPending {
		p.syncWindow(true)
	} else {
		p.syncWindow(false)
	}
}

func (p *Plugin) failCorrupt(message string) {
	p.mu.Lock()
	p.corrupt = true
	p.mu.Unlock()
	p.logf("%s", message)
	p.notifyToast(message)
}

func (p *Plugin) processDue(now time.Time) bool {
	p.mu.Lock()
	hasDue := false
	for _, item := range p.timers {
		if item.Status == statusRunning {
			if !item.EndsAt.After(now) || (item.OriginalSeconds > 60 && !item.PreReminded && !item.EndsAt.IsZero() && !now.Before(item.EndsAt.Add(-preReminderLead))) {
				hasDue = true
				break
			}
		} else if item.Status == statusPending && !item.NextReminderAt.After(now) {
			hasDue = true
			break
		}
	}
	if !hasDue {
		p.mu.Unlock()
		return false
	}
	before := make(map[string]timer, len(p.timers))
	for id, item := range p.timers {
		before[id] = *item
	}
	var reminderTitles []string
	var preReminderTitles []string
	for _, item := range p.timers {
		switch item.Status {
		case statusRunning:
			if !item.EndsAt.After(now) {
				item.Status = statusPending
				item.EndedAt = item.EndsAt
				item.NextReminderAt = now.Add(time.Duration(p.repeatFirstSecondsLocked()) * time.Second)
				item.ReminderCount = 0
				reminderTitles = append(reminderTitles, item.Title)
			} else if item.OriginalSeconds > 60 && !item.PreReminded && !item.EndsAt.IsZero() && !now.Before(item.EndsAt.Add(-preReminderLead)) {
				item.PreReminded = true
				preReminderTitles = append(preReminderTitles, item.Title)
			}
		case statusPending:
			if !item.NextReminderAt.After(now) {
				item.ReminderCount++
				item.NextReminderAt = now.Add(time.Duration(p.repeatEverySecondsLocked()) * time.Second)
				reminderTitles = append(reminderTitles, item.Title)
			}
		case statusPaused:
			// Paused timers are intentionally excluded from expiry processing.
		}
	}
	if len(reminderTitles) == 0 && len(preReminderTitles) == 0 {
		p.mu.Unlock()
		return false
	}
	if err := p.saveLocked(); err != nil {
		p.timers = make(map[string]*timer, len(before))
		for id := range before {
			item := before[id]
			p.timers[id] = &item
		}
		p.mu.Unlock()
		p.logf("expiry/pre-reminder save failed: %v", err)
		return false
	}
	// Only expiry/repeat reminders play sound. The one-minute pre-reminder is
	// toast-only and must never show the acknowledgement window.
	if len(reminderTitles) > 0 {
		// Keep the lock through persistence and asynchronous sound start so an
		// acknowledge/snooze cannot race ahead and leave audio playing afterward.
		p.playReminderSoundLocked()
	}
	floating := len(reminderTitles) > 0 && p.floatingEnabledLocked()
	refreshList := len(reminderTitles) > 0 && p.listVisible
	if refreshList {
		p.refreshList()
	}
	p.mu.Unlock()
	p.refreshTray()
	for _, title := range preReminderTitles {
		p.notifyToast("还有 1 分钟到期：" + title)
	}
	if len(reminderTitles) > 0 {
		if floating {
			p.syncWindow(true)
		} else {
			p.notifyPendingTitles(reminderTitles)
		}
	}
	return true
}

func (p *Plugin) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.corrupt {
		return
	}
	if err := p.saveLocked(); err != nil {
		p.logf("shutdown save failed: %v", err)
	}
}

type parsedInput struct {
	Title   string
	Seconds int64
}

var durationPart = regexp.MustCompile(`(\d+)(小时|时|h|分钟|分|m|秒|s)`)

func parseInput(text string) (parsedInput, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return parsedInput{}, errors.New("empty")
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return parsedInput{}, errors.New("empty")
	}
	seconds, err := parseDurationToken(fields[len(fields)-1])
	if err != nil {
		return parsedInput{}, err
	}
	title := strings.TrimSpace(strings.Join(fields[:len(fields)-1], " "))
	if title == "" {
		title = displayName
	}
	return parsedInput{Title: title, Seconds: seconds}, nil
}

func parseDurationToken(token string) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, ".。") {
		return 0, errors.New("invalid duration")
	}
	if strings.Contains(token, ":") {
		parts := strings.Split(token, ":")
		if len(parts) != 2 {
			return 0, errors.New("invalid duration")
		}
		minutes, err1 := strconv.ParseInt(parts[0], 10, 64)
		seconds, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil || minutes < 0 || seconds < 0 || seconds > 59 {
			return 0, errors.New("invalid duration")
		}
		if minutes > (maxDuration-seconds)/60 {
			return 0, errors.New("duration too large")
		}
		total := minutes*60 + seconds
		if total <= 0 {
			return 0, errors.New("invalid duration")
		}
		return total, nil
	}
	if allDigits(token) {
		minutes, err := strconv.ParseInt(token, 10, 64)
		if err != nil || minutes > maxDuration/60 {
			return 0, errors.New("duration too large")
		}
		return checkedAdd(0, minutes*60, 0)
	}
	indexes := durationPart.FindAllStringSubmatchIndex(token, -1)
	if len(indexes) == 0 {
		return 0, errors.New("invalid duration")
	}
	var total int64
	position := 0
	for _, match := range indexes {
		if match[0] != position {
			return 0, errors.New("invalid duration")
		}
		number, err := strconv.ParseInt(token[match[2]:match[3]], 10, 64)
		if err != nil {
			return 0, errors.New("invalid duration")
		}
		unit := token[match[4]:match[5]]
		multiplier := int64(1)
		switch unit {
		case "h", "时", "小时":
			multiplier = 3600
		case "m", "分", "分钟":
			multiplier = 60
		}
		if number > (maxDuration-total)/multiplier {
			return 0, errors.New("duration too large")
		}
		total += number * multiplier
		position = match[1]
	}
	if position != len(token) {
		return 0, errors.New("invalid duration")
	}
	if total <= 0 || total > maxDuration {
		return 0, errors.New("invalid duration")
	}
	return total, nil
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func checkedAdd(parts ...int64) (int64, error) {
	var total int64
	for _, value := range parts {
		if value < 0 || total > maxDuration-value {
			return 0, errors.New("duration too large")
		}
		total += value
	}
	if total <= 0 || total > maxDuration {
		return 0, errors.New("invalid duration")
	}
	return total, nil
}

func humanDuration(seconds int64) string {
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	parts := make([]string, 0, 3)
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d 小时", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d 分钟", minutes))
	}
	if secs > 0 {
		parts = append(parts, fmt.Sprintf("%d 秒", secs))
	}
	if len(parts) == 0 {
		return "0 秒"
	}
	return strings.Join(parts, " ")
}

func formatSeconds(seconds int64) string {
	if seconds < 1 {
		seconds = 1
	}
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, secs)
	}
	return fmt.Sprintf("%02d:%02d", minutes, secs)
}

func formatRemaining(endsAt, now time.Time) string {
	seconds := int64(math.Ceil(endsAt.Sub(now).Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, secs)
	}
	return fmt.Sprintf("%02d:%02d", minutes, secs)
}

func previewID(title string, seconds int64) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s\x00%d", title, seconds)))
	return "start-" + hex.EncodeToString(sum[:6])
}

func centerPositionInArea(left, top, right, bottom, width, height int) (int, int) {
	areaWidth := right - left
	areaHeight := bottom - top
	x := left
	y := top
	if areaWidth > width {
		x = left + (areaWidth-width)/2
	}
	if areaHeight > height {
		y = top + (areaHeight-height)/2
	}
	return x, y
}

func validItem(id, title, subtitle string) map[string]any {
	return map[string]any{"id": id, "title": title, "subtitle": subtitle, "valid": false}
}

func actionItem(id, title, subtitle string, data map[string]any, action string) map[string]any {
	actionTitle := actionTitle(action)
	item := map[string]any{
		"id": id, "title": title, "subtitle": subtitle, "data": data,
		"valid":   true,
		"actions": []map[string]any{{"id": action, "title": actionTitle, "key": "Enter"}},
	}
	return item
}

func actionTitle(action string) string {
	switch action {
	case "start":
		return "开始"
	case "acknowledge":
		return "确认"
	case "snooze":
		return "稍后提醒"
	case "pause":
		return "暂停"
	case "resume":
		return "继续"
	case "restart":
		return "重新开始"
	default:
		return "取消"
	}
}

func setItemActions(item map[string]any, actions ...string) {
	result := make([]map[string]any, 0, len(actions))
	for index, action := range actions {
		entry := map[string]any{"id": action, "title": actionTitle(action)}
		if index == 0 {
			entry["key"] = "Enter"
		}
		result = append(result, entry)
	}
	item["actions"] = result
}

func runningItemView(item *timer, now time.Time) map[string]any {
	result := actionItem(
		item.ID, item.Title,
		"剩余 "+formatRemaining(item.EndsAt, now)+" · "+item.EndsAt.Format("15:04")+" 结束 · Enter 取消",
		map[string]any{"type": "cancel", "timerId": item.ID}, "cancel",
	)
	setItemActions(result, "cancel", "pause", "restart")
	return result
}

func pausedItemView(item *timer) map[string]any {
	result := actionItem(
		item.ID, item.Title,
		"已暂停，剩余 "+formatSeconds(item.RemainingSeconds)+" · Enter 继续",
		map[string]any{"type": "resume", "timerId": item.ID}, "resume",
	)
	setItemActions(result, "resume", "cancel")
	return result
}

func pendingItemView(item *timer) map[string]any {
	result := actionItem(
		item.ID, item.Title,
		"已经结束，等待确认。 · Enter 确认已知晓",
		map[string]any{"type": "acknowledge", "timerId": item.ID}, "acknowledge",
	)
	setItemActions(result, "acknowledge", "snooze", "restart")
	return result
}

type durationPreset struct {
	Title   string
	Seconds int64
}

var countdownPresets = [...]durationPreset{
	{Title: "5 分钟", Seconds: 5 * 60},
	{Title: "10 分钟", Seconds: 10 * 60},
	{Title: "15 分钟", Seconds: 15 * 60},
	{Title: "25 分钟", Seconds: 25 * 60},
	{Title: "30 分钟", Seconds: 30 * 60},
	{Title: "45 分钟", Seconds: 45 * 60},
	{Title: "60 分钟", Seconds: 60 * 60},
	{Title: "番茄专注", Seconds: 25 * 60},
	{Title: "休息", Seconds: 5 * 60},
}

func presetTimerItems() []map[string]any {
	items := make([]map[string]any, 0, len(countdownPresets))
	for _, preset := range countdownPresets {
		items = append(items, actionItem(
			previewID(preset.Title, preset.Seconds),
			preset.Title,
			humanDuration(preset.Seconds)+" · Enter 开始",
			map[string]any{"type": "start", "title": preset.Title, "seconds": preset.Seconds},
			"start",
		))
	}
	return items
}

func (p *Plugin) query(text string) ([]map[string]any, int) {
	text = strings.TrimSpace(text)
	now := p.now()
	if text != "" {
		p.setListVisible(false)
		parsed, err := parseInput(text)
		if err != nil {
			return []map[string]any{validItem("parse-error", parseErrorText, "例如：泡面 3")}, 0
		}
		p.mu.Lock()
		corrupt := p.corrupt
		running := runningCountLocked(p.timers)
		p.mu.Unlock()
		if corrupt {
			return []map[string]any{validItem("corrupt", "倒计时数据文件损坏，请检查 countdown.json。", "")}, 0
		}
		if running >= maxTimers {
			return []map[string]any{validItem("limit", "最多同时运行 20 个倒计时。", "")}, 0
		}
		return []map[string]any{actionItem(
			previewID(parsed.Title, parsed.Seconds),
			"开始倒计时："+parsed.Title,
			humanDuration(parsed.Seconds)+" · Enter 开始",
			map[string]any{"type": "start", "title": parsed.Title, "seconds": parsed.Seconds},
			"start",
		)}, 0
	}

	p.mu.Lock()
	corrupt := p.corrupt
	ordered := p.timerListLocked()
	all := make([]timer, len(ordered))
	for index, item := range ordered {
		all[index] = *item
	}
	p.mu.Unlock()
	if corrupt {
		p.setListVisible(false)
		return []map[string]any{validItem("corrupt", "倒计时数据文件损坏，请检查 countdown.json。", "")}, 0
	}
	pending := make([]*timer, 0)
	running := make([]*timer, 0)
	paused := make([]*timer, 0)
	for index := range all {
		item := &all[index]
		switch item.Status {
		case statusPending:
			pending = append(pending, item)
		case "", statusRunning:
			if item.EndsAt.After(now) {
				running = append(running, item)
			}
		case statusPaused:
			paused = append(paused, item)
		}
	}
	if len(pending) == 0 && len(running) == 0 && len(paused) == 0 {
		p.setListVisible(false)
		return presetTimerItems(), 0
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].EndedAt.Equal(pending[j].EndedAt) {
			return pending[i].ID < pending[j].ID
		}
		return pending[i].EndedAt.Before(pending[j].EndedAt)
	})
	sort.Slice(running, func(i, j int) bool {
		if running[i].EndsAt.Equal(running[j].EndsAt) {
			return running[i].ID < running[j].ID
		}
		return running[i].EndsAt.Before(running[j].EndsAt)
	})
	sort.Slice(paused, func(i, j int) bool {
		if paused[i].RemainingSeconds == paused[j].RemainingSeconds {
			return paused[i].ID < paused[j].ID
		}
		return paused[i].RemainingSeconds < paused[j].RemainingSeconds
	})
	items := make([]map[string]any, 0, len(pending)+len(running)+len(paused)+len(countdownPresets))
	for _, item := range pending {
		items = append(items, pendingItemView(item))
	}
	for _, item := range running {
		items = append(items, runningItemView(item, now))
	}
	for _, item := range paused {
		items = append(items, pausedItemView(item))
	}
	items = append(items, presetTimerItems()...)
	p.setListVisible(true)
	return items, 1
}

func (p *Plugin) setListVisible(visible bool) {
	p.mu.Lock()
	p.listVisible = visible
	p.mu.Unlock()
}

type actionData struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Seconds int64  `json:"seconds"`
	TimerID string `json:"timerId"`
}

func (p *Plugin) doAction(data actionData) map[string]any {
	return p.doActionNamed(data, "")
}

func (p *Plugin) doActionNamed(data actionData, action string) map[string]any {
	if action == "" {
		action = data.Type
	}
	switch action {
	case "start":
		return p.startTimer(data.Title, data.Seconds)
	case "cancel":
		return p.cancelTimer(data.TimerID)
	case "acknowledge":
		return p.acknowledgeTimer(data.TimerID)
	case "snooze":
		return p.snoozeTimer(data.TimerID)
	case "pause":
		return p.pauseTimer(data.TimerID)
	case "resume":
		return p.resumeTimer(data.TimerID)
	case "restart":
		return p.restartTimer(data.TimerID)
	default:
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "未知操作。"}}}
	}
}

func (p *Plugin) startTimer(title string, seconds int64) map[string]any {
	title = strings.TrimSpace(title)
	if title == "" {
		title = displayName
	}
	if seconds < 1 || seconds > maxDuration {
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "时长无效。"}}}
	}
	p.mu.Lock()
	if p.corrupt {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "倒计时数据文件损坏，未创建。"}}}
	}
	running := runningCountLocked(p.timers)
	if running >= maxTimers {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "最多同时运行 20 个倒计时。"}}}
	}
	now := p.now()
	id := p.newID()
	for p.timers[id] != nil {
		id = p.newID()
	}
	item := &timer{
		ID: id, Title: title, CreatedAt: now,
		EndsAt: now.Add(time.Duration(seconds) * time.Second), Status: statusRunning,
		OriginalSeconds: seconds,
	}
	p.timers[item.ID] = item
	if err := p.saveLocked(); err != nil {
		delete(p.timers, item.ID)
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，倒计时未创建。"}}}
	}
	p.listVisible = false
	p.mu.Unlock()
	p.wakeScheduler()
	p.refreshTray()
	p.notifyToast("倒计时已开始：" + title + " " + humanDuration(seconds))
	return map[string]any{"effects": []map[string]any{{"type": "hide"}}}
}

func (p *Plugin) pauseTimer(id string) map[string]any {
	id = strings.TrimSpace(id)
	p.mu.Lock()
	item := p.timers[id]
	if item == nil || (item.Status != "" && item.Status != statusRunning) {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "该计时器当前不能暂停。"}}}
	}
	now := p.now()
	if item.EndsAt.Before(now) {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "倒计时已经到期，不能暂停。"}}}
	}
	before := *item
	title := item.Title
	remaining := int64(math.Ceil(item.EndsAt.Sub(now).Seconds()))
	if remaining < 1 {
		remaining = 1
	}
	item.Status = statusPaused
	item.EndsAt = time.Time{}
	item.RemainingSeconds = remaining
	if item.OriginalSeconds < 1 {
		item.OriginalSeconds = remaining
	}
	if err := p.saveLocked(); err != nil {
		*item = before
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，未暂停。"}}}
	}
	p.listVisible = false
	p.mu.Unlock()
	p.wakeScheduler()
	p.refreshTray()
	p.notifyToast("已暂停：" + title)
	return map[string]any{"effects": []map[string]any{{"type": "hide"}}}
}

func (p *Plugin) resumeTimer(id string) map[string]any {
	id = strings.TrimSpace(id)
	p.mu.Lock()
	item := p.timers[id]
	if item == nil || item.Status != statusPaused || item.RemainingSeconds < 1 {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "该计时器当前不能继续。"}}}
	}
	if runningCountLocked(p.timers) >= maxTimers {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "最多同时运行 20 个倒计时。"}}}
	}
	before := *item
	title := item.Title
	item.EndsAt = p.now().Add(time.Duration(item.RemainingSeconds) * time.Second)
	item.RemainingSeconds = 0
	item.Status = statusRunning
	if item.OriginalSeconds < 1 {
		item.OriginalSeconds = int64(math.Ceil(item.EndsAt.Sub(item.CreatedAt).Seconds()))
		if item.OriginalSeconds < 1 {
			item.OriginalSeconds = 1
		}
	}
	if err := p.saveLocked(); err != nil {
		*item = before
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，未继续倒计时。"}}}
	}
	p.listVisible = false
	p.mu.Unlock()
	p.wakeScheduler()
	p.refreshTray()
	p.notifyToast("已继续：" + title)
	return map[string]any{"effects": []map[string]any{{"type": "hide"}}}
}

func (p *Plugin) restartTimer(id string) map[string]any {
	id = strings.TrimSpace(id)
	p.mu.Lock()
	item := p.timers[id]
	if item == nil || (item.Status != statusRunning && item.Status != statusPending) {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "该计时器当前不能重新开始。"}}}
	}
	if item.Status == statusPending && runningCountLocked(p.timers) >= maxTimers {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "最多同时运行 20 个倒计时。"}}}
	}
	seconds := item.OriginalSeconds
	if seconds < 1 || seconds > maxDuration {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "原始时长无效，无法重新开始。"}}}
	}
	before := *item
	title := item.Title
	now := p.now()
	item.EndsAt = now.Add(time.Duration(seconds) * time.Second)
	item.Status = statusRunning
	item.EndedAt = time.Time{}
	item.NextReminderAt = time.Time{}
	item.ReminderCount = 0
	item.RemainingSeconds = 0
	item.PreReminded = false
	if err := p.saveLocked(); err != nil {
		*item = before
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，未重新开始。"}}}
	}
	if before.Status == statusPending {
		p.stopReminderSoundLocked()
	}
	p.listVisible = false
	p.mu.Unlock()
	p.wakeScheduler()
	p.refreshTray()
	p.syncWindow(false)
	p.notifyToast("已重新开始：" + title)
	return map[string]any{"effects": []map[string]any{{"type": "hide"}}}
}

func (p *Plugin) cancelTimer(id string) map[string]any {
	id = strings.TrimSpace(id)
	p.mu.Lock()
	item := p.timers[id]
	if item == nil {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "计时器已不存在。"}}}
	}
	if item.Status == statusPending {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "已经结束，请按 Enter 确认。"}}}
	}
	title := item.Title
	delete(p.timers, id)
	if err := p.saveLocked(); err != nil {
		p.timers[id] = item
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，未取消。"}}}
	}
	visible := p.listVisible
	p.listVisible = false
	p.mu.Unlock()
	p.wakeScheduler()
	p.refreshTray()
	p.notifyToast("已取消：" + title)
	if visible {
		p.refreshList()
	}
	return map[string]any{"effects": []map[string]any{{"type": "hide"}}}
}

func (p *Plugin) acknowledgeTimer(id string) map[string]any {
	id = strings.TrimSpace(id)
	p.mu.Lock()
	item := p.timers[id]
	if item == nil || item.Status != statusPending {
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "该计时器已确认或不存在。"}}}
	}
	title := item.Title
	delete(p.timers, id)
	if err := p.saveLocked(); err != nil {
		p.timers[id] = item
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，未确认。"}}}
	}
	p.listVisible = false
	p.stopReminderSoundLocked()
	p.mu.Unlock()
	p.wakeScheduler()
	p.refreshTray()
	p.syncWindow(false)
	p.refreshList()
	return map[string]any{"effects": []map[string]any{
		{"type": "notify", "text": "已确认：" + title},
		{"type": "hide"},
	}}
}

// ---- JSON-RPC ----------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (p *Plugin) reply(id json.RawMessage, result any, errObj *rpcError) {
	if len(id) == 0 {
		return
	}
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if errObj != nil {
		response["error"] = errObj
	} else {
		response["result"] = result
	}
	p.emit(response)
}

func (p *Plugin) handleLine(line []byte) bool {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		p.logf("invalid JSON-RPC: %v", err)
		return false
	}
	switch req.Method {
	case "initialize":
		var params struct {
			DataDir  string         `json:"dataDir"`
			Settings map[string]any `json:"settings"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				p.reply(req.ID, nil, &rpcError{Code: -32602, Message: "initialize params invalid"})
				return false
			}
		}
		p.mu.Lock()
		p.dataDir = params.DataDir
		if params.Settings != nil {
			p.settings = params.Settings
		}
		initialSoundFile := p.stringSettingLocked("sound_file", "")
		p.mu.Unlock()
		p.applyWindowSettings()
		p.applyTraySettings()
		p.reply(req.ID, map[string]any{"protocolVersion": "1", "name": appName}, nil)
		p.warnInvalidSoundFile(initialSoundFile)
		go p.start()
		return false
	case "query":
		p.beginInteraction()
		defer p.endInteraction()
		p.noteQuery()
		var params struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(req.Params, &params)
		items, rerun := p.query(params.Text)
		p.reply(req.ID, map[string]any{"items": items, "rerun": rerun, "cache": 0}, nil)
		return false
	case "action":
		p.beginInteraction()
		defer p.endInteraction()
		var params struct {
			Action string `json:"action"`
			Item   struct {
				Data json.RawMessage `json:"data"`
			} `json:"item"`
		}
		_ = json.Unmarshal(req.Params, &params)
		var data actionData
		if len(params.Item.Data) > 0 {
			_ = json.Unmarshal(params.Item.Data, &data)
		}
		p.reply(req.ID, p.doActionNamed(data, params.Action), nil)
		return false
	case "cancel":
		// Query cancellation is not a launcher-close signal; rerun heartbeat
		// expiry is used to infer that instead.
		return false
	case "settingsChanged":
		var params struct {
			Settings map[string]any `json:"settings"`
		}
		_ = json.Unmarshal(req.Params, &params)
		p.mu.Lock()
		oldSoundFile := p.stringSettingLocked("sound_file", "")
		if params.Settings != nil {
			p.settings = params.Settings
		}
		newSoundFile := p.stringSettingLocked("sound_file", "")
		if p.floatingEnabledLocked() {
			p.windowBroken = false
		}
		soundEnabled := p.soundEnabledLocked()
		p.mu.Unlock()
		if newSoundFile != "" && newSoundFile != oldSoundFile {
			p.warnInvalidSoundFile(newSoundFile)
		}
		if !soundEnabled && p.sound != nil {
			p.sound.Stop()
		}
		p.applyWindowSettings()
		p.applyTraySettings()
		p.syncWindow(false)
		p.wakeScheduler()
		return false
	case "shutdown":
		p.flush()
		p.stop()
		p.reply(req.ID, map[string]any{}, nil)
		return true
	default:
		p.reply(req.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
		return false
	}
}

func (p *Plugin) serve(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if p.handleLine([]byte(line)) {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		p.logf("stdin: %v", err)
	}
}

func main() {
	initDpiAwareness()
	plugin := newPlugin(bufio.NewWriter(os.Stdout), "", nil, nil)
	plugin.serve(os.Stdin)
}
