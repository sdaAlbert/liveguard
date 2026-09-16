const $ = selector => document.querySelector(selector);
let selectedTaskId = new URLSearchParams(location.search).get("task");
let sessionState = {open: false, inspecting: false};

const statusText = {
  queued: "等待中", planning: "准备中", running: "检查中", needs_human: "需要人工处理",
  completed: "已完成", failed: "执行失败", cancelled: "已取消"
};
const verdictText = {passed: "正常", failed: "发现异常", unverified: "需要复核", needs_human: "需要人工处理"};

function escapeHTML(value = "") {
  return String(value).replace(/[&<>'"]/g, ch => ({"&":"&amp;","<":"&lt;",">":"&gt;","'":"&#39;",'"':"&quot;"}[ch]));
}

async function request(path, options) {
  const response = await fetch(path, options);
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `请求失败：${response.status}`);
  return data;
}

function taskResult(task) {
  if (["queued", "planning", "running"].includes(task.status)) return {key: task.status, label: statusText[task.status]};
  if (task.status === "needs_human") return {key: "needs_human", label: "需要人工处理"};
  if (task.status === "failed") return {key: "failed", label: "执行失败"};
  const verdict = task.report?.verdict || "unverified";
  return {key: verdict, label: verdictText[verdict] || "需要复核"};
}

function hostLabel(rawURL) {
  try { const parsed = new URL(rawURL); return parsed.hostname + parsed.pathname; }
  catch { return rawURL; }
}

function friendlyError(error = "") {
  if (!error) return "";
  if (error.includes("关闭窗口")) return "登录窗口仍然打开，请关闭后重新巡检";
  if (error.includes("超过") || error.includes("deadline")) return "页面加载超时，请稍后重新巡检";
  if (error.includes("Chrome") || error.includes("chrome")) return "浏览器没有成功读取页面，请重新巡检";
  return error.replace(/^browser inspection:\s*/i, "");
}

function friendlySummary(summary = "") {
  if (summary.includes("活动入口缺失")) return "页面没有发现活动入口";
  if (summary.includes("登录或安全验证")) return "需要登录或完成人工验证";
  return summary;
}

async function loadTasks() {
  const tasks = await request("/api/tasks");
  $("#runningCount").textContent = tasks.filter(t => ["queued", "planning", "running"].includes(t.status)).length;
  $("#passedCount").textContent = tasks.filter(t => t.report?.verdict === "passed").length;
  $("#attentionCount").textContent = tasks.filter(t => t.status === "failed" || t.status === "needs_human" || ["failed", "unverified"].includes(t.report?.verdict)).length;
  const recent = tasks.slice(0, 6);
  $("#taskList").innerHTML = recent.length ? recent.map(task => {
    const result = taskResult(task);
    const summary = friendlySummary(task.report?.summary) || friendlyError(task.error) || statusText[task.status];
    return `<button class="task" data-id="${escapeHTML(task.id)}"><span class="task-main"><b>${escapeHTML(hostLabel(task.url))}</b><small>${escapeHTML(summary)}</small></span><span class="result-badge ${escapeHTML(result.key)}">${escapeHTML(result.label)}</span></button>`;
  }).join("") : '<div class="empty">还没有巡检记录</div>';
  document.querySelectorAll(".task").forEach(button => button.addEventListener("click", () => showTask(button.dataset.id)));
  if (selectedTaskId) await showTask(selectedTaskId, false);
}

function friendlyEvents(task) {
  const labels = {
    created: "巡检已提交",
    browser: task.use_authenticated_session ? "正在使用抖音登录态打开直播间" : "正在打开直播间",
    evidence: "页面截图和内容已保存",
    report: friendlySummary(task.report?.summary) || "巡检完成",
    error: friendlyError(task.error),
    cancelled: "巡检已取消"
  };
  return (task.events || []).filter(event => labels[event.type]).map(event => ({at: event.at, message: labels[event.type]}));
}

async function showTask(id, reveal = true) {
  selectedTaskId = id;
  const task = await request(`/api/tasks/${encodeURIComponent(id)}`);
  if (reveal) $("#detail").classList.remove("hidden");
  const result = taskResult(task);
  $("#detailStatus").textContent = result.label;
  $("#detailStatus").className = `result-badge ${result.key}`;
  $("#detailSummary").textContent = friendlySummary(task.report?.summary) || friendlyError(task.error) || statusText[task.status];
  $("#detailURL").textContent = task.url;
  $("#detailURL").href = task.url;
  $("#detailTime").textContent = new Date(task.updated_at).toLocaleString("zh-CN", {hour12:false});
  $("#resultContract").innerHTML = `<span>直播状态：${task.expected_live_status === "live" ? "应该正在直播" : task.expected_live_status === "offline" ? "应该未开播" : "不限定"}</span><span>${task.expected_texts?.length ? `确认内容：${escapeHTML(task.expected_texts.join("、"))}` : "未指定页面文案"}</span><span>${task.use_authenticated_session ? "使用抖音登录态" : "访客方式检查"}</span>`;

  const terminal = !["queued", "planning", "running"].includes(task.status);
  $("#retryTask").classList.toggle("hidden", !terminal);
  const help = $("#failureHelp");
  if (task.status === "failed" || task.status === "needs_human") {
    help.classList.remove("hidden");
    help.innerHTML = `<b>下一步怎么做</b><span>${task.status === "needs_human" ? "打开登录窗口，在同一直播间完成人工验证；关闭窗口后点击重新巡检。" : escapeHTML(friendlyError(task.error))}</span>`;
  } else {
    help.classList.add("hidden");
  }

  $("#screenshot").innerHTML = task.report?.screenshot ? `<img src="${escapeHTML(task.report.screenshot)}?v=${task.version}" alt="直播间页面截图">` : "<span>完成后显示截图</span>";
  const checks = task.report?.checks || [];
  $("#checks").innerHTML = checks.length ? checks.map(check => `<article class="check"><span class="check-icon ${check.status}">${check.status === "passed" ? "✓" : check.status === "failed" ? "!" : "?"}</span><div><b>${escapeHTML(check.label)}</b><p>${escapeHTML(check.observed)}</p></div></article>`).join("") : '<div class="empty small">正在等待结果</div>';
  $("#timeline").innerHTML = friendlyEvents(task).reverse().map(item => `<li><time>${new Date(item.at).toLocaleTimeString("zh-CN", {hour12:false})}</time>${escapeHTML(item.message)}</li>`).join("");
}

function updateSubmitState() {
  const blocked = $("#useSession").checked && (sessionState.open || sessionState.inspecting);
  $("#submitTask").disabled = blocked;
  if (blocked) $("#formError").textContent = sessionState.open ? "请先关闭抖音登录窗口，再开始巡检" : "另一个登录态巡检正在执行，请稍候";
  else if ($("#formError").textContent.includes("登录窗口") || $("#formError").textContent.includes("正在执行")) $("#formError").textContent = "";
}

async function loadBrowserSession() {
  sessionState = await request("/api/browser/session");
  const status = $("#sessionStatus");
  if (sessionState.open) {
    status.textContent = "登录窗口已打开，登录完成后请关闭窗口";
    status.className = "warning";
  } else if (sessionState.inspecting) {
    status.textContent = "正在使用登录态巡检";
    status.className = "warning";
  } else {
    status.textContent = "登录态可以使用";
    status.className = "ready";
  }
  $("#openSession").disabled = sessionState.open || sessionState.inspecting;
  $("#openSession").textContent = sessionState.open ? "窗口已打开" : sessionState.inspecting ? "巡检中" : "打开登录窗口";
  updateSubmitState();
}

$("#openSession").addEventListener("click", async event => {
  event.currentTarget.disabled = true;
  $("#formError").textContent = "";
  try {
    await request("/api/browser/session", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({url:$("#url").value || "https://www.douyin.com/"})});
    await loadBrowserSession();
  } catch (error) { $("#formError").textContent = error.message; event.currentTarget.disabled = false; }
});

$("#useSession").addEventListener("change", updateSubmitState);

$("#taskForm").addEventListener("submit", async event => {
  event.preventDefault();
  $("#formError").textContent = "";
  updateSubmitState();
  if ($("#submitTask").disabled) return;
  $("#submitTask").disabled = true;
  try {
    const expectedTexts = $("#expectedTexts").value.split(/[，,\n]/).map(text => text.trim()).filter(Boolean);
    const body = {url:$("#url").value, objective:"检查直播间是否可以正常访问，识别直播状态，并验证指定页面内容。", expected_texts:expectedTexts, expected_live_status:$("#expectedLiveStatus").value, use_authenticated_session:$("#useSession").checked};
    const task = await request("/api/tasks", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(body)});
    selectedTaskId = task.id;
    await loadTasks();
    await showTask(task.id);
    $("#detail").scrollIntoView({behavior:"smooth", block:"start"});
  } catch (error) { $("#formError").textContent = error.message; }
  finally { updateSubmitState(); }
});

$("#retryTask").addEventListener("click", async event => {
  if (!selectedTaskId) return;
  event.currentTarget.disabled = true;
  try {
    const task = await request(`/api/tasks/${encodeURIComponent(selectedTaskId)}/retry`, {method:"POST"});
    selectedTaskId = task.id;
    await loadTasks();
    await showTask(task.id);
  } catch (error) { $("#failureHelp").classList.remove("hidden"); $("#failureHelp").textContent = error.message; }
  finally { event.currentTarget.disabled = false; }
});

$("#refresh").addEventListener("click", loadTasks);
$("#closeDetail").addEventListener("click", () => { $("#detail").classList.add("hidden"); selectedTaskId = null; });

const events = new EventSource("/api/events");
events.addEventListener("update", () => loadTasks().catch(() => {}));
loadTasks().catch(error => { $("#taskList").innerHTML = `<div class="empty">${escapeHTML(error.message)}</div>`; });
loadBrowserSession().catch(error => { $("#sessionStatus").textContent = error.message; });
setInterval(() => loadBrowserSession().catch(() => {}), 1500);
