const $ = (selector) => document.querySelector(selector);
let selectedTaskId = null;
let sandboxPolling = null;
const pageParams = new URLSearchParams(location.search);
selectedTaskId = pageParams.get("task");
if (pageParams.has("eval")) document.body.classList.add("eval-focus");
if (selectedTaskId) {
  document.body.classList.add("task-focus");
  $("#detail").classList.remove("hidden");
}

const statusText = {
  queued: "等待中", planning: "规划中", running: "执行中", needs_human: "需要人工",
  completed: "已完成", failed: "失败", cancelled: "已取消"
};

function escapeHTML(value = "") {
  return String(value).replace(/[&<>'"]/g, ch => ({"&":"&amp;","<":"&lt;",">":"&gt;","'":"&#39;",'"':"&quot;"}[ch]));
}

async function request(path, options) {
  const response = await fetch(path, options);
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `请求失败：${response.status}`);
  return data;
}

async function loadTasks() {
  const tasks = await request("/api/tasks");
  $("#totalCount").textContent = tasks.length;
  $("#runningCount").textContent = tasks.filter(t => ["queued","planning","running"].includes(t.status)).length;
  $("#attentionCount").textContent = tasks.filter(t => t.status === "needs_human").length;
  $("#taskList").innerHTML = tasks.length ? tasks.map((task, index) => `
    <button class="task" data-id="${escapeHTML(task.id)}">
      <span class="task-index">${String(index + 1).padStart(2, "0")}</span>
      <span><strong>${escapeHTML(task.objective)}</strong><small>${escapeHTML(task.url)}</small></span>
      <span class="status ${task.status}">${statusText[task.status] || task.status}</span>
    </button>`).join("") : '<div class="empty">尚无巡检任务</div>';
  document.querySelectorAll(".task").forEach(button => button.addEventListener("click", () => showTask(button.dataset.id)));
  if (selectedTaskId) await showTask(selectedTaskId, false);
}

const sandboxStatusText = {queued:"排队中", running:"执行中", passed:"策略通过", failed:"验收失败"};

function formatDuration(run) {
  if (run.status === "queued") return "WAITING";
  if (run.status === "running") return "RUNNING";
  return `${run.duration_ms || 0} ms`;
}

function renderSandboxRun(run) {
  const exit = run.timed_out ? "TIMEOUT" : (run.exit_code ?? "—");
  const cleanup = run.cleaned ? "CLEAN" : (run.status === "queued" || run.status === "running" ? "PENDING" : "LEAK");
  const output = run.output || run.error || "等待 Docker 返回执行证据…";
  return `<article class="sandbox-run ${escapeHTML(run.status)}">
    <div class="sandbox-run-title"><span class="status ${escapeHTML(run.status)}">${escapeHTML(sandboxStatusText[run.status] || run.status)}</span><b>${escapeHTML(run.scenario_label)}</b><small>${escapeHTML(run.id)}</small></div>
    <div class="sandbox-facts"><span><small>DURATION</small><b>${escapeHTML(formatDuration(run))}</b></span><span><small>EXIT</small><b>${escapeHTML(exit)}</b></span><span><small>CONTAINER</small><b>${cleanup}</b></span></div>
    ${run.trace_id ? `<a class="run-trace" href="http://127.0.0.1:16686/trace/${encodeURIComponent(run.trace_id)}" target="_blank" rel="noreferrer">TRACE ${escapeHTML(run.trace_id)}</a>` : ""}
    <pre>${escapeHTML(output)}</pre>
  </article>`;
}

async function loadSandboxRuns() {
  const runs = await request("/api/sandbox/runs");
  $("#sandboxRuns").innerHTML = runs.length ? runs.map(renderSandboxRun).join("") : '<div class="sandbox-empty">DOCKER READY? 点击「运行全部验收」给出证据。</div>';
  const active = runs.filter(run => ["queued", "running"].includes(run.status)).length;
  const latestSuite = runs.find(run => run.suite_id)?.suite_id;
  const latest = latestSuite ? runs.filter(run => run.suite_id === latestSuite) : runs.slice(0, 4);
  const passed = latest.filter(run => run.status === "passed").length;
  $("#sandboxSummary").textContent = active ? `${active} 个容器正在执行` : (latest.length ? `${passed}/${latest.length} 项策略通过` : "等待第一次验收");
  if (active && !sandboxPolling) sandboxPolling = setInterval(() => loadSandboxRuns().catch(showSandboxError), 700);
  if (!active && sandboxPolling) { clearInterval(sandboxPolling); sandboxPolling = null; }
}

async function loadInfraStatus() {
  const status = await request("/api/infra/status");
  if (status.mode === "durable") {
    $("#infraMode").textContent = `PG ${status.postgres.toUpperCase()} · REDIS ${status.redis.toUpperCase()} · WORKER ${status.worker.toUpperCase()} · OUTBOX ${status.outbox} · PENDING ${status.pending} · DLQ ${status.dead_letter}`;
  } else {
    $("#infraMode").textContent = `IN-MEMORY MODE · ${status.stored_runs} RUNS`;
  }
}

function renderAgentEval(report) {
  const percent = Math.round((report.pass_rate || 0) * 100);
  $("#evalMode").textContent = `${String(report.mode || "unknown").toUpperCase()} · ${report.passed}/${report.total} PASSED`;
  $("#evalMetrics").innerHTML = `<span><small>PASS RATE</small><b>${percent}%</b></span><span><small>AVG LATENCY</small><b>${escapeHTML(report.average_ms)} ms</b></span><span><small>MODEL TOKENS</small><b>${escapeHTML((report.input_tokens || 0) + (report.output_tokens || 0))}</b></span>`;
  $("#evalCases").innerHTML = (report.cases || []).map(item => `<article class="eval-case ${item.passed ? "passed" : "failed"}">
    <div><span class="status ${item.passed ? "passed" : "failed"}">${item.passed ? "PASS" : "FAIL"}</span><b>${escapeHTML(item.id)}</b><small>${escapeHTML(item.source)}</small></div>
    <p>${escapeHTML(item.description)}</p>
    <dl><dt>EXPECTED</dt><dd>${escapeHTML(item.expected_tool || "NO TOOL")} / ${item.expected_approved ? "ALLOW" : "DENY"}</dd><dt>ACTUAL</dt><dd>${escapeHTML(item.actual_tool || "NO TOOL")} / ${item.actual_approved ? "ALLOW" : "DENY"}</dd><dt>POLICY</dt><dd>${escapeHTML(item.policy_reason)}</dd></dl>
    ${item.error || item.model_error ? `<pre>${escapeHTML(item.error || item.model_error)}</pre>` : ""}
  </article>`).join("");
}

async function loadAgentEval(silent = true) {
  try { renderAgentEval(await request("/api/evals/agent")); }
  catch (error) { if (!silent) $("#evalError").textContent = error.message; }
}

$("#runAgentEval").addEventListener("click", async event => {
  event.currentTarget.disabled = true;
  $("#evalError").textContent = "";
  $("#evalMode").textContent = "评测运行中…";
  try { renderAgentEval(await request("/api/evals/agent", {method:"POST"})); }
  catch (error) { $("#evalError").textContent = error.message; }
  finally { event.currentTarget.disabled = false; }
});

function showSandboxError(error) { $("#sandboxError").textContent = error.message; }

async function submitSandbox(path, body) {
  $("#sandboxError").textContent = "";
  const idempotencyKey = `ui-${Date.now()}-${crypto.randomUUID?.() || Math.random().toString(16).slice(2)}`;
  await request(path, {method:"POST", headers:{"Content-Type":"application/json", "Idempotency-Key":idempotencyKey}, body: body ? JSON.stringify(body) : undefined});
  await loadSandboxRuns();
}

$("#runSandboxSuite").addEventListener("click", async event => {
  event.currentTarget.disabled = true;
  try { await submitSandbox("/api/sandbox/suite"); }
  catch (error) { showSandboxError(error); }
  finally { event.currentTarget.disabled = false; }
});

document.querySelectorAll("[data-sandbox]").forEach(button => button.addEventListener("click", async () => {
  button.disabled = true;
  try { await submitSandbox("/api/sandbox/runs", {scenario:button.dataset.sandbox}); }
  catch (error) { showSandboxError(error); }
  finally { button.disabled = false; }
}));

async function showTask(id, reveal = true) {
  selectedTaskId = id;
  const task = await request(`/api/tasks/${encodeURIComponent(id)}`);
  if (reveal) $("#detail").classList.remove("hidden");
  $("#detailId").textContent = task.id;
  $("#detailObjective").textContent = task.objective;
  $("#detailURL").textContent = task.url;
  $("#detailURL").href = task.url;
  const traceLink = $("#detailTrace");
  if (task.trace_id) {
    traceLink.textContent = `TRACE ${task.trace_id} ↗`;
    traceLink.href = `http://127.0.0.1:16686/trace/${encodeURIComponent(task.trace_id)}`;
    traceLink.classList.remove("hidden");
  } else {
    traceLink.classList.add("hidden");
  }
  $("#detailStatus").textContent = statusText[task.status] || task.status;
  $("#detailStatus").className = `status ${task.status}`;
  $("#timeline").innerHTML = (task.events || []).slice().reverse().map(event => `<li><time>${new Date(event.at).toLocaleString()}</time><b>${escapeHTML(event.type)}</b><div>${escapeHTML(event.message)}</div></li>`).join("");
  const report = task.report;
  $("#screenshot").innerHTML = report?.screenshot ? `<img src="${escapeHTML(report.screenshot)}?v=${task.version}" alt="浏览器巡检截图">` : "<span>任务完成后显示截图</span>";
  const toolCalls = report?.tool_calls || [];
  const audit = report?.decision;
  const decisionHTML = audit ? `<section class="decision-audit"><h3>TOOL DECISION AUDIT</h3><div class="audit-grid"><span><small>SOURCE</small><b>${escapeHTML(audit.source)}</b></span><span><small>REQUEST</small><b>${escapeHTML(audit.requested_tool || "NO TOOL")}</b></span><span><small>POLICY</small><b>${audit.approved ? "APPROVED" : "NOT APPROVED"}</b></span><span><small>LATENCY</small><b>${escapeHTML(audit.latency_ms)} ms</b></span></div><p>${escapeHTML(audit.policy_reason)}</p>${audit.response_id ? `<small>RESPONSE ${escapeHTML(audit.response_id)} · CALL ${escapeHTML(audit.call_id || "—")} · TOKENS ${(audit.input_tokens || 0) + (audit.output_tokens || 0)}</small>` : ""}${audit.model_error ? `<pre>${escapeHTML(audit.model_error)}</pre>` : ""}</section>` : "";
  const toolHTML = toolCalls.length ? `<section class="tool-chain"><h3>AGENT TOOL LOOP</h3>${toolCalls.map(tool => `<article><div><span class="status ${escapeHTML(tool.status)}">${escapeHTML(tool.status)}</span><b>${escapeHTML(tool.name)}</b><small>${escapeHTML(tool.duration_ms)} ms · container ${tool.cleaned ? "cleaned" : "not cleaned"}</small>${tool.trace_id ? `<a href="http://127.0.0.1:16686/trace/${encodeURIComponent(tool.trace_id)}" target="_blank" rel="noreferrer">TRACE ${escapeHTML(tool.trace_id)}</a>` : ""}</div><p>${escapeHTML(tool.reason)}</p><pre>${escapeHTML(tool.output)}</pre></article>`).join("")}</section>` : "";
  $("#report").innerHTML = report ? `<div class="report-summary">${escapeHTML(report.summary)}</div>${decisionHTML}${toolHTML}<div class="checks">${report.checks.map(check => `<article class="check"><span class="status ${check.status}">${escapeHTML(check.status)}</span><b>${escapeHTML(check.label)}</b><p>${escapeHTML(check.observed)}</p></article>`).join("")}</div>` : (task.error ? `<p class="error">${escapeHTML(task.error)}</p>` : "");
}

$("#loadAgentDemo").addEventListener("click", () => {
  $("#url").value = `${location.origin}/demo/live-broken`;
  $("#objective").value = "检查直播页面是否正常、是否正在直播，并确认活动入口已经开启；如果浏览器证据不足，请调用诊断工具并给出修复建议。";
  $("#url").focus();
});

$("#taskForm").addEventListener("submit", async event => {
  event.preventDefault();
  $("#formError").textContent = "";
  const button = event.currentTarget.querySelector("button[type=submit]");
  button.disabled = true;
  try {
    const task = await request("/api/tasks", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({url:$("#url").value, objective:$("#objective").value})});
    selectedTaskId = task.id;
    await loadTasks();
    await showTask(task.id);
  } catch (error) { $("#formError").textContent = error.message; }
  finally { button.disabled = false; }
});

$("#refresh").addEventListener("click", loadTasks);
$("#closeDetail").addEventListener("click", () => { $("#detail").classList.add("hidden"); selectedTaskId = null; });
setInterval(() => { $("#clock").textContent = new Date().toLocaleTimeString("zh-CN", {hour12:false}); }, 1000);
if (!pageParams.has("snapshot")) {
  const events = new EventSource("/api/events");
  events.addEventListener("update", () => loadTasks().catch(console.error));
}
loadTasks().catch(error => { $("#taskList").innerHTML = `<div class="empty">${escapeHTML(error.message)}</div>`; });
loadSandboxRuns().catch(showSandboxError);
loadInfraStatus().catch(showSandboxError);
loadAgentEval();
