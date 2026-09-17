const $ = selector => document.querySelector(selector);
const monitorRuns = new Map();
let refreshTimer = null;

const statusLabels = {running:"LIVE / 监听中",stopped:"STOPPED / 已停止",failed:"ERROR / 异常",completed:"DONE / 已结束"};
const decisionLabels = {waiting_more_evidence:"继续观察",confirmed:"已提醒",suppressed:"已忽略",needs_human:"需要用户"};
const eventLabels = {coupon_drop:"优惠活动",giveaway:"抽奖活动",major_announcement:"重要消息",game_moment:"游戏关键时刻",custom_goal:"自定义目标",human_verification:"安全验证"};
const toolLabels = {read_live_window:"读取证据窗口",query_recent_alerts:"检查重复提醒",reanalyze_clip:"复核字幕和画面",create_alert:"生成个人提醒",request_human_takeover:"请求用户处理"};

function escapeHTML(value) {
  const node = document.createElement("div");
  node.textContent = value || "";
  return node.innerHTML;
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

function renderAll() {
  const openDetails = new Set([...document.querySelectorAll(".monitor-card details[open]")].map(node => node.closest(".monitor-card")?.dataset.id));
  const runs = [...monitorRuns.values()].sort((a,b) => {
    if (a.status === "running" && b.status !== "running") return -1;
    if (b.status === "running" && a.status !== "running") return 1;
    return new Date(b.started_at) - new Date(a.started_at);
  });
  const grid = $("#monitorGrid");
  grid.innerHTML = "";
  $("#emptyRoom").classList.toggle("hidden", runs.length > 0);
  grid.classList.toggle("hidden", runs.length === 0);
  runs.forEach(run => grid.appendChild(renderCard(run, openDetails.has(run.id))));

  $("#activeCount").textContent = runs.filter(run => run.status === "running").length;
  $("#signalCount").textContent = runs.reduce((sum, run) => sum + (run.events?.length || 0), 0);
  $("#alertCount").textContent = runs.reduce((sum, run) => sum + (run.alerts?.length || 0), 0);
  $("#humanCount").textContent = runs.reduce((sum, run) => sum + (run.investigations || []).filter(item => item.status === "needs_human").length, 0);
}

function renderCard(run, detailOpen) {
  const fragment = $("#monitorCard").content.cloneNode(true);
  const card = fragment.querySelector(".monitor-card");
  const room = run.rooms?.[0] || {name:run.name,status:"listening"};
  const active = run.status === "running";
  const needsHuman = (run.investigations || []).some(item => item.status === "needs_human");
  card.dataset.id = run.id;
  card.classList.toggle("active", active);
  card.classList.toggle("needs-human", needsHuman);
  card.querySelector(".room-name").textContent = room.name || run.name;
  const link = card.querySelector(".room-link");
  link.textContent = run.target_url;
  link.href = run.target_url;
  card.querySelector(".window-status").textContent = needsHuman ? "ACTION / 需要处理" : (statusLabels[run.status] || run.status);
  card.querySelector(".watch-goal p").textContent = run.goal;
  card.querySelector(".screen-id").textContent = `MON-${run.id.slice(-6).toUpperCase()}`;

  const latest = run.events?.[run.events.length - 1];
  const signalBox = card.querySelector(".latest-signal");
  if (latest) {
    signalBox.querySelector("span").textContent = `${latest.source.toUpperCase()} · ${timeLabel(latest.observed_at)}`;
    signalBox.querySelector("p").textContent = latest.text;
  }

  const alertSlot = card.querySelector(".alert-slot");
  const latestAlert = run.alerts?.[run.alerts.length - 1];
  if (latestAlert) {
    const evidence = latestAlert.evidence?.map(item => `${item.source.toUpperCase()}「${item.text}」`).join(" + ") || "";
    alertSlot.innerHTML = `<article class="personal-alert"><div><b>● 重要时刻已命中</b><time>${timeLabel(latestAlert.created_at)}</time></div><p>${escapeHTML(latestAlert.summary)}</p><small>证据：${escapeHTML(evidence)} · 可信度 ${Math.round(latestAlert.confidence * 100)}%</small></article>`;
  }

  card.querySelector(".card-signals").textContent = run.events?.length || 0;
  card.querySelector(".card-investigations").textContent = run.investigations?.length || 0;
  card.querySelector(".card-alerts").textContent = run.alerts?.length || 0;
  card.querySelector(".source-mode").textContent = run.source_mode === "demo_replay" ? "DEMO · ASR/OCR 回放" : "LIVE · 实时信号";

  const decisions = card.querySelector(".decision-list");
  const investigations = [...(run.investigations || [])].reverse();
  decisions.innerHTML = investigations.length ? investigations.map(inv => {
    const tools = (inv.steps || []).map(step => toolLabels[step.tool] || step.tool).join(" → ");
    return `<article class="decision"><div class="decision-head"><b>${escapeHTML(eventLabels[inv.event_type] || "目标信号")}</b><span class="decision-state ${escapeHTML(inv.status)}">${escapeHTML(decisionLabels[inv.status] || inv.status)}</span></div><p>${escapeHTML(inv.conclusion)}</p><div class="tool-path">${escapeHTML(tools || "尚未调用工具")}</div></article>`;
  }).join("") : '<div class="decision"><p>还没有发现与你需求相关的信号。</p></div>';
  card.querySelector("details").open = detailOpen;

  const stop = card.querySelector(".stop-monitor");
  stop.disabled = !active;
  stop.textContent = active ? "停止监听" : "监听已停止";
  stop.addEventListener("click", () => stopMonitor(run.id, stop));
  return fragment;
}

async function refreshRuns() {
  const summaries = await request("/api/monitors");
  // Stopped runs remain durable for audit/recovery, but a closed monitor should
  // disappear from the active control room instead of leaving a dead card.
  const personal = summaries.filter(run => run.target_url && !["stopped", "completed"].includes(run.status));
  const details = await Promise.all(personal.slice(0, 20).map(run => request(`/api/monitors/${encodeURIComponent(run.id)}`)));
  monitorRuns.clear();
  details.forEach(run => monitorRuns.set(run.id, run));
  renderAll();
}

async function stopMonitor(id, button) {
  button.disabled = true;
  try {
    const run = await request(`/api/monitors/${encodeURIComponent(id)}/stop`, {method:"POST"});
    monitorRuns.set(run.id, run);
    renderAll();
  } catch (error) {
    $("#formError").textContent = error.message;
    button.disabled = false;
  }
}

document.querySelectorAll(".presets button").forEach(button => button.addEventListener("click", () => {
  $("#monitorGoal").value = button.dataset.goal;
  $("#targetURL").focus();
}));

$("#monitorForm").addEventListener("submit", async event => {
  event.preventDefault();
  const submit = $("#openMonitor");
  $("#formError").textContent = "";
  submit.disabled = true;
  submit.querySelector("span").textContent = "正在连接…";
  try {
    const run = await request("/api/monitors", {
      method:"POST",
      headers:{"Content-Type":"application/json"},
      body:JSON.stringify({target_url:$("#targetURL").value.trim(), goal:$("#monitorGoal").value.trim()})
    });
    monitorRuns.set(run.id, run);
    renderAll();
    $("#targetURL").value = "";
    $("#monitorGoal").value = "";
  } catch (error) {
    $("#formError").textContent = error.message;
  } finally {
    submit.disabled = false;
    submit.querySelector("span").textContent = "打开监控窗口";
  }
});

const updates = new EventSource("/api/events");
updates.addEventListener("update", () => {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => refreshRuns().catch(() => {}), 80);
});
refreshRuns().catch(error => { $("#formError").textContent = error.message; });
