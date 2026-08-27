let cfg;
let runtime = {sources:{}, targets:{}};
let statusTimer;
let currentView = 'tasks';
let toastTimer;

const $ = s => document.querySelector(s);
const $$ = s => [...document.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, m => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]));
const clone = v => JSON.parse(JSON.stringify(v));
const uid = p => `${p}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2,7)}`;

const DEFAULT_CFST = {
  binary:'cfst',
  url:'https://speed.cloudflare.com/__down?bytes=200000000',
  port:443,
  threads:4,
  ping_count:200,
  download_count:10,
  download_time:10,
  latency_max_ms:200,
  latency_min_ms:0,
  loss_max:0.2,
  speed_min_mb:5,
  httping:true,
  colo:[],
  all_ip:false
};

async function api(url, opt={}) {
  const r = await fetch(url, {headers:{'Content-Type':'application/json'}, ...opt});
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText || `HTTP ${r.status}`);
  return j;
}

function toast(msg, bad=false) {
  const el = $('#toast');
  el.textContent = msg;
  el.classList.toggle('bad', bad);
  el.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('show'), 2200);
}

async function load() {
  try {
    cfg = await api('/api/config');
    cfg.providers ||= [];
    cfg.sources ||= [];
    cfg.targets ||= [];
    renderAll();
    await refreshStatus();
    clearInterval(statusTimer);
    statusTimer = setInterval(refreshStatus, 3000);
  } catch (e) {
    toast(`加载失败：${e.message}`, true);
  }
}

function renderAll() {
  renderTasks();
  renderProviders();
  renderSettings();
}

function sourceById(id) { return (cfg.sources || []).find(s => s.id === id); }
function providerById(id) { return (cfg.providers || []).find(p => p.id === id); }
function sourceUseCount(id, exceptTargetId='') {
  return (cfg.targets || []).reduce((n,t) => n + (t.id === exceptTargetId ? 0 : (t.sources || []).filter(r => r.source_id === id).length), 0);
}
function taskLines(t) {
  return (t.sources || []).map(ref => ({ref, source:sourceById(ref.source_id)})).filter(x => x.source);
}
function fmtFamily(f) { return f === 'ipv6' ? 'IPv6' : 'IPv4'; }
function recordType(f) { return f === 'ipv6' ? 'AAAA' : 'A'; }
function fmtInterval(n) {
  n = Number(n || 0);
  if (!n) return '手动';
  if (n % 1440 === 0) return `${n/1440}天`;
  if (n % 60 === 0) return `${n/60}小时`;
  return `${n}分钟`;
}
function lineTitle(source) {
  const colo = (source.cfst?.colo || []).join('/');
  return colo ? `${fmtFamily(source.family)} · ${colo}` : fmtFamily(source.family);
}
function resultCountFor(sourceId) { return runtime.sources?.[sourceId]?.results?.length || 0; }
function taskRunning(t) { return taskLines(t).some(x => runtime.sources?.[x.source.id]?.running); }
function taskError(t) {
  for (const x of taskLines(t)) {
    const e = runtime.sources?.[x.source.id]?.error;
    if (e) return e;
  }
  return runtime.targets?.[t.id]?.ok === false ? runtime.targets[t.id].error : '';
}

function renderTasks() {
  const list = $('#taskList');
  const targets = cfg.targets || [];
  const enabled = targets.filter(t => t.enabled).length;
  const running = targets.filter(taskRunning).length;
  const totalResults = (cfg.sources || []).reduce((n,s) => n + resultCountFor(s.id), 0);
  $('#summary').innerHTML = `
    <div class="summary-box"><b>${targets.length}</b><span>域名任务</span></div>
    <div class="summary-box"><b>${running || enabled}</b><span>${running ? '正在运行' : '已启用'}</span></div>
    <div class="summary-box"><b>${totalResults}</b><span>当前结果</span></div>`;

  if (!targets.length) {
    list.innerHTML = `<div class="empty"><b>还没有任务</b>一个域名就是一个任务。先添加 DNS 账号，再创建第一个域名。</div>`;
    return;
  }
  list.innerHTML = '';
  targets.forEach(t => {
    const lines = taskLines(t);
    const runningNow = taskRunning(t);
    const err = taskError(t);
    const synced = runtime.targets?.[t.id];
    let stateClass = runningNow ? 'running' : err ? 'bad' : synced?.ok ? 'ok' : '';
    let stateText = runningNow ? '优选中' : err ? '有错误' : synced?.ok ? '已同步' : '待运行';
    const chips = [];
    [...new Set(lines.map(x => x.source.family))].forEach(f => chips.push(`<span class="chip ${f==='ipv6'?'v6':'v4'}">${recordType(f)} ${fmtFamily(f)}</span>`));
    [...new Set(lines.flatMap(x => x.source.cfst?.colo || []))].slice(0,3).forEach(c => chips.push(`<span class="chip colo">${esc(c)}</span>`));
    const p = providerById(t.provider_id);
    if (p) chips.push(`<span class="chip">${esc(p.name || 'Cloudflare')}</span>`);

    const lineRows = lines.map(x => {
      const s = x.source;
      const st = runtime.sources?.[s.id] || {};
      const meta = [
        (s.cfst?.colo || []).join(','),
        `${x.ref.count || 1} 个`,
        fmtInterval(s.interval_minutes)
      ].filter(Boolean).join(' · ');
      return `<div class="line-row">
        <span class="line-family">${recordType(s.family)}</span>
        <span class="line-meta">${esc(s.name || lineTitle(s))}${meta ? ` · ${esc(meta)}` : ''}</span>
        <span class="line-result">${st.running ? '测速中' : `${st.results?.length || 0} 条`}</span>
      </div>`;
    }).join('') || `<div class="line-row"><span class="line-meta">尚未配置 IP 线路</span></div>`;

    const d = document.createElement('article');
    d.className = `task-card ${t.enabled ? '' : 'disabled'}`;
    d.innerHTML = `
      <div class="task-top">
        <div>
          <div class="task-domain">${esc(t.hostname)}</div>
          <div class="task-name">${esc(t.name || '未命名任务')}</div>
        </div>
        <div class="status-dot ${stateClass}">${stateText}</div>
      </div>
      <div class="chips">${chips.join('')}</div>
      <div class="line-preview">${lineRows}</div>
      ${err ? `<div class="help" style="color:#ff8f96;margin-top:8px">${esc(err)}</div>` : ''}
      <div class="card-actions">
        <button class="run" data-run>立即优选</button>
        <button class="sync" data-sync>同步 DNS</button>
        <button class="more" data-edit>•••</button>
      </div>`;
    d.querySelector('[data-run]').onclick = () => runTask(t);
    d.querySelector('[data-sync]').onclick = () => syncTask(t);
    d.querySelector('[data-edit]').onclick = () => openTaskEditor(t.id);
    list.appendChild(d);
  });
}

async function runTask(t) {
  const lines = taskLines(t);
  if (!t.enabled) return toast('这个任务当前已停用', true);
  if (!lines.length) return toast('请先添加至少一条 IP 线路', true);
  try {
    for (const x of lines) await api('/api/run/source?id=' + encodeURIComponent(x.source.id), {method:'POST'});
    toast(`已启动 ${lines.length} 条线路，完成后会自动同步 DNS`);
    setTimeout(refreshStatus, 500);
  } catch (e) { toast(e.message, true); }
}

async function syncTask(t) {
  try {
    await api('/api/sync/target?id=' + encodeURIComponent(t.id), {method:'POST'});
    toast('DNS 同步成功');
    refreshStatus();
  } catch (e) { toast(`同步失败：${e.message}`, true); }
}

async function refreshStatus() {
  try {
    runtime = await api('/api/status');
    if (currentView === 'tasks') renderTasks();
  } catch (_) {}
}

function renderProviders() {
  const box = $('#providerList');
  const arr = cfg.providers || [];
  if (!arr.length) {
    box.innerHTML = `<div class="empty"><b>还没有 DNS 账号</b>先添加 Cloudflare Zone，之后新建域名时直接下拉选择。</div>`;
    return;
  }
  box.innerHTML = '';
  arr.forEach(p => {
    const used = (cfg.targets || []).filter(t => t.provider_id === p.id).length;
    const d = document.createElement('div');
    d.className = 'provider-card';
    d.innerHTML = `<div class="provider-head">
      <div><b>${esc(p.name || 'Cloudflare')}</b><div class="provider-meta">Cloudflare · ${used} 个任务 · Zone ${esc(mask(p.zone_id))}</div></div>
      <div class="provider-actions"><button data-edit>编辑</button><button data-del>删除</button></div>
    </div>`;
    d.querySelector('[data-edit]').onclick = () => openProviderEditor(p.id);
    d.querySelector('[data-del]').onclick = () => deleteProvider(p.id);
    box.appendChild(d);
  });
}

function mask(v) {
  v = String(v || '');
  if (!v) return '未配置';
  if (v.length < 10) return v;
  return `${v.slice(0,5)}…${v.slice(-4)}`;
}

function renderSettings() {
  const panel = $('#settingsPanel');
  panel.innerHTML = `
    <div class="settings-card">
      <div class="settings-row"><label>最大并发测速任务</label><input id="setConcurrency" type="number" min="1" max="64" value="${Number(cfg.max_concurrency || 2)}"></div>
      <div class="help">建议 1–3。修改后保存并重启容器，新的任务槽数量才会完全生效。</div>
      <button id="saveGlobal" class="primary compact" style="margin-top:12px">保存设置</button>
    </div>
    <div class="settings-card">
      <details>
        <summary>高级 / 调试</summary>
        <div class="field" style="margin-top:12px"><span>监听地址</span><input id="setListen" value="${esc(cfg.listen || ':8080')}"></div>
        <div class="help">日常不要修改监听地址。下面的完整 JSON 仅用于备份或故障排查。</div>
        <textarea id="rawConfig" class="raw">${esc(JSON.stringify(cfg,null,2))}</textarea>
        <div class="grid2" style="margin-top:9px"><button id="copyRaw" class="ghost">复制 JSON</button><button id="applyRaw" class="danger-btn">应用 JSON</button></div>
      </details>
    </div>`;
  $('#saveGlobal').onclick = async () => {
    cfg.max_concurrency = Math.max(1, Number($('#setConcurrency').value) || 2);
    cfg.listen = $('#setListen')?.value.trim() || cfg.listen || ':8080';
    await saveConfig('全局设置已保存');
  };
  $('#copyRaw').onclick = async () => {
    try { await navigator.clipboard.writeText($('#rawConfig').value); toast('JSON 已复制'); } catch { toast('浏览器不允许复制', true); }
  };
  $('#applyRaw').onclick = async () => {
    try {
      const next = JSON.parse($('#rawConfig').value);
      cfg = next;
      cfg.providers ||= []; cfg.sources ||= []; cfg.targets ||= [];
      await saveConfig('JSON 已应用');
      renderAll();
    } catch (e) { toast(e.message, true); }
  };
}

async function saveConfig(okText='已保存') {
  try {
    await api('/api/config', {method:'PUT', body:JSON.stringify(cfg)});
    toast(okText);
    renderAll();
    return true;
  } catch (e) {
    toast(`保存失败：${e.message}`, true);
    return false;
  }
}

function openTaskEditor(targetId='') {
  if (!(cfg.providers || []).length) {
    toast('请先在“我的”里添加一个 DNS 账号', true);
    switchView('account');
    return;
  }
  const existing = targetId ? cfg.targets.find(t => t.id === targetId) : null;
  const t = existing ? clone(existing) : {
    id:uid('task'), name:'', enabled:true,
    provider_id:cfg.providers[0].id, hostname:'', ttl:60, proxied:false, sources:[]
  };
  const drafts = existing ? taskLines(existing).map(x => ({
    originalSourceId:x.source.id,
    ref:clone(x.ref),
    source:clone(x.source),
    shared:sourceUseCount(x.source.id, existing.id) > 0
  })) : [newLineDraft('ipv4')];

  const root = $('#modalRoot');
  root.innerHTML = `<div class="overlay" id="taskOverlay"><div class="sheet">
    <div class="sheet-head"><h3>${existing ? '编辑域名任务' : '新建域名任务'}</h3><button class="sheet-close" data-close>×</button></div>
    <div class="sheet-body">
      <label class="field"><span>域名</span><input id="taskHost" value="${esc(t.hostname)}" placeholder="例如 v4.629717.xyz" autocomplete="off"></label>
      <div class="grid2">
        <label class="field"><span>任务名称（可选）</span><input id="taskName" value="${esc(t.name)}" placeholder="例如 日本优选"></label>
        <label class="field"><span>DNS 账号</span><select id="taskProvider">${providerOptions(t.provider_id)}</select></label>
      </div>
      <div class="toggle-line"><div><b style="font-size:13px">启用任务</b><div class="help">关闭后不会定时优选</div></div><input id="taskEnabled" class="switch" type="checkbox" ${t.enabled?'checked':''}></div>
      <div class="mini-label" style="margin-top:10px">IP 线路</div>
      <div class="modal-note">一条线路就是一组独立的 IP 段和筛选规则。一个域名可以同时添加 IPv4、IPv6、NRT IPv4 等任意组合。</div>
      <div id="lineEditors"></div>
      <button id="addLineBtn" class="add-line">＋ 添加一条线路</button>
      <details class="advanced">
        <summary>DNS 高级设置</summary>
        <div class="grid2">
          <label class="field"><span>TTL</span><input id="taskTTL" type="number" min="1" value="${Number(t.ttl || 60)}"></label>
          <label class="field"><span>Cloudflare 代理</span><select id="taskProxied"><option value="false" ${!t.proxied?'selected':''}>关闭（推荐）</option><option value="true" ${t.proxied?'selected':''}>开启</option></select></label>
        </div>
      </details>
      ${existing ? `<div class="danger-zone"><button id="deleteTaskBtn" class="danger-btn">删除这个域名任务</button></div>` : ''}
    </div>
    <div class="sheet-actions"><button data-close>取消</button><button id="saveTaskBtn" class="primary">保存任务</button></div>
  </div></div>`;

  function drawLines() {
    const box = $('#lineEditors');
    box.innerHTML = '';
    drafts.forEach((d,i) => {
      d.source.cfst ||= clone(DEFAULT_CFST);
      const s = d.source;
      const ref = d.ref;
      const e = document.createElement('div');
      e.className = 'line-editor';
      e.innerHTML = `
        <div class="line-editor-head"><div class="line-editor-title">线路 ${i+1}${d.shared?' · 原配置曾被其他任务共享':''}</div><button class="remove-line" data-remove>删除</button></div>
        <div class="grid2">
          <label class="field"><span>线路名称</span><input data-name value="${esc(s.name || '')}" placeholder="例如 NRT IPv4"></label>
          <label class="field"><span>协议</span><select data-family><option value="ipv4" ${s.family==='ipv4'?'selected':''}>IPv4 / A</option><option value="ipv6" ${s.family==='ipv6'?'selected':''}>IPv6 / AAAA</option></select></label>
        </div>
        <label class="field"><span>IP 段 / IP 源</span><textarea data-inputs placeholder="每行一个，可填 URL、CIDR 或单个 IP">${esc((s.inputs || []).join('\n'))}</textarea></label>
        <div class="grid3">
          <label class="field"><span>Colo / 地区（可空）</span><input data-colo value="${esc((s.cfst.colo || []).join(','))}" placeholder="NRT"></label>
          <label class="field"><span>写入数量</span><input data-count type="number" min="1" max="100" value="${Number(ref.count || 5)}"></label>
          <label class="field"><span>自动周期</span><select data-interval>
            ${intervalOptions(s.interval_minutes)}
          </select></label>
        </div>
        <details class="advanced"><summary>测速高级参数</summary>
          <div class="grid2">
            <label class="field"><span>最低速度 MB/s</span><input data-speed type="number" step="0.1" min="0" value="${Number(s.cfst.speed_min_mb ?? 5)}"></label>
            <label class="field"><span>延迟上限 ms</span><input data-latency type="number" step="1" min="0" value="${Number(s.cfst.latency_max_ms ?? 200)}"></label>
          </div>
          <div class="grid2">
            <label class="field"><span>丢包上限 0–1</span><input data-loss type="number" step="0.01" min="0" max="1" value="${Number(s.cfst.loss_max ?? .2)}"></label>
            <label class="field"><span>结果缓存数量</span><input data-keep type="number" min="1" value="${Number(s.keep_results || 50)}"></label>
          </div>
          <label class="field"><span>测速 URL</span><input data-url value="${esc(s.cfst.url || DEFAULT_CFST.url)}"></label>
          <div class="grid2">
            <label class="field"><span>HTTPing</span><select data-httping><option value="true" ${s.cfst.httping?'selected':''}>开启</option><option value="false" ${!s.cfst.httping?'selected':''}>关闭</option></select></label>
            <label class="field"><span>All IP</span><select data-allip><option value="false" ${!s.cfst.all_ip?'selected':''}>关闭</option><option value="true" ${s.cfst.all_ip?'selected':''}>开启</option></select></label>
          </div>
        </details>`;
      e.querySelector('[data-remove]').onclick = () => { drafts.splice(i,1); drawLines(); };
      e.querySelector('[data-name]').oninput = ev => s.name = ev.target.value;
      e.querySelector('[data-family]').onchange = ev => s.family = ev.target.value;
      e.querySelector('[data-inputs]').oninput = ev => s.inputs = splitLines(ev.target.value);
      e.querySelector('[data-colo]').oninput = ev => s.cfst.colo = splitComma(ev.target.value).map(x => x.toUpperCase());
      e.querySelector('[data-count]').oninput = ev => ref.count = Math.max(1, Number(ev.target.value) || 1);
      e.querySelector('[data-interval]').onchange = ev => s.interval_minutes = Number(ev.target.value);
      e.querySelector('[data-speed]').oninput = ev => s.cfst.speed_min_mb = Number(ev.target.value) || 0;
      e.querySelector('[data-latency]').oninput = ev => s.cfst.latency_max_ms = Number(ev.target.value) || 0;
      e.querySelector('[data-loss]').oninput = ev => s.cfst.loss_max = Number(ev.target.value) || 0;
      e.querySelector('[data-keep]').oninput = ev => s.keep_results = Math.max(1, Number(ev.target.value) || 50);
      e.querySelector('[data-url]').oninput = ev => s.cfst.url = ev.target.value.trim();
      e.querySelector('[data-httping]').onchange = ev => s.cfst.httping = ev.target.value === 'true';
      e.querySelector('[data-allip]').onchange = ev => s.cfst.all_ip = ev.target.value === 'true';
      box.appendChild(e);
    });
  }
  drawLines();
  $('#addLineBtn').onclick = () => {
    const usedFamilies = drafts.map(d => d.source.family);
    drafts.push(newLineDraft(usedFamilies.includes('ipv4') && !usedFamilies.includes('ipv6') ? 'ipv6' : 'ipv4'));
    drawLines();
  };
  root.querySelectorAll('[data-close]').forEach(b => b.onclick = closeModal);
  $('#taskOverlay').onclick = e => { if (e.target.id === 'taskOverlay') closeModal(); };
  $('#saveTaskBtn').onclick = async () => {
    t.hostname = $('#taskHost').value.trim().toLowerCase();
    t.name = $('#taskName').value.trim() || t.hostname;
    t.provider_id = $('#taskProvider').value;
    t.enabled = $('#taskEnabled').checked;
    t.ttl = Math.max(1, Number($('#taskTTL').value) || 60);
    t.proxied = $('#taskProxied').value === 'true';
    if (!t.hostname) return toast('请填写域名', true);
    if (!t.provider_id) return toast('请选择 DNS 账号', true);
    if (!drafts.length) return toast('至少保留一条 IP 线路', true);
    if (drafts.some(d => !(d.source.inputs || []).length)) return toast('每条线路至少填写一个 IP 段或 IP 源', true);
    if (await persistTask(t, drafts, existing)) closeModal();
  };
  if (existing) $('#deleteTaskBtn').onclick = async () => {
    if (!confirm(`删除 ${existing.hostname}？\n同时会删除仅属于这个任务的 IP 线路。`)) return;
    deleteTask(existing.id);
    if (await saveConfig('任务已删除')) closeModal();
  };
}

function newLineDraft(family='ipv4') {
  return {
    originalSourceId:'', shared:false,
    ref:{source_id:'',count:5},
    source:{
      id:uid('line'), name:family === 'ipv6' ? 'IPv6' : 'IPv4', enabled:true, family,
      inputs:[], interval_minutes:360, keep_results:50, cfst:clone(DEFAULT_CFST)
    }
  };
}

async function persistTask(t, drafts, existing) {
  const oldSourceIds = existing ? (existing.sources || []).map(r => r.source_id) : [];
  const nextRefs = [];
  const nextSourceIds = new Set();

  for (const d of drafts) {
    const s = clone(d.source);
    s.enabled = t.enabled;
    s.cfst = {...clone(DEFAULT_CFST), ...(s.cfst || {})};
    s.inputs = (s.inputs || []).map(x => x.trim()).filter(Boolean);
    s.cfst.colo = (s.cfst.colo || []).map(x => String(x).trim().toUpperCase()).filter(Boolean);
    if (!s.name.trim()) s.name = lineTitle(s);

    let id = d.originalSourceId || s.id || uid('line');
    if (d.shared) id = uid('line');
    s.id = id;
    nextSourceIds.add(id);
    const idx = cfg.sources.findIndex(x => x.id === id);
    if (idx >= 0) cfg.sources[idx] = s; else cfg.sources.push(s);
    nextRefs.push({source_id:id, count:Math.max(1, Number(d.ref.count) || 1)});
  }

  // Remove old source records only when no other target needs them and they are no longer used by this task.
  for (const oldId of oldSourceIds) {
    if (!nextSourceIds.has(oldId) && sourceUseCount(oldId, existing?.id || '') === 0) {
      cfg.sources = cfg.sources.filter(s => s.id !== oldId);
    }
  }
  t.sources = nextRefs;
  const ti = cfg.targets.findIndex(x => x.id === t.id);
  if (ti >= 0) cfg.targets[ti] = t; else cfg.targets.push(t);
  return await saveConfig(existing ? '任务已更新' : '任务已创建');
}

function deleteTask(id) {
  const t = cfg.targets.find(x => x.id === id);
  if (!t) return;
  const refs = (t.sources || []).map(r => r.source_id);
  cfg.targets = cfg.targets.filter(x => x.id !== id);
  for (const sid of refs) {
    const stillUsed = cfg.targets.some(x => (x.sources || []).some(r => r.source_id === sid));
    if (!stillUsed) cfg.sources = cfg.sources.filter(s => s.id !== sid);
  }
}

function providerOptions(selected) {
  return (cfg.providers || []).map(p => `<option value="${esc(p.id)}" ${p.id===selected?'selected':''}>${esc(p.name || p.id)}</option>`).join('');
}
function intervalOptions(selected) {
  const opts = [[0,'手动'],[180,'每 3 小时'],[360,'每 6 小时'],[720,'每 12 小时'],[1440,'每天']];
  if (selected && !opts.some(x => x[0] === Number(selected))) opts.push([Number(selected),`${selected} 分钟`]);
  return opts.map(([v,n]) => `<option value="${v}" ${Number(selected)===v?'selected':''}>${n}</option>`).join('');
}
function splitLines(v) { return String(v || '').split(/\n+/).map(x => x.trim()).filter(Boolean); }
function splitComma(v) { return String(v || '').split(',').map(x => x.trim()).filter(Boolean); }

function openProviderEditor(providerId='') {
  const existing = providerId ? cfg.providers.find(p => p.id === providerId) : null;
  const p = existing ? clone(existing) : {id:uid('cf'),name:'Cloudflare',type:'cloudflare',zone_id:'',api_token:''};
  const root = $('#modalRoot');
  root.innerHTML = `<div class="overlay" id="providerOverlay"><div class="sheet" style="max-width:580px">
    <div class="sheet-head"><h3>${existing?'编辑 DNS 账号':'添加 DNS 账号'}</h3><button class="sheet-close" data-close>×</button></div>
    <div class="sheet-body">
      <label class="field"><span>名称</span><input id="providerName" value="${esc(p.name)}" placeholder="例如 Cloudflare 主账号"></label>
      <label class="field"><span>Zone ID</span><input id="providerZone" value="${esc(p.zone_id || '')}" autocomplete="off"></label>
      <label class="field"><span>API Token</span><input id="providerToken" type="password" value="${esc(p.api_token || '')}" autocomplete="new-password"></label>
      <div class="modal-note">建议使用仅具有该 Zone DNS 编辑权限的 API Token。这里的账号配置会保存到 /data/config.json。</div>
    </div>
    <div class="sheet-actions"><button data-close>取消</button><button id="saveProviderBtn" class="primary">保存账号</button></div>
  </div></div>`;
  root.querySelectorAll('[data-close]').forEach(b => b.onclick = closeModal);
  $('#providerOverlay').onclick = e => { if (e.target.id === 'providerOverlay') closeModal(); };
  $('#saveProviderBtn').onclick = async () => {
    p.name = $('#providerName').value.trim() || 'Cloudflare';
    p.zone_id = $('#providerZone').value.trim();
    p.api_token = $('#providerToken').value.trim();
    if (!p.zone_id || !p.api_token) return toast('Zone ID 和 API Token 都要填写', true);
    const i = cfg.providers.findIndex(x => x.id === p.id);
    if (i >= 0) cfg.providers[i] = p; else cfg.providers.push(p);
    if (await saveConfig(existing ? 'DNS 账号已更新' : 'DNS 账号已添加')) closeModal();
  };
}

async function deleteProvider(id) {
  const p = providerById(id);
  const used = (cfg.targets || []).filter(t => t.provider_id === id);
  if (used.length) return toast(`还有 ${used.length} 个域名任务正在使用这个账号`, true);
  if (!confirm(`删除 DNS 账号“${p?.name || id}”？`)) return;
  cfg.providers = cfg.providers.filter(x => x.id !== id);
  await saveConfig('DNS 账号已删除');
}

function closeModal() { $('#modalRoot').innerHTML = ''; }

function switchView(view) {
  currentView = view;
  const titles = {tasks:'域名任务',account:'我的',settings:'设置'};
  $('#pageTitle').textContent = titles[view] || 'BestIP';
  $$('.view').forEach(v => v.classList.remove('active'));
  $$('.nav-item').forEach(v => v.classList.remove('active'));
  $(`#view${view[0].toUpperCase()+view.slice(1)}`)?.classList.add('active');
  $(`.nav-item[data-view="${view}"]`)?.classList.add('active');
  $('#addTaskBtn').style.display = view === 'tasks' ? '' : 'none';
  if (view === 'tasks') renderTasks();
  if (view === 'account') renderProviders();
  if (view === 'settings') renderSettings();
}

$$('.nav-item').forEach(b => b.onclick = () => switchView(b.dataset.view));
$('#addTaskBtn').onclick = () => openTaskEditor();
$('#addProviderBtn').onclick = () => openProviderEditor();
$('#refreshBtn').onclick = async () => { await refreshStatus(); toast('已刷新'); };

load();
