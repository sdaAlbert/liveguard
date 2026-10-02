const $ = selector => document.querySelector(selector);
const monitorRuns = new Map();
const audioCaptures = new Map();
const goalDrafts = new Map();
let refreshTimer = null;

const statusLabels = {running:"监听中",stopped:"已停止",failed:"异常",completed:"已结束"};
const decisionLabels = {waiting_more_evidence:"继续观察",confirmed:"已提醒",suppressed:"已忽略",needs_human:"需要用户",failed:"判断失败·继续监听"};
const eventLabels = {coupon_drop:"优惠活动",giveaway:"抽奖活动",major_announcement:"重要消息",game_moment:"游戏关键时刻",chat_match:"聊天命中",custom_goal:"关注内容",human_verification:"安全验证"};
const toolLabels = {read_live_window:"读取证据窗口",read_recent_evidence:"读取近期信号",read_transcript_window:"读取口播转写",query_recent_alerts:"检查重复提醒",search_recent_alerts:"检查同类提醒",inspect_live_frame:"按需查看画面",analyze_signal_window:"分析文字重复与提示",finish_review:"结束本次判断",reanalyze_clip:"复核字幕和画面",reanalyze_evidence:"复核证据",review_unexpected_event:"判断关注目标与重要内容",create_alert:"生成个人提醒",request_human_takeover:"请求用户处理"};

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
  $("#roomCount").textContent = runs.filter(run => run.status === "running").length;
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
  card.querySelector(".window-status").textContent = needsHuman ? "需要处理" : (statusLabels[run.status] || run.status);
  const link = card.querySelector(".room-link");
  link.textContent = run.target_url;
  link.href = run.target_url;
  const goalInput = card.querySelector(".goal-input");
  goalInput.value = goalDrafts.has(run.id) ? goalDrafts.get(run.id) : run.goal;
  goalInput.disabled = !active;
  goalInput.addEventListener("input", () => goalDrafts.set(run.id, goalInput.value));
  const goalForm = card.querySelector(".goal-edit");
  const goalSave = card.querySelector(".goal-save");
  goalSave.disabled = !active;
  goalForm.addEventListener("submit", event => saveGoal(event, run.id, goalInput, goalSave, card.querySelector(".goal-status")));
  const plan = run.plan || {};
  card.querySelector(".plan-summary").textContent = plan.summary || "";

  const latest = run.events?.[run.events.length - 1];
  const signalBox = card.querySelector(".latest-signal");
  if (latest) {
    signalBox.textContent = `最近信号（${latest.source.toUpperCase()}）：${latest.text}`;
  } else {
    signalBox.textContent = room.last_error || "还没有收到信号";
  }
  const backpressure = run.review_backpressure_count ? `AI 判断繁忙，已有 ${run.review_backpressure_count} 条有效信号先保存、暂未做语义复核` : "";
  card.querySelector(".capture-error").textContent = room.last_error || backpressure || (run.history_truncated ? "监控时间较长，已只保留最近一段记录" : "");

  const alerts = [...(run.alerts || [])].slice(-10).reverse();
  const alertList = card.querySelector(".alert-list");
  alertList.innerHTML = alerts.length ? alerts.map(alert => `<article class="personal-alert"><time>${timeLabel(alert.created_at)} · ${escapeHTML(eventLabels[alert.event_type?.replace(/^proactive_/, "")] || "提醒")}</time><p>${escapeHTML(alert.summary)}</p></article>`).join("") : '<p class="no-alert">暂无提醒</p>';

  card.querySelector(".card-signals").textContent = run.events?.length || 0;
  card.querySelector(".card-investigations").textContent = run.investigations?.length || 0;

  const decisions = card.querySelector(".decision-list");
  const investigations = [...(run.investigations || [])].reverse();
  decisions.innerHTML = investigations.length ? investigations.map(inv => {
    const tools = (inv.steps || []).map(step => `${toolLabels[step.tool] || step.tool}${step.outcome ? `（${step.outcome}）` : ""}`).join(" → ");
    const category = (inv.event_type || "").replace(/^proactive_/, "");
    return `<article class="decision"><div class="decision-head"><b>${escapeHTML(eventLabels[category] || "目标信号")}</b><span class="decision-state ${escapeHTML(inv.status)}">${escapeHTML(decisionLabels[inv.status] || inv.status)}</span></div><p>${escapeHTML(inv.conclusion)}</p><div class="tool-path">${escapeHTML(tools || "尚未调用工具")}</div></article>`;
  }).join("") : '<div class="decision"><p>还没有发现与你需求相关的信号。</p></div>';
  card.querySelector("details").open = detailOpen;

  const stop = card.querySelector(".stop-monitor");
  stop.disabled = !active;
  stop.textContent = active ? "停止" : "已停止";
  stop.addEventListener("click", () => stopMonitor(run.id, stop));
  const audioButton = card.querySelector(".audio-capture");
  audioButton.disabled = !active;
  const hasASRSignal = (run.events || []).some(event => event.source === "asr");
  audioButton.textContent = audioCaptures.has(run.id)
    ? (hasASRSignal ? "停止口播识别（已收到转写）" : "停止口播识别")
    : (plan.recommend_asr ? "开启口播识别（建议）" : "开启口播识别");
  audioButton.addEventListener("click", () => toggleAudioCapture(run.id, audioButton));
  return fragment;
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
    const capture = {stream, recorder:null, stopCurrent:null};
    audioCaptures.set(id, capture);
    // The display video track is only required by Chrome's share picker. We
    // record and upload audio only, so its lifecycle must not stop ASR.
    audioTracks.forEach(track => track.addEventListener("ended", () => {
      if (audioCaptures.get(id) !== capture) return;
      stopAudioCapture(id, capture);
      $("#formError").textContent = "口播识别已停止：Chrome 的共享音频轨道结束了。请重新共享直播标签页音频。";
      renderAll();
    }, {once:true}));
    $("#formError").textContent = "";
    renderAll();
    captureAudioLoop(id, capture).catch(error => {
      if (audioCaptures.get(id) === capture) {
        stopAudioCapture(id, capture);
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
  capture.stopCurrent?.();
  capture.stream.getTracks().forEach(track => track.stop());
}

function recordAudioChunk(capture) {
  const audioStream = new MediaStream(capture.stream.getAudioTracks());
  const mimeType = MediaRecorder.isTypeSupported("audio/webm;codecs=opus") ? "audio/webm;codecs=opus" : "audio/webm";
  const recorder = new MediaRecorder(audioStream, {mimeType});
  const chunks = [];
  const observedAt = new Date().toISOString();
  return new Promise((resolve, reject) => {
    let timer;
    recorder.ondataavailable = event => { if (event.data.size) chunks.push(event.data); };
    recorder.onerror = () => reject(new Error("浏览器录制音频片段失败"));
    recorder.onstop = () => {
      clearTimeout(timer);
      capture.recorder = null;
      capture.stopCurrent = null;
      resolve({blob:new Blob(chunks, {type:mimeType}), observedAt});
    };
    capture.recorder = recorder;
    capture.stopCurrent = () => { if (recorder.state !== "inactive") recorder.stop(); };
    recorder.start();
    timer = setTimeout(() => { if (recorder.state !== "inactive") recorder.stop(); }, 6000);
  });
}

async function captureAudioLoop(id, capture) {
  while (audioCaptures.get(id) === capture) {
    const {blob, observedAt} = await recordAudioChunk(capture);
    if (audioCaptures.get(id) !== capture) break;
    if (blob.size < 1024) continue;
    const body = new FormData();
    body.append("audio", blob, "live-chunk.webm");
    body.append("observed_at", observedAt);
    const eventID = crypto.randomUUID();
    try {
      const run = await request(`/api/monitors/${encodeURIComponent(id)}/audio`, {method:"POST", body, headers:{"Idempotency-Key":eventID}});
      monitorRuns.set(run.id, run);
      $("#formError").textContent = "";
      renderAll();
    } catch (error) {
      // A silent/failed chunk is not a reason to revoke the user's browser
      // share. Keep listening and expose the error so the next chunk can work.
      $("#formError").textContent = `本段口播识别失败，仍在继续监听：${error.message}`;
      renderAll();
      await new Promise(resolve => setTimeout(resolve, 1500));
    }
  }
}

async function saveGoal(event, id, input, button, status) {
  event.preventDefault();
  const goal = input.value.trim();
  if (goal.length < 2 || goal.length > 240) {
    status.textContent = "请输入 2-240 个字符";
    return;
  }
  button.disabled = true;
  status.textContent = "Agent 正在更新监控策略…";
  try {
    const run = await request(`/api/monitors/${encodeURIComponent(id)}/goal`, {
      method:"PUT", headers:{"Content-Type":"application/json"}, body:JSON.stringify({goal})
    });
    monitorRuns.set(run.id, run);
    goalDrafts.delete(id);
    status.textContent = "需求已更新";
    renderAll();
  } catch (error) {
    status.textContent = error.message;
    button.disabled = false;
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
  const capture = audioCaptures.get(id);
  if (capture) stopAudioCapture(id, capture);
  try {
    const run = await request(`/api/monitors/${encodeURIComponent(id)}/stop`, {method:"POST"});
    monitorRuns.set(run.id, run);
    renderAll();
  } catch (error) {
    $("#formError").textContent = error.message;
    button.disabled = false;
  }
}

window.addEventListener("beforeunload", () => {
  for (const [id, capture] of audioCaptures) stopAudioCapture(id, capture);
});

document.querySelectorAll(".presets button").forEach(button => button.addEventListener("click", () => {
  $("#monitorGoal").value = button.dataset.goal;
  $("#targetURL").focus();
}));

async function openDouyinLogin(button, automatic = false) {
  button.disabled = true;
  try {
    await request("/api/browser/session", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({url:"https://www.douyin.com/"})});
    button.textContent = "抖音窗口已打开 · 登录后直接开始监控";
  } catch (error) {
    if (!automatic) $("#formError").textContent = error.message;
    else button.textContent = "自动打开失败，请点击这里登录";
  } finally {
    button.disabled = false;
  }
}

$("#douyinLogin").addEventListener("click", event => openDouyinLogin(event.currentTarget));

// Open the dedicated Douyin profile once when the control room is first used.
// The backend launches Chrome, so this does not depend on browser popup rules.
request("/api/browser/session").then(state => {
  if (state.configured && !state.open && !sessionStorage.getItem("liveguard-login-launched")) {
    sessionStorage.setItem("liveguard-login-launched", "1");
    openDouyinLogin($("#douyinLogin"), true);
  }
}).catch(() => {});

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
updates.addEventListener("update", () => {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => refreshRuns().catch(() => {}), 80);
});
refreshRuns().catch(error => { $("#formError").textContent = error.message; });
