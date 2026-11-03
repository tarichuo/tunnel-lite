"use strict";
const $ = (s) => document.querySelector(s);
const esc = (v) =>
  String(v ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const labels = {
  ACTIVE: "运行中",
  STARTING: "启动中",
  OFFLINE: "Agent 已断开",
  STOPPED: "已下线",
  ERROR: "启动或恢复失败",
  ONLINE: "在线",
  PENDING: "待接入",
};
let csrf = "",
  view = "tunnels",
  agents = [],
  tunnels = [],
  meta = {},
  page = 1,
  total = 0,
  busy = false,
  authenticated = false,
  agentSecret = null,
  dirty = false,
  createKey = "",
  lastSuccess = "";
const badge = (s) =>
  `<span class="badge ${esc(s)}">${esc(labels[s] || s)}</span>`;
const address = (host, port) =>
  `${host.includes(":") ? "[" + host + "]" : host}:${port}`;
const time = (value) =>
  value
    ? new Date(value).toLocaleString(undefined, { timeZoneName: "short" })
    : "尚未连接";
const agentName = (id) => agents.find((a) => a.id === id)?.name || id;
async function api(path, options = {}) {
  const headers = {
    "Content-Type": "application/json",
    "X-CSRF-Token": csrf,
    ...options.headers,
  };
  const response = await fetch("/api/v1/" + path, {
    ...options,
    headers,
    credentials: "same-origin",
    cache: "no-store",
  });
  const data = await response.json();
  if (!response.ok) {
    const err = new Error(data.error?.message || "请求失败");
    err.field = data.error?.field;
    err.code = data.error?.code;
    if (response.status === 401 && path !== "session") {
      showLogin();
    }
    throw err;
  }
  return data;
}
function toast(message) {
  const el = $("#toast");
  el.textContent = message;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (el.hidden = true), 4000);
}
function showLogin() {
  authenticated = false;
  csrf = "";
  agentSecret = null;
  $("#login-token").value = "";
  $("#login-error").textContent = "";
  $("#console").hidden = true;
  $("#login").hidden = false;
  $("#drawer").close();
  $("#drawer-body").replaceChildren();
  dirty = false;
  $("#confirm").close();
  $("#login-token").focus();
}
async function init() {
  try {
    const result = await api("session");
    csrf = result.csrf_token;
    authenticated = true;
    $("#login").hidden = true;
    $("#console").hidden = false;
    changeView();
  } catch {
    showLogin();
  }
}
$("#show-token").addEventListener(
  "change",
  (e) => ($("#login-token").type = e.target.checked ? "text" : "password"),
);
$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const btn = e.submitter;
  btn.disabled = true;
  try {
    const result = await api("session", {
      method: "POST",
      body: JSON.stringify({ token: $("#login-token").value }),
    });
    csrf = result.csrf_token;
    authenticated = true;
    $("#login-token").value = "";
    $("#login").hidden = true;
    $("#console").hidden = false;
    await changeView();
  } catch (err) {
    $("#login-error").textContent = err.message;
  } finally {
    btn.disabled = false;
  }
});
$("#logout").addEventListener("click", async () => {
  try {
    await api("session", { method: "DELETE" });
    showLogin();
  } catch (err) {
    toast(err.message);
  }
});
function changeView() {
  view = location.hash === "#agents" ? "agents" : "tunnels";
  page = 1;
  $("#page-title").textContent = view === "agents" ? "Agent" : "端口映射";
  $("#page-desc").textContent =
    view === "agents"
      ? "接入本地机器，管理连接与目标授权。"
      : "将本地 TCP 服务连接至公网入口。";
  $("#primary-action").textContent =
    view === "agents" ? "＋ 添加 Agent" : "＋ 创建映射";
  $("#filters").hidden = view === "agents";
  $("#nav-agents").classList.toggle("active", view === "agents");
  $("#nav-tunnels").classList.toggle("active", view === "tunnels");
  return refresh();
}
window.addEventListener("hashchange", changeView);
async function refresh() {
  if (busy || !authenticated) return;
  busy = true;
  try {
    const query = new URLSearchParams({
      q: $("#search").value,
      status: $("#status-filter").value,
      agent_id: $("#agent-filter").value,
      page: String(page),
    });
    const [a, t, o, m] = await Promise.all([
      api("agents"),
      api("tunnels?" + query),
      api("overview"),
      api("meta"),
    ]);
    if (!authenticated) return;
    agents = a.items;
    tunnels = t.items;
    page = t.page;
    total = t.total;
    meta = m;
    $("#server-label").textContent = meta.public_host;
    const selected = $("#agent-filter").value;
    $("#agent-filter").innerHTML =
      '<option value="">全部 Agent</option>' +
      agents
        .map((a) => `<option value="${esc(a.id)}">${esc(a.name)}</option>`)
        .join("");
    $("#agent-filter").value = selected;
    $("#stats").innerHTML = [
      ["在线 Agent", o.online_agents, "本地机器连接状态"],
      ["有效映射", o.enabled_tunnels, "保持启用的端口映射"],
      ["运行中", o.active_tunnels, "Agent 与公网监听就绪"],
      ["需处理", o.attention_tunnels, "断开或恢复失败"],
    ]
      .map(
        ([title, n, sub], i) =>
          `<div class="stat ${i === 3 && n ? "warn" : ""}"><p>${title}</p><strong>${n}</strong><small>${sub}</small></div>`,
      )
      .join("");
    renderList();
    lastSuccess = new Date().toLocaleTimeString();
    $("#updated").textContent = "更新于 " + lastSuccess;
    $("#stale").hidden = true;
    const waiting = $("#agent-waiting");
    if (waiting && agentSecret) {
      const online =
        agents.find((a) => a.id === agentSecret.agent.id)?.status === "ONLINE";
      waiting.textContent = online
        ? "Agent 已在线，可以创建第一个映射。"
        : "等待 Agent 连接… 请将配置与 CA 文件保存到本地后启动。";
      const next = $("#first-tunnel");
      if (next) next.disabled = !online;
    }
  } catch (err) {
    if (authenticated) {
      $("#stale").textContent =
        `更新失败：${err.message}。${lastSuccess ? "数据可能已过期，最后更新于 " + lastSuccess : "请点击刷新重试。"}`;
      $("#stale").hidden = false;
    }
  } finally {
    busy = false;
  }
}
function renderList() {
  if (view === "agents") {
    $("#pagination").hidden = true;
    if (!agents.length) {
      $("#list").innerHTML = empty(
        "接入你的第一台机器",
        "在本地运行 Agent，即可开始创建公网映射。",
        "add-agent",
        "添加 Agent",
      );
      return;
    }
    $("#list").innerHTML =
      `<div class="table-wrap"><table><thead><tr><th>Agent 名称</th><th>状态</th><th>最后心跳</th><th>有效映射</th><th>操作</th></tr></thead><tbody>${agents.map((a) => `<tr><td><strong>${esc(a.name)}</strong><small class="mono">${esc(a.id)}</small></td><td class="status-cell">${badge(a.status)}</td><td data-label="最后心跳">${esc(time(a.last_heartbeat))}</td><td data-label="有效映射">${a.tunnel_count}</td><td class="actions-cell"><button class="link-button" data-action="agent-detail" data-id="${esc(a.id)}">详情</button><button class="link-button" data-action="create-for-agent" data-id="${esc(a.id)}" ${a.status !== "ONLINE" ? 'disabled title="Agent 上线后可以创建"' : ""}>创建映射</button></td></tr>`).join("")}</tbody></table></div>`;
    return;
  }
  $("#pagination").hidden = false;
  if (!tunnels.length) {
    const filtered =
      $("#search").value ||
      $("#agent-filter").value ||
      $("#status-filter").value;
    $("#list").innerHTML = filtered
      ? empty(
          "没有找到匹配的映射",
          "调整搜索条件后再试。",
          "clear-filters",
          "清除筛选",
        )
      : !agents.length
        ? empty(
            "先接入一台本地机器",
            "完成 Agent 接入后，即可创建 TCP 映射。",
            "add-agent",
            "添加 Agent",
          )
        : empty(
            "创建你的第一个映射",
            "选择在线 Agent，把本地服务连接到公网。",
            "create-tunnel",
            "创建映射",
          );
  } else {
    $("#list").innerHTML =
      `<div class="table-wrap"><table><thead><tr><th>映射名称</th><th>Agent</th><th>本地目标</th><th>公网地址</th><th>状态</th><th>操作</th></tr></thead><tbody>${tunnels.map((t) => `<tr><td><strong>${esc(t.name)}</strong><small class="mono">${esc(t.id.slice(0, 17))}…</small></td><td class="agent-cell" data-label="Agent">${esc(agentName(t.agent_id))}</td><td class="mono" data-label="本地目标">${esc(address(t.local_address, t.local_port))}</td><td class="mono address-cell" data-label="公网地址">${esc(address(t.public_address, t.public_port))}${t.status === "ACTIVE" ? ` <button class="copy" data-action="copy-address" data-id="${esc(t.id)}" aria-label="复制公网地址">⧉</button>` : "<small>当前不可用</small>"}</td><td class="status-cell">${badge(t.status)}${t.last_forward_error ? "<small>最近转发失败，查看详情</small>" : ""}</td><td class="actions-cell"><button class="link-button" data-action="tunnel-detail" data-id="${esc(t.id)}">详情</button><button class="link-button" data-action="${t.enabled ? "stop-tunnel" : "start-tunnel"}" data-id="${esc(t.id)}">${t.enabled ? "下线" : "启用"}</button></td></tr>`).join("")}</tbody></table></div>`;
  }
  const pages = Math.max(1, Math.ceil(total / 20));
  $("#pagination").innerHTML =
    `<span>共 ${total} 条记录</span><div><button data-action="prev" ${page <= 1 ? "disabled" : ""}>上一页</button> <span>${page} / ${pages}</span> <button data-action="next" ${page >= pages ? "disabled" : ""}>下一页</button></div>`;
}
function empty(title, desc, action, text) {
  return `<div class="empty"><h3>${title}</h3><p>${desc}</p><button class="primary" data-action="${action}">${text}</button></div>`;
}
function openDrawer(title, body) {
  if ($("#drawer").open && !closeDrawer()) return false;
  agentSecret = null;
  dirty = false;
  $("#drawer-title").textContent = title;
  $("#drawer-body").innerHTML = body;
  $("#drawer").showModal();
  return true;
}
function closeDrawer() {
  if (dirty && !window.confirm("还有未保存的内容，确定关闭？")) return false;
  $("#drawer").close();
  $("#drawer-body").replaceChildren();
  agentSecret = null;
  dirty = false;
  return true;
}
$("#drawer").addEventListener("cancel", (e) => {
  e.preventDefault();
  closeDrawer();
});
$("#drawer").addEventListener("input", () => (dirty = true));
function fieldError(form, err) {
  form.querySelectorAll(".field-error").forEach((n) => (n.textContent = ""));
  const node =
    Array.from(form.querySelectorAll("[data-field]")).find(
      (n) => n.dataset.field === err.field,
    ) || form.querySelector(".form-error");
  if (node) node.textContent = err.message;
  const field = err.field && form.elements.namedItem(err.field);
  if (field?.focus) field.focus();
}
const field = (name) =>
  `<div class="field-error" data-field="${name}" role="alert"></div>`;
function createTunnel(agentID = "") {
  const online = agents.filter((a) => a.status === "ONLINE");
  if (!online.length) {
    toast("请先接入并启动一个 Agent");
    if (!agents.length) addAgent();
    return;
  }
  if (
    !openDrawer(
      "创建映射",
      `<form id="create-form"><p class="muted">选择本地服务，为它分配一个公网入口。</p><label>Agent *<select name="agent_id" required>${agents.map((a) => `<option value="${esc(a.id)}" ${a.status !== "ONLINE" ? "disabled" : ""} ${a.id === agentID ? "selected" : ""}>${esc(a.name)}${a.status !== "ONLINE" ? "（离线）" : ""}</option>`).join("")}</select></label>${field("agent_id")}<label>映射名称 *<input name="name" maxlength="64" placeholder="例如：开发环境 Web" required></label>${field("name")}<label>协议<input value="TCP" readonly></label><label>本地 IP *<input name="local_address" value="127.0.0.1" required></label><p class="help">相对于 Agent 所在机器，且必须位于授权范围内。</p>${field("local_address")}<label>本地端口 *<input name="local_port" type="number" min="1" max="65535" placeholder="3000" required></label>${field("local_port")}<label>公网端口方式<select name="port_mode"><option value="auto">自动分配</option><option value="manual">指定端口</option></select></label><label id="remote-port-row" hidden>公网端口 *<input name="remote_port" type="number" min="${meta.port_start}" max="${meta.port_end}"></label><p class="help">端口池 ${meta.port_start}—${meta.port_end}</p>${field("remote_port")}<div class="notice">公网可访问此 TCP 服务，请确认目标服务已有适当的认证措施。</div><div class="field-error form-error" role="alert"></div><div class="form-actions"><button type="button" data-action="close-drawer">取消</button><button type="submit" class="primary">创建映射</button></div></form>`,
    )
  )
    return;
  createKey = crypto.randomUUID();
  const f = $("#create-form");
  if (!agentID) f.elements.agent_id.value = online[0].id;
  const updateTarget = () => {
    const a = agents.find((a) => a.id === f.elements.agent_id.value);
    f.elements.local_address.value = a?.allowed_targets.includes("127.0.0.1")
      ? "127.0.0.1"
      : a?.allowed_targets[0] || "127.0.0.1";
  };
  updateTarget();
  f.elements.agent_id.addEventListener("change", updateTarget);
  f.elements.port_mode.addEventListener("change", () => {
    const manual = f.elements.port_mode.value === "manual";
    $("#remote-port-row").hidden = !manual;
    f.elements.remote_port.required = manual;
  });
  f.addEventListener("submit", async (e) => {
    e.preventDefault();
    const btn = e.submitter;
    btn.disabled = true;
    try {
      const data = {
        agent_id: f.elements.agent_id.value,
        name: f.elements.name.value,
        local_address: f.elements.local_address.value.trim(),
        local_port: Number(f.elements.local_port.value),
        remote_port:
          f.elements.port_mode.value === "auto"
            ? 0
            : Number(f.elements.remote_port.value),
      };
      await api("tunnels", {
        method: "POST",
        headers: { "Idempotency-Key": createKey },
        body: JSON.stringify(data),
      });
      dirty = false;
      closeDrawer();
      toast("映射正在启动");
      await refresh();
    } catch (err) {
      fieldError(f, err);
      if (err.code === "IDEMPOTENCY_CONFLICT") createKey = crypto.randomUUID();
    } finally {
      btn.disabled = false;
    }
  });
  // Changing payload after a definite response creates a different request; an ambiguous network retry keeps the key.
  f.addEventListener("change", () => (createKey = crypto.randomUUID()));
}
function addAgent() {
  if (
    !openDrawer(
      "添加 Agent",
      `<form id="agent-form"><p class="muted">为本地机器创建独立身份，然后下载配置并启动 Agent。</p><label>Agent 名称 *<input name="name" maxlength="64" placeholder="例如：我的开发机" required></label>${field("name")}<label>允许的目标 IP *<textarea name="allowed_targets" rows="3" required>127.0.0.1</textarea></label><p class="help">每行一个 IP。默认仅开放本机回环地址；Agent 本地配置也会校验授权范围。</p>${field("allowed_targets")}<div class="notice">创建后生成专属 Token，仅展示一次。请将配置与 CA 文件保存在运行 Agent 的机器上。</div><div class="field-error form-error" role="alert"></div><div class="form-actions"><button type="button" data-action="close-drawer">取消</button><button type="submit" class="primary">生成接入配置</button></div></form>`,
    )
  )
    return;
  $("#agent-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target,
      btn = e.submitter;
    btn.disabled = true;
    try {
      const result = await api("agents", {
        method: "POST",
        body: JSON.stringify({
          name: f.elements.name.value,
          allowed_targets: f.elements.allowed_targets.value
            .split(/[\n,]+/)
            .map((s) => s.trim())
            .filter(Boolean),
        }),
      });
      dirty = false;
      showAgentConfig(result);
      await refresh();
    } catch (err) {
      fieldError(f, err);
    } finally {
      btn.disabled = false;
    }
  });
}
function showAgentConfig(result) {
  agentSecret = result;
  dirty = true;
  const yaml = `server: ${JSON.stringify(address(meta.public_host, meta.control_port))}\nagent_id: ${JSON.stringify(result.agent.id)}\nname: ${JSON.stringify(result.agent.name)}\ntoken: ${JSON.stringify(result.token)}\nca_cert: "./ca.crt"\nallowed_targets:\n${result.agent.allowed_targets.map((t) => "  - " + JSON.stringify(t)).join("\n")}\n`;
  agentSecret.yaml = yaml;
  $("#drawer-body").innerHTML =
    `<div class="notice warn">Token 仅此次展示，关闭后无法再次查看。请先保存配置文件。</div><h3>1. 保存配置与 CA</h3><div class="actions"><button data-action="download-config">下载 agent.yaml</button><button data-action="download-ca">下载 ca.crt</button></div><pre>${esc(yaml)}</pre><button data-action="copy-config">复制配置</button><h3>2. 启动本地 Agent</h3><p class="help">从项目构建输出获取对应平台的 tunnel-agent，将它与上述两个文件放在同一目录。</p><label>运行平台<select id="platform"><option value="unix">Linux / macOS</option><option value="windows">Windows</option></select></label><pre id="agent-command">./tunnel-agent -c agent.yaml</pre><h3>3. 等待接入</h3><div id="agent-waiting" class="notice">等待 Agent 连接…</div><p class="help">若长时间未连接，请检查 7001（或配置的控制端口）、服务端地址、Token 与 CA 证书，并查看本地进程输出。</p><div class="form-actions"><button data-action="close-drawer">关闭</button><button id="first-tunnel" class="primary" data-action="first-tunnel" disabled>创建第一个映射</button></div>`;
  $("#platform").addEventListener(
    "change",
    (e) =>
      ($("#agent-command").textContent =
        e.target.value === "windows"
          ? ".\\tunnel-agent.exe -c agent.yaml"
          : "./tunnel-agent -c agent.yaml"),
  );
}
function download(name, text) {
  const u = URL.createObjectURL(
    new Blob([text], { type: "text/plain;charset=utf-8" }),
  );
  const a = document.createElement("a");
  a.href = u;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(u), 1000);
}
async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("已复制");
  } catch {
    window.prompt("请复制以下内容：", text);
  }
}
function operations(list) {
  return (
    "<h3>最近操作</h3>" +
    (list.length
      ? list
          .map(
            (o) =>
              `<div class="operation"><p>${esc(o.detail)}</p><small>${esc(time(o.created_at))}</small></div>`,
          )
          .join("")
      : '<p class="muted">暂无操作记录</p>')
  );
}
function definition(items) {
  return (
    "<dl>" +
    items.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("") +
    "</dl>"
  );
}
async function tunnelDetail(id) {
  const { tunnel: t, operations: logs } = await api("tunnels/" + id);
  openDrawer(
    "映射详情",
    badge(t.status) +
      definition([
        ["映射名称", esc(t.name)],
        ["映射 ID", esc(t.id)],
        ["Agent", esc(agentName(t.agent_id))],
        ["本地目标", esc(address(t.local_address, t.local_port))],
        ["公网地址", esc(address(t.public_address, t.public_port))],
        ["端口分配", t.port_mode === "auto" ? "自动分配" : "指定端口"],
        ["创建时间", esc(time(t.created_at))],
        ["状态更新时间", esc(time(t.updated_at))],
      ]) +
      (t.status === "ACTIVE"
        ? '<div class="notice">Agent 在线且公网端口已监听。本地目标服务的可用性取决于服务进程与网络。</div>'
        : "") +
      (t.error ? `<div class="notice warn">${esc(t.error)}</div>` : "") +
      (t.last_forward_error
        ? `<div class="notice warn">${esc(t.last_forward_error)}</div>`
        : "") +
      operations(logs) +
      '<p class="hint">详情为打开时的数据，关闭后可重新打开查看最新状态。</p>',
  );
}
async function agentDetail(id) {
  const { agent: a, operations: logs } = await api("agents/" + id);
  openDrawer(
    "Agent 详情",
    badge(a.status) +
      definition([
        ["名称", esc(a.name)],
        ["Agent ID", esc(a.id)],
        ["最后连接", esc(time(a.last_connected))],
        ["最后心跳", esc(time(a.last_heartbeat))],
        ["目标授权", a.allowed_targets.map(esc).join("<br>")],
      ]) +
      (a.status !== "ONLINE"
        ? '<div class="notice warn">请检查本地 Agent 进程、服务器地址、Token、CA 与网络连接。</div>'
        : "") +
      '<h3>所属映射</h3><button data-action="filter-agent" data-id="' +
      esc(a.id) +
      '">查看此 Agent 的全部映射</button>' +
      operations(logs),
  );
}
async function stopTunnel(id) {
  const t = tunnels.find((t) => t.id === id);
  if (!t) return;
  $("#confirm-title").textContent = "下线映射？";
  $("#confirm-body").innerHTML =
    `<p><strong>${esc(t.name)}</strong></p><p class="mono">${esc(address(t.public_address, t.public_port))}</p><p>下线将关闭监听、断开此映射的现有连接并释放公网端口。记录保留，之后可以再次启用。</p><p class="field-error" id="confirm-error" role="alert"></p><div class="actions"><button data-action="cancel-confirm">取消</button><button class="danger" data-action="confirm-stop" data-id="${esc(id)}">下线映射</button></div>`;
  $("#confirm").showModal();
}
function startTunnel(id) {
  const t = tunnels.find((t) => t.id === id);
  if (!t) return;
  if (agents.find((a) => a.id === t.agent_id)?.status !== "ONLINE") {
    toast("Agent 离线，请先恢复连接");
    return;
  }
  $("#confirm-title").textContent = "再次启用映射";
  $("#confirm-body").innerHTML =
    `<form id="start-form"><p>${esc(t.name)} · ${esc(address(t.local_address, t.local_port))}</p><label>公网端口方式<select name="port_mode"><option value="auto" ${t.port_mode === "auto" ? "selected" : ""}>自动分配</option><option value="manual" ${t.port_mode === "manual" ? "selected" : ""}>指定端口</option></select></label><label id="start-port-row" ${t.port_mode === "auto" ? "hidden" : ""}>公网端口<input name="remote_port" type="number" min="${meta.port_start}" max="${meta.port_end}" value="${t.public_port}" ${t.port_mode === "manual" ? "required" : ""}></label><p class="help">自动分配将优先尝试原端口，占用时分配新端口；指定端口冲突时保留当前配置。</p><div class="field-error form-error" role="alert"></div><div class="actions"><button type="button" data-action="cancel-confirm">取消</button><button type="submit" class="primary">启用映射</button></div></form>`;
  $("#confirm").showModal();
  const f = $("#start-form");
  f.elements.port_mode.addEventListener("change", () => {
    const manual = f.elements.port_mode.value === "manual";
    $("#start-port-row").hidden = !manual;
    f.elements.remote_port.required = manual;
  });
  f.addEventListener("submit", async (e) => {
    e.preventDefault();
    const btn = e.submitter;
    btn.disabled = true;
    try {
      const result = await api(`tunnels/${id}/start`, {
        method: "POST",
        body: JSON.stringify({
          remote_port:
            f.elements.port_mode.value === "auto"
              ? 0
              : Number(f.elements.remote_port.value),
        }),
      });
      $("#confirm").close();
      toast(
        result.public_port === t.public_port
          ? "映射正在启动"
          : "公网端口已变更为 " + result.public_port + "，映射正在启动",
      );
      await refresh();
    } catch (err) {
      fieldError(f, err);
    } finally {
      btn.disabled = false;
    }
  });
}
$("#primary-action").addEventListener("click", () =>
  view === "agents" ? addAgent() : createTunnel(),
);
$("#refresh").addEventListener("click", refresh);
for (const selector of ["#status-filter", "#agent-filter"])
  $(selector).addEventListener("change", () => {
    page = 1;
    refresh();
  });
let searchTimer;
$("#search").addEventListener("input", () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    page = 1;
    refresh();
  }, 250);
});
document.addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-action]");
  if (!btn) return;
  const action = btn.dataset.action,
    id = btn.dataset.id;
  try {
    switch (action) {
      case "close-drawer":
        closeDrawer();
        break;
      case "cancel-confirm":
        $("#confirm").close();
        break;
      case "add-agent":
        addAgent();
        break;
      case "create-tunnel":
        createTunnel();
        break;
      case "create-for-agent":
        createTunnel(id);
        break;
      case "first-tunnel": {
        const aid = agentSecret.agent.id;
        if (closeDrawer()) createTunnel(aid);
        break;
      }
      case "clear-filters":
        $("#search").value = "";
        $("#status-filter").value = "";
        $("#agent-filter").value = "";
        page = 1;
        await refresh();
        break;
      case "prev":
        page--;
        await refresh();
        break;
      case "next":
        page++;
        await refresh();
        break;
      case "copy-address": {
        const t = tunnels.find((t) => t.id === id);
        await copy(address(t.public_address, t.public_port));
        break;
      }
      case "tunnel-detail":
        await tunnelDetail(id);
        break;
      case "agent-detail":
        await agentDetail(id);
        break;
      case "stop-tunnel":
        await stopTunnel(id);
        break;
      case "start-tunnel":
        startTunnel(id);
        break;
      case "confirm-stop":
        btn.disabled = true;
        try {
          await api("tunnels/" + id, { method: "DELETE" });
          $("#confirm").close();
          toast("映射已下线");
          await refresh();
        } catch (err) {
          $("#confirm-error").textContent = err.message;
        } finally {
          btn.disabled = false;
        }
        break;
      case "download-config":
        download("agent.yaml", agentSecret.yaml);
        break;
      case "download-ca":
        download("ca.crt", meta.ca_cert);
        break;
      case "copy-config":
        await copy(agentSecret.yaml);
        break;
      case "filter-agent":
        if (closeDrawer()) {
          location.hash = "#tunnels";
          $("#agent-filter").value = id;
          $("#status-filter").value = "ALL";
          page = 1;
          await changeView();
        }
        break;
    }
  } catch (err) {
    toast(err.message);
  }
});
setInterval(() => {
  if (!document.hidden) refresh();
}, 5000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh();
});
window.addEventListener("beforeunload", (e) => {
  if (dirty) {
    e.preventDefault();
    e.returnValue = "";
  }
});
init();
