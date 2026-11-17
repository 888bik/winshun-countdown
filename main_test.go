package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedNow() time.Time {
	return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
}

type fakeWindow struct {
	mu          sync.Mutex
	visible     bool
	items       []pendingItem
	shows       int
	refreshes   int
	hides       int
	showOK      bool
	autoClose   int
	popupCenter bool
}

func (f *fakeWindow) SetAutoClose(seconds int) {
	f.mu.Lock()
	f.autoClose = seconds
	f.mu.Unlock()
}

func (f *fakeWindow) SetAnimations(bool) {}

func (f *fakeWindow) SetPopupCenter(enabled bool) {
	f.mu.Lock()
	f.popupCenter = enabled
	f.mu.Unlock()
}

func (f *fakeWindow) Show(items []pendingItem) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.showOK {
		return false
	}
	f.visible = true
	f.items = append([]pendingItem(nil), items...)
	f.shows++
	return true
}

func (f *fakeWindow) Refresh(items []pendingItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append([]pendingItem(nil), items...)
	f.refreshes++
}

func (f *fakeWindow) Hide() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visible = false
	f.hides++
}

func (f *fakeWindow) Visible() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.visible
}

func (f *fakeWindow) snapshot() (bool, []pendingItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.visible, append([]pendingItem(nil), f.items...)
}

func testPlugin(t *testing.T, now time.Time) (*Plugin, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	out := &bytes.Buffer{}
	p := newPlugin(out, dir, func() time.Time { return now }, nil)
	p.sound = &fakeSound{}
	p.window = &fakeWindow{showOK: true}
	p.tray = &fakeTrayBackend{iconOwned: true, nextIconHandle: 1}
	p.island = &fakeIslandBackend{}
	p.settings["floating_window"] = false
	return p, out, dir
}

func TestParseDurationTable(t *testing.T) {
	valid := map[string]int64{
		"25":       25 * 60,
		"90s":      90,
		"90秒":      90,
		"25m":      25 * 60,
		"25分":      25 * 60,
		"1h":       3600,
		"1小时":      3600,
		"1h30m":    5400,
		"1h30m10s": 5410,
		"25:00":    1500,
		"1小时30分":   5400,
	}
	for input, want := range valid {
		got, err := parseDurationToken(input)
		if err != nil || got != want {
			t.Fatalf("parse %q = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-5", "abc", "0", "1.5h", "25:99", "99999h"} {
		if _, err := parseDurationToken(input); err == nil {
			t.Fatalf("parse %q should fail", input)
		}
	}
}

func TestHugeBareNumberRejected(t *testing.T) {
	for _, input := range []string{"4611686018427387905", "9999999999999999999"} {
		if _, err := parseDurationToken(input); err == nil {
			t.Fatalf("parse %q should fail without overflow", input)
		}
	}
}

func TestParseInputKeepsTitleWithSpaces(t *testing.T) {
	parsed, err := parseInput("写 周报 1h30m")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Title != "写 周报" || parsed.Seconds != 5400 {
		t.Fatalf("unexpected parse: %+v", parsed)
	}
	parsed, err = parseInput("25")
	if err != nil || parsed.Title != displayName || parsed.Seconds != 1500 {
		t.Fatalf("default title parse: %+v %v", parsed, err)
	}
}

func TestCreateListCancelAndExpire(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.startTimer("泡面", 180)
	p.startTimer("午休", 600)

	items, rerun := p.query("")
	if rerun != 1 || len(items) != 11 {
		t.Fatalf("list = %d items rerun=%d", len(items), rerun)
	}
	if items[0]["title"] != "泡面" {
		t.Fatalf("not sorted by remaining: %#v", items)
	}
	if !strings.Contains(items[0]["subtitle"].(string), "剩余 03:00") {
		t.Fatalf("subtitle: %v", items[0]["subtitle"])
	}

	id := p.timerListLocked()[0].ID
	p.doAction(actionData{Type: "cancel", TimerID: id})
	if len(p.timers) != 1 {
		t.Fatalf("cancel left %d timers", len(p.timers))
	}
	if !strings.Contains(out.String(), "已取消：泡面") {
		t.Fatalf("cancel notify missing: %s", out.String())
	}

	p.processDue(now.Add(601 * time.Second))
	if len(p.timers) != 1 {
		t.Fatalf("expiry should keep pending, got %d timers", len(p.timers))
	}
	expired := p.timerListLocked()[0]
	if expired.Status != statusPending {
		t.Fatalf("expired status = %q, want %q", expired.Status, statusPending)
	}
	if !strings.Contains(out.String(), "午休倒计时已结束。点我打开列表，按 Enter 确认。") {
		t.Fatalf("expiry notify missing: %s", out.String())
	}
	items, _ = p.query("")
	if len(items) != 10 || !strings.Contains(items[0]["subtitle"].(string), "等待确认") {
		t.Fatalf("pending list plus presets: %#v", items)
	}
	result := p.doAction(actionData{Type: "acknowledge", TimerID: expired.ID})
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "已确认：午休") {
		t.Fatalf("acknowledge result: %s", encoded)
	}
	if len(p.timers) != 0 {
		t.Fatalf("acknowledge left %d timers", len(p.timers))
	}
}

func TestReminderScheduleAndAck(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	out.Reset()
	ends := now.Add(60 * time.Second).Add(15 * time.Second)
	p.processDue(now.Add(60 * time.Second))

	item := p.timerListLocked()[0]
	if item.Status != statusPending || item.ReminderCount != 0 {
		t.Fatalf("pending state: %#v", item)
	}
	if !item.NextReminderAt.Equal(ends) {
		t.Fatalf("next reminder = %v, want %v", item.NextReminderAt, ends)
	}
	if got := notificationCount(out.String()); got != 1 {
		t.Fatalf("initial notifications = %d, want 1: %s", got, out.String())
	}

	out.Reset()
	p.processDue(ends.Add(-time.Second))
	if notificationCount(out.String()) != 0 {
		t.Fatalf("early reminder: %s", out.String())
	}

	p.processDue(ends)
	item = p.timerListLocked()[0]
	if item.ReminderCount != 1 || !item.NextReminderAt.Equal(ends.Add(60*time.Second)) {
		t.Fatalf("first repeat: %#v", item)
	}
	if !strings.Contains(out.String(), "泡面倒计时已结束。点我打开列表，按 Enter 确认。") {
		t.Fatalf("repeat text: %s", out.String())
	}

	out.Reset()
	p.processDue(ends.Add(59 * time.Second))
	if notificationCount(out.String()) != 0 {
		t.Fatalf("second repeat too early: %s", out.String())
	}
	p.processDue(ends.Add(60 * time.Second))
	item = p.timerListLocked()[0]
	if item.ReminderCount != 2 {
		t.Fatalf("second repeat count: %#v", item)
	}

	id := item.ID
	if encoded, _ := json.Marshal(p.doAction(actionData{Type: "acknowledge", TimerID: id})); !strings.Contains(string(encoded), "已确认：泡面") {
		t.Fatalf("ack result: %s", encoded)
	}
	out.Reset()
	p.processDue(ends.Add(10 * time.Minute))
	if notificationCount(out.String()) != 0 {
		t.Fatalf("reminder after acknowledge: %s", out.String())
	}
	if len(p.timers) != 0 {
		t.Fatalf("acknowledged timer still present")
	}
}

func TestReminderIntervalsTable(t *testing.T) {
	now := fixedNow()
	cases := []struct {
		name       string
		advance    time.Duration
		wantCount  int
		wantNotify int
	}{
		{"initial expiry", 0, 0, 1},
		{"before 15s", 14 * time.Second, 0, 0},
		{"first repeat at 15s", 15 * time.Second, 1, 1},
		{"before next 60s", 74 * time.Second, 1, 0},
		{"second repeat at 75s", 75 * time.Second, 2, 1},
	}
	p, out, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	out.Reset()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			p.processDue(now.Add(60*time.Second + tc.advance))
			item := p.timerListLocked()[0]
			if item.ReminderCount != tc.wantCount {
				t.Fatalf("count=%d, want %d", item.ReminderCount, tc.wantCount)
			}
			if got := notificationCount(out.String()); got != tc.wantNotify {
				t.Fatalf("notifications=%d, want %d: %s", got, tc.wantNotify, out.String())
			}
		})
	}
}

func TestPendingListOrderAndQueryDoesNotMutate(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	p.startTimer("午休", 120)
	p.processDue(now.Add(120 * time.Second))
	out.Reset()

	before := p.timerListLocked()
	items, rerun := p.query("")
	if rerun != 1 || len(items) != 11 {
		t.Fatalf("list: %d rerun=%d", len(items), rerun)
	}
	if items[0]["title"] != "泡面" || !strings.Contains(items[0]["subtitle"].(string), "等待确认") {
		t.Fatalf("pending should be first: %#v", items)
	}
	after := p.timerListLocked()
	if len(before) != len(after) || before[0].Status != after[0].Status {
		t.Fatalf("query mutated state")
	}
	if notificationCount(out.String()) != 0 {
		t.Fatalf("query sent notification: %s", out.String())
	}
}

func TestMultiplePendingNotificationsAreMerged(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	p.startTimer("午休", 60)
	out.Reset()
	p.processDue(now.Add(60 * time.Second))
	if got := notificationCount(out.String()); got != 1 {
		t.Fatalf("merged initial notifications = %d: %s", got, out.String())
	}
	if !strings.Contains(out.String(), "泡面") || !strings.Contains(out.String(), "午休") || !strings.Contains(out.String(), "倒计时已结束。点我打开列表，按 Enter 确认。") {
		t.Fatalf("merged text: %s", out.String())
	}
	out.Reset()
	p.processDue(now.Add(75 * time.Second))
	if got := notificationCount(out.String()); got != 1 {
		t.Fatalf("merged repeat notifications = %d: %s", got, out.String())
	}
}

func TestNotificationClickAndDisappearanceDoNotChangeState(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	item := p.timerListLocked()[0]
	var notification map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var message map[string]any
		if json.Unmarshal([]byte(line), &message) == nil && message["method"] == "winshun/notify" {
			notification = message
			break
		}
	}
	if notification == nil {
		t.Fatalf("no notification: %s", out.String())
	}
	params := notification["params"].(map[string]any)
	click := params["click"].([]any)[0].(map[string]any)
	if click["type"] != "setQuery" || click["text"] != "倒计时 " {
		t.Fatalf("notification click: %#v", click)
	}
	if item.Status != statusPending || len(p.timers) != 1 {
		t.Fatalf("notification changed state: %#v", p.timers)
	}
	// No scheduler call means the notification merely disappearing changes nothing.
	if p.timerListLocked()[0].Status != statusPending {
		t.Fatal("notification disappearance changed state")
	}
}

func TestRestartRestoresPendingAndNextReminder(t *testing.T) {
	now := fixedNow()
	p, _, dir := testPlugin(t, now)
	p.startTimer("泡面", 60)
	p.startTimer("会议", 600)
	p.processDue(now.Add(60 * time.Second))

	p2 := newPlugin(&bytes.Buffer{}, dir, func() time.Time { return now.Add(70 * time.Second) }, nil)
	p2.sound = &fakeSound{}
	p2.window = &fakeWindow{showOK: true}
	p2.settings["floating_window"] = false
	p2.load()
	if len(p2.timers) != 2 {
		t.Fatalf("restart restored %d", len(p2.timers))
	}
	var pending, running *timer
	for _, item := range p2.timers {
		if item.Status == statusPending {
			pending = item
		} else {
			running = item
		}
	}
	if pending == nil || running == nil {
		t.Fatalf("missing states: %#v", p2.timers)
	}
	wantNext := now.Add(75 * time.Second)
	if !pending.NextReminderAt.Equal(wantNext) {
		t.Fatalf("pending next = %v, want %v", pending.NextReminderAt, wantNext)
	}
	out := &bytes.Buffer{}
	p2.out = out
	p2.processDue(now.Add(74 * time.Second))
	if notificationCount(out.String()) != 0 {
		t.Fatalf("early reminder after restart: %s", out.String())
	}
	p2.processDue(now.Add(75 * time.Second))
	if !strings.Contains(out.String(), "泡面倒计时已结束。点我打开列表，按 Enter 确认。") {
		t.Fatalf("restart reminder: %s", out.String())
	}
}

func TestV1MigrationKeepsExpiredAsPending(t *testing.T) {
	now := fixedNow()
	dir := t.TempDir()
	doc := storedFile{Version: 1, Timers: []storedTimer{
		{ID: "new", Title: "未到期", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), EndsAt: now.Add(time.Hour).Format(time.RFC3339)},
		{ID: "old", Title: "已到期", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), EndsAt: now.Add(-time.Minute).Format(time.RFC3339)},
	}}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, dataFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	p := newPlugin(out, dir, func() time.Time { return now }, nil)
	p.sound = &fakeSound{}
	p.window = &fakeWindow{showOK: true}
	p.settings["floating_window"] = false
	p.load()
	if len(p.timers) != 2 {
		t.Fatalf("migration kept %d timers", len(p.timers))
	}
	if p.timers["new"].Status != statusRunning || p.timers["old"].Status != statusPending {
		t.Fatalf("statuses after migration: %#v", p.timers)
	}
	if !strings.Contains(out.String(), "已到期倒计时已结束。点我打开列表，按 Enter 确认。") {
		t.Fatalf("migration notification: %s", out.String())
	}
}

func TestPendingDoesNotConsumeRunningSlot(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	for index := 0; index < maxTimers-1; index++ {
		p.startTimer("任务", 600)
	}
	p.startTimer("到期", 1)
	p.processDue(now.Add(2 * time.Second))
	if len(p.timers) != maxTimers {
		t.Fatalf("setup timers = %d", len(p.timers))
	}
	result := p.startTimer("第二十个", 600)
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "最多同时运行") {
		t.Fatalf("pending consumed running slot: %s", encoded)
	}
	result = p.startTimer("第二十一个", 600)
	encoded, _ = json.Marshal(result)
	if !strings.Contains(string(encoded), "最多同时运行") {
		t.Fatalf("running limit bypassed: %s", encoded)
	}
}

func TestAcknowledgeRefreshesList(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	p.query("")
	out.Reset()
	id := p.timerListLocked()[0].ID
	p.acknowledgeTimer(id)
	if !strings.Contains(out.String(), "winshun/refresh") {
		t.Fatalf("ack did not refresh: %s", out.String())
	}
}

func TestAcknowledgeAndSchedulerRace(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		now := fixedNow().Add(time.Duration(iteration) * time.Minute)
		p, out, _ := testPlugin(t, now)
		p.startTimer("泡面", 60)
		p.processDue(now.Add(60 * time.Second))
		id := p.timerListLocked()[0].ID
		out.Reset()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			p.acknowledgeTimer(id)
		}()
		go func() {
			defer wg.Done()
			p.processDue(now.Add(75 * time.Second))
		}()
		wg.Wait()
		if p.timers[id] != nil {
			t.Fatalf("iteration %d: acknowledged timer remains", iteration)
		}
		out.Reset()
		p.processDue(now.Add(10 * time.Minute))
		if notificationCount(out.String()) != 0 {
			t.Fatalf("iteration %d: reminder after ack: %s", iteration, out.String())
		}
	}
}

func TestFloatingWindowShowsListsAndAcknowledges(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.settings["floating_window"] = true
	fake := p.window.(*fakeWindow)
	p.startTimer("泡面", 60)
	p.startTimer("午休", 120)
	p.processDue(now.Add(120 * time.Second))

	visible, items := fake.snapshot()
	if !visible || len(items) != 2 || items[0].Title != "泡面" || items[1].Title != "午休" {
		t.Fatalf("window items: visible=%v items=%#v", visible, items)
	}
	id := p.timerListLocked()[0].ID
	if !p.confirmPending(id) {
		t.Fatal("confirm failed")
	}
	p.syncWindow(false)
	visible, items = fake.snapshot()
	if !visible || len(items) != 1 || items[0].Title != "午休" {
		t.Fatalf("after single confirm: visible=%v items=%#v", visible, items)
	}
	p.acknowledgeAllFromWindow()
	visible, _ = fake.snapshot()
	if visible || len(p.timers) != 0 {
		t.Fatalf("all acknowledge did not clear: visible=%v timers=%d", visible, len(p.timers))
	}
}

func TestFloatingWindowCloseDoesNotAcknowledgeAndRepeatReshows(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.settings["floating_window"] = true
	fake := p.window.(*fakeWindow)
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	if !fake.Visible() {
		t.Fatal("window not shown")
	}
	fake.Hide()
	if p.timerListLocked()[0].Status != statusPending {
		t.Fatal("close acknowledged")
	}
	p.processDue(now.Add(75 * time.Second))
	if !fake.Visible() || fake.shows < 2 {
		t.Fatalf("repeat did not reopen: visible=%v shows=%d", fake.Visible(), fake.shows)
	}
}

func TestFloatingWindowDeferredWhileListVisible(t *testing.T) {
	now := fixedNow()
	current := now
	p, _, _ := testPlugin(t, now)
	p.now = func() time.Time { return current }
	p.settings["floating_window"] = true
	fake := p.window.(*fakeWindow)
	p.startTimer("泡面", 60)
	p.mu.Lock()
	p.lastQueryAt = now
	p.mu.Unlock()

	p.processDue(now.Add(60 * time.Second))
	if fake.Visible() {
		t.Fatal("window shown while list visible")
	}
	current = now.Add(2 * time.Second)
	p.flushDeferredWindow()
	if fake.Visible() {
		t.Fatal("window shown before 2.5s list heartbeat expired")
	}
	current = now.Add(3 * time.Second)
	p.flushDeferredWindow()
	if !fake.Visible() {
		t.Fatal("deferred window not shown after list closed")
	}
}

func TestFloatingWindowSettingAndQueryDoNotPopup(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.settings["floating_window"] = true
	fake := p.window.(*fakeWindow)
	p.settings["floating_window"] = false
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	if fake.Visible() {
		t.Fatal("disabled setting still popped")
	}

	p.settings["floating_window"] = true
	p.beginInteraction()
	p.syncWindow(true)
	p.endInteraction()
	if fake.Visible() {
		t.Fatal("window shown during protocol interaction")
	}
	p.flushDeferredWindow()
	if !fake.Visible() {
		t.Fatal("deferred popup did not flush")
	}
}

func TestFloatingWindowListAcknowledgeSyncs(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.settings["floating_window"] = true
	fake := p.window.(*fakeWindow)
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	p.query("")
	out.Reset()
	id := p.timerListLocked()[0].ID
	p.acknowledgeTimer(id)
	if fake.Visible() {
		t.Fatal("window visible after list acknowledge")
	}
	if !strings.Contains(out.String(), "winshun/refresh") {
		t.Fatalf("ack did not refresh list: %s", out.String())
	}
}

func TestFloatingWindowSettingsChangedHides(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.settings["floating_window"] = true
	fake := p.window.(*fakeWindow)
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	if !fake.Visible() {
		t.Fatal("window not shown")
	}
	p.handleLine([]byte(`{"jsonrpc":"2.0","method":"settingsChanged","params":{"settings":{"floating_window":false}}}`))
	if fake.Visible() {
		t.Fatal("settingsChanged(false) did not hide")
	}
}

func TestFloatingWindowSuppressesNotification(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.settings["floating_window"] = true
	fake := p.window.(*fakeWindow)
	p.startTimer("泡面", 60)
	out.Reset()
	p.processDue(now.Add(60 * time.Second))
	if notificationCount(out.String()) != 0 {
		t.Fatalf("floating window sent notification: %s", out.String())
	}
	if !fake.Visible() {
		t.Fatal("floating window did not show")
	}
}

func TestFloatingDisabledKeepsNotification(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	fake := p.window.(*fakeWindow)
	p.startTimer("泡面", 60)
	out.Reset()
	p.processDue(now.Add(60 * time.Second))
	if notificationCount(out.String()) != 1 {
		t.Fatalf("notification count = %d: %s", notificationCount(out.String()), out.String())
	}
	if fake.Visible() {
		t.Fatal("disabled floating window showed")
	}
}

func TestFloatingFailureFallsBackToNotification(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.settings["floating_window"] = true
	fake := p.window.(*fakeWindow)
	fake.showOK = false
	p.startTimer("泡面", 60)
	out.Reset()
	p.processDue(now.Add(60 * time.Second))
	if notificationCount(out.String()) != 1 {
		t.Fatalf("fallback notification count = %d: %s", notificationCount(out.String()), out.String())
	}
	if fake.Visible() {
		t.Fatal("failed window became visible")
	}
}

func TestFloatingStartupSuppressesNotification(t *testing.T) {
	now := fixedNow()
	dir := t.TempDir()
	doc := storedFile{Version: 1, Timers: []storedTimer{
		{ID: "old", Title: "泡面", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), EndsAt: now.Add(-time.Minute).Format(time.RFC3339)},
	}}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, dataFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	p := newPlugin(out, dir, func() time.Time { return now }, nil)
	p.sound = &fakeSound{}
	p.window = &fakeWindow{showOK: true}
	p.settings["floating_window"] = true
	p.load()
	if notificationCount(out.String()) != 0 {
		t.Fatalf("startup sent notification with floating window: %s", out.String())
	}
	if !p.window.Visible() {
		t.Fatal("startup did not show floating window")
	}
}

func TestAnimationEasingAndReverse(t *testing.T) {
	now := fixedNow()
	var a animation
	a.begin(animEnter, now, animationEnterDuration, 0, 1)
	if got, done := a.progress(now); got != 0 || done {
		t.Fatalf("start = %v %v", got, done)
	}
	if got, done := a.progress(now.Add(animationEnterDuration / 2)); got <= 0 || got >= 1 || done {
		t.Fatalf("mid = %v %v", got, done)
	}
	if got, done := a.progress(now.Add(animationEnterDuration)); got != 1 || !done {
		t.Fatalf("end = %v %v", got, done)
	}
	a.begin(animExit, now, animationExitDuration, 1, 0)
	if got, done := a.progress(now.Add(animationExitDuration / 2)); got <= 0 || got >= 1 || done {
		t.Fatalf("exit mid = %v %v", got, done)
	}
	a.begin(animEnter, now, 0, 0, 1)
	if got, done := a.progress(now); got != 1 || !done {
		t.Fatalf("zero duration = %v %v", got, done)
	}
}

func TestAnimationProgressMonotonicAtFrameIntervals(t *testing.T) {
	now := fixedNow()
	var enter animation
	enter.begin(animEnter, now, animationEnterDuration, 0, 1)
	previous := -1.0
	step := time.Duration(animationFrameInterval) * time.Millisecond
	for elapsed := time.Duration(0); elapsed <= animationEnterDuration; elapsed += step {
		value, _ := enter.progress(now.Add(elapsed))
		if value < previous {
			t.Fatalf("enter animation moved backwards at %s: %v < %v", elapsed, value, previous)
		}
		previous = value
	}
	if previous != 1 {
		value, done := enter.progress(now.Add(animationEnterDuration))
		if !done || value != 1 {
			t.Fatalf("enter animation final state = %v, done=%v", value, done)
		}
	}

	var exit animation
	exit.begin(animExit, now, animationExitDuration, 1, 0)
	previous = 2
	for elapsed := time.Duration(0); elapsed <= animationExitDuration; elapsed += step {
		value, _ := exit.progress(now.Add(elapsed))
		if value > previous {
			t.Fatalf("exit animation moved backwards at %s: %v > %v", elapsed, value, previous)
		}
		previous = value
	}
	if value, done := exit.progress(now.Add(animationExitDuration)); value != 0 || !done {
		t.Fatalf("exit animation final state = %v, done=%v", value, done)
	}
}

func TestNextWakeDelayDeferredCapped(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	item := p.timerListLocked()[0]
	item.NextReminderAt = now.Add(120 * time.Second)
	p.deferredWindow = true
	if got, ok := p.nextWakeDelayLocked(now.Add(60 * time.Second)); !ok || got != 2500*time.Millisecond {
		t.Fatalf("deferred delay = %v, %v", got, ok)
	}
	p.deferredWindow = false
	if got, ok := p.nextWakeDelayLocked(now.Add(60 * time.Second)); !ok || got != 30*time.Second {
		t.Fatalf("normal delay = %v, %v", got, ok)
	}
}

func TestNextWakeDelay(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	if _, ok := p.nextWakeDelayLocked(now); ok {
		t.Fatal("idle plugin should have no wake deadline")
	}
	p.startTimer("泡面", 60)
	delay, ok := p.nextWakeDelayLocked(now)
	if !ok || delay != 30*time.Second {
		t.Fatalf("cap delay = %v, %v", delay, ok)
	}
	p.settings["repeat_first_seconds"] = 10
	p.settings["repeat_every_seconds"] = 10
	p.processDue(now.Add(60 * time.Second))
	delay, ok = p.nextWakeDelayLocked(now.Add(60 * time.Second))
	if !ok || delay != 10*time.Second {
		t.Fatalf("pending delay = %v, %v", delay, ok)
	}
}

func TestAutoCloseLogic(t *testing.T) {
	if autoCloseDue(true, 0, time.Hour) {
		t.Fatal("zero auto-close must never fire")
	}
	if autoCloseDue(true, 10, 9*time.Second) {
		t.Fatal("auto-close fired early")
	}
	if !autoCloseDue(true, 10, 10*time.Second) {
		t.Fatal("auto-close did not fire")
	}
	if autoCloseDue(false, 10, time.Hour) {
		t.Fatal("hidden window auto-closed")
	}
}

func TestSnoozeReturnsToRunning(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.settings["snooze_minutes"] = 5
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	id := p.timerListLocked()[0].ID
	result := p.snoozeTimer(id)
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "已稍后提醒：泡面，5 分钟后再次提醒") {
		t.Fatalf("snooze result: %s", encoded)
	}
	item := p.timers[id]
	if item == nil || item.Status != statusRunning {
		t.Fatalf("snoozed state: %#v", item)
	}
	if !item.EndsAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("snooze endsAt = %v", item.EndsAt)
	}
	if item.ReminderCount != 0 || !item.NextReminderAt.IsZero() {
		t.Fatalf("snooze reminder state: %#v", item)
	}
	items, _ := p.query("")
	if len(items) != 10 || !strings.Contains(items[0]["subtitle"].(string), "剩余 05:00") {
		t.Fatalf("snoozed list plus presets: %#v", items)
	}
}

func TestSnoozeAllReschedulesAll(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.settings["snooze_minutes"] = 7
	p.startTimer("泡面", 60)
	p.startTimer("午休", 60)
	p.processDue(now.Add(60 * time.Second))
	if len(p.timerListLocked()) != 2 {
		t.Fatal("setup pending failed")
	}
	p.snoozeAllFromWindow()
	for _, item := range p.timers {
		if item.Status != statusRunning || !item.EndsAt.Equal(now.Add(7*time.Minute)) {
			t.Fatalf("snooze all state: %#v", item)
		}
	}
}

func TestRepeatSettingsAffectSchedule(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.settings["repeat_first_seconds"] = 5
	p.settings["repeat_every_seconds"] = 20
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	item := p.timerListLocked()[0]
	if !item.NextReminderAt.Equal(now.Add(65 * time.Second)) {
		t.Fatalf("first repeat = %v", item.NextReminderAt)
	}
	p.processDue(now.Add(65 * time.Second))
	item = p.timerListLocked()[0]
	if !item.NextReminderAt.Equal(now.Add(85 * time.Second)) {
		t.Fatalf("later repeat = %v", item.NextReminderAt)
	}
}

func TestRepeatSettingsClamp(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	p.settings["repeat_first_seconds"] = 1
	p.settings["repeat_every_seconds"] = 99999
	if got := p.repeatFirstSecondsLocked(); got != 5 {
		t.Fatalf("first clamp = %d", got)
	}
	if got := p.repeatEverySecondsLocked(); got != 3600 {
		t.Fatalf("every clamp = %d", got)
	}
}

func TestTrayMenuModelGroupsSortsAndBuildsActions(t *testing.T) {
	base := fixedNow()
	items := []trayTimerSnapshot{
		{ID: "late", Title: "午休", Status: statusRunning, RemainingSeconds: 300, EndsAt: base.Add(5 * time.Minute)},
		{ID: "paused", Title: "学习", Status: statusPaused, RemainingSeconds: 45},
		{ID: "pending", Title: "泡面", Status: statusPending, EndedAt: base.Add(-time.Minute)},
		{ID: "early", Title: "会议", Status: statusRunning, RemainingSeconds: 60, EndsAt: base.Add(time.Minute)},
	}
	model := buildTrayMenuModel(items)
	if len(model.Items) != 7 {
		t.Fatalf("menu item count = %d, want 7: %#v", len(model.Items), model.Items)
	}
	if got, want := model.Items[0].Label, "倒计时 · 2 个在跑 · 1 个待确认"; got != want {
		t.Fatalf("header = %q, want %q", got, want)
	}
	wantLabels := []string{
		"会议 · 剩余 01:00",
		"午休 · 剩余 05:00",
		"学习 · 已暂停，剩余 00:45",
		"泡面 · 已到期，等待确认",
	}
	for index, want := range wantLabels {
		if got := model.Items[index+1].Label; got != want {
			t.Fatalf("menu item %d = %q, want %q", index, got, want)
		}
	}
	wantActions := [][]string{
		{"pause", "cancel", "restart"},
		{"pause", "cancel", "restart"},
		{"resume", "cancel"},
		{"acknowledge", "snooze", "restart"},
	}
	wantIDs := []string{"early", "late", "paused", "pending"}
	for index, want := range wantActions {
		entry := model.Items[index+1]
		if len(entry.Children) != len(want) {
			t.Fatalf("children for %q = %#v", entry.Label, entry.Children)
		}
		for childIndex, action := range want {
			binding, ok := model.Commands[entry.Children[childIndex].CommandID]
			if !ok || binding.Action != action || binding.TimerID != wantIDs[index] {
				t.Fatalf("command binding for %q/%d = %#v, ok=%v", entry.Label, childIndex, binding, ok)
			}
		}
	}
	if !model.Items[5].Separator || model.Items[6].Label != "打开倒计时列表…" {
		t.Fatalf("open-list placement: %#v", model.Items[5:])
	}
	if len(model.Commands) != 12 {
		t.Fatalf("command count = %d, want 12", len(model.Commands))
	}
}

func TestTrayEmptyMenuHasOnlyOpenListAction(t *testing.T) {
	model := buildTrayMenuModel(nil)
	if len(model.Items) != 3 || model.Items[0].Label != "倒计时 · 没有正在运行的计时器" {
		t.Fatalf("empty menu = %#v", model.Items)
	}
	if !model.Items[0].Disabled || !model.Items[1].Separator || model.Items[2].Label != "打开倒计时列表…" {
		t.Fatalf("empty menu structure = %#v", model.Items)
	}
	if len(model.Commands) != 1 {
		t.Fatalf("empty menu command count = %d, want 1", len(model.Commands))
	}
	for _, binding := range model.Commands {
		if !binding.OpenList {
			t.Fatalf("empty menu command is not open-list: %#v", binding)
		}
	}
}

func TestTrayMenuRuneTruncationAndTwentyTimerLimit(t *testing.T) {
	longTitle := strings.Repeat("中", 45)
	items := make([]trayTimerSnapshot, 0, 21)
	items = append(items, trayTimerSnapshot{
		ID: "long", Title: longTitle, Status: statusRunning,
		RemainingSeconds: 10, EndsAt: fixedNow().Add(10 * time.Second),
	})
	for index := 0; index < 20; index++ {
		items = append(items, trayTimerSnapshot{
			ID:    fmt.Sprintf("timer-%02d", index),
			Title: fmt.Sprintf("任务%d", index), Status: statusRunning,
			RemainingSeconds: int64(index + 20), EndsAt: fixedNow().Add(time.Duration(index+20) * time.Second),
		})
	}
	model := buildTrayMenuModel(items)
	if got, want := model.Items[1].Label, strings.Repeat("中", 40)+"… · 剩余 00:10"; got != want {
		t.Fatalf("truncated title = %q, want %q", got, want)
	}
	if len(model.Items) != 24 {
		t.Fatalf("overflow menu item count = %d, want 24", len(model.Items))
	}
	if got, want := model.Items[21].Label, "还有 1 个，去列表里看"; got != want || !model.Items[21].Disabled {
		t.Fatalf("overflow item = %#v, want disabled %q", model.Items[21], want)
	}
	if len(model.Commands) != 61 {
		t.Fatalf("visible action/open command count = %d, want 61", len(model.Commands))
	}
}

func TestTrayCommandDispatchUsesExistingTimerActions(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		action     string
		wantStatus string
		wantGone   bool
		wantEnd    time.Duration
	}{
		{name: "pause", status: statusRunning, action: "pause", wantStatus: statusPaused},
		{name: "resume", status: statusPaused, action: "resume", wantStatus: statusRunning, wantEnd: 30 * time.Second},
		{name: "cancel running", status: statusRunning, action: "cancel", wantGone: true},
		{name: "restart running", status: statusRunning, action: "restart", wantStatus: statusRunning, wantEnd: 60 * time.Second},
		{name: "acknowledge", status: statusPending, action: "acknowledge", wantGone: true},
		{name: "snooze", status: statusPending, action: "snooze", wantStatus: statusRunning, wantEnd: 5 * time.Minute},
		{name: "restart pending", status: statusPending, action: "restart", wantStatus: statusRunning, wantEnd: 60 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := fixedNow()
			p, _, _ := testPlugin(t, now)
			item := &timer{
				ID: "timer-id", Title: "任务", CreatedAt: now,
				EndsAt: now.Add(30 * time.Second), Status: tc.status,
				OriginalSeconds: 60, RemainingSeconds: 30,
			}
			if tc.status == statusPending {
				item.EndsAt = time.Time{}
				item.EndedAt = now
				item.NextReminderAt = now.Add(time.Minute)
			}
			if tc.status == statusPaused {
				item.EndsAt = time.Time{}
			}
			p.timers[item.ID] = item
			p.handleTrayAction(item.ID, tc.action)
			got := p.timers[item.ID]
			if tc.wantGone {
				if got != nil {
					t.Fatalf("timer still exists after %s: %#v", tc.action, got)
				}
				return
			}
			if got == nil || got.Status != tc.wantStatus {
				t.Fatalf("timer after %s = %#v, want status %q", tc.action, got, tc.wantStatus)
			}
			if tc.wantEnd > 0 && !got.EndsAt.Equal(now.Add(tc.wantEnd)) {
				t.Fatalf("EndsAt after %s = %v, want %v", tc.action, got.EndsAt, now.Add(tc.wantEnd))
			}
		})
	}
}

func TestTrayTooltipTextStates(t *testing.T) {
	base := fixedNow()
	tests := []struct {
		name  string
		items []trayTimerSnapshot
		want  string
	}{
		{
			name:  "running",
			items: []trayTimerSnapshot{{ID: "a", Status: statusRunning, RemainingSeconds: 150, EndsAt: base.Add(150 * time.Second)}},
			want:  "倒计时：1 个在跑，最近 02:30 后到期",
		},
		{
			name: "running and pending",
			items: []trayTimerSnapshot{
				{ID: "a", Status: statusRunning, RemainingSeconds: 150, EndsAt: base.Add(150 * time.Second)},
				{ID: "b", Status: statusPending, EndedAt: base},
			},
			want: "倒计时：1 个在跑，最近 02:30 后到期 · 1 个待确认",
		},
		{
			name: "empty",
			want: "倒计时：没有正在运行的计时器",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildTrayTooltip(tc.items); got != tc.want {
				t.Fatalf("tooltip = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTraySettingChangesApplyImmediately(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	fake := p.tray.(*fakeTrayBackend)
	p.settings["tray_icon"] = true
	p.applyTraySettings()
	if enabled, adds, _, _, _, _, _ := fake.snapshot(); !enabled || adds != 1 {
		t.Fatalf("enable state: enabled=%v adds=%d", enabled, adds)
	}
	p.handleLine([]byte(`{"jsonrpc":"2.0","method":"settingsChanged","params":{"settings":{"tray_icon":false}}}`))
	if enabled, _, _, deletes, _, _, _ := fake.snapshot(); enabled || deletes != 1 {
		t.Fatalf("disable state: enabled=%v deletes=%d", enabled, deletes)
	}
	p.handleLine([]byte(`{"jsonrpc":"2.0","method":"settingsChanged","params":{"settings":{"tray_icon":true}}}`))
	if enabled, adds, _, _, _, _, _ := fake.snapshot(); !enabled || adds != 2 {
		t.Fatalf("re-enable state: enabled=%v adds=%d", enabled, adds)
	}
}

func TestTrayTaskbarCreatedRestoresIcon(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	fake := p.tray.(*fakeTrayBackend)
	p.settings["tray_icon"] = true
	p.applyTraySettings()
	p.tray.TaskbarCreated()
	if enabled, adds, _, _, readds, _, _ := fake.snapshot(); !enabled || adds != 2 || readds != 1 {
		t.Fatalf("Explorer restart state: enabled=%v adds=%d readds=%d", enabled, adds, readds)
	}
}

func TestTrayCloseDeletesIconOnStop(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	fake := p.tray.(*fakeTrayBackend)
	p.settings["tray_icon"] = true
	p.applyTraySettings()
	p.stop()
	enabled, _, _, deletes, _, closes, _ := fake.snapshot()
	if enabled || deletes != 1 || closes != 1 {
		t.Fatalf("stop did not remove tray icon: enabled=%v deletes=%d closes=%d", enabled, deletes, closes)
	}
}

func TestTrayTooltipRefreshesAfterTimerStateChanges(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	fake := p.tray.(*fakeTrayBackend)
	p.settings["tray_icon"] = true
	p.applyTraySettings()

	p.startTimer("托盘更新", 60)
	_, _, _, _, _, _, items := fake.snapshot()
	if len(items) != 1 || items[0].Status != statusRunning {
		t.Fatalf("tray snapshot after start = %#v", items)
	}
	id := items[0].ID
	p.pauseTimer(id)
	_, _, _, _, _, _, items = fake.snapshot()
	if len(items) != 1 || items[0].Status != statusPaused {
		t.Fatalf("tray snapshot after pause = %#v", items)
	}
	p.resumeTimer(id)
	_, _, _, _, _, _, items = fake.snapshot()
	if len(items) != 1 || items[0].Status != statusRunning {
		t.Fatalf("tray snapshot after resume = %#v", items)
	}
	p.restartTimer(id)
	_, _, _, _, _, _, items = fake.snapshot()
	if len(items) != 1 || items[0].Status != statusRunning {
		t.Fatalf("tray snapshot after restart = %#v", items)
	}
	p.processDue(now.Add(time.Minute))
	_, _, _, _, _, _, items = fake.snapshot()
	if len(items) != 1 || items[0].Status != statusPending {
		t.Fatalf("tray snapshot after expiry = %#v", items)
	}
	p.snoozeTimer(id)
	_, _, _, _, _, _, items = fake.snapshot()
	if len(items) != 1 || items[0].Status != statusRunning {
		t.Fatalf("tray snapshot after snooze = %#v", items)
	}
	p.processDue(now.Add(5 * time.Minute))
	_, _, _, _, _, _, items = fake.snapshot()
	if len(items) != 1 || items[0].Status != statusPending {
		t.Fatalf("tray snapshot after snooze expiry = %#v", items)
	}
	p.acknowledgeTimer(id)
	_, _, _, _, _, _, items = fake.snapshot()
	if len(items) != 0 {
		t.Fatalf("tray snapshot after acknowledge = %#v", items)
	}
}

func TestTrayOpenListUsesNotificationSetQueryBridge(t *testing.T) {
	p, out, _ := testPlugin(t, fixedNow())
	p.openListFromTray()
	var message map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &message); err != nil {
		t.Fatalf("decode tray open-list notification: %v; output=%s", err, out.String())
	}
	if message["method"] != "winshun/notify" {
		t.Fatalf("method = %v, want winshun/notify", message["method"])
	}
	params := message["params"].(map[string]any)
	click := params["click"].([]any)
	if len(click) != 1 {
		t.Fatalf("click effects = %#v", click)
	}
	effect := click[0].(map[string]any)
	if effect["type"] != "setQuery" || effect["text"] != "倒计时 " {
		t.Fatalf("click effect = %#v, want setQuery with the countdown keyword", effect)
	}
}

func TestPendingActionsIncludeSnooze(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	items, _ := p.query("")
	actions := items[0]["actions"].([]map[string]any)
	if len(actions) != 3 || actions[0]["id"] != "acknowledge" || actions[1]["id"] != "snooze" || actions[2]["id"] != "restart" {
		t.Fatalf("pending actions: %#v", actions)
	}
}

func TestAutoCloseSettingReachesWindow(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	p.handleLine([]byte(`{"jsonrpc":"2.0","method":"settingsChanged","params":{"settings":{"auto_close_seconds":30}}}`))
	if got := p.window.(*fakeWindow).autoClose; got != 30 {
		t.Fatalf("auto-close setting = %d", got)
	}
}

func TestWindowPageScroll(t *testing.T) {
	got := pageIndices(8, 1, 6)
	want := []int{1, 2, 3, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("page = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("page = %v", got)
		}
	}
	if got := pageIndices(3, 0, 6); len(got) != 3 || got[2] != 2 {
		t.Fatalf("short page = %v", got)
	}
}

func notificationCount(raw string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.Contains(line, `"method":"winshun/notify"`) {
			count++
		}
	}
	return count
}

func TestLimit(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	for index := 0; index < maxTimers; index++ {
		p.startTimer("任务", 600)
	}
	result := p.doAction(actionData{Type: "start", Title: "第21个", Seconds: 60})
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "最多同时运行 20 个倒计时") {
		t.Fatalf("limit result: %s", encoded)
	}
	if len(p.timers) != maxTimers {
		t.Fatalf("limit created %d", len(p.timers))
	}
}

func TestPersistenceAndRecovery(t *testing.T) {
	now := fixedNow()
	p, out, dir := testPlugin(t, now)
	p.startTimer("泡面", 180)
	p.startTimer("会议", 3600)
	if err := p.saveLocked(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !strings.Contains(string(raw), "泡面") {
		t.Fatalf("saved file: %s %v", raw, err)
	}

	p2 := newPlugin(&bytes.Buffer{}, dir, func() time.Time { return now.Add(60 * time.Second) }, nil)
	p2.sound = &fakeSound{}
	p2.window = &fakeWindow{showOK: true}
	p2.settings["floating_window"] = false
	p2.load()
	if len(p2.timers) != 2 {
		t.Fatalf("restored %d timers", len(p2.timers))
	}
	items, _ := p2.query("")
	if !strings.Contains(items[0]["subtitle"].(string), "02:00") {
		t.Fatalf("restored subtitle: %v", items[0]["subtitle"])
	}
	_ = out
}

func TestExpiredRecoveryNotifiesOnce(t *testing.T) {
	now := fixedNow()
	dir := t.TempDir()
	doc := storedFile{Version: 1, Timers: []storedTimer{
		{ID: "old", Title: "泡面", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), EndsAt: now.Add(-time.Minute).Format(time.RFC3339)},
	}}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, dataFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	p := newPlugin(out, dir, func() time.Time { return now }, nil)
	p.sound = &fakeSound{}
	p.window = &fakeWindow{showOK: true}
	p.settings["floating_window"] = false
	p.load()
	if !strings.Contains(out.String(), "泡面倒计时已结束。点我打开列表，按 Enter 确认。") {
		t.Fatalf("recovery toast: %s", out.String())
	}
	if len(p.timers) != 1 || p.timerListLocked()[0].Status != statusPending {
		t.Fatalf("v1 expired should become pending: %#v", p.timers)
	}
	out.Reset()
	p2 := newPlugin(out, dir, func() time.Time { return now }, nil)
	p2.sound = &fakeSound{}
	p2.window = &fakeWindow{showOK: true}
	p2.settings["floating_window"] = false
	p2.load()
	if strings.Contains(out.String(), "倒计时已结束。点我打开列表，按 Enter 确认。") {
		t.Fatalf("duplicate recovery toast: %s", out.String())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, dataFileName))
	if !strings.Contains(string(raw), `"version": 2`) || !strings.Contains(string(raw), `"status": "pending_ack"`) {
		t.Fatalf("migration did not write v2: %s", raw)
	}
}

func TestCorruptFileIsReportedAndNotOverwritten(t *testing.T) {
	now := fixedNow()
	dir := t.TempDir()
	path := filepath.Join(dir, dataFileName)
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	p := newPlugin(out, dir, func() time.Time { return now }, nil)
	p.sound = &fakeSound{}
	p.window = &fakeWindow{showOK: true}
	p.load()
	if !p.corrupt {
		t.Fatal("corrupt flag not set")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	items, _ := p.query("")
	if !strings.Contains(items[0]["title"].(string), "损坏") {
		t.Fatalf("corrupt query: %v", items)
	}
	if err := p.saveLocked(); err == nil {
		t.Fatal("save over corrupt file should fail")
	}
}

func TestSaveFailureDoesNotCreate(t *testing.T) {
	now := fixedNow()
	dir := t.TempDir()
	block := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(block, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	p := newPlugin(&bytes.Buffer{}, block, func() time.Time { return now }, nil)
	p.sound = &fakeSound{}
	p.window = &fakeWindow{showOK: true}
	result := p.startTimer("泡面", 60)
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "保存失败") {
		t.Fatalf("save failure result: %s", encoded)
	}
	if len(p.timers) != 0 {
		t.Fatalf("timer created after save failure")
	}
}

func TestFormatting(t *testing.T) {
	p := &Plugin{now: fixedNow}
	_ = p
	if got := formatRemaining(fixedNow().Add(155*time.Second), fixedNow()); got != "02:35" {
		t.Fatalf("short format: %s", got)
	}
	if got := formatRemaining(fixedNow().Add(3661*time.Second), fixedNow()); got != "1:01:01" {
		t.Fatalf("long format: %s", got)
	}
	if humanDuration(90) != "1 分钟 30 秒" {
		t.Fatalf("human duration: %s", humanDuration(90))
	}
}

// TestProtocolSmokeWithBuiltBinary is run by build.ps1 after the Windows build.
func TestProtocolSmokeWithBuiltBinary(t *testing.T) {
	exe := os.Getenv("COUNTDOWN_EXE")
	if exe == "" {
		t.Skip("COUNTDOWN_EXE is not set; build.ps1 runs this smoke test")
	}
	dataDir := t.TempDir()
	cmd := exec.Command(exe)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	readID := func(id int) map[string]any {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			type lineResult struct {
				line string
				err  error
			}
			ch := make(chan lineResult, 1)
			go func() {
				line, err := reader.ReadString('\n')
				ch <- lineResult{line, err}
			}()
			select {
			case <-deadline:
				t.Fatalf("timeout waiting for id %d; stderr=%s", id, stderr.String())
			case result := <-ch:
				if result.err != nil {
					t.Fatalf("read: %v; stderr=%s", result.err, stderr.String())
				}
				var message map[string]any
				if err := json.Unmarshal([]byte(result.line), &message); err != nil {
					continue
				}
				if value, ok := message["id"].(float64); ok && int(value) == id {
					return message
				}
			}
		}
	}
	write := func(value any) {
		data, _ := json.Marshal(value)
		_, _ = stdin.Write(append(data, '\n'))
	}

	write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "1", "dataDir": dataDir, "settings": map[string]any{},
	}})
	init := readID(1)
	result := init["result"].(map[string]any)
	if result["protocolVersion"] != "1" || result["name"] != appName {
		t.Fatalf("initialize: %v", init)
	}

	write(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "query", "params": map[string]any{"text": ""}})
	query := readID(2)
	queryResult := query["result"].(map[string]any)
	if _, ok := queryResult["items"].([]any); !ok {
		t.Fatalf("query items: %v", query)
	}

	write(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "query", "params": map[string]any{"text": "泡面 25m"}})
	preview := readID(3)
	items := preview["result"].(map[string]any)["items"].([]any)
	item := items[0].(map[string]any)
	write(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "action", "params": map[string]any{
		"item": item, "action": "start",
	}})
	action := readID(4)
	if _, ok := action["result"].(map[string]any)["effects"].([]any); !ok {
		t.Fatalf("action effects: %v", action)
	}

	write(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "shutdown"})
	shutdown := readID(5)
	if _, ok := shutdown["result"].(map[string]any); !ok {
		t.Fatalf("shutdown: %v", shutdown)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("process exit: %v; stderr=%s", err, stderr.String())
	}
}

type fakeTrayBackend struct {
	mu                   sync.Mutex
	enabled              bool
	addCalls             int
	modifyCalls          int
	deleteCalls          int
	taskbarReadds        int
	closeCalls           int
	lastItems            []trayTimerSnapshot
	setCalls             []bool
	updates              int
	iconValue            string
	iconValueInitialized bool
	iconRebuilds         int
	destroyedOwnedIcons  int
	iconHandle           uintptr
	iconOwned            bool
	nextIconHandle       uintptr
}

func (f *fakeTrayBackend) SetEnabled(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, enabled)
	if enabled && !f.enabled {
		f.addCalls++
	}
	if !enabled && f.enabled {
		f.deleteCalls++
	}
	f.enabled = enabled
}

func (f *fakeTrayBackend) Update(items []trayTimerSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastItems = append([]trayTimerSnapshot(nil), items...)
	f.updates++
	if f.enabled {
		f.modifyCalls++
		value := trayIconMinuteValue(items)
		if trayIconValueChanged(f.iconValue, f.iconValueInitialized, value) {
			nextHandle := f.nextIconHandle
			if nextHandle == 0 {
				nextHandle = 1
			}
			if shouldDestroyOwnedTrayIcon(f.iconHandle, f.iconOwned, nextHandle) {
				f.destroyedOwnedIcons++
			}
			f.iconHandle = nextHandle
			f.nextIconHandle = nextHandle + 1
			f.iconOwned = true
			f.iconValue = value
			f.iconValueInitialized = true
			f.iconRebuilds++
		}
	}
}

func (f *fakeTrayBackend) TaskbarCreated() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enabled {
		f.addCalls++
		f.taskbarReadds++
	}
}

func (f *fakeTrayBackend) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enabled {
		f.deleteCalls++
	}
	f.enabled = false
	f.closeCalls++
}

func (f *fakeTrayBackend) snapshot() (enabled bool, adds, modifies, deletes, readds, closes int, items []trayTimerSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled, f.addCalls, f.modifyCalls, f.deleteCalls, f.taskbarReadds, f.closeCalls, append([]trayTimerSnapshot(nil), f.lastItems...)
}

type fakeIslandBackend struct {
	mu        sync.Mutex
	enabled   bool
	visible   bool
	items     []trayTimerSnapshot
	pending   bool
	refreshes int
	hides     int
	sets      []bool
}

func (f *fakeIslandBackend) SetEnabled(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = enabled
	f.sets = append(f.sets, enabled)
	if !enabled {
		f.visible = false
	}
}
func (f *fakeIslandBackend) Refresh(items []trayTimerSnapshot, hasPending bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append([]trayTimerSnapshot(nil), items...)
	f.pending = hasPending
	f.refreshes++
	model := buildIslandViewModel(items, hasPending, f.enabled, false, fixedNow())
	f.visible = model.Visible
}
func (f *fakeIslandBackend) Hide() {
	f.mu.Lock()
	f.visible = false
	f.hides++
	f.mu.Unlock()
}
func (f *fakeIslandBackend) Visible() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.visible
}
func (f *fakeIslandBackend) snapshot() (bool, bool, int, []trayTimerSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.visible, f.pending, f.refreshes, append([]trayTimerSnapshot(nil), f.items...)
}

type fakeSound struct {
	mu           sync.Mutex
	files        []string
	defaultPlays int
	stops        int
	fileErr      error
	defaultErr   error
}

func (f *fakeSound) PlayFile(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = append(f.files, path)
	return f.fileErr
}

func (f *fakeSound) PlayDefault() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultPlays++
	return f.defaultErr
}

func (f *fakeSound) Stop() {
	f.mu.Lock()
	f.stops++
	f.mu.Unlock()
}

func (f *fakeSound) snapshot() (files []string, defaults, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.files...), f.defaultPlays, f.stops
}

func TestPresetRowsAndListComposition(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	items, rerun := p.query("")
	if rerun != 0 || len(items) != 9 {
		t.Fatalf("empty list = %d presets, rerun=%d", len(items), rerun)
	}
	want := []durationPreset{
		{Title: "5 分钟", Seconds: 300},
		{Title: "10 分钟", Seconds: 600},
		{Title: "15 分钟", Seconds: 900},
		{Title: "25 分钟", Seconds: 1500},
		{Title: "30 分钟", Seconds: 1800},
		{Title: "45 分钟", Seconds: 2700},
		{Title: "60 分钟", Seconds: 3600},
		{Title: "番茄专注", Seconds: 1500},
		{Title: "休息", Seconds: 300},
	}
	for index, preset := range want {
		row := items[index]
		if row["title"] != preset.Title || row["subtitle"] != humanDuration(preset.Seconds)+" · Enter 开始" {
			t.Fatalf("preset row %d = %#v", index, row)
		}
		data := row["data"].(map[string]any)
		if data["type"] != "start" || data["title"] != preset.Title || data["seconds"] != preset.Seconds {
			t.Fatalf("preset data %d = %#v", index, data)
		}
		actions := row["actions"].([]map[string]any)
		if len(actions) != 1 || actions[0]["id"] != "start" || actions[0]["key"] != "Enter" {
			t.Fatalf("preset actions %d = %#v", index, actions)
		}
	}

	p.startTimer("已有计时器", 120)
	items, rerun = p.query("")
	if rerun != 1 || len(items) != 10 || items[0]["title"] != "已有计时器" || items[1]["title"] != "5 分钟" {
		t.Fatalf("existing timer/preset order = %#v rerun=%d", items, rerun)
	}

	manual, manualRerun := p.query("手动任务 3")
	if manualRerun != 0 || len(manual) != 1 || manual[0]["title"] != "开始倒计时：手动任务" {
		t.Fatalf("manual input should not include presets: %#v rerun=%d", manual, manualRerun)
	}
}

func TestPresetStartRespectsRunningLimit(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	p.settings["sound_enabled"] = false
	for index := 0; index < maxTimers; index++ {
		p.startTimer(fmt.Sprintf("任务%d", index), 600)
	}
	items, _ := p.query("")
	preset := items[len(items)-9]
	data := preset["data"].(map[string]any)
	result := p.doActionNamed(actionData{
		Title: data["title"].(string), Seconds: data["seconds"].(int64),
	}, "start")
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), "最多同时运行 20 个倒计时") {
		t.Fatalf("preset action did not respect running limit: %s", encoded)
	}
}

func TestRunningSubtitleShowsLocalEndTimeOnly(t *testing.T) {
	now := fixedNow()
	running := &timer{ID: "running", Title: "运行中", EndsAt: now.Add(1500 * time.Second)}
	runningView := runningItemView(running, now)
	wantEnd := running.EndsAt.Format("15:04") + " 结束 · Enter 取消"
	if !strings.Contains(runningView["subtitle"].(string), wantEnd) {
		t.Fatalf("running subtitle = %q, want ending %q", runningView["subtitle"], wantEnd)
	}
	pausedView := pausedItemView(&timer{ID: "paused", Title: "暂停", Status: statusPaused, RemainingSeconds: 60})
	if strings.Contains(pausedView["subtitle"].(string), "结束 ·") || strings.Contains(pausedView["subtitle"].(string), now.Format("15:04")) {
		t.Fatalf("paused subtitle should not show an end time: %q", pausedView["subtitle"])
	}
	pendingView := pendingItemView(&timer{ID: "pending", Title: "待确认", Status: statusPending})
	if strings.Contains(pendingView["subtitle"].(string), now.Format("15:04")) {
		t.Fatalf("pending subtitle should not show an end time: %q", pendingView["subtitle"])
	}
}

func TestTrayIconMinuteValueRoundsAndUsesEarliestRunningTimer(t *testing.T) {
	base := fixedNow()
	tests := []struct {
		name  string
		items []trayTimerSnapshot
		want  string
	}{
		{name: "59 seconds rounds to one", items: []trayTimerSnapshot{{ID: "a", Status: statusRunning, RemainingSeconds: 59, EndsAt: base.Add(time.Second)}}, want: "1"},
		{name: "61 seconds rounds to two", items: []trayTimerSnapshot{{ID: "a", Status: statusRunning, RemainingSeconds: 61, EndsAt: base.Add(time.Second)}}, want: "2"},
		{name: "25 minutes", items: []trayTimerSnapshot{{ID: "a", Status: statusRunning, RemainingSeconds: 1500, EndsAt: base.Add(time.Second)}}, want: "25"},
		{name: "over 99 minutes", items: []trayTimerSnapshot{{ID: "a", Status: statusRunning, RemainingSeconds: 5941, EndsAt: base.Add(time.Second)}}, want: "99+"},
		{name: "paused only", items: []trayTimerSnapshot{{ID: "a", Status: statusPaused, RemainingSeconds: 59}}, want: ""},
		{name: "pending only", items: []trayTimerSnapshot{{ID: "a", Status: statusPending, RemainingSeconds: 59}}, want: ""},
		{name: "empty", want: ""},
		{
			name: "earliest ending timer wins",
			items: []trayTimerSnapshot{
				{ID: "later", Status: statusRunning, RemainingSeconds: 600, EndsAt: base.Add(10 * time.Minute)},
				{ID: "earlier", Status: statusRunning, RemainingSeconds: 61, EndsAt: base.Add(time.Minute)},
			},
			want: "2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := trayIconMinuteValue(tc.items); got != tc.want {
				t.Fatalf("tray icon value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTrayIconRebuildCacheAndOwnedIconCleanupRules(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	current := fixedNow()
	p.now = func() time.Time { return current }
	fake := p.tray.(*fakeTrayBackend)
	p.settings["tray_icon"] = true
	p.applyTraySettings()
	p.startTimer("托盘数字", 1500)

	fake.mu.Lock()
	initialRebuilds := fake.iconRebuilds
	fake.mu.Unlock()
	p.refreshTray()
	fake.mu.Lock()
	unchangedRebuilds := fake.iconRebuilds
	fake.mu.Unlock()
	if unchangedRebuilds != initialRebuilds {
		t.Fatalf("unchanged icon value rebuilt: before=%d after=%d", initialRebuilds, unchangedRebuilds)
	}

	current = current.Add(time.Minute)
	p.refreshTray()
	fake.mu.Lock()
	changedRebuilds := fake.iconRebuilds
	destroyedOwned := fake.destroyedOwnedIcons
	fake.iconOwned = false // Model an IDI_APPLICATION fallback as the old icon.
	fake.mu.Unlock()
	if changedRebuilds != initialRebuilds+1 || destroyedOwned == 0 {
		t.Fatalf("changed value did not rebuild/destroy owned icon: rebuilds=%d destroyed=%d", changedRebuilds, destroyedOwned)
	}
	current = current.Add(time.Minute)
	p.refreshTray()
	fake.mu.Lock()
	destroyedAfterFallback := fake.destroyedOwnedIcons
	fake.mu.Unlock()
	if destroyedAfterFallback != destroyedOwned {
		t.Fatalf("fallback icon must not be destroyed: before=%d after=%d", destroyedOwned, destroyedAfterFallback)
	}
	if !shouldDestroyOwnedTrayIcon(1, true, 2) || shouldDestroyOwnedTrayIcon(1, false, 2) || shouldDestroyOwnedTrayIcon(1, true, 1) {
		t.Fatal("owned/fallback icon destruction predicate is incorrect")
	}
}

func TestSchedulerWakeRefreshesTrayWhenNothingIsDue(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	fake := p.tray.(*fakeTrayBackend)
	p.settings["tray_icon"] = true
	p.applyTraySettings()
	p.startTimer("动态分钟", 180)
	fake.mu.Lock()
	initialUpdates := fake.updates
	initialValue := fake.iconValue
	fake.mu.Unlock()
	if initialValue != "3" {
		t.Fatalf("initial tray minute = %q, want 3", initialValue)
	}

	current = current.Add(31 * time.Second)
	p.handleSchedulerWake(current)
	fake.mu.Lock()
	firstUpdates := fake.updates
	firstValue := fake.iconValue
	fake.mu.Unlock()
	if firstUpdates <= initialUpdates || firstValue != "3" {
		t.Fatalf("scheduler wake did not refresh tray snapshot: updates %d->%d value=%q", initialUpdates, firstUpdates, firstValue)
	}

	current = current.Add(31 * time.Second)
	p.handleSchedulerWake(current)
	fake.mu.Lock()
	secondValue := fake.iconValue
	secondItems := append([]trayTimerSnapshot(nil), fake.lastItems...)
	fake.mu.Unlock()
	if secondValue != "2" || len(secondItems) != 1 || secondItems[0].RemainingSeconds != 118 {
		t.Fatalf("tray minute did not follow elapsed time: value=%q items=%#v", secondValue, secondItems)
	}
}

func TestPreReminderSchedulesAtOneMinuteBeforeEndAndToastsOnce(t *testing.T) {
	current := fixedNow()
	p, out, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = true
	p.settings["floating_window"] = true
	fakeSound := &fakeSound{}
	p.sound = fakeSound
	p.startTimer("番茄专注", 25*60)
	out.Reset()
	id := p.timerListLocked()[0].ID

	current = current.Add(25*time.Minute - time.Minute - time.Second)
	p.mu.Lock()
	delay, ok := p.nextWakeDelayLocked(current)
	p.mu.Unlock()
	if !ok || delay != time.Second {
		t.Fatalf("pre-reminder scheduler delay = %v, ok=%v; want 1s", delay, ok)
	}
	current = current.Add(time.Second)
	if !p.processDue(current) {
		t.Fatal("pre-reminder deadline was not processed")
	}
	if got := notificationCount(out.String()); got != 1 || !strings.Contains(out.String(), "还有 1 分钟到期：番茄专注") {
		t.Fatalf("pre-reminder toast = %d notifications: %s", got, out.String())
	}
	if p.timers[id].Status != statusRunning || !p.timers[id].PreReminded {
		t.Fatalf("pre-reminder changed timer state incorrectly: %+v", p.timers[id])
	}
	_, defaultPlays, _ := fakeSound.snapshot()
	if defaultPlays != 0 {
		t.Fatalf("pre-reminder must not play sound, got %d", defaultPlays)
	}
	visible, _ := p.window.(*fakeWindow).snapshot()
	if visible {
		t.Fatal("pre-reminder must not show the floating acknowledgement window")
	}
	out.Reset()
	p.processDue(current.Add(time.Second))
	if notificationCount(out.String()) != 0 {
		t.Fatalf("pre-reminder was sent more than once: %s", out.String())
	}
}

func TestPreReminderNeverFiresForSixtySecondsOrLess(t *testing.T) {
	current := fixedNow()
	p, out, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = false
	for _, seconds := range []int64{1, 60} {
		out.Reset()
		p.timers = map[string]*timer{}
		p.startTimer(fmt.Sprintf("%d 秒", seconds), seconds)
		id := p.timerListLocked()[0].ID
		p.processDue(current.Add(time.Duration(maxInt64(0, seconds-1)) * time.Second))
		if p.timers[id] == nil || p.timers[id].PreReminded {
			t.Fatalf("%d-second timer unexpectedly pre-reminded: %#v", seconds, p.timers[id])
		}
		if strings.Contains(out.String(), "还有 1 分钟到期") {
			t.Fatalf("%d-second timer emitted pre-reminder: %s", seconds, out.String())
		}
	}
}

func TestRestartResetsPreReminderButResumePreservesIt(t *testing.T) {
	current := fixedNow()
	p, out, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = false
	p.startTimer("重新运行", 1500)
	id := p.timerListLocked()[0].ID
	p.processDue(current.Add(24 * time.Minute))
	if !p.timers[id].PreReminded {
		t.Fatal("pre-reminder flag was not set")
	}
	p.restartTimer(id)
	if p.timers[id].PreReminded {
		t.Fatal("restart must reset PreReminded")
	}
	current = current.Add(24 * time.Minute)
	out.Reset()
	p.processDue(current)
	if !p.timers[id].PreReminded || !strings.Contains(out.String(), "还有 1 分钟到期：重新运行") {
		t.Fatalf("restarted timer did not emit a new pre-reminder: timer=%+v output=%s", p.timers[id], out.String())
	}
	p.pauseTimer(id)
	if !p.timers[id].PreReminded {
		t.Fatal("pause must preserve PreReminded")
	}
	current = current.Add(time.Minute)
	p.resumeTimer(id)
	if !p.timers[id].PreReminded {
		t.Fatal("resume must preserve PreReminded")
	}
}

func TestPausedTimerSkipsPreReminderAndResumeSchedulesItAgain(t *testing.T) {
	current := fixedNow()
	p, out, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = false
	p.startTimer("暂停测试", 25*60)
	id := p.timerListLocked()[0].ID

	current = current.Add(10 * time.Minute)
	p.pauseTimer(id)
	if delay, ok := p.nextWakeDelayLocked(current); ok {
		t.Fatalf("paused timer should not schedule a pre-reminder: delay=%v", delay)
	}
	out.Reset()
	p.processDue(current.Add(20 * time.Minute))
	if strings.Contains(out.String(), "还有 1 分钟到期") || p.timers[id].PreReminded {
		t.Fatalf("paused timer pre-reminded: output=%s timer=%+v", out.String(), p.timers[id])
	}

	current = current.Add(20 * time.Minute)
	p.resumeTimer(id)
	remaining := p.timers[id].EndsAt.Sub(current)
	if remaining <= time.Minute {
		t.Fatalf("test setup expected more than one minute remaining, got %v", remaining)
	}
	out.Reset()
	p.processDue(p.timers[id].EndsAt.Add(-time.Minute))
	if !p.timers[id].PreReminded || !strings.Contains(out.String(), "还有 1 分钟到期：暂停测试") {
		t.Fatalf("resumed timer did not receive pre-reminder: timer=%+v output=%s", p.timers[id], out.String())
	}
}

func TestPreReminderPersistsAcrossRestart(t *testing.T) {
	current := fixedNow()
	p, _, dir := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = false
	p.startTimer("持久化提醒", 1500)
	id := p.timerListLocked()[0].ID
	p.processDue(current.Add(24 * time.Minute))
	if !p.timers[id].PreReminded {
		t.Fatal("setup did not set pre-reminded flag")
	}

	out := &bytes.Buffer{}
	restored := newPlugin(out, dir, func() time.Time { return current.Add(24 * time.Minute) }, nil)
	restored.settings["floating_window"] = false
	restored.settings["sound_enabled"] = false
	restored.load()
	if restored.timers[id] == nil || !restored.timers[id].PreReminded {
		t.Fatalf("pre-reminded state not restored: %#v", restored.timers[id])
	}
	restored.processDue(current.Add(24*time.Minute + time.Second))
	if strings.Contains(out.String(), "还有 1 分钟到期") {
		t.Fatalf("restored timer emitted duplicate pre-reminder: %s", out.String())
	}
}

func TestV2RecordWithoutPreRemindedDefaultsFalse(t *testing.T) {
	current := fixedNow()
	dir := t.TempDir()
	doc := storedFile{Version: dataVersion, Timers: []storedTimer{
		{
			ID: "legacy-v2", Title: "旧版计时器",
			CreatedAt: current.Format(time.RFC3339Nano),
			EndsAt:    current.Add(120 * time.Second).Format(time.RFC3339Nano),
			Status:    statusRunning, OriginalSeconds: 120,
		},
	}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	p := newPlugin(&bytes.Buffer{}, dir, func() time.Time { return current }, nil)
	p.load()
	item := p.timers["legacy-v2"]
	if item == nil {
		t.Fatal("legacy v2 timer was not restored")
	}
	if item.PreReminded {
		t.Fatal("missing preReminded field must default to false")
	}
	p.mu.Lock()
	delay, ok := p.nextWakeDelayLocked(current)
	p.mu.Unlock()
	if !ok || delay != 30*time.Second {
		t.Fatalf("legacy v2 timer pre-reminder delay = %v, ok=%v; want 30s scheduler cap", delay, ok)
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func TestPauseResumeLifecycleAndScheduler(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = false
	p.startTimer("泡面", 5)
	id := p.timerListLocked()[0].ID

	current = current.Add(2 * time.Second)
	p.pauseTimer(id)
	item := p.timers[id]
	if item.Status != statusPaused || item.RemainingSeconds != 3 || !item.EndsAt.IsZero() {
		t.Fatalf("paused timer: %+v", item)
	}
	if delay, ok := p.nextWakeDelayLocked(current); ok {
		t.Fatalf("paused timer should not create scheduler deadline: %v", delay)
	}
	p.processDue(current.Add(10 * time.Second))
	if p.timers[id].Status != statusPaused {
		t.Fatalf("paused timer expired: %+v", p.timers[id])
	}

	current = current.Add(20 * time.Second)
	p.resumeTimer(id)
	item = p.timers[id]
	if item.Status != statusRunning || item.RemainingSeconds != 0 || !item.EndsAt.Equal(current.Add(3*time.Second)) {
		t.Fatalf("resumed timer: %+v", item)
	}
	p.processDue(current.Add(2 * time.Second))
	if p.timers[id].Status != statusRunning {
		t.Fatalf("timer expired before remaining duration: %+v", p.timers[id])
	}
	p.processDue(current.Add(3 * time.Second))
	if p.timers[id].Status != statusPending {
		t.Fatalf("timer did not expire after resume: %+v", p.timers[id])
	}
}

func TestPausedTimerPersistsAndRestores(t *testing.T) {
	current := fixedNow()
	p, _, dir := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = false
	p.startTimer("午休", 25)
	id := p.timerListLocked()[0].ID
	current = current.Add(7 * time.Second)
	p.pauseTimer(id)

	p2 := newPlugin(&bytes.Buffer{}, dir, func() time.Time { return current.Add(time.Hour) }, nil)
	p2.sound = &fakeSound{}
	p2.window = &fakeWindow{showOK: true}
	p2.settings["floating_window"] = false
	p2.load()
	item := p2.timers[id]
	if item == nil || item.Status != statusPaused || item.RemainingSeconds != 18 || item.OriginalSeconds != 25 {
		t.Fatalf("paused restore: %+v", item)
	}
	if delay, ok := p2.nextWakeDelayLocked(current.Add(time.Hour)); ok {
		t.Fatalf("restored paused timer created deadline: %v", delay)
	}
}

func TestPausedTimersDoNotConsumeRunningLimit(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.settings["sound_enabled"] = false
	ids := make([]string, 0, maxTimers)
	for index := 0; index < maxTimers; index++ {
		p.startTimer(fmt.Sprintf("任务%d", index), 600)
		for id, item := range p.timers {
			if item.Title == fmt.Sprintf("任务%d", index) {
				ids = append(ids, id)
				break
			}
		}
	}
	p.pauseTimer(ids[0])
	result := p.startTimer("替补", 600)
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "最多同时运行 20 个") {
		t.Fatalf("paused timer consumed a running slot: %s", encoded)
	}
	result = p.resumeTimer(ids[0])
	encoded, _ = json.Marshal(result)
	if !strings.Contains(string(encoded), "最多同时运行 20 个") {
		t.Fatalf("resume should enforce running limit: %s", encoded)
	}
}

func TestPausedListSortsByRemainingSeconds(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = false
	p.startTimer("剩余较多", 60)
	p.startTimer("剩余较少", 30)
	ids := make(map[string]string)
	for id, item := range p.timers {
		ids[item.Title] = id
	}
	current = current.Add(10 * time.Second)
	p.pauseTimer(ids["剩余较多"])
	current = current.Add(20 * time.Second)
	p.pauseTimer(ids["剩余较少"])

	items, _ := p.query("")
	if len(items) != 11 || items[0]["title"] != "剩余较少" || items[1]["title"] != "剩余较多" {
		t.Fatalf("paused list order and trailing presets: %#v", items)
	}
}

func TestRestartRunningAndPendingUsesOriginalDuration(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	p.settings["sound_enabled"] = false
	p.startTimer("运行中", 30)
	var runningID string
	for id, item := range p.timers {
		if item.Title == "运行中" {
			runningID = id
		}
	}
	current = current.Add(5 * time.Second)
	p.restartTimer(runningID)
	if got, want := p.timers[runningID].EndsAt, current.Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("running restart endsAt=%v want=%v", got, want)
	}
	if p.timers[runningID].ReminderCount != 0 || !p.timers[runningID].EndedAt.IsZero() || !p.timers[runningID].NextReminderAt.IsZero() {
		t.Fatalf("running restart did not clear reminder state: %+v", p.timers[runningID])
	}

	current = current.Add(31 * time.Second)
	p.processDue(current)
	if p.timers[runningID].Status != statusPending {
		t.Fatalf("expected pending after running timer expires: %+v", p.timers[runningID])
	}
	p.timers[runningID].ReminderCount = 4
	p.timers[runningID].NextReminderAt = current.Add(time.Minute)
	p.restartTimer(runningID)
	item := p.timers[runningID]
	if item.Status != statusRunning || !item.EndsAt.Equal(current.Add(30*time.Second)) || item.ReminderCount != 0 || !item.EndedAt.IsZero() || !item.NextReminderAt.IsZero() {
		t.Fatalf("pending restart: %+v", item)
	}
}

func TestReminderSoundPlaysAndStops(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	fake := &fakeSound{}
	p.sound = fake
	p.settings["sound_enabled"] = true
	p.settings["sound_file"] = ""
	p.startTimer("泡面", 1)
	id := p.timerListLocked()[0].ID

	p.processDue(current.Add(time.Second))
	_, defaults, _ := fake.snapshot()
	if defaults != 1 {
		t.Fatalf("initial expiry sound count=%d want=1", defaults)
	}
	p.processDue(current.Add(16 * time.Second))
	_, defaults, _ = fake.snapshot()
	if defaults != 2 {
		t.Fatalf("repeat sound count=%d want=2", defaults)
	}
	if !p.confirmPending(id) {
		t.Fatal("confirm pending failed")
	}
	_, _, stops := fake.snapshot()
	if stops != 1 {
		t.Fatalf("confirm stop count=%d want=1", stops)
	}
	p.processDue(current.Add(10 * time.Minute))
	_, defaultsAfter, _ := fake.snapshot()
	if defaultsAfter != 2 {
		t.Fatalf("sound played after confirm: %d", defaultsAfter)
	}
}

func TestReminderSoundCustomFileAndFallback(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	fake := &fakeSound{}
	p.sound = fake
	p.settings["sound_enabled"] = true
	wav := filepath.Join(t.TempDir(), "custom.wav")
	writeTestWAV(t, wav)
	p.settings["sound_file"] = wav
	p.mu.Lock()
	p.playReminderSoundLocked()
	p.mu.Unlock()
	files, defaults, _ := fake.snapshot()
	if len(files) != 1 || files[0] != wav || defaults != 0 {
		t.Fatalf("custom WAV playback: files=%v defaults=%d", files, defaults)
	}

	p.settings["sound_file"] = filepath.Join(t.TempDir(), "missing.wav")
	p.mu.Lock()
	p.playReminderSoundLocked()
	p.mu.Unlock()
	_, defaults, _ = fake.snapshot()
	if defaults != 1 {
		t.Fatalf("missing WAV should fall back to default, defaults=%d", defaults)
	}
	p.settings["sound_file"] = wav + ".mp3"
	p.mu.Lock()
	p.playReminderSoundLocked()
	p.mu.Unlock()
	_, defaults, _ = fake.snapshot()
	if defaults != 2 {
		t.Fatalf("non-WAV should fall back to default, defaults=%d", defaults)
	}

	directoryWAV := filepath.Join(t.TempDir(), "directory.wav")
	if err := os.Mkdir(directoryWAV, 0700); err != nil {
		t.Fatal(err)
	}
	p.settings["sound_file"] = directoryWAV
	p.mu.Lock()
	p.playReminderSoundLocked()
	p.mu.Unlock()
	_, defaults, _ = fake.snapshot()
	if defaults != 3 {
		t.Fatalf("directory path should fall back to default, defaults=%d", defaults)
	}

	fake.fileErr = os.ErrInvalid
	p.settings["sound_file"] = wav
	p.mu.Lock()
	p.playReminderSoundLocked()
	p.mu.Unlock()
	_, defaults, _ = fake.snapshot()
	if defaults != 4 {
		t.Fatalf("failed WAV playback should fall back to default, defaults=%d", defaults)
	}
}

func TestSoundDisabledAndSnoozeStops(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	fake := &fakeSound{}
	p.sound = fake
	p.settings["sound_enabled"] = false
	p.startTimer("关闭声音", 1)
	id := p.timerListLocked()[0].ID
	p.processDue(current.Add(time.Second))
	_, defaults, _ := fake.snapshot()
	if defaults != 0 {
		t.Fatalf("disabled sound played %d times", defaults)
	}

	p.settings["sound_enabled"] = true
	p.processDue(current.Add(time.Minute))
	_, defaults, _ = fake.snapshot()
	if defaults != 1 {
		t.Fatalf("enabled repeat sound count=%d want=1", defaults)
	}
	p.snoozeIDs([]string{id})
	_, _, stops := fake.snapshot()
	if stops != 1 {
		t.Fatalf("snooze stop count=%d want=1", stops)
	}
}

func TestSettingsDisableStopsReminderSound(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	fake := &fakeSound{}
	p.sound = fake
	p.settings["sound_enabled"] = true
	p.startTimer("设置关闭声音", 1)
	p.processDue(current.Add(time.Second))
	_, defaults, _ := fake.snapshot()
	if defaults != 1 {
		t.Fatalf("initial sound count=%d want=1", defaults)
	}
	params, err := json.Marshal(map[string]any{"settings": map[string]any{"sound_enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "settingsChanged", "params": json.RawMessage(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.handleLine(line)
	_, _, stops := fake.snapshot()
	if stops != 1 {
		t.Fatalf("disabling sound should stop current playback, stops=%d", stops)
	}
}

func TestCenterPositionInWorkAreas(t *testing.T) {
	cases := []struct {
		name                     string
		left, top, right, bottom int
		width, height            int
		wantX, wantY             int
	}{
		{"primary", 0, 0, 1920, 1080, 340, 220, 790, 430},
		{"secondary negative origin", -1600, 0, 0, 900, 400, 300, -1000, 300},
		{"window larger than work area", 100, 50, 500, 300, 600, 400, 100, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, y := centerPositionInArea(tc.left, tc.top, tc.right, tc.bottom, tc.width, tc.height)
			if x != tc.wantX || y != tc.wantY {
				t.Fatalf("center=(%d,%d), want=(%d,%d)", x, y, tc.wantX, tc.wantY)
			}
			if tc.width <= tc.right-tc.left && (x < tc.left || x+tc.width > tc.right) {
				t.Fatalf("horizontal bounds exceeded: x=%d width=%d area=[%d,%d]", x, tc.width, tc.left, tc.right)
			}
			if tc.height <= tc.bottom-tc.top && (y < tc.top || y+tc.height > tc.bottom) {
				t.Fatalf("vertical bounds exceeded: y=%d height=%d area=[%d,%d]", y, tc.height, tc.top, tc.bottom)
			}
		})
	}
}

func TestPopupCenterSettingAppliedToWindowBackend(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	fake := p.window.(*fakeWindow)
	p.settings["popup_center"] = true
	p.applyWindowSettings()
	fake.mu.Lock()
	centered := fake.popupCenter
	fake.mu.Unlock()
	if !centered {
		t.Fatal("popup_center setting was not applied")
	}
	p.settings["popup_center"] = false
	p.applyWindowSettings()
	fake.mu.Lock()
	centered = fake.popupCenter
	fake.mu.Unlock()
	if centered {
		t.Fatal("popup_center false was not applied")
	}
}

func TestV2RecordsWithoutOriginalSecondsAreBackfilled(t *testing.T) {
	now := fixedNow()
	dir := t.TempDir()
	doc := storedFile{Version: 2, Timers: []storedTimer{
		{ID: "running", Title: "运行中", CreatedAt: now.Format(time.RFC3339Nano), EndsAt: now.Add(25 * time.Second).Format(time.RFC3339Nano), Status: statusRunning},
		{ID: "pending", Title: "待确认", CreatedAt: now.Add(-60 * time.Second).Format(time.RFC3339Nano), EndsAt: now.Add(-35 * time.Second).Format(time.RFC3339Nano), Status: statusPending, EndedAt: now.Add(-35 * time.Second).Format(time.RFC3339Nano), NextReminderAt: now.Add(time.Minute).Format(time.RFC3339Nano)},
	}}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, dataFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	p := newPlugin(&bytes.Buffer{}, dir, func() time.Time { return now }, nil)
	p.sound = &fakeSound{}
	p.settings["floating_window"] = false
	p.load()
	if p.timers["running"].OriginalSeconds != 25 || p.timers["pending"].OriginalSeconds != 25 {
		t.Fatalf("original duration backfill: running=%+v pending=%+v", p.timers["running"], p.timers["pending"])
	}
	raw, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"originalSeconds": 25`) {
		t.Fatalf("backfilled originalSeconds not persisted: %s", raw)
	}
}

func TestListActionsExposePauseResumeRestart(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.settings["sound_enabled"] = false
	p.startTimer("运行中", 60)
	id := p.timerListLocked()[0].ID
	items, _ := p.query("")
	actions := items[0]["actions"].([]map[string]any)
	if len(actions) != 3 || actions[0]["id"] != "cancel" || actions[1]["id"] != "pause" || actions[2]["id"] != "restart" {
		t.Fatalf("running actions: %#v", actions)
	}
	p.pauseTimer(id)
	items, _ = p.query("")
	actions = items[0]["actions"].([]map[string]any)
	if len(actions) != 2 || actions[0]["id"] != "resume" || actions[1]["id"] != "cancel" {
		t.Fatalf("paused actions: %#v", actions)
	}
	p.resumeTimer(id)
	p.processDue(current.Add(61 * time.Second))
	items, _ = p.query("")
	actions = items[0]["actions"].([]map[string]any)
	if len(actions) != 3 || actions[0]["id"] != "acknowledge" || actions[1]["id"] != "snooze" || actions[2]["id"] != "restart" {
		t.Fatalf("pending actions: %#v", actions)
	}
}

func TestAcknowledgeActionStopsCurrentSound(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	fake := &fakeSound{}
	p.sound = fake
	p.settings["sound_enabled"] = true
	p.startTimer("列表确认", 1)
	id := p.timerListLocked()[0].ID
	p.processDue(current.Add(time.Second))
	p.acknowledgeTimer(id)
	_, _, stops := fake.snapshot()
	if stops != 1 {
		t.Fatalf("list acknowledge stop count=%d want=1", stops)
	}
}

func TestRestartPendingStopsCurrentSound(t *testing.T) {
	current := fixedNow()
	p, _, _ := testPlugin(t, current)
	p.now = func() time.Time { return current }
	fake := &fakeSound{}
	p.sound = fake
	p.settings["sound_enabled"] = true
	p.startTimer("重新开始", 2)
	id := p.timerListLocked()[0].ID
	p.processDue(current.Add(2 * time.Second))
	p.restartTimer(id)
	_, _, stops := fake.snapshot()
	if stops != 1 {
		t.Fatalf("pending restart stop count=%d want=1", stops)
	}
	if p.timers[id].Status != statusRunning {
		t.Fatalf("pending restart state=%q want running", p.timers[id].Status)
	}
}

func TestIslandVisibilityRules(t *testing.T) {
	now := fixedNow()
	running := []trayTimerSnapshot{{ID: "r", Status: statusRunning, EndsAt: now.Add(5 * time.Minute)}}
	paused := []trayTimerSnapshot{{ID: "p", Status: statusPaused, RemainingSeconds: 90}}
	pending := []trayTimerSnapshot{{ID: "d", Status: statusPending}}
	if got := buildIslandViewModel(running, false, true, false, now); !got.Visible {
		t.Fatal("island should be visible with a running timer")
	}
	if got := buildIslandViewModel(paused, false, true, false, now); got.Visible {
		t.Fatal("island should hide when only paused timers exist")
	}
	if got := buildIslandViewModel(nil, false, true, false, now); got.Visible {
		t.Fatal("island should hide when no timers exist")
	}
	if got := buildIslandViewModel(pending, true, true, false, now); !got.Visible || got.CollapsedText != "已到期" || !got.HasPending {
		t.Fatalf("pending-only model = %#v", got)
	}
	if got := buildIslandViewModel(running, false, false, false, now); got.Visible {
		t.Fatal("island should remain hidden when island mode is disabled")
	}
}

func TestIslandCollapsedContentUsesEarliestRunningAndFixedWidth(t *testing.T) {
	now := fixedNow()
	items := []trayTimerSnapshot{
		{ID: "later", Status: statusRunning, EndsAt: now.Add(9 * time.Minute)},
		{ID: "earlier", Status: statusRunning, EndsAt: now.Add(2*time.Minute + 10*time.Second)},
	}
	first := buildIslandViewModel(items, false, true, false, now)
	second := buildIslandViewModel(items, false, true, false, now.Add(time.Second))
	if first.CollapsedText != formatRemaining(items[1].EndsAt, now) {
		t.Fatalf("collapsed text = %q, want earliest timer %q", first.CollapsedText, formatRemaining(items[1].EndsAt, now))
	}
	if first.Width != 200 || second.Width != first.Width {
		t.Fatalf("collapsed width changed: first=%d second=%d", first.Width, second.Width)
	}
}

func TestIslandHoverLeaveGrace(t *testing.T) {
	now := fixedNow()
	var state islandHoverState
	state.Hover()
	if !state.expanded {
		t.Fatal("hover should expand")
	}
	state.Leave(now)
	if state.Tick(now.Add(299*time.Millisecond), false) || !state.expanded {
		t.Fatal("island collapsed before the 300ms leave grace elapsed")
	}
	if !state.Tick(now.Add(300*time.Millisecond), false) || state.expanded {
		t.Fatal("island did not collapse at the 300ms deadline")
	}
	state.Hover()
	state.Leave(now)
	if state.Tick(now.Add(300*time.Millisecond), true) || !state.expanded {
		t.Fatal("pointer inside expanded panel should prevent collapse")
	}
}

func TestIslandRowActionMapping(t *testing.T) {
	cases := []struct{ status, first, second string }{
		{statusRunning, "pause", "cancel"},
		{statusPaused, "resume", "cancel"},
		{statusPending, "acknowledge", "snooze"},
	}
	for _, test := range cases {
		first, second := islandRowActions(test.status)
		if first != test.first || second != test.second {
			t.Errorf("%s actions = %q/%q, want %q/%q", test.status, first, second, test.first, test.second)
		}
	}
	if islandAckAllEnabled(false) || !islandAckAllEnabled(true) {
		t.Fatal("ack-all enabled state must follow hasPending")
	}
}

func TestIslandModeProcessDueRefreshesIslandWithoutFloatingWindow(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.settings["island_mode"] = true
	p.applyIslandSettings()
	p.settings["sound_enabled"] = true
	window := p.window.(*fakeWindow)
	island := p.island.(*fakeIslandBackend)
	p.timers["expired"] = &timer{
		ID: "expired", Title: "泡面", CreatedAt: now.Add(-time.Minute),
		EndsAt: now.Add(-time.Second), Status: statusRunning, OriginalSeconds: 60,
	}
	if !p.processDue(now) {
		t.Fatal("processDue should process the expired timer")
	}
	visible, pending, refreshes, _ := island.snapshot()
	if !visible || !pending || refreshes == 0 {
		t.Fatalf("island state visible=%v pending=%v refreshes=%d", visible, pending, refreshes)
	}
	window.mu.Lock()
	shows := window.shows
	window.mu.Unlock()
	if shows != 0 {
		t.Fatalf("floating window Show called %d times in island mode", shows)
	}
	_, defaults, _ := p.sound.(*fakeSound).snapshot()
	if defaults != 1 {
		t.Fatalf("expiry sound plays=%d, want 1", defaults)
	}
}

func TestIslandModeSettingTogglesBackendAndHidesFloatingWindow(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	island := p.island.(*fakeIslandBackend)
	window := p.window.(*fakeWindow)
	p.applyIslandSettings()
	if p.islandModeLocked() {
		t.Fatal("island_mode must default to false")
	}
	p.settings["island_mode"] = true
	p.applyIslandSettings()
	if !island.enabled {
		t.Fatal("island backend was not enabled")
	}
	window.mu.Lock()
	hides := window.hides
	window.mu.Unlock()
	if hides == 0 {
		t.Fatal("enabling island mode should hide the old floating window")
	}
	p.settings["island_mode"] = false
	p.applyIslandSettings()
	if island.enabled {
		t.Fatal("island backend was not disabled")
	}
}

func TestIslandRefreshOnSchedulerWake(t *testing.T) {
	now := fixedNow()
	current := now
	p, _, _ := testPlugin(t, now)
	p.now = func() time.Time { return current }
	p.settings["island_mode"] = true
	p.island.SetEnabled(true)
	p.timers["running"] = &timer{ID: "running", Title: "计时", Status: statusRunning, CreatedAt: now, EndsAt: now.Add(10 * time.Minute), OriginalSeconds: 600}
	island := p.island.(*fakeIslandBackend)
	_, _, before, _ := island.snapshot()
	p.handleSchedulerWake(current)
	_, _, after, items := island.snapshot()
	if after <= before {
		t.Fatal("scheduler wake did not refresh island")
	}
	if len(items) != 1 || items[0].RemainingSeconds != 600 {
		t.Fatalf("scheduler snapshot = %#v", items)
	}
	current = current.Add(time.Minute)
	p.handleSchedulerWake(current)
	_, _, _, items = island.snapshot()
	if len(items) != 1 || items[0].RemainingSeconds != 540 {
		t.Fatalf("remaining time did not update after scheduler wake: %#v", items)
	}
}

func TestDispatchIslandActionUsesExistingActionNames(t *testing.T) {
	var got string
	callbacks := islandCallbacks{
		Pause:   func(string) { got = "pause" },
		Resume:  func(string) { got = "resume" },
		Restart: func(string) { got = "restart" },
		Cancel:  func(string) { got = "cancel" },
		Ack:     func(string) { got = "acknowledge" },
		Snooze:  func(string) { got = "snooze" },
	}
	for _, action := range []string{"pause", "resume", "restart", "cancel", "acknowledge", "snooze"} {
		got = ""
		dispatchIslandAction(callbacks, "timer-id", action)
		if got != action {
			t.Errorf("dispatch %q called %q", action, got)
		}
	}
	got = ""
	dispatchIslandAction(callbacks, "timer-id", "unknown")
	if got != "" {
		t.Fatalf("unknown action unexpectedly dispatched %q", got)
	}
}

func TestIslandModeSuppressesFloatingWindow(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	p.settings["island_mode"] = true
	p.timers["pending"] = &timer{ID: "pending", Title: "到期任务", Status: statusPending, EndedAt: fixedNow()}
	p.syncWindow(true)
	window := p.window.(*fakeWindow)
	window.mu.Lock()
	shows, hides := window.shows, window.hides
	window.mu.Unlock()
	if shows != 0 || hides == 0 {
		t.Fatalf("island mode floating window shows=%d hides=%d; want no show and at least one hide", shows, hides)
	}
}

func TestIslandFailureFallsBackToToast(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.settings["island_mode"] = true
	p.islandBroken = true
	p.timers["expired"] = &timer{ID: "expired", Title: "失败兜底", CreatedAt: now.Add(-time.Minute), EndsAt: now.Add(-time.Second), Status: statusRunning, OriginalSeconds: 60}
	if !p.processDue(now) {
		t.Fatal("processDue should process expired timer")
	}
	if got := notificationCount(out.String()); got != 1 {
		t.Fatalf("island failure toast count=%d want=1; output=%s", got, out.String())
	}
}

func TestDisabledFloatingAndIslandModesUseToast(t *testing.T) {
	now := fixedNow()
	p, out, _ := testPlugin(t, now)
	p.settings["floating_window"] = false
	p.settings["island_mode"] = false
	p.timers["expired"] = &timer{ID: "expired", Title: "普通通知", CreatedAt: now.Add(-time.Minute), EndsAt: now.Add(-time.Second), Status: statusRunning, OriginalSeconds: 60}
	if !p.processDue(now) {
		t.Fatal("processDue should process expired timer")
	}
	if got := notificationCount(out.String()); got != 1 {
		t.Fatalf("fallback toast count=%d want=1; output=%s", got, out.String())
	}
}

func TestSettingsChangedEnablesAndDisablesIsland(t *testing.T) {
	p, _, _ := testPlugin(t, fixedNow())
	island := p.island.(*fakeIslandBackend)
	p.handleLine([]byte(`{"jsonrpc":"2.0","method":"settingsChanged","params":{"settings":{"island_mode":true,"floating_window":true}}}`))
	if !island.enabled {
		t.Fatal("settingsChanged did not enable island backend")
	}
	p.handleLine([]byte(`{"jsonrpc":"2.0","method":"settingsChanged","params":{"settings":{"island_mode":false,"floating_window":false}}}`))
	if island.enabled {
		t.Fatal("settingsChanged did not disable island backend")
	}
	if len(island.sets) < 2 || !island.sets[len(island.sets)-2] || island.sets[len(island.sets)-1] {
		t.Fatalf("island SetEnabled calls = %v", island.sets)
	}
}

func TestIslandOpenListUsesExistingToastQueryBridge(t *testing.T) {
	p, out, _ := testPlugin(t, fixedNow())
	p.openListFromTray()
	raw := out.String()
	if !strings.Contains(raw, `"type":"setQuery"`) || !strings.Contains(raw, `"text":"倒计时 "`) {
		t.Fatalf("open list bridge missing setQuery payload: %s", raw)
	}
}
