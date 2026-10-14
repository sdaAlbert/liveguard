package monitor

import (
	"fmt"
	"time"

	"liveguard/internal/domain"
)

// Plan is the small, inspectable decision produced before a tool runs.
// Keeping it explicit makes the Agent loop testable without requiring an LLM.
type Plan struct {
	Actions []string
}

type Planner interface {
	Plan(run *Run, event SignalEvent, eventType string, investigation *Investigation) Plan
}

type ToolExecutor interface {
	Execute(run *Run, investigation *Investigation, action string, event SignalEvent, at time.Time) ToolResult
}

type Verifier interface {
	Verify(run *Run, investigation *Investigation, event SignalEvent, eventType string) Verification
}

type ToolResult struct {
	Tool    string
	Status  string
	Outcome string
}

type Verification struct {
	Status     string
	Conclusion string
	Confidence float64
	AllowAlert bool
}

type Runtime struct {
	planner  Planner
	executor ToolExecutor
	verifier Verifier
}

const (
	maxRuntimeSteps      = 6
	maxRepeatedToolCalls = 2
)

var defaultRuntime = Runtime{
	planner:  deterministicPlanner{},
	executor: defaultToolExecutor{},
	verifier: evidenceVerifier{},
}

func (r Runtime) Handle(run *Run, event SignalEvent, eventType string) {
	now := time.Now().UTC()
	index := waitingInvestigation(run, event.RoomID, eventType)
	if index < 0 {
		run.Investigations = append(run.Investigations, Investigation{
			GoalVersion: run.GoalVersion,
			ID:          domain.NewID("investigation"), RoomID: event.RoomID, RoomName: event.RoomName,
			EventType: eventType, Status: InvestigationWaiting, CreatedAt: now, UpdatedAt: now,
		})
		index = len(run.Investigations) - 1
	}
	inv := &run.Investigations[index]
	if eventType != "human_verification" && (run.Plan.AlertScope == "chat" || goalNeedsChatRegion(run.Goal)) {
		inv.Status = InvestigationWaiting
		inv.Conclusion = "聊天区目标需要模型看图确认；本地规则模式继续保存文字，不生成未经确认的提醒"
		return
	}
	inv.Evidence = append(inv.Evidence, Evidence{EventID: event.ID, Source: string(event.Source), Text: event.Text, Confidence: event.Confidence, ObservedAt: event.ObservedAt})
	inv.UpdatedAt = now

	plan := r.planner.Plan(run, event, eventType, inv)
	seenTools := map[string]int{}
	for _, action := range plan.Actions {
		if len(inv.Steps) >= maxRuntimeSteps {
			inv.Status = InvestigationSuppressed
			inv.Conclusion = "Agent 达到最大执行步数，已停止继续调用工具"
			return
		}
		seenTools[action]++
		if seenTools[action] > maxRepeatedToolCalls {
			inv.Status = InvestigationSuppressed
			inv.Conclusion = "Agent 检测到重复工具调用，已停止执行"
			return
		}
		result := r.executor.Execute(run, inv, action, event, now)
		if result.Status == "failed" {
			inv.Status = InvestigationSuppressed
			inv.Conclusion = result.Outcome
			return
		}
		if action == "request_human_takeover" {
			inv.Status = InvestigationNeedsHuman
			inv.Conclusion = result.Outcome
			setRoomStatus(run, event.RoomID, "needs_human")
			for i := range run.Rooms {
				if run.Rooms[i].ID == event.RoomID {
					run.Rooms[i].Health = SourceNeedsLogin
				}
			}
			return
		}
	}
	verification := r.verifier.Verify(run, inv, event, eventType)
	inv.Status = verification.Status
	inv.Conclusion = verification.Conclusion
	if !verification.AllowAlert {
		return
	}
	profile := profileForGoal(run.Goal)
	r.executor.Execute(run, inv, "create_alert", event, now)
	run.Alerts = append(run.Alerts, Alert{
		SignalToAlertMS: max(0, now.Sub(event.ObservedAt).Milliseconds()),
		ID:              domain.NewID("alert"), RoomID: event.RoomID, RoomName: event.RoomName,
		EventType: eventType, Priority: profile.priority, Summary: profile.conclusion,
		Confidence: verification.Confidence, DedupeKey: event.RoomID + ":" + eventType,
		Investigation: inv.ID, Evidence: append([]Evidence(nil), inv.Evidence...), CreatedAt: now,
	})
}

type deterministicPlanner struct{}

func (deterministicPlanner) Plan(run *Run, event SignalEvent, eventType string, inv *Investigation) Plan {
	if eventType == "human_verification" {
		return Plan{Actions: []string{"read_live_window", "request_human_takeover"}}
	}
	if containsAny(event.Text, []string{"没有", "不会", "不安排", "取消", "别等", "暂时不"}) {
		return Plan{Actions: []string{"read_live_window"}}
	}
	if hasRecentAlert(run, event.RoomID, eventType) {
		return Plan{Actions: []string{"read_live_window", "query_recent_alerts"}}
	}
	profile := profileForGoal(run.Goal)
	strong := containsAny(event.Text, profile.strongTerms)
	crossSource := hasCrossSource(inv.Evidence)
	actions := []string{"read_live_window"}
	if strong || crossSource {
		actions = append(actions, "query_recent_alerts")
		if combinedConfidence(inv.Evidence) < 0.88 || crossSource {
			actions = append(actions, "reanalyze_evidence")
		}
	}
	return Plan{Actions: actions}
}

type defaultToolExecutor struct{}

func (defaultToolExecutor) Execute(run *Run, inv *Investigation, action string, event SignalEvent, at time.Time) ToolResult {
	switch action {
	case "read_live_window":
		addStep(inv, action, "读取这个直播间最近几条字幕和画面文字", fmt.Sprintf("拿到 %d 条相关证据", len(evidenceWindow(run, event.RoomID, 8))), at)
	case "query_recent_alerts":
		outcome := "已查询这个监控窗口的提醒记录"
		if hasRecentAlert(run, event.RoomID, inv.EventType) {
			outcome = "发现已有同类提醒"
		}
		addStep(inv, action, "检查最近是否已经提醒过同一件事", outcome, at)
	case "reanalyze_evidence":
		profile := profileForGoal(run.Goal)
		matched := containsAny(event.Text, profile.keywords)
		confidence := combinedConfidence(inv.Evidence)
		outcome := fmt.Sprintf("复核了目标匹配和证据置信度（%.0f%%）", confidence*100)
		if !matched {
			outcome = "复核未命中关注目标关键词"
		}
		addStep(inv, action, "检查信号是否命中目标，并查看证据置信度", outcome, at)
	case "create_alert":
		addStep(inv, action, "把确认的重要时刻提醒给用户", "已生成一条带原文证据的个人提醒", at)
	case "request_human_takeover":
		addStep(inv, action, "机器无法代替用户完成安全验证", "已暂停这个监控窗口，等待用户处理", at)
	default:
		return ToolResult{Tool: action, Status: "failed", Outcome: "未知工具，已拒绝执行"}
	}
	return ToolResult{Tool: action, Status: "succeeded"}
}

type evidenceVerifier struct{}

func (evidenceVerifier) Verify(run *Run, inv *Investigation, event SignalEvent, eventType string) Verification {
	if eventType == "human_verification" {
		return Verification{Status: InvestigationNeedsHuman, Conclusion: "直播页面要求登录或安全验证"}
	}
	if containsAny(event.Text, []string{"没有", "不会", "不安排", "取消", "别等", "暂时不"}) {
		return Verification{Status: InvestigationSuppressed, Conclusion: "检测到否定语义，没有打扰用户"}
	}
	if hasRecentAlert(run, event.RoomID, eventType) {
		return Verification{Status: InvestigationSuppressed, Conclusion: "同一事件已经提醒，本次重复内容已去重"}
	}
	profile := profileForGoal(run.Goal)
	strong := containsAny(event.Text, profile.strongTerms)
	crossSource := hasCrossSource(inv.Evidence)
	if !strong && !crossSource {
		return Verification{Status: InvestigationWaiting, Conclusion: "信息还不够明确，继续监听"}
	}
	confidence := combinedConfidence(inv.Evidence)
	if confidence < 0.5 {
		return Verification{Status: InvestigationWaiting, Conclusion: "识别置信度偏低，继续等待更清楚的证据", Confidence: confidence}
	}
	return Verification{Status: InvestigationConfirmed, Conclusion: profile.conclusion, Confidence: confidence, AllowAlert: true}
}
