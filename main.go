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
	appName        = "me.countdown"
	displayName    = "倒计时"
	dataFileName   = "countdown.json"
	maxTimers      = 20
	maxDuration    = int64(365 * 24 * 60 * 60)
	tickInterval   = 1 * time.Second
	repeatFirst    = 15 * time.Second
	repeatEvery    = 60 * time.Second
	dataVersion    = 2
	statusRunning  = "running"
	statusPending  = "pending_ack"
	acknowledged   = "acknowledged"
	parseErrorText = "时长看不懂，试试 25、25m、90s、1h30m、25:00。"
	emptyTitle     = "目前没有正在运行的倒计时。"
	emptyHint      = "输入 泡面 3 即可开始。"
)

type timer struct {
	ID             string
	Title          string
	CreatedAt      time.Time
	EndsAt         time.Time
	Status         string
	EndedAt        time.Time
	NextReminderAt time.Time
	ReminderCount  int
}

type storedTimer struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	CreatedAt      string `json:"createdAt"`
	EndsAt         string `json:"endsAt"`
	Status         string `json:"status,omitempty"`
	EndedAt        string `json:"endedAt,omitempty"`
	NextReminderAt string `json:"nextReminderAt,omitempty"`
	ReminderCount  int    `json:"reminderCount,omitempty"`
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
	corrupt         bool
	started         bool
	stopped         bool
	stopCh          chan struct{}
	doneCh          chan struct{}

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
	}
	p.window = newWindowBackend(windowCallbacks{
		Ack:       p.acknowledgeFromWindow,
		AckAll:    p.acknowledgeAllFromWindow,
		Snooze:    p.snoozeFromWindow,
		SnoozeAll: p.snoozeAllFromWindow,
		Failed:    p.windowFailed,
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

func (p *Plugin) applyWindowSettings() {
	p.mu.Lock()
	seconds := p.autoCloseSecondsLocked()
	p.mu.Unlock()
	p.window.SetAutoClose(seconds)
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
	p.mu.Unlock()
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
	p.mu.Unlock()
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
	if p.started {
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
	if !p.started || p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.stopCh)
	done := p.doneCh
	p.mu.Unlock()
	<-done
}

func (p *Plugin) run() {
	defer close(p.doneCh)
	p.load()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.processDue(p.now())
			p.flushDeferredWindow()
		}
	}
}

func (p *Plugin) timerListLocked() []*timer {
	list := make([]*timer, 0, len(p.timers))
	for _, item := range p.timers {
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].EndsAt.Equal(list[j].EndsAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].EndsAt.Before(list[j].EndsAt)
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
			EndsAt:    item.EndsAt.Format(time.RFC3339Nano),
			Status:    status,
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
		created, err1 := time.Parse(time.RFC3339, raw.CreatedAt)
		ends, err2 := time.Parse(time.RFC3339, raw.EndsAt)
		if err1 != nil || err2 != nil || strings.TrimSpace(raw.ID) == "" || strings.TrimSpace(raw.Title) == "" {
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
		item := &timer{
			ID: raw.ID, Title: raw.Title, CreatedAt: created,
			EndsAt: ends, Status: status,
		}
		switch status {
		case statusRunning:
			if ends.After(now) {
				restored[item.ID] = item
			} else {
				item.Status = statusPending
				item.EndedAt = ends
				item.NextReminderAt = now.Add(time.Duration(p.repeatFirstSecondsLocked()) * time.Second)
				restored[item.ID] = item
				newlyPending = append(newlyPending, item.Title)
				changed = true
			}
		case statusPending:
			item.EndedAt = ends
			if raw.EndedAt != "" {
				parsed, parseErr := time.Parse(time.RFC3339, raw.EndedAt)
				if parseErr != nil {
					p.failCorrupt("countdown.json 的 endedAt 无效，原文件未覆盖")
					return
				}
				item.EndedAt = parsed
			}
			if raw.NextReminderAt != "" {
				parsed, parseErr := time.Parse(time.RFC3339, raw.NextReminderAt)
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

func (p *Plugin) processDue(now time.Time) {
	p.mu.Lock()
	before := make(map[string]timer, len(p.timers))
	for id, item := range p.timers {
		before[id] = *item
	}
	var titles []string
	for _, item := range p.timers {
		switch item.Status {
		case statusRunning:
			if !item.EndsAt.After(now) {
				item.Status = statusPending
				item.EndedAt = item.EndsAt
				item.NextReminderAt = now.Add(time.Duration(p.repeatFirstSecondsLocked()) * time.Second)
				item.ReminderCount = 0
				titles = append(titles, item.Title)
			}
		case statusPending:
			if !item.NextReminderAt.After(now) {
				item.ReminderCount++
				item.NextReminderAt = now.Add(time.Duration(p.repeatEverySecondsLocked()) * time.Second)
				titles = append(titles, item.Title)
			}
		}
	}
	if len(titles) == 0 {
		p.mu.Unlock()
		return
	}
	if err := p.saveLocked(); err != nil {
		p.timers = make(map[string]*timer, len(before))
		for id := range before {
			item := before[id]
			p.timers[id] = &item
		}
		p.mu.Unlock()
		p.logf("expiry save failed: %v", err)
		return
	}
	// Keep the lock through state changes so an acknowledge cannot race
	// between the state check and the reminder being scheduled.
	floating := p.floatingEnabledLocked()
	if p.listVisible {
		p.refreshList()
	}
	p.mu.Unlock()
	if floating {
		p.syncWindow(true)
	} else {
		p.notifyPendingTitles(titles)
	}
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

func validItem(id, title, subtitle string) map[string]any {
	return map[string]any{"id": id, "title": title, "subtitle": subtitle, "valid": false}
}

func actionItem(id, title, subtitle string, data map[string]any, action string) map[string]any {
	actionTitle := "取消"
	if action == "start" {
		actionTitle = "开始"
	} else if action == "acknowledge" {
		actionTitle = "确认"
	}
	item := map[string]any{
		"id": id, "title": title, "subtitle": subtitle, "data": data,
		"valid":   true,
		"actions": []map[string]any{{"id": action, "title": actionTitle, "key": "Enter"}},
	}
	return item
}

func pendingItemView(item *timer) map[string]any {
	result := actionItem(
		item.ID, item.Title,
		"已经结束，等待确认。 · Enter 确认已知晓",
		map[string]any{"type": "acknowledge", "timerId": item.ID}, "acknowledge",
	)
	result["actions"] = []map[string]any{
		{"id": "acknowledge", "title": "确认", "key": "Enter"},
		{"id": "snooze", "title": "稍后提醒"},
	}
	return result
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
		running := 0
		for _, item := range p.timers {
			if item.Status == "" || item.Status == statusRunning {
				running++
			}
		}
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
	all := p.timerListLocked()
	p.mu.Unlock()
	if corrupt {
		p.setListVisible(false)
		return []map[string]any{validItem("corrupt", "倒计时数据文件损坏，请检查 countdown.json。", "")}, 0
	}
	pending := make([]*timer, 0)
	running := make([]*timer, 0)
	for _, item := range all {
		switch item.Status {
		case statusPending:
			pending = append(pending, item)
		case "", statusRunning:
			if item.EndsAt.After(now) {
				running = append(running, item)
			}
		}
	}
	if len(pending) == 0 && len(running) == 0 {
		p.setListVisible(false)
		return []map[string]any{validItem("empty", emptyTitle, emptyHint)}, 0
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
	items := make([]map[string]any, 0, len(pending)+len(running))
	for _, item := range pending {
		items = append(items, pendingItemView(item))
	}
	for _, item := range running {
		items = append(items, actionItem(
			item.ID, item.Title,
			"剩余 "+formatRemaining(item.EndsAt, now)+" · Enter 取消",
			map[string]any{"type": "cancel", "timerId": item.ID}, "cancel",
		))
	}
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
	if action == "snooze" {
		return p.snoozeTimer(data.TimerID)
	}
	switch data.Type {
	case "start":
		return p.startTimer(data.Title, data.Seconds)
	case "cancel":
		return p.cancelTimer(data.TimerID)
	case "acknowledge":
		return p.acknowledgeTimer(data.TimerID)
	case "snooze":
		return p.snoozeTimer(data.TimerID)
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
	running := 0
	for _, item := range p.timers {
		if item.Status == "" || item.Status == statusRunning {
			running++
		}
	}
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
	}
	p.timers[item.ID] = item
	if err := p.saveLocked(); err != nil {
		delete(p.timers, item.ID)
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，倒计时未创建。"}}}
	}
	p.listVisible = false
	p.mu.Unlock()
	p.notifyToast("倒计时已开始：" + title + " " + humanDuration(seconds))
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
	delete(p.timers, id)
	if err := p.saveLocked(); err != nil {
		p.timers[id] = item
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，未取消。"}}}
	}
	visible := p.listVisible
	p.listVisible = false
	p.mu.Unlock()
	p.notifyToast("已取消：" + item.Title)
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
	delete(p.timers, id)
	if err := p.saveLocked(); err != nil {
		p.timers[id] = item
		p.mu.Unlock()
		return map[string]any{"effects": []map[string]any{{"type": "notify", "text": "保存失败，未确认。"}}}
	}
	p.listVisible = false
	p.mu.Unlock()
	p.syncWindow(false)
	p.refreshList()
	return map[string]any{"effects": []map[string]any{
		{"type": "notify", "text": "已确认：" + item.Title},
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
		p.mu.Unlock()
		p.applyWindowSettings()
		p.reply(req.ID, map[string]any{"protocolVersion": "1", "name": appName}, nil)
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
		if params.Settings != nil {
			p.settings = params.Settings
		}
		if p.floatingEnabledLocked() {
			p.windowBroken = false
		}
		p.mu.Unlock()
		p.applyWindowSettings()
		p.syncWindow(false)
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
