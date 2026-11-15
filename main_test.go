package main

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	mu        sync.Mutex
	visible   bool
	items     []pendingItem
	shows     int
	refreshes int
	hides     int
	showOK    bool
	autoClose int
}

func (f *fakeWindow) SetAutoClose(seconds int) {
	f.mu.Lock()
	f.autoClose = seconds
	f.mu.Unlock()
}

func (f *fakeWindow) SetAnimations(bool) {}

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
	p.window = &fakeWindow{showOK: true}
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
	if rerun != 1 || len(items) != 2 {
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
	if len(items) != 1 || !strings.Contains(items[0]["subtitle"].(string), "等待确认") {
		t.Fatalf("pending list: %#v", items)
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
	if rerun != 1 || len(items) != 2 {
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
	a.begin(animEnter, now, 180*time.Millisecond, 0, 1)
	if got, done := a.progress(now); got != 0 || done {
		t.Fatalf("start = %v %v", got, done)
	}
	if got, done := a.progress(now.Add(90 * time.Millisecond)); got <= 0 || got >= 1 || done {
		t.Fatalf("mid = %v %v", got, done)
	}
	if got, done := a.progress(now.Add(180 * time.Millisecond)); got != 1 || !done {
		t.Fatalf("end = %v %v", got, done)
	}
	a.begin(animExit, now, 120*time.Millisecond, 1, 0)
	if got, done := a.progress(now.Add(60 * time.Millisecond)); got <= 0 || got >= 1 || done {
		t.Fatalf("exit mid = %v %v", got, done)
	}
	a.begin(animEnter, now, 0, 0, 1)
	if got, done := a.progress(now); got != 1 || !done {
		t.Fatalf("zero duration = %v %v", got, done)
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
	if len(items) != 1 || !strings.Contains(items[0]["subtitle"].(string), "剩余 05:00") {
		t.Fatalf("snoozed list: %#v", items)
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

func TestPendingActionsIncludeSnooze(t *testing.T) {
	now := fixedNow()
	p, _, _ := testPlugin(t, now)
	p.startTimer("泡面", 60)
	p.processDue(now.Add(60 * time.Second))
	items, _ := p.query("")
	actions := items[0]["actions"].([]map[string]any)
	if len(actions) != 2 || actions[0]["id"] != "acknowledge" || actions[1]["id"] != "snooze" {
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
