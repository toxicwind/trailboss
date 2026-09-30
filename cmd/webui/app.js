/* trailboss ranch office — vanilla JS, no build step */
"use strict";

const $ = (sel, root) => (root || document).querySelector(sel);
const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

let currentSelection = null; // {name, path}
let jobEventSource = null;
let jobsPoll = null;

function toast(msg, ms) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.add("hidden"), ms || 4000);
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
  return data;
}
const getJSON = (p) => api("GET", p);
const postJSON = (p, b) => api("POST", p, b);

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

/* ---- view switching ---- */
function show(view) {
  $$(".view").forEach((v) => v.classList.add("hidden"));
  $("#view-" + view).classList.remove("hidden");
  $$(".nav-btn").forEach((b) => b.classList.toggle("active", b.dataset.view === view || (view === "selection" && b.dataset.view === "dashboard") || (view === "job" && b.dataset.view === "jobs")));
  if (jobEventSource) { jobEventSource.close(); jobEventSource = null; }
  if (jobsPoll) { clearInterval(jobsPoll); jobsPoll = null; }
  if (view === "dashboard") loadSelections();
  if (view === "jobs") { loadJobs(); jobsPoll = setInterval(loadJobs, 3000); }
  window.scrollTo(0, 0);
}
$$(".nav-btn, .back").forEach((b) => b.addEventListener("click", () => show(b.dataset.view)));

/* ---- dashboard ---- */
async function loadSelections() {
  const tb = $("#selections-table tbody");
  try {
    const data = await getJSON("/api/selections");
    const sels = data.selections || [];
    if (!sels.length) {
      tb.innerHTML = '<tr><td colspan="5" class="muted">no tally books yet — run a roundup above.</td></tr>';
      return;
    }
    tb.innerHTML = sels.map((s) => {
      const repos = s.repoCount == null ? '<span class="muted">(unreadable)</span>' : s.repoCount;
      const resolved = s.resolvedAt ? s.resolvedAt.slice(0, 10) : "—";
      const err = s.error ? `<div class="outcome-err">${esc(s.error)}</div>` : "";
      return `<tr><td>${esc(s.name)}${err}</td><td>${esc(s.owner || "—")}</td>` +
        `<td class="num">${repos}</td><td>${esc(resolved)}</td>` +
        `<td><button class="btn" data-open="${esc(s.name)}">Open</button></td></tr>`;
    }).join("");
    $$("[data-open]", tb).forEach((b) => b.addEventListener("click", () => openSelection(b.dataset.open)));
  } catch (e) {
    tb.innerHTML = `<tr><td colspan="5" class="outcome-err">${esc(e.message)}</td></tr>`;
  }
}

$("#select-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const fd = new FormData(e.target);
  const topics = String(fd.get("topics") || "").split(",").map((t) => t.trim()).filter(Boolean);
  const btn = $("button[type=submit]", e.target);
  btn.disabled = true;
  try {
    const data = await postJSON("/api/select", {
      org: String(fd.get("org")).trim(),
      topics,
      allRepos: fd.get("allRepos") === "on",
      name: String(fd.get("name") || "").trim(),
    });
    const n = data.selection.repos.length;
    toast(`roundup done: ${n} repos → ${data.selectionPath}`);
    e.target.reset();
    openSelection(data.name);
  } catch (err) {
    toast("roundup failed: " + err.message, 7000);
  } finally {
    btn.disabled = false;
  }
});

/* ---- selection detail ---- */
async function openSelection(name) {
  try {
    const data = await getJSON("/api/selection?name=" + encodeURIComponent(name));
    const sel = data.selection;
    currentSelection = { name, path: data.path };
    $("#sel-title").textContent = "📖 " + name;
    $("#sel-meta").textContent = `${sel.owner} · ${sel.repos.length} repos · resolved ${String(sel.resolvedAt).slice(0, 10)} · digest ${data.digest} · ${data.path}`;
    $("#sel-count").textContent = `(${sel.repos.length})`;
    const tb = $("#repos-table tbody");
    tb.innerHTML = sel.repos.map((r) =>
      `<tr><td>${esc(r.owner)}/${esc(r.name)}${r.archived ? ' <span class="pill">archived</span>' : ""}</td>` +
      `<td>${esc(r.defaultBranch || "—")}</td><td class="muted">${esc((r.topics || []).join(", "))}</td><td></td></tr>`
    ).join("");
    show("selection");
  } catch (e) {
    toast("cannot open selection: " + e.message, 7000);
  }
}

$("#sel-mirror").addEventListener("click", async () => {
  if (!currentSelection) return;
  try {
    const { id } = await postJSON("/api/jobs", { kind: "mirror", selection: currentSelection.name });
    openJob(id);
  } catch (e) { toast("mirror job failed to start: " + e.message, 7000); }
});

$("#scan-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!currentSelection) return;
  const pattern = new FormData(e.target).get("pattern");
  try {
    const { id } = await postJSON("/api/jobs", { kind: "scan", selection: currentSelection.name, pattern: String(pattern) });
    openJob(id);
  } catch (err) { toast("scan job failed to start: " + err.message, 7000); }
});

$("#apply-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!currentSelection) return;
  const fd = new FormData(e.target);
  const dryRun = fd.get("dryRun") === "on";
  const confirm = fd.get("confirm") === "on";
  if (!dryRun && !confirm && !window.confirm("LIVE run: this will push commits / open PRs across the whole selection. Continue?")) return;
  const owners = String(fd.get("owners") || "").split(",").map((o) => o.trim()).filter(Boolean);
  try {
    const { id } = await postJSON("/api/jobs", {
      kind: "apply",
      selection: currentSelection.name,
      scriptLine: String(fd.get("scriptLine")),
      commitMessage: String(fd.get("commitMessage")),
      mode: String(fd.get("mode")),
      branch: String(fd.get("branch") || ""),
      owners,
      sign: String(fd.get("sign")),
      dryRun,
      confirm,
    });
    openJob(id);
  } catch (err) { toast("apply job failed to start: " + err.message, 7000); }
});

/* ---- jobs list ---- */
function pill(status) {
  return `<span class="pill ${esc(status)}">${esc(status)}</span>`;
}

async function loadJobs() {
  const tb = $("#jobs-table tbody");
  try {
    const data = await getJSON("/api/jobs");
    const jobs = data.jobs || [];
    if (!jobs.length) {
      tb.innerHTML = '<tr><td colspan="6" class="muted">no jobs yet.</td></tr>';
      return;
    }
    tb.innerHTML = jobs.map((j) =>
      `<tr><td class="muted">${esc(j.id)}</td><td>${esc(j.kind)}</td><td>${pill(j.status)}</td>` +
      `<td class="muted">${esc(String(j.startedAt).slice(5, 16).replace("T", " "))}</td>` +
      `<td>${esc(j.summary || "—")}</td>` +
      `<td><button class="btn" data-job="${esc(j.id)}">Watch</button></td></tr>`
    ).join("");
    $$("[data-job]", tb).forEach((b) => b.addEventListener("click", () => openJob(b.dataset.job)));
  } catch (e) {
    tb.innerHTML = `<tr><td colspan="6" class="outcome-err">${esc(e.message)}</td></tr>`;
  }
}

/* ---- job detail + live log ---- */
async function openJob(id) {
  show("job");
  $("#job-title").textContent = "📋 job " + id;
  $("#job-meta").textContent = "connecting…";
  $("#job-log").textContent = "";
  $("#job-result").innerHTML = "";
  try {
    const detail = await getJSON("/api/jobs/" + encodeURIComponent(id));
    renderJobMeta(detail);
    (detail.log || []).forEach(appendLogLine);
    if (detail.result) renderJobResult(detail);
  } catch (e) {
    $("#job-meta").textContent = "cannot load job: " + e.message;
    return;
  }
  jobEventSource = new EventSource("/api/jobs/" + encodeURIComponent(id) + "/events");
  jobEventSource.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch { return; }
    if (msg.type === "log") appendLogLine(msg.line);
    else if (msg.type === "status") {
      getJSON("/api/jobs/" + encodeURIComponent(id)).then((d) => { renderJobMeta(d); if (d.result) renderJobResult(d); }).catch(() => {});
      if (msg.status === "done" || msg.status === "error") jobEventSource.close();
    }
  };
  jobEventSource.onerror = () => { /* stream closed (terminal state) — detail already refreshed */ };
}

function renderJobMeta(d) {
  $("#job-meta").innerHTML = `${pill(d.status)} · kind ${esc(d.kind)} · started ${esc(String(d.startedAt).slice(0, 16).replace("T", " "))}` +
    (d.summary ? ` · ${esc(d.summary)}` : "");
}

function appendLogLine(line) {
  const pre = $("#job-log");
  pre.textContent += line + "\n";
  pre.scrollTop = pre.scrollHeight;
}

function renderJobResult(d) {
  const el = $("#job-result");
  const res = d.result;
  if (!res) { el.innerHTML = ""; return; }
  if (d.kind === "apply" && res.outcomes) {
    const rows = res.outcomes.map((o) => {
      const status = o.err ? `<span class="outcome-err">failed</span>`
        : o.pushed ? `<span class="pill done">pushed</span>`
        : o.dryRun ? `<span class="pill running">dry-run</span>`
        : `<span class="pill queued">no-change</span>`;
      const diff = o.diffStat ? `<pre class="diff">${esc(o.diffStat)}</pre>` : "";
      const err = o.err ? `<div class="outcome-err">${esc(o.err)}</div>` : "";
      return `<tr><td>${esc(o.fullName)}</td><td>${status}</td><td>${diff}${err}</td></tr>`;
    }).join("");
    el.innerHTML = `<h3>🐄 Per-repo outcomes</h3><table class="tbl"><thead><tr><th>Repo</th><th>Result</th><th>Diff / error</th></tr></thead><tbody>${rows}</tbody></table>`;
  } else if (d.kind === "scan" && res.matches) {
    const rows = res.matches.map((m) =>
      `<tr><td>${esc(m.repo)}</td><td>${esc(m.file)}:${m.line}</td><td><code>${esc(m.text)}</code></td></tr>`
    ).join("");
    const skipped = (res.repos || []).filter((r) => !r.scanned).map((r) =>
      `<div class="muted">${esc(r.repo)} — ${esc(r.skipReason)}</div>`).join("");
    el.innerHTML = `<h3>🔍 Matches (${res.totalMatches}${res.truncated ? ", truncated" : ""})</h3>` +
      (rows ? `<table class="tbl"><thead><tr><th>Repo</th><th>Location</th><th>Match</th></tr></thead><tbody>${rows}</tbody></table>` : '<p class="muted">no matches.</p>') + skipped;
  }
}

/* ---- boot ---- */
show("dashboard");
