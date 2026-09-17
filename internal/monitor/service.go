package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"liveguard/internal/domain"
)

const (
	InvestigationWaiting    = "waiting_more_evidence"
	InvestigationConfirmed  = "confirmed"
	InvestigationSuppressed = "suppressed"
	InvestigationNeedsHuman = "needs_human"
	maxActiveMonitors       = 8
)

type StartInput struct {
	TargetURL string `json:"target_url"`
	Goal      string `json:"goal"`
}

type Service struct {
	repository Repository
	onUpdate   func()
	replayGap  time.Duration

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewService(repository Repository, onUpdate func()) *Service {
	if onUpdate == nil {
		onUpdate = func() {}
	}
	return &Service{repository: repository, onUpdate: onUpdate, replayGap: 700 * time.Millisecond, cancels: map[string]context.CancelFunc{}}
}

// SetReplayGap makes the deterministic signal source fast in tests. The local
// demo spaces events out so the control room visibly changes over time.
func (s *Service) SetReplayGap(gap time.Duration) { s.replayGap = gap }

func (s *Service) Start(ctx context.Context, input StartInput) (*Run, error) {
	input.TargetURL = strings.TrimSpace(input.TargetURL)
	input.Goal = strings.TrimSpace(input.Goal)
	parsed, err := url.Parse(input.TargetURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("target URL must be a valid http/https address")
	}
	if utf8.RuneCountInString(input.Goal) < 2 || utf8.RuneCountInString(input.Goal) > 240 {
		return nil, errors.New("monitoring goal must contain 2-240 characters")
	}

	s.mu.Lock()
	if len(s.cancels) >= maxActiveMonitors {
		s.mu.Unlock()
		return nil, ErrCapacity
	}
	now := time.Now().UTC()
	roomID := domain.NewID("room")
	roomName := roomNameFromURL(parsed)
	run := &Run{
		ID: domain.NewID("monitor"), Name: roomName, TargetURL: input.TargetURL, Goal: input.Goal,
		SourceMode: "demo_replay", Status: RunRunning, StartedAt: now, UpdatedAt: now, Version: 1,
		Rooms: []Room{{ID: roomID, Name: roomName, Status: "listening"}},
	}
	if err := s.repository.Create(ctx, run); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancels[run.ID] = cancel
	s.mu.Unlock()
	s.onUpdate()
	go s.replay(runCtx, run.ID, demoSignals(run))
	return run, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Run, error) {
	return s.repository.Get(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]Summary, error) {
	runs, err := s.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(runs))
	for _, run := range runs {
		summaries = append(summaries, run.Summary())
	}
	return summaries, nil
}

func (s *Service) Stop(ctx context.Context, id string) (*Run, error) {
	s.mu.Lock()
	cancel := s.cancels[id]
	delete(s.cancels, id)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	completedAt := time.Now().UTC()
	run, err := s.repository.Update(ctx, id, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is already stopped")
		}
		run.Status = RunStopped
		run.CompletedAt = &completedAt
		for i := range run.Rooms {
			run.Rooms[i].Status = "stopped"
		}
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
	return run, err
}

func (s *Service) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cancel := range s.cancels {
		cancel()
		delete(s.cancels, id)
	}
}

func (s *Service) RecoverInterrupted(ctx context.Context) error {
	runs, err := s.repository.List(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status != RunRunning {
			continue
		}
		completedAt := time.Now().UTC()
		if _, err := s.repository.Update(ctx, run.ID, func(current *Run) error {
			current.Status = RunStopped
			current.CompletedAt = &completedAt
			for i := range current.Rooms {
				current.Rooms[i].Status = "stopped"
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) replay(ctx context.Context, runID string, events []SignalEvent) {
	defer func() {
		s.mu.Lock()
		delete(s.cancels, runID)
		s.mu.Unlock()
	}()
	for _, event := range events {
		if !wait(ctx, s.replayGap) {
			return
		}
		if err := s.Ingest(ctx, runID, event); err != nil {
			if !errors.Is(err, context.Canceled) {
				s.fail(runID)
			}
			return
		}
	}
	// A personal monitor stays active after the sample burst. A real SignalSource
	// will continue feeding events through the same Ingest boundary.
	<-ctx.Done()
}

func (s *Service) fail(runID string) {
	completedAt := time.Now().UTC()
	_, _ = s.repository.Update(context.Background(), runID, func(run *Run) error {
		run.Status = RunFailed
		run.CompletedAt = &completedAt
		return nil
	})
	s.onUpdate()
}

func wait(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) Ingest(ctx context.Context, runID string, event SignalEvent) error {
	if event.ID == "" {
		event.ID = domain.NewID("signal")
	}
	if event.ObservedAt.IsZero() {
		event.ObservedAt = time.Now().UTC()
	}
	_, err := s.repository.Update(ctx, runID, func(run *Run) error {
		if run.Status != RunRunning {
			return errors.New("monitor is not running")
		}
		run.Events = append(run.Events, event)
		for i := range run.Rooms {
			if run.Rooms[i].ID == event.RoomID {
				run.Rooms[i].LastSeen = event.ObservedAt
			}
		}
		eventType := classify(event.Text, run.Goal)
		if eventType != "" {
			investigate(run, event, eventType)
		}
		return nil
	})
	if err == nil {
		s.onUpdate()
	}
	return err
}

func investigate(run *Run, event SignalEvent, eventType string) {
	now := event.ObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	index := waitingInvestigation(run, event.RoomID, eventType)
	if index < 0 {
		run.Investigations = append(run.Investigations, Investigation{
			ID: domain.NewID("investigation"), RoomID: event.RoomID, RoomName: event.RoomName,
			EventType: eventType, Status: InvestigationWaiting, CreatedAt: now, UpdatedAt: now,
		})
		index = len(run.Investigations) - 1
	}
	inv := &run.Investigations[index]
	inv.Evidence = append(inv.Evidence, Evidence{EventID: event.ID, Source: event.Source, Text: event.Text, Confidence: event.Confidence, ObservedAt: event.ObservedAt})
	inv.UpdatedAt = now
	addStep(inv, "read_live_window", "读取这个直播间最近 30 秒的字幕和画面文字", fmt.Sprintf("拿到 %d 条相关证据", len(evidenceWindow(run, event.RoomID, 8))), now)

	if eventType == "human_verification" {
		addStep(inv, "request_human_takeover", "机器无法代替用户完成安全验证", "已暂停这个监控窗口，等待用户处理", now)
		inv.Status = InvestigationNeedsHuman
		inv.Conclusion = "直播页面要求登录或安全验证"
		setRoomStatus(run, event.RoomID, "needs_human")
		return
	}

	if containsAny(event.Text, []string{"没有", "不会", "不安排", "取消", "别等", "暂时不"}) {
		inv.Status = InvestigationSuppressed
		inv.Conclusion = "检测到否定语义，没有打扰用户"
		return
	}
	if hasRecentAlert(run, event.RoomID, eventType) {
		addStep(inv, "query_recent_alerts", "检查最近是否已经提醒过同一件事", "发现已有同类提醒", now)
		inv.Status = InvestigationSuppressed
		inv.Conclusion = "同一事件已经提醒，本次重复内容已去重"
		return
	}

	profile := profileForGoal(run.Goal)
	strong := containsAny(event.Text, profile.strongTerms)
	crossSource := hasCrossSource(inv.Evidence)
	if !strong && !crossSource {
		inv.Status = InvestigationWaiting
		inv.Conclusion = "信息还不够明确，继续监听"
		return
	}

	addStep(inv, "query_recent_alerts", "检查最近是否已经提醒过同一件事", "已查询这个监控窗口的提醒记录", now)

	confidence := combinedConfidence(inv.Evidence)
	if confidence < 0.88 || crossSource {
		addStep(inv, "reanalyze_clip", "对低置信度或跨模态证据做一次复核", "字幕和画面文字表达一致，置信度已校准", now)
		confidence += 0.08
		if confidence > 0.98 {
			confidence = 0.98
		}
	}
	addStep(inv, "create_alert", "把确认的重要时刻提醒给用户", "已生成一条带原文证据的个人提醒", now)
	inv.Status = InvestigationConfirmed
	inv.Conclusion = profile.conclusion
	run.Alerts = append(run.Alerts, Alert{
		ID: domain.NewID("alert"), RoomID: event.RoomID, RoomName: event.RoomName,
		EventType: eventType, Priority: profile.priority, Summary: profile.conclusion,
		Confidence: confidence, DedupeKey: event.RoomID + ":" + eventType,
		Investigation: inv.ID, Evidence: append([]Evidence(nil), inv.Evidence...), CreatedAt: now,
	})
}

type goalProfile struct {
	kind         string
	keywords     []string
	strongTerms  []string
	negative     string
	vague        string
	confirmation string
	duplicate    string
	conclusion   string
	priority     string
}

func profileForGoal(goal string) goalProfile {
	switch {
	case containsAny(goal, []string{"优惠券", "领券", "红包", "降价", "价格", "满减", "折扣"}):
		return goalProfile{kind: "coupon_drop", keywords: []string{"优惠券", "领券", "红包", "福利", "折扣"}, strongTerms: []string{"限量优惠券", "点击领取", "现在上", "马上发"}, negative: "今天没有优惠券，大家不用等", vague: "稍后给大家安排一波福利", confirmation: "限量优惠券 50 元，马上领取", duplicate: "优惠券已经发出来了", conclusion: "你关注的优惠活动已经出现，可以进入直播间查看", priority: "urgent"}
	case containsAny(goal, []string{"抽奖", "福袋", "开奖", "中奖"}):
		return goalProfile{kind: "giveaway", keywords: []string{"抽奖", "福袋", "开奖", "中奖", "互动福利"}, strongTerms: []string{"抽奖倒计时", "福袋开启", "现在开奖", "马上抽奖"}, negative: "本场暂时不安排抽奖", vague: "等会儿有一个互动福利", confirmation: "抽奖倒计时 00:30", duplicate: "抽奖马上开始", conclusion: "你关注的抽奖活动即将开始", priority: "urgent"}
	case containsAny(goal, []string{"官宣", "新品", "发布", "预售", "重大消息", "重要消息", "嘉宾"}):
		return goalProfile{kind: "major_announcement", keywords: []string{"官宣", "新品", "发布", "预售", "重要消息", "嘉宾"}, strongTerms: []string{"正式官宣", "开启预售", "正式发布", "发布时间"}, negative: "今天不会公布新品消息", vague: "稍后会公布一个重要消息", confirmation: "新品发布时间：今晚 20:00", duplicate: "刚刚已经正式官宣", conclusion: "你关注的重要消息已经公布", priority: "high"}
	case containsAny(goal, []string{"团战", "决胜", "比分", "比赛", "击杀", "Boss", "boss", "上分"}):
		return goalProfile{kind: "game_moment", keywords: []string{"团战", "打团", "决胜", "比分", "比赛", "击杀", "Boss", "boss", "关键时刻"}, strongTerms: []string{"决胜团战", "决胜局", "最终比分", "Boss 开始"}, negative: "这局暂时不会打团", vague: "马上进入比赛关键阶段", confirmation: "决胜局 2:2，团战开始", duplicate: "决胜团战已经开始", conclusion: "比赛进入了你关注的关键时刻", priority: "urgent"}
	default:
		keyword := focusKeyword(goal)
		return goalProfile{kind: "custom_goal", keywords: []string{keyword}, strongTerms: []string{"现在开始", "正式公布", "已经出现"}, negative: "暂时不会提到" + keyword, vague: "稍后会聊到" + keyword, confirmation: keyword + "现在开始", duplicate: "已经提到过" + keyword, conclusion: "直播间出现了你关注的内容：" + keyword, priority: "high"}
	}
}

func classify(text, goal string) string {
	if containsAny(text, []string{"验证码", "安全验证", "请登录", "完成验证"}) {
		return "human_verification"
	}
	profile := profileForGoal(goal)
	if containsAny(text, profile.keywords) {
		return profile.kind
	}
	return ""
}

func focusKeyword(goal string) string {
	result := goal
	for _, noise := range []string{"请帮我", "帮我", "我想", "监控", "关注", "提醒", "直播间", "主播", "什么时候", "如果", "出现", "关于", "内容", "消息", "的"} {
		result = strings.ReplaceAll(result, noise, "")
	}
	result = strings.TrimSpace(strings.Trim(result, "，,。！？!?：: "))
	if result == "" {
		return "关注目标"
	}
	runes := []rune(result)
	if len(runes) > 16 {
		return string(runes[:16])
	}
	return result
}

func roomNameFromURL(parsed *url.URL) string {
	room := strings.Trim(path.Base(strings.TrimSuffix(parsed.Path, "/")), " ")
	if room == "" || room == "." || room == "/" {
		room = parsed.Hostname()
	}
	if len([]rune(room)) > 18 {
		room = string([]rune(room)[:18])
	}
	return "直播间 " + room
}

func demoSignals(run *Run) []SignalEvent {
	profile := profileForGoal(run.Goal)
	room := run.Rooms[0]
	base := time.Now().UTC()
	items := []SignalEvent{
		{RoomID: room.ID, RoomName: room.Name, Source: "asr", Text: "欢迎来到直播间，我们先聊聊今天的日常内容", Confidence: .96},
		{RoomID: room.ID, RoomName: room.Name, Source: "asr", Text: profile.negative, Confidence: .94},
		{RoomID: room.ID, RoomName: room.Name, Source: "asr", Text: profile.vague, Confidence: .76},
		{RoomID: room.ID, RoomName: room.Name, Source: "ocr", Text: profile.confirmation, Confidence: .84},
		{RoomID: room.ID, RoomName: room.Name, Source: "asr", Text: profile.duplicate, Confidence: .96},
		{RoomID: room.ID, RoomName: room.Name, Source: "ocr", Text: "直播进行中", Confidence: .99},
	}
	for i := range items {
		items[i].ID = domain.NewID("signal")
		items[i].ObservedAt = base.Add(time.Duration(i) * time.Second)
	}
	return items
}

func addStep(inv *Investigation, tool, purpose, outcome string, at time.Time) {
	inv.Steps = append(inv.Steps, AgentStep{ID: domain.NewID("step"), Sequence: len(inv.Steps) + 1, Tool: tool, Purpose: purpose, Outcome: outcome, CreatedAt: at})
}

func waitingInvestigation(run *Run, roomID, eventType string) int {
	for i := len(run.Investigations) - 1; i >= 0; i-- {
		inv := &run.Investigations[i]
		if inv.RoomID == roomID && inv.EventType == eventType && inv.Status == InvestigationWaiting {
			return i
		}
	}
	return -1
}

func evidenceWindow(run *Run, roomID string, limit int) []SignalEvent {
	result := make([]SignalEvent, 0, limit)
	for i := len(run.Events) - 1; i >= 0 && len(result) < limit; i-- {
		if run.Events[i].RoomID == roomID {
			result = append(result, run.Events[i])
		}
	}
	return result
}

func containsAny(text string, values []string) bool {
	for _, value := range values {
		if value != "" && strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func hasCrossSource(evidence []Evidence) bool {
	sources := map[string]bool{}
	for _, item := range evidence {
		sources[item.Source] = true
	}
	return len(sources) >= 2
}

func hasRecentAlert(run *Run, roomID, eventType string) bool {
	for _, alert := range run.Alerts {
		if alert.RoomID == roomID && alert.EventType == eventType {
			return true
		}
	}
	return false
}

func combinedConfidence(evidence []Evidence) float64 {
	product := 1.0
	for _, item := range evidence {
		confidence := item.Confidence
		if confidence <= 0 || confidence > 1 {
			confidence = 0.7
		}
		product *= 1 - confidence
	}
	return 1 - product
}

func setRoomStatus(run *Run, roomID, status string) {
	for i := range run.Rooms {
		if run.Rooms[i].ID == roomID {
			run.Rooms[i].Status = status
			return
		}
	}
}
