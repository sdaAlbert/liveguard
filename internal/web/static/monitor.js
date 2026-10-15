const $ = selector => document.querySelector(selector);
const monitorRuns = new Map();
const audioCaptures = new Map();
const goalDrafts = new Map();
const clarificationDrafts = new Map();
const goalSaving = new Set();
const goalFeedback = new Map();
const instructionDrafts = new Map();
const instructionSaving = new Set();
const instructionFeedback = new Map();
const monitorIssues = new Map();
const stopping = new Set();
const pausing = new Set();
const replyingToAgent = new Set();
const openDetailIDs = new Set();
const readAlerts = new Map();
const alertFlashUntil = new Map();
const alertFlashTimers = new Map();
const readStorageKey = "liveguard-read-alerts-v1";
const alertFlashDurationMs = 3000;
let detailRunID = null;
let globalModelSettings = null;
let streamConnected = false;
let snapshotsConnected = false;
let refreshSequence = 0;
let refreshTimer = null;

try {
  const saved = JSON.parse(localStorage.getItem(readStorageKey) || "{}");
  for (const [id, keys] of Object.entries(saved || {})) {
    if (Array.isArray(keys)) readAlerts.set(id, new Set(keys.filter(key => typeof key === "string").slice(-500)));
  }
} catch (_) { /* Storage is optional; in-memory acknowledgement still works. */ }

const statusLabels = {running:"监听中",paused:"已暂停",stopped:"已停止",failed:"异常",completed:"已结束"};
const decisionLabels = {waiting_more_evidence:"继续观察",waiting_user:"需要你补充",reviewing_clarification:"结合补充判断中",confirmed:"已提醒",suppressed:"已忽略",needs_human:"需要处理",incomplete:"调查未完成·继续监听",failed:"判断失败·继续监听"};
const eventLabels = {coupon_drop:"优惠活动",giveaway:"抽奖活动",major_announcement:"重要消息",game_moment:"游戏关键时刻",chat_match:"聊天命中",custom_goal:"关注内容",human_verification:"安全验证"};
const toolLabels = {review_unexpected_event:"判断关注目标与重要内容",request_user_input:"向你询问判断规则",read_live_window:"读取证据窗口",search_observations:"读取相关文字",query_recent_alerts:"检查重复提醒",search_recent_alerts:"检查同类提醒",inspect_live_frame:"查看直播画面",save_frame_evidence:"选用画面证据",reanalyze_clip:"复核字幕和画面",reanalyze_evidence:"复核证据",create_alert:"提醒用户",request_human_takeover:"请求用户处理"};
const sourceLabels = {asr:"口播转写",frame:"画面采样",vision:"视觉判断"};
const providerLabels = {minimax:"MiniMax",openai:"OpenAI",deepseek:"DeepSeek",rules:"本地规则"};
const healthLabels = {unconfigured:"未配置",waiting_signal:"等待信号",healthy:"正常",delayed:"采集延迟",needs_login:"需要登录",capture_failed:"采集失败"};

function escapeHTML(value) {
  const node = document.createElement("div");
  node.textContent = value || "";
  return node.innerHTML;
}

function escapeAttribute(value) {
  return escapeHTML(value).replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

async function request(url, options = {}) {
  const response = await fetch(url, options);
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `请求失败 (${response.status})`);
  return body;
}

function timeLabel(value) {
  if (!value) return "--:--:--";
  return new Date(value).toLocaleTimeString("zh-CN", {hour12:false});
}

function alertKey(alert) {
  return alert.id || `${alert.created_at}:${alert.summary}`;
}

function unreadAlerts(run) {
  const seen = readAlerts.get(run.id);
  return (run.alerts || []).filter(alert => !seen?.has(alertKey(alert)));
}

function flashNewAlert(id) {
  const expiresAt = Date.now() + alertFlashDurationMs;
  alertFlashUntil.set(id, expiresAt);
  clearTimeout(alertFlashTimers.get(id));
  alertFlashTimers.set(id, setTimeout(() => {
    if (alertFlashUntil.get(id) !== expiresAt) return;
    alertFlashUntil.delete(id);
    alertFlashTimers.delete(id);
    renderAll();
  }, alertFlashDurationMs));
}

function recordNewAlertFlashes(previousRuns, nextRuns) {
  for (const run of nextRuns) {
    const previous = previousRuns.get(run.id);
    if (!previous) continue;
    const previousKeys = new Set((previous.alerts || []).map(alertKey));
    if ((run.alerts || []).some(alert => !previousKeys.has(alertKey(alert)))) flashNewAlert(run.id);
  }
}

function acknowledgeAlerts(id) {
  const run = monitorRuns.get(id);
  if (!run) return;
  const keys = new Set(readAlerts.get(id) || []);
  (run.alerts || []).forEach(alert => keys.add(alertKey(alert)));
  readAlerts.set(id, new Set([...keys].slice(-500)));
  try {
    localStorage.setItem(readStorageKey, JSON.stringify(Object.fromEntries([...readAlerts].slice(-100).map(([key, values]) => [key, [...values]]))));
  } catch (_) { /* Keep the acknowledged state for this page if storage is full. */ }
  renderAll();
}

function monitorState(run) {
  const room = run.rooms?.[0] || {};
  const unread = unreadAlerts(run).length;
  if (run.status === "stopped" || run.status === "completed") return {tone:"gray", label:statusLabels[run.status], unread};
  if (run.status === "paused") return {tone:"gray", label:"已暂停", unread};
  if (run.status === "failed" && run.stop_reason === "api_quota_exhausted") return {tone:"yellow", label:"API 额度已耗尽", unread};
  if (run.baseline?.status === "pending" || run.baseline?.status === "reviewing") return {tone:"green", label:"Agent 正在初看", unread};
  if (run.baseline?.status === "needs_login") return {tone:"yellow", label:"需要登录 / 验证", unread};
  if (run.status === "failed") return {tone:"yellow", label:"监控异常", unread};
  if (!snapshotsConnected || !streamConnected) return {tone:"yellow", label:"连接恢复中", unread};
  if (stopping.has(run.id)) return {tone:"gray", label:"正在停止", unread};
  if (goalSaving.has(run.id)) return {tone:"gray", label:"更新策略中", unread};
  if (monitorIssues.has(run.id)) return {tone:"yellow", label:monitorIssues.get(run.id), unread};
  if (room.health === "needs_login") return {tone:"yellow", label:"需要登录 / 验证", unread};
  if (room.health === "delayed" && room.last_error) return {tone:"yellow", label:"采集重试中", unread};
  if (room.last_error || ["capture_failed", "delayed", "unconfigured"].includes(room.health)) return {tone:"yellow", label:"采集异常", unread};
  if ((alertFlashUntil.get(run.id) || 0) > Date.now()) return {tone:"red", label:"新提醒", unread};
  const latest = [...(run.investigations || [])].reverse().find(inv => !inv.goal_version || !run.goal_version || inv.goal_version === run.goal_version);
  if (run.pending?.reviewing) return {tone:"green", label:"Agent 正在判断", unread};
  if (latest?.status === "waiting_user") return {tone:"yellow", label:"需要你补充", unread};
  if (latest?.status === "reviewing_clarification") return {tone:"green", label:"Agent 正在结合补充判断", unread};
  if (latest?.status === "needs_human") return {tone:"yellow", label:"需要处理", unread};
  if (latest?.status === "incomplete") return {tone:"yellow", label:"单次调查未完成", unread};
  if (latest?.status === "failed") return {tone:"yellow", label:"判断异常", unread};
  if (latest?.status === "waiting_more_evidence") return {tone:"green", label:"继续观察", unread};
  if (run.pending?.waiting) return {tone:"green", label:"等待 Agent 处理", unread};
  if (room.health === "waiting_signal") return {tone:"green", label:"等待新信号", unread};
  return {tone:"green", label:"监听中", unread};
}

function agentState(run) {
  const room = run.rooms?.[0] || {};
  if (run.status === "stopped" || run.status === "completed") return {tone:"gray", label:"监控已结束"};
  if (run.status === "paused") return {tone:"gray", label:"监控已暂停"};
  if (run.status === "failed") return {tone:"yellow", label:run.stop_reason === "api_quota_exhausted" ? "模型额度不可用" : "监控异常"};
  if (!snapshotsConnected || !streamConnected) return {tone:"yellow", label:"状态同步中"};
  if (stopping.has(run.id)) return {tone:"gray", label:"正在停止"};
  if (goalSaving.has(run.id)) return {tone:"gray", label:"正在更新目标"};

  const latest = [...(run.investigations || [])].reverse().find(inv => !inv.goal_version || !run.goal_version || inv.goal_version === run.goal_version);
  if (run.pending?.reviewing || latest?.status === "reviewing") return {tone:"green", label:"Agent 正在判断"};
  if (latest?.status === "reviewing_clarification") return {tone:"green", label:"Agent 正在结合补充判断"};
  if (run.baseline?.status === "pending" || run.baseline?.status === "reviewing") return {tone:"green", label:"Agent 正在初看"};
  if (latest?.status === "waiting_user") return {tone:"yellow", label:"等待你补充"};
  if (latest?.status === "needs_human") return {tone:"yellow", label:"需要你处理"};
  if (latest?.status === "incomplete") return {tone:"yellow", label:"上次调查未完成"};
  if (latest?.status === "failed") return {tone:"yellow", label:"上次判断异常"};
  if (room.health === "needs_login" || run.baseline?.status === "needs_login") return {tone:"yellow", label:"等待登录或验证"};
  if (monitorIssues.has(run.id)) return {tone:"yellow", label:monitorIssues.get(run.id)};
  if (run.last_error || room.last_error || ["capture_failed", "delayed", "unconfigured"].includes(room.health)) return {tone:"yellow", label:"采集异常"};
  if (latest?.status === "waiting_more_evidence") return {tone:"green", label:"证据不足，继续观察"};
  if (run.pending?.waiting) return {tone:"green", label:"等待 Agent 调度"};
  return {tone:"green", label:"正常监听"};
}

function renderTabs(runs) {
  const tabs = $("#monitorTabs");
  tabs.replaceChildren();
  $("#allMonitors").classList.toggle("selected", !detailRunID);
  $("#allMonitors").setAttribute("aria-pressed", String(!detailRunID));
  $("#navCount").textContent = runs.length;
  runs.forEach(run => {
    const state = monitorState(run);
    const name = run.rooms?.[0]?.name || run.name || "直播窗口";
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.id = run.id;
    button.className = detailRunID === run.id ? "selected" : "";
    button.setAttribute("aria-pressed", String(detailRunID === run.id));
    button.setAttribute("aria-label", `${name}，${state.label}`);
    button.title = `${name} · ${state.label}`;
    button.innerHTML = `<i class="status-dot ${state.tone}" aria-hidden="true"></i><span class="tab-name">${escapeHTML(name)}</span>${state.unread ? `<span class="tab-unread">${state.unread}</span>` : ""}`;
    button.addEventListener("click", () => openMonitorDetail(run.id));
    tabs.appendChild(button);
  });
}

function renderAll() {
  for (const [id, capture] of audioCaptures) {
    const run = monitorRuns.get(id);
    if (run && run.status !== "running") stopAudioCapture(id, capture);
  }
  const focused = document.activeElement;
  const focusState = focused?.matches("#detailInstructionInput")
    ? {kind:"instruction", id:detailRunID, value:focused.value, start:focused.selectionStart, end:focused.selectionEnd}
    : focused?.matches(".goal-input")
    ? {kind:"goal", id:focused.closest(".monitor-card")?.dataset.id || detailRunID, value:focused.value, start:focused.selectionStart, end:focused.selectionEnd}
    : focused?.matches(".clarification-form textarea")
      ? {kind:"clarification", id:focused.closest(".clarification-form")?.dataset.investigationId, start:focused.selectionStart, end:focused.selectionEnd}
      : null;
  if (focusState?.kind === "instruction" && focusState.id) instructionDrafts.set(focusState.id, focusState.value);
  if (focusState?.kind === "goal" && focusState.id) goalDrafts.set(focusState.id, focusState.value);
  const runs = [...monitorRuns.values()].filter(run => !["stopped", "completed"].includes(run.status)).sort((a,b) => {
    if (a.status === "running" && b.status !== "running") return -1;
    if (b.status === "running" && a.status !== "running") return 1;
    return new Date(b.started_at) - new Date(a.started_at);
  });
  renderTabs(runs);
  const grid = $("#monitorGrid");
  grid.replaceChildren();
  $("#emptyRoom").classList.toggle("hidden", runs.length > 0);
  grid.classList.toggle("hidden", runs.length === 0);
  runs.forEach(run => grid.appendChild(renderCard(run)));
  $("#roomCount").textContent = runs.filter(run => run.status === "running").length;
  $("#roomHeading").textContent = "全部直播";
  const newCount = runs.reduce((count, run) => count + unreadAlerts(run).length, 0);
  const warningCount = runs.filter(run => monitorState(run).tone === "yellow").length;
  $("#roomOverview").textContent = [newCount ? `${newCount} 条新提醒` : "", warningCount ? `${warningCount} 个待处理` : ""].filter(Boolean).join(" · ");
  const connected = snapshotsConnected && streamConnected;
  $("#connectionDot").className = `status-dot ${connected ? "green" : "yellow"}`;
  $("#connectionLabel").textContent = connected ? "已连接 · 实时更新" : "连接恢复中";
  renderMonitorDetail();
  if (focusState && focusState.kind !== "instruction" && !focused?.closest("#monitorDetailContent")) {
    const restored = focusState.kind === "clarification"
      ? [...document.querySelectorAll(".clarification-form textarea")].find(input => input.closest(".clarification-form")?.dataset.investigationId === focusState.id)
      : focusState.kind === "instruction"
        ? $("#detailInstructionInput")
        : [...document.querySelectorAll(".goal-input")].find(input => input.id !== "detailInstructionInput" && (input.closest(".monitor-card")?.dataset.id === focusState.id || (detailRunID === focusState.id && input.closest("#monitorDetailDialog"))));
    if (restored && !restored.disabled) {
      if (focusState.kind === "goal") restored.value = focusState.value;
      if (focusState.kind === "instruction") restored.value = focusState.value;
      restored.focus({preventScroll:true});
      restored.setSelectionRange(focusState.start, focusState.end);
    }
  }
}

function renderCard(run) {
  const fragment = $("#monitorCard").content.cloneNode(true);
  const card = fragment.querySelector(".monitor-card");
  const room = run.rooms?.[0] || {name:run.name,status:"listening"};
  const state = monitorState(run);
  card.dataset.id = run.id;
  card.dataset.tone = state.tone;
  const open = card.querySelector(".card-open-detail");
  open.setAttribute("aria-label", `查看监控详情：${room.name || run.name}，${state.label}`);
  open.title = `${room.name || run.name} · ${state.label}`;
  open.querySelector(".status-dot").classList.add(state.tone);
  open.querySelector(".room-name").textContent = room.name || run.name || "直播监控";
  const platform = open.querySelector(".room-platform");
  platform.textContent = room.platform || "";
  platform.hidden = !room.platform;
  open.querySelector(".goal-summary").textContent = run.goal || "未设置关注目标";
  const feedback = monitorFeedback(run, state);
  const feedbackNode = open.querySelector(".card-feedback");
  feedbackNode.textContent = feedback;
  feedbackNode.title = feedback;
  open.addEventListener("click", () => openMonitorDetail(run.id));

  const pause = card.querySelector(".pause-monitor");
  pause.hidden = !["running", "paused"].includes(run.status);
  pause.disabled = pausing.has(run.id) || stopping.has(run.id);
  pause.textContent = pausing.has(run.id) ? (run.status === "paused" ? "继续中…" : "暂停中…") : (run.status === "paused" ? "继续" : "暂停");
  pause.setAttribute("aria-label", run.status === "paused" ? "继续监控" : "暂停监控");
  pause.addEventListener("click", () => toggleMonitorPause(run.id, pause));
  const stop = card.querySelector(".stop-monitor");
  stop.disabled = !["running", "paused"].includes(run.status) || stopping.has(run.id);
  stop.textContent = stopping.has(run.id) ? "结束中…" : (["running", "paused"].includes(run.status) ? "结束" : "已结束");
  stop.addEventListener("click", () => stopMonitor(run.id, stop));
  return fragment;
}

function monitorFeedback(run, state) {
  const latest = [...(run.events || [])].reverse().find(event => event.source !== "frame");
  const room = run.rooms?.[0] || {};
  const newestAlert = unreadAlerts(run).at(-1);
  const investigations = [...(run.investigations || [])].reverse();
  const linkedInvestigation = latest && investigations.find(inv => (inv.evidence || []).some(evidence => evidence.event_id === latest.id));
  const recentInvestigation = linkedInvestigation || (latest && investigations.find(inv => inv.updated_at && new Date(inv.updated_at) >= new Date(latest.observed_at)));
  if (newestAlert) return `新提醒 · ${newestAlert.summary}`;
  if (run.status === "paused") return "监控已暂停 · 点击继续恢复采集";
  if (run.status === "stopped" || run.status === "completed") return "监控已结束";
  if (run.status === "failed" && run.stop_reason === "api_quota_exhausted") return "模型额度不可用 · 监控已结束";
  if (run.baseline?.status === "pending" || run.baseline?.status === "reviewing") return "正在初看直播画面，随后开始持续监控";
  if (run.baseline?.status === "unavailable" && !(run.events || []).length) return "初看不可用 · 继续使用 OCR 监控";
  if (run.last_error || room.last_error || monitorIssues.has(run.id)) return `${state.label} · ${monitorIssues.get(run.id) || room.last_error || run.last_error}`;
  if (room.health === "needs_login") return "需要登录或完成验证 · 请处理后重新监控";
  if (run.pending?.reviewing) return "Agent 正在判断最新信号 · 监听中";
  const latestInvestigation = [...(run.investigations || [])].reverse().find(inv => !inv.goal_version || !run.goal_version || inv.goal_version === run.goal_version);
  if (latestInvestigation?.status === "waiting_user") return `需要你补充：${latestInvestigation.user_question || "点开查看 Agent 的问题"} · 监听中`;
  if (latestInvestigation?.status === "reviewing_clarification") return "Agent 正在结合你的补充重新判断 · 监听中";
  if (recentInvestigation) {
    const conclusion = recentInvestigation.conclusion || recentInvestigation.steps?.[0]?.outcome;
    if (recentInvestigation.status === "confirmed") return `发现：${conclusion || "符合提醒条件"} · 监听中`;
    if (recentInvestigation.status === "waiting_more_evidence") return `待确认：${conclusion || "继续收集信号"} · 监听中`;
    if (recentInvestigation.status === "suppressed") return `已判断：${conclusion || recentInvestigation.reason || "当前无需提醒"} · 监听中`;
    if (recentInvestigation.status === "needs_human") return `需要处理：${conclusion || recentInvestigation.reason || "Agent 需要你确认"}`;
    if (recentInvestigation.status === "incomplete") return `本次调查未判断目标是否出现 · 继续监听`;
    if (recentInvestigation.status === "failed") return `判断未完成 · 监听中`;
    return `Agent 判断：${conclusion || decisionLabels[recentInvestigation.status] || "处理中"} · 监听中`;
  }
  if (run.pending?.waiting) return latest?.source === "ocr" ? "文字信号已收录 · 等待 Agent 判断 · 监听中" : "信号已收录 · 等待 Agent 判断 · 监听中";
  if (latest?.source === "ocr") return "检测到文字信号 · 等待 Agent 判断 · 监听中";
  if (latest?.source === "ocr") return "文字信号已记录 · 等待下一次 Agent 判断";
  if (latest?.source === "asr") return "口播信号已记录 · 等待下一次 Agent 判断";
  if (latest) return `${sourceLabels[latest.source] || "信号"} · 等待处理`;
  if (room.health === "waiting_signal") return "等待直播画面或文字信号";
  return "正在监听 · 等待新的直播信号";
}

function openMonitorDetail(id) {
  if (!monitorRuns.has(id)) return;
  if (detailRunID === id) return;
  detailRunID = id;
  history.pushState({...history.state, liveguardDetail:id}, "", location.href);
  renderAll();
  const dialog = $("#monitorDetailDialog");
  if (dialog.open) $("#detailBack").focus({preventScroll:true});
  refreshGlobalModelSettings();
}

function closeMonitorDetail(fromHistory = false) {
  if (!detailRunID) return;
  detailRunID = null;
  const dialog = $("#monitorDetailDialog");
  if (dialog.open) dialog.close();
  renderAll();
  if (!fromHistory && history.state?.liveguardDetail) history.back();
}

window.addEventListener("popstate", () => {
  if (detailRunID) closeMonitorDetail(true);
});

$("#monitorDetailDialog").addEventListener("close", () => {
  if (detailRunID) closeMonitorDetail(false);
});
$("#monitorDetailDialog").addEventListener("click", event => {
  if (event.target === $("#monitorDetailDialog")) closeMonitorDetail();
});

function eventTime(value) {
  if (!value) return "时间未知";
  return new Date(value).toLocaleString("zh-CN", {hour12:false, month:"2-digit", day:"2-digit", hour:"2-digit", minute:"2-digit", second:"2-digit"});
}

function confidenceLabel(value) {
  return `${Math.round((Number(value) || 0) * 100)}%`;
}

function keyEvidenceMarkup(evidence = []) {
  const items = evidence.filter(item => (item.text || "").trim());
  const selected = [...items].reverse().find(item => item.source === "vision") || items.find(item => item.source === "ocr" || item.source === "asr");
  if (!selected) return "";
  let text = selected.text;
  if (selected.source === "ocr") {
    text = String(text).split("\n").map(line => line.replace(/^《(?:主屏幕|聊天区)》\s*/, "").trim()).filter(Boolean).join(" · ");
  }
  const summary = compactText(text, 180);
  return `<p class="key-evidence"><span>${selected.source === "vision" ? "画面判断" : selected.source === "asr" ? "口播证据" : "文字证据"}</span>${escapeHTML(summary)}</p>`;
}

function compactText(value, limit = 180) {
  const text = Array.from(String(value || "").replace(/\s+/g, " ").trim());
  return text.length > limit ? `${text.slice(0, limit - 1).join("")}…` : text.join("");
}

function compactAgentActions(steps = []) {
  const actual = steps.filter(step => (step.succeeded === true || step.succeeded === false) && step.tool !== "review_unexpected_event");
  const grouped = new Map();
  for (const step of actual) {
    const key = step.tool === "search_observations" ? `${step.tool}:${/整场/.test(step.outcome || "") ? "all" : "recent"}` : step.tool;
    grouped.set(key, step);
  }
  return [...grouped.values()].map(step => {
    const tool = step.tool;
    let label = toolLabels[tool] || tool;
    let outcome = step.outcome || (step.succeeded ? "完成" : "未完成");
    if (tool === "search_observations") {
      label = /整场/.test(outcome) ? "查整场记录" : "查近期记录";
      const count = outcome.match(/找到(?:了)?\s*(\d+)\s*条/);
      outcome = count ? `${count[1]} 条` : (step.succeeded ? "已完成" : "失败");
    } else if (tool === "inspect_live_frame") {
      label = "看直播画面";
      outcome = step.succeeded ? "已核验" : "未完成";
    } else if (tool === "search_recent_alerts") {
      label = "查重复提醒";
      outcome = /未发现/.test(outcome) ? "无同类提醒" : step.succeeded ? "已检查" : "未完成";
    } else if (tool === "save_frame_evidence") {
      label = "选用画面证据";
      outcome = step.succeeded ? "仅本次判断使用" : "未完成";
    } else if (tool === "create_alert") {
      label = "发送提醒";
      outcome = step.succeeded ? "已提交" : "未通过校验";
    } else if (tool === "request_user_input") {
      label = "向你提问";
      outcome = step.succeeded ? "等待回复" : "未完成";
    }
    return `<span class="agent-action ${step.succeeded === false ? "failed" : ""}"><b>${escapeHTML(label)}</b> ${escapeHTML(outcome)}</span>`;
  }).join("");
}

function renderAlert(alert) {
  const unread = unreadAlerts(monitorRuns.get(detailRunID) || {}).some(item => alertKey(item) === alertKey(alert));
  return `<article class="detail-alert ${unread ? "unread" : ""}"><div class="detail-item-meta"><span>${escapeHTML(displayEventLabel(alert.event_type))}</span><time>${eventTime(alert.created_at)}</time></div><h3>${escapeHTML(alert.summary)}</h3>${keyEvidenceMarkup(alert.evidence)}</article>`;
}

function displayEventLabel(type) {
  const normalized = String(type || "").replace(/^proactive_/, "");
  return eventLabels[normalized] || normalized || "判断";
}

function renderInvestigation(inv) {
  const review = inv.review;
  const noFinalDecision = Boolean(review?.stop_reason && !["finished", "terminal_tool"].includes(review.stop_reason));
  const status = noFinalDecision ? "调查未完成·继续监听" : decisionLabels[inv.status] || inv.status || "判断中";
  const steps = inv.steps || [];
  const reason = (inv.reason || "").trim();
  const genericFailure = /^(AI 暂时无法完成判断，监控会继续运行|Agent 本次未形成完整结论，监控会继续运行)$/.test(inv.conclusion || "");
  const conclusion = compactText(noFinalDecision && reason ? reason : noFinalDecision && genericFailure ? "本次调查未完成，未判断目标是否出现。" : inv.conclusion || status, 200);
  const runPaused = monitorRuns.get(detailRunID)?.status !== "running";
  const actions = compactAgentActions(steps);
  const activity = actions ? `<p class="agent-activity"><span>Agent</span>${actions}</p>` : "";
  const question = inv.status === "waiting_user" && !inv.user_answer ? `<form class="clarification-form" data-investigation-id="${escapeAttribute(inv.id)}"><p class="clarification-prompt">${escapeHTML(inv.user_question || "Agent 需要你补充判断规则")}</p>${inv.user_options?.length ? `<div class="clarification-options">${inv.user_options.map(option => `<button class="clarification-option" type="button" data-answer="${escapeAttribute(option)}">${escapeHTML(option)}</button>`).join("")}</div>` : ""}<textarea name="answer" rows="3" maxlength="1000" ${runPaused ? "disabled" : ""} required placeholder="补充这次判断需要的条件…">${escapeHTML(clarificationDrafts.get(inv.id) || "")}</textarea><div class="clarification-submit-row"><small>${runPaused ? "继续监控后可回复 Agent" : "你的回答只用于这次判断；监控会继续运行"}</small><button class="detail-primary" type="submit" ${runPaused || replyingToAgent.has(inv.id) ? "disabled" : ""}>${replyingToAgent.has(inv.id) ? "提交中…" : "回复 Agent"}</button></div><p class="clarification-error" aria-live="polite"></p></form>` : inv.user_answer ? `<div class="clarification-answer"><small>你的补充</small><p>${escapeHTML(inv.user_answer)}</p></div>` : "";
  return `<article class="investigation-item"><div class="detail-item-meta"><span>${escapeHTML(displayEventLabel(inv.event_type))}</span><time>${eventTime(inv.updated_at || inv.created_at)}</time><span class="decision-state ${escapeHTML(inv.status)}">${escapeHTML(status)}</span></div><p class="investigation-conclusion">${escapeHTML(conclusion)}</p>${keyEvidenceMarkup(inv.evidence, detailRunID)}${activity}${question}${inv.user_answer && inv.status === "reviewing_clarification" ? `<p class="clarification-working">Agent 正在结合你的补充重新判断。</p>` : ""}</article>`;
}
function detailModelSummary() {
  const status = globalModelSettings;
  if (!status) return "正在读取全局模型设置…";
  if (!status.configured) return "本地规则 · 未配置大模型";
  return `${providerLabels[status.provider] || status.provider} · ${status.model || "默认模型"}${status.quota_exhausted ? " · 额度不可用" : " · 已配置"}`;
}

function renderMonitorDetail() {
  const dialog = $("#monitorDetailDialog");
  const content = $("#monitorDetailContent");
  const run = detailRunID && monitorRuns.get(detailRunID);
  if (!run) {
    if (dialog.open) dialog.close();
    content.replaceChildren();
    return;
  }
  // Snapshot pushes arrive frequently while a live stream is being sampled.
  // Replacing a focused textarea interrupts IME composition (notably Chinese
  // input), even if its value and caret are restored afterwards.
  if (content.contains(document.activeElement) && document.activeElement.matches("textarea, input, [contenteditable=true]")) {
    const liveState = content.querySelector(".detail-live-state");
    if (liveState) {
      const state = agentState(run);
      liveState.querySelector(".status-dot").className = `status-dot ${state.tone}`;
      liveState.querySelector(".agent-status-label").textContent = state.label;
    }
    return;
  }
  const room = run.rooms?.[0] || {name:run.name || "直播窗口"};
  const state = agentState(run);
  const sampling = run.sampling || {};
  const investigations = [...(run.investigations || [])].reverse();
  const alerts = [...(run.alerts || [])].reverse();
  const draft = instructionDrafts.has(run.id) ? instructionDrafts.get(run.id) : "";
  const instructionStatus = instructionFeedback.get(run.id) || "";
  const capture = audioCaptures.get(run.id);
  const canControl = ["running", "paused"].includes(run.status);
  const instructionDisabled = !canControl || instructionSaving.has(run.id);
  const audioLabel = capture ? `停止口播识别${capture.inFlight ? ` · ${capture.inFlight} 段处理中` : ""}` : (run.plan?.recommend_asr ? "开启口播识别（建议）" : "开启口播识别");
  const previousScroll = $("#detailScroll")?.scrollTop || 0;
  const openDetails = [...content.querySelectorAll("details")].map(item => item.open);
  content.innerHTML = `
    <header class="detail-topbar"><button id="detailBack" class="detail-back" type="button"><span aria-hidden="true">←</span> 返回监控室</button><div class="detail-live-state" role="status" aria-live="polite"><i class="status-dot ${state.tone}" aria-hidden="true"></i><span class="agent-status-label">${escapeHTML(state.label)}</span></div><div class="detail-top-actions">${canControl ? `<button id="detailPause" class="detail-control" type="button" ${pausing.has(run.id) || stopping.has(run.id) ? "disabled" : ""}>${pausing.has(run.id) ? "处理中…" : run.status === "paused" ? "继续" : "暂停"}</button><button id="detailStop" class="detail-control danger" type="button" ${stopping.has(run.id) ? "disabled" : ""}>${stopping.has(run.id) ? "结束中…" : "结束监控"}</button>` : ""}</div></header>
    <div id="detailScroll" class="detail-scroll"><section class="detail-hero"><div class="detail-kicker">LIVEGUARD / 监控详情</div><h1>${escapeHTML(room.name || run.name || "直播监控")}</h1><a class="detail-room-link" href="${escapeAttribute(run.target_url || "#")}" target="_blank" rel="noopener noreferrer">${escapeHTML(run.target_url || "未提供直播链接")} <span aria-hidden="true">↗</span></a><p class="detail-goal-hero">${escapeHTML(run.goal || "未设置关注目标")}</p></section>
      <div class="detail-layout"><div class="detail-main-column">
        <section class="detail-section"><div class="detail-section-heading"><div><span class="detail-section-label">Agent 状态</span><h2>最近判断</h2></div></div><div class="detail-list">${investigations.length ? investigations.slice(0, 10).map(renderInvestigation).join("") : `<p class="detail-empty">${globalModelSettings?.configured ? "等待新信号触发调查。" : "尚未配置模型，暂时不能开始 Agent 调查。"}</p>`}</div></section>
        <section class="detail-section"><div class="detail-section-heading"><div><span class="detail-section-label">用户提醒</span><h2>提醒记录</h2></div>${unreadAlerts(run).length ? `<button id="detailAcknowledge" class="text-action" type="button">全部标为已读</button>` : ""}</div><div class="detail-list">${alerts.length ? alerts.map(renderAlert).join("") : `<p class="detail-empty">暂时没有提醒。LiveGuard 会在检测到与关注目标有关的内容时显示在这里。</p>`}</div></section>
      </div><aside class="detail-side-column">
        <section class="detail-panel"><div class="detail-section-heading"><div><span class="detail-section-label">与 Agent 协作</span><h2>下达指令</h2></div></div><form id="detailInstructionForm" class="detail-goal-form"><label for="detailInstructionInput">补充条件、反馈页面已处理或提出新问题。更新关注目标时，请以“目标：”开头。</label><textarea id="detailInstructionInput" class="goal-input" maxlength="1000" ${instructionDisabled ? "disabled" : ""} placeholder="例如：验证码已处理，请继续；或者：目标：只关注主播正式公布的新品价格">${escapeHTML(draft)}</textarea><button class="detail-primary" type="submit" ${instructionDisabled || instructionSaving.has(run.id) ? "disabled" : ""}>${instructionSaving.has(run.id) ? "发送中…" : "发送给 Agent"}</button><p class="goal-status">${escapeHTML(instructionStatus)}</p></form></section>
        <section class="detail-panel"><div class="detail-section-heading"><div><span class="detail-section-label">采集状态</span><h2>直播来源</h2></div></div><dl class="detail-facts"><div><dt>状态</dt><dd>${escapeHTML(healthLabels[room.health] || room.status || "未知")}</dd></div><div><dt>最近采集</dt><dd>${eventTime(room.last_capture_at || room.last_seen)}</dd></div><div><dt>截图尝试</dt><dd>${sampling.capture_count || 0}</dd></div><div><dt>实际间隔</dt><dd>${room.capture_interval_ms ? `${Math.round(room.capture_interval_ms / 1000)} 秒` : `${sampling.capture_interval_seconds || run.plan?.capture_interval_seconds || "—"} 秒`}</dd></div><div><dt>采集耗时</dt><dd>${room.capture_duration_ms ? `${room.capture_duration_ms} ms` : "—"}</dd></div><div><dt>下次信号</dt><dd>${sampling.next_event_at ? eventTime(sampling.next_event_at) : (sampling.reason || "持续观察")}</dd></div><div><dt>OCR积压跳过</dt><dd>${sampling.perception_dropped || 0}</dd></div></dl>${room.last_error || run.last_error ? `<p class="detail-problem">${escapeHTML(room.last_error || run.last_error)}</p>` : ""}${run.plan?.capability_warning ? `<p class="detail-problem">${escapeHTML(run.plan.capability_warning)}</p>` : ""}</section>
        <section class="detail-panel"><div class="detail-section-heading"><div><span class="detail-section-label">可选输入</span><h2>口播识别</h2></div></div><p class="detail-muted">只共享并处理你在浏览器弹窗中选择的直播音频。</p><button class="audio-capture" type="button" ${run.status !== "running" ? "disabled" : ""}>${escapeHTML(audioLabel)}</button>${run.status !== "running" ? `<p class="detail-muted">监控运行时可以开启口播识别。</p>` : ""}</section>
        <section class="detail-panel"><div class="detail-section-heading"><div><span class="detail-section-label">全局配置</span><h2>模型设置</h2></div></div><p class="detail-model-summary">${escapeHTML(detailModelSummary())}</p><button id="detailModelSettings" class="detail-secondary" type="button">管理全局模型设置</button><p class="detail-muted">模型配置对所有监控生效。</p></section>
      </aside></div>
    </div>`;
  if (!dialog.open) dialog.showModal();
  $("#detailScroll").scrollTop = previousScroll;
  [...content.querySelectorAll("details")].forEach((item, index) => { item.open = Boolean(openDetails[index]); });
  $("#detailBack").addEventListener("click", () => closeMonitorDetail());
  $("#detailPause")?.addEventListener("click", event => toggleMonitorPause(run.id, event.currentTarget));
  $("#detailStop")?.addEventListener("click", event => stopMonitor(run.id, event.currentTarget));
  $("#detailAcknowledge")?.addEventListener("click", () => acknowledgeAlerts(run.id));
  content.querySelectorAll(".clarification-form").forEach(form => {
    form.addEventListener("submit", event => submitAgentReply(event, run.id, form));
    form.elements.answer.addEventListener("input", event => clarificationDrafts.set(form.dataset.investigationId, event.currentTarget.value));
    form.querySelectorAll(".clarification-option").forEach(button => button.addEventListener("click", () => {
      form.elements.answer.value = button.dataset.answer || "";
      clarificationDrafts.set(form.dataset.investigationId, form.elements.answer.value);
      form.elements.answer.focus();
    }));
  });
  const instructionInput = $("#detailInstructionInput");
  instructionInput.addEventListener("input", () => instructionDrafts.set(run.id, instructionInput.value));
  $("#detailInstructionForm").addEventListener("submit", event => submitInstruction(event, run.id, instructionInput));
  $(".audio-capture").addEventListener("click", event => toggleAudioCapture(run.id, event.currentTarget));
  $("#detailModelSettings").addEventListener("click", async () => {
    $("#modelSettingsStatus").textContent = "正在读取当前设置…";
    $("#modelSettingsStatus").className = "settings-status";
    $("#modelSettingsDialog").showModal();
    try { await loadModelSettings(); }
    catch (error) { $("#modelSettingsStatus").textContent = error.message; $("#modelSettingsStatus").className = "settings-status error"; }
  });
}

async function submitInstruction(event, id, input) {
  event.preventDefault();
  const instruction = input.value.trim();
  if (!instruction || instruction.length > 1000) {
    instructionFeedback.set(id, "请输入 1-1000 个字符");
    renderAll();
    return;
  }
  if (instructionSaving.has(id)) return;
  instructionSaving.add(id);
  instructionFeedback.set(id, "正在发送给 Agent…");
  const form = input.closest("form");
  const button = form.querySelector("button[type=submit]");
  const status = form.querySelector(".goal-status");
  button.disabled = true;
  button.textContent = "发送中…";
  status.textContent = "正在发送给 Agent…";
  try {
    const run = await request(`/api/monitors/${encodeURIComponent(id)}/instruction`, {
      method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({instruction})
    });
    monitorRuns.set(run.id, run);
    instructionDrafts.delete(id);
    const latest = [...(run.investigations || [])].reverse()[0];
    instructionFeedback.set(id, latest?.event_type === "user_instruction" && latest.status === "reviewing"
      ? "已发送；Agent 正在结合近期信号处理"
      : "已记录；Agent 会结合后续信号处理");
    input.value = "";
  } catch (error) {
    instructionFeedback.set(id, error.message);
  } finally {
    instructionSaving.delete(id);
    button.disabled = false;
    button.textContent = "发送给 Agent";
    status.textContent = instructionFeedback.get(id) || "";
    renderAll();
  }
}

async function refreshGlobalModelSettings() {
  try {
    globalModelSettings = await request("/api/settings/model");
    if (detailRunID) renderMonitorDetail();
  } catch (_) {
    globalModelSettings = null;
  }
}

async function toggleAudioCapture(id, button) {
  const current = audioCaptures.get(id);
  if (current) {
    stopAudioCapture(id, current);
    renderAll();
    return;
  }
  if (!navigator.mediaDevices?.getDisplayMedia || typeof MediaRecorder === "undefined") {
    $("#formError").textContent = "当前浏览器不支持直播音频共享，请使用新版 Chrome。";
    return;
  }
  button.disabled = true;
  try {
    // Browser permission is explicit. Video is used only to enable the browser
    // share picker; only the returned audio track is ever recorded or uploaded.
    const stream = await navigator.mediaDevices.getDisplayMedia({
      video:{displaySurface:"browser"},
      audio:{suppressLocalAudioPlayback:true}
    });
    const audioTracks = stream.getAudioTracks();
    if (!audioTracks.length) {
      stream.getTracks().forEach(track => track.stop());
      throw new Error("没有共享到声音。请重新选择直播标签，并勾选“共享标签页音频”或系统音频。");
    }
    const capture = {stream, recorder:null, stopCurrent:null, uploads:new Set(), inFlight:0};
    audioCaptures.set(id, capture);
    // The display video track is only required by Chrome's share picker. We
    // record and upload audio only, so its lifecycle must not stop ASR.
    audioTracks.forEach(track => track.addEventListener("ended", () => {
      if (audioCaptures.get(id) !== capture) return;
      stopAudioCapture(id, capture);
      monitorIssues.set(id, "口播识别暂停");
      $("#formError").textContent = "口播识别已停止：Chrome 的共享音频轨道结束了。请重新共享直播标签页音频。";
      renderAll();
    }, {once:true}));
    $("#formError").textContent = "";
    renderAll();
    captureAudioLoop(id, capture).catch(error => {
      if (audioCaptures.get(id) === capture) {
        stopAudioCapture(id, capture);
        monitorIssues.set(id, "口播识别暂停");
        $("#formError").textContent = `口播识别暂停：${error.message}`;
        renderAll();
      }
    });
  } catch (error) {
    $("#formError").textContent = error.message || "没有获得直播音频共享权限。";
    button.disabled = false;
  }
}

function stopAudioCapture(id, capture) {
  if (audioCaptures.get(id) !== capture) return;
  audioCaptures.delete(id);
  monitorIssues.delete(id);
  capture.stopCurrent?.();
  capture.uploads.forEach(controller => controller.abort());
  capture.uploads.clear();
  capture.stream.getTracks().forEach(track => track.stop());
}

function uploadAudioChunk(id, capture, blob, observedAt) {
  if (audioCaptures.get(id) !== capture) return;
  if (blob.size < 1024) return;
  if (capture.inFlight >= 2) {
    capture.dropped = (capture.dropped || 0) + 1;
    monitorIssues.set(id, `口播积压，累计跳过 ${capture.dropped} 段`);
    renderAll();
    return;
  }
  const body = new FormData();
  body.append("audio", blob, "live-chunk.webm");
  body.append("observed_at", observedAt);
  const eventID = crypto.randomUUID();
  const controller = new AbortController();
  capture.uploads.add(controller);
  capture.inFlight++;
  request(`/api/monitors/${encodeURIComponent(id)}/audio`, {method:"POST", body, headers:{"Idempotency-Key":eventID}, signal:controller.signal})
    .then(run => {
      if (audioCaptures.get(id) !== capture) return;
      monitorRuns.set(run.id, run);
      monitorIssues.delete(id);
      $("#formError").textContent = "";
      renderAll();
    })
    .catch(error => {
      if (audioCaptures.get(id) !== capture || error.name === "AbortError") return;
      $("#formError").textContent = `本段口播识别失败，仍在继续监听：${error.message}`;
      monitorIssues.set(id, "口播识别异常");
      renderAll();
    })
    .finally(() => {
      capture.uploads.delete(controller);
      capture.inFlight = Math.max(0, capture.inFlight - 1);
    });
}

function recordAudioChunk(capture) {
  const audioStream = new MediaStream(capture.stream.getAudioTracks());
  const mimeType = MediaRecorder.isTypeSupported("audio/webm;codecs=opus") ? "audio/webm;codecs=opus" : "audio/webm";
  const recorder = new MediaRecorder(audioStream, {mimeType});
  const observedAt = new Date().toISOString();
  const chunks = [];
  return new Promise((resolve, reject) => {
    let timer;
    recorder.ondataavailable = event => { if (event.data.size) chunks.push(event.data); };
    recorder.onerror = () => { clearTimeout(timer); reject(new Error("浏览器录制音频片段失败")); };
    recorder.onstop = () => {
      clearTimeout(timer);
      capture.recorder = null;
      capture.stopCurrent = null;
      resolve({blob:new Blob(chunks, {type:mimeType}), observedAt});
    };
    capture.recorder = recorder;
    capture.stopCurrent = () => { if (recorder.state !== "inactive") recorder.stop(); };
    recorder.start();
    timer = setTimeout(capture.stopCurrent, 6000);
  });
}

async function captureAudioLoop(id, capture) {
  while (audioCaptures.get(id) === capture) {
    const chunk = await recordAudioChunk(capture);
    if (audioCaptures.get(id) !== capture) return;
    // Every chunk is a complete WebM recording. Start the next recording
    // without waiting for network/ASR; timeslice fragments are not standalone.
    uploadAudioChunk(id, capture, chunk.blob, chunk.observedAt);
  }
}

async function saveGoal(event, id, input, button, status) {
  event.preventDefault();
  const goal = input.value.trim();
  if (goal.length < 2 || goal.length > 240) {
    goalFeedback.set(id, "请输入 2-240 个字符");
    status.textContent = "请输入 2-240 个字符";
    return;
  }
  button.disabled = true;
  goalSaving.add(id);
  goalFeedback.set(id, "正在更新监控策略…");
  renderAll();
  try {
    const run = await request(`/api/monitors/${encodeURIComponent(id)}/goal`, {
      method:"PUT", headers:{"Content-Type":"application/json"}, body:JSON.stringify({goal})
    });
    monitorRuns.set(run.id, run);
    goalDrafts.delete(id);
    goalFeedback.set(id, "需求已更新");
  } catch (error) {
    goalFeedback.set(id, error.message);
  } finally {
    goalSaving.delete(id);
    renderAll();
  }
}

async function submitAgentReply(event, runID, form) {
  event.preventDefault();
  const investigationID = form.dataset.investigationId;
  const answer = form.elements.answer.value.trim();
  const error = form.querySelector(".clarification-error");
  if (!answer || answer.length > 1000) {
    error.textContent = "请填写 1-1000 个字符的补充说明";
    return;
  }
  if (replyingToAgent.has(investigationID)) return;
  replyingToAgent.add(investigationID);
  error.textContent = "";
  const submit = form.querySelector("button[type=submit]");
  submit.disabled = true;
  submit.textContent = "提交中…";
  let accepted = false;
  try {
    const run = await request("/api/monitors/" + encodeURIComponent(runID) + "/investigations/" + encodeURIComponent(investigationID) + "/reply", {
      method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({answer})
    });
    monitorRuns.set(run.id, run);
    clarificationDrafts.delete(investigationID);
    accepted = true;
  } catch (requestError) {
    error.textContent = requestError.message;
    submit.disabled = false;
    submit.textContent = "回复 Agent";
  } finally {
    replyingToAgent.delete(investigationID);
    if (accepted) renderAll();
  }
}

async function uploadOCR(id, input, button) {
  const file = input.files?.[0];
  if (!file) return;
  button.disabled = true;
  button.textContent = "识别中…";
  const body = new FormData();
  body.append("image", file);
  const eventID = crypto.randomUUID();
  try {
    const run = await request(`/api/monitors/${encodeURIComponent(id)}/ocr`, {method:"POST", body, headers:{"Idempotency-Key":eventID}});
    monitorRuns.set(run.id, run);
    renderAll();
  } catch (error) {
    $("#formError").textContent = error.message;
  } finally {
    input.value = "";
    button.disabled = false;
    button.textContent = "上传 OCR 图片";
  }
}

async function refreshRuns() {
  const sequence = ++refreshSequence;
  try {
    const summaries = await request("/api/monitors");
    // Stopped runs remain in the backend for audit, but leave the control room.
    const personal = summaries.filter(run => run.target_url && !["stopped", "completed"].includes(run.status));
    const selected = personal.slice(0, 20);
    if (detailRunID && !selected.some(run => run.id === detailRunID)) {
      const selectedSummary = summaries.find(run => run.id === detailRunID);
      if (selectedSummary) selected.push(selectedSummary);
    }
    const details = await Promise.all(selected.map(run => request(`/api/monitors/${encodeURIComponent(run.id)}`)));
    if (sequence !== refreshSequence) return;
    const previousRuns = new Map(monitorRuns);
    recordNewAlertFlashes(previousRuns, details);
    snapshotsConnected = true;
    monitorRuns.clear();
    details.forEach(run => monitorRuns.set(run.id, run));
    renderAll();
  } catch (error) {
    if (sequence !== refreshSequence) return;
    snapshotsConnected = false;
    renderAll();
    throw error;
  }
}

async function stopMonitor(id, button) {
  button.disabled = true;
  stopping.add(id);
  renderAll();
  const capture = audioCaptures.get(id);
  if (capture) stopAudioCapture(id, capture);
  try {
    const run = await request(`/api/monitors/${encodeURIComponent(id)}/stop`, {method:"POST"});
    monitorRuns.set(run.id, run);
    renderAll();
  } catch (error) {
    $("#formError").textContent = error.message;
    button.disabled = false;
  } finally {
    stopping.delete(id);
    renderAll();
  }
}

async function toggleMonitorPause(id, button) {
  const run = monitorRuns.get(id);
  if (!run || pausing.has(id)) return;
  const resume = run.status === "paused";
  pausing.add(id);
  button.disabled = true;
  if (!resume) {
    const capture = audioCaptures.get(id);
    if (capture) stopAudioCapture(id, capture);
  }
  renderAll();
  try {
    const updated = await request(`/api/monitors/${encodeURIComponent(id)}/${resume ? "resume" : "pause"}`, {method:"POST"});
    monitorRuns.set(updated.id, updated);
  } catch (error) {
    $("#formError").textContent = error.message;
  } finally {
    pausing.delete(id);
    renderAll();
  }
}

window.addEventListener("beforeunload", () => {
  for (const [id, capture] of audioCaptures) stopAudioCapture(id, capture);
});

document.querySelectorAll(".presets button").forEach(button => button.addEventListener("click", () => {
  $("#monitorGoal").value = button.dataset.goal;
  $("#targetURL").focus();
}));

let providerDefaults = {rules: {model:"", endpoint:"", capabilities:{vision:false}}};

function updateModelProviderFields(reset = false) {
  const provider = $("#modelProvider").value;
  const local = provider === "rules";
  const options = providerDefaults[provider] || {model:"", endpoint:"", models:[], vision_models:[], capabilities:{vision:false}};
  $("#modelNameLabel").classList.toggle("hidden", local);
  $("#modelEndpointLabel").classList.toggle("hidden", local);
  $("#modelKeyLabel").classList.toggle("hidden", local);
  $("#modelVisionLabel").classList.toggle("hidden", local);
  const modelOptions = (options.models || []).map(model => {
    const option = document.createElement("option");
    option.value = model;
    option.textContent = model;
    return option;
  });
  const customOption = document.createElement("option");
  customOption.value = "__custom__";
  customOption.textContent = "自定义模型 ID…";
  modelOptions.push(customOption);
  $("#modelPreset").replaceChildren(...modelOptions);
  if (reset) {
    $("#modelName").value = options.model;
    $("#modelPreset").value = options.model;
    $("#modelName").classList.add("hidden");
    $("#modelEndpoint").value = options.endpoint;
    $("#modelApiKey").value = "";
    $("#modelSupportsVision").checked = Boolean(options.vision_models?.includes(options.model) || options.capabilities?.vision);
  } else {
    const matchedModel = (options.models || []).find(model => model.toLowerCase() === $("#modelName").value.trim().toLowerCase());
    if (matchedModel) {
      $("#modelPreset").value = matchedModel;
      $("#modelName").classList.add("hidden");
    } else {
      $("#modelPreset").value = "__custom__";
      $("#modelName").classList.remove("hidden");
    }
  }
  $("#modelProviderNote").textContent = local
    ? "目标规划和事件判断改用本地规则。已单独配置的 MiniMax ASR 不受影响。"
    : "各提供方使用同一套调查工具与记忆策略，所选模型需支持工具调用。请按模型实际能力设置图片理解；不支持看图时，画面及聊天区目标会等待确认。ASR 配置独立。";
}

function modelProviderName(provider) {
  return ({minimax:"MiniMax",openai:"OpenAI",deepseek:"DeepSeek",rules:"本地规则"})[provider] || provider;
}

async function loadModelSettings() {
  const status = await request("/api/settings/model");
  globalModelSettings = status;
  providerDefaults = status.provider_options || providerDefaults;
  $("#modelProvider").value = providerDefaults[status.provider] ? status.provider : "rules";
  $("#modelName").value = status.model || providerDefaults[$("#modelProvider").value].model;
  $("#modelEndpoint").value = status.endpoint || providerDefaults[$("#modelProvider").value].endpoint;
  $("#modelApiKey").value = "";
  $("#modelSupportsVision").checked = Boolean(status.capabilities?.vision);
  updateModelProviderFields();
  const label = status.configured
    ? `${modelProviderName(status.provider)} · ${status.model} · 已配置${status.key_present ? "（密钥已保存在当前进程）" : ""}`
    : "当前使用本地规则，未配置大模型";
  $("#modelSettingsStatus").className = "settings-status";
  $("#modelSettingsStatus").textContent = status.quota_exhausted ? `${label} · 额度不可用，恢复后请重新保存配置` : label;
}

$("#modelSettingsOpen").addEventListener("click", async () => {
  $("#modelSettingsStatus").textContent = "正在读取当前设置…";
  $("#modelSettingsStatus").className = "settings-status";
  $("#modelSettingsDialog").showModal();
  try { await loadModelSettings(); }
  catch (error) {
    $("#modelSettingsStatus").textContent = error.message;
    $("#modelSettingsStatus").className = "settings-status error";
  }
});

function closeModelSettings() {
  $("#modelApiKey").value = "";
  $("#modelSettingsDialog").close();
}

$("#modelSettingsClose").addEventListener("click", closeModelSettings);
$("#modelSettingsCancel").addEventListener("click", closeModelSettings);
$("#modelSettingsDialog").addEventListener("click", event => {
  if (event.target === $("#modelSettingsDialog")) closeModelSettings();
});
$("#modelProvider").addEventListener("change", () => updateModelProviderFields(true));
$("#modelPreset").addEventListener("change", () => {
  const preset = $("#modelPreset").value;
  const options = providerDefaults[$("#modelProvider").value];
  if (preset === "__custom__") {
    $("#modelName").value = "";
    $("#modelName").classList.remove("hidden");
    $("#modelSupportsVision").checked = false;
    $("#modelName").focus();
    return;
  }
  $("#modelName").value = preset;
  $("#modelName").classList.add("hidden");
  $("#modelSupportsVision").checked = Boolean(options.vision_models?.some(name => name.toLowerCase() === preset.toLowerCase()));
});
$("#modelName").addEventListener("input", () => {
  const defaults = providerDefaults[$("#modelProvider").value];
  const model = $("#modelName").value.trim().toLowerCase();
  $("#modelPreset").value = "__custom__";
  $("#modelSupportsVision").checked = Boolean(defaults.vision_models?.some(name => name.toLowerCase() === model));
});
$("#modelSettingsForm").addEventListener("submit", async event => {
  event.preventDefault();
  const save = $("#modelSettingsSave");
  const provider = $("#modelProvider").value;
  save.disabled = true;
  $("#modelSettingsStatus").className = "settings-status";
  $("#modelSettingsStatus").textContent = "正在应用设置…";
  try {
    const status = await request("/api/settings/model", {
      method:"PUT",
      headers:{"Content-Type":"application/json"},
      body:JSON.stringify({provider, model:$("#modelName").value.trim(), endpoint:$("#modelEndpoint").value.trim(), api_key:$("#modelApiKey").value, supports_vision:$("#modelSupportsVision").checked})
    });
    globalModelSettings = status;
    if (detailRunID) renderMonitorDetail();
    $("#modelApiKey").value = "";
    $("#modelSettingsStatus").className = "settings-status success";
    $("#modelSettingsStatus").textContent = status.configured
      ? `已启用 ${modelProviderName(status.provider)} · ${status.model}；密钥只保存在当前服务进程中。`
      : "已切换到本地规则模式；独立配置的 ASR 不受影响。";
  } catch (error) {
    $("#modelSettingsStatus").className = "settings-status error";
    $("#modelSettingsStatus").textContent = error.message;
  } finally {
    save.disabled = false;
  }
});

async function openPlatformLogin(button) {
  const url = window.prompt("输入要登录的直播平台首页或直播间 URL");
  if (!url) return;
  button.disabled = true;
  try {
    await request("/api/browser/session", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({url})});
    button.textContent = "登录窗口已打开 ↗";
    button.title = "在专用浏览器中完成登录，随后可监控该平台的直播间";
  } catch (error) {
    $("#formError").textContent = error.message;
  } finally {
    button.disabled = false;
  }
}

$("#platformLogin").addEventListener("click", event => openPlatformLogin(event.currentTarget));
$("#allMonitors").addEventListener("click", () => closeMonitorDetail());

$("#monitorForm").addEventListener("submit", async event => {
  event.preventDefault();
  const submit = $("#openMonitor");
  const originalLabel = submit.querySelector("span")?.textContent.trim() || submit.textContent.trim();
  $("#formError").textContent = "";
  submit.disabled = true;
  const submitLabel = submit.querySelector("span");
  if (submitLabel) submitLabel.textContent = "正在连接…";
  else submit.textContent = "正在连接…";
  try {
    const run = await request("/api/monitors", {
      method:"POST",
      headers:{"Content-Type":"application/json"},
      body:JSON.stringify({target_url:$("#targetURL").value.trim(), goal:$("#monitorGoal").value.trim(), source_mode:"browser_ocr"})
    });
    monitorRuns.set(run.id, run);
    renderAll();
    $("#targetURL").value = "";
    $("#monitorGoal").value = "";
  } catch (error) {
    $("#formError").textContent = error.message;
  } finally {
    submit.disabled = false;
    const currentLabel = submit.querySelector("span");
    if (currentLabel) currentLabel.textContent = originalLabel || "开始监控";
    else submit.textContent = originalLabel || "开始监控";
  }
});

const updates = new EventSource("/api/events");
document.addEventListener("focusout", event => {
  const content = $("#monitorDetailContent");
  if (!content?.contains(event.target) || !event.target.matches("textarea, input, [contenteditable=true]")) return;
  if (event.relatedTarget?.closest("form") && event.relatedTarget.form === event.target.form) return;
  setTimeout(() => {
    if (!content.contains(document.activeElement) || !document.activeElement.matches("textarea, input, [contenteditable=true]")) renderAll();
  }, 0);
});
// SSE carries invalidation notices, not durable history. Reload the complete
// snapshot after reconnect to recover updates received while disconnected.
updates.addEventListener("ready", () => {
  streamConnected = true;
  refreshRuns().catch(() => {});
});
updates.addEventListener("update", () => {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => refreshRuns().catch(() => {}), 80);
});
updates.addEventListener("error", () => {
  streamConnected = false;
  renderAll();
});
// Keep the screen current while the event stream is reconnecting.
setInterval(() => {
  if (!streamConnected) refreshRuns().catch(() => {});
}, 5000);
refreshRuns().catch(error => { $("#formError").textContent = error.message; });

$("#shutdownApp")?.addEventListener("click", async event => {
  const button = event.currentTarget;
  if (!window.confirm("退出 LiveGuard 会停止所有监控并关闭本地服务，确定继续吗？")) return;
  button.disabled = true;
  button.textContent = "正在退出…";
  try {
    await request("/api/shutdown", {method:"POST"});
    $("#connectionLabel").textContent = "LiveGuard 已关闭";
  } catch (_) {
    // The process may close the connection before the browser receives 202.
    $("#connectionLabel").textContent = "LiveGuard 正在关闭";
  }
});
