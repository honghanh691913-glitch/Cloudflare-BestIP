let cfg;
let runtime = {sources:{}, targets:{}};
let buildInfo = {version:'dev',commit:'unknown',built_at:'unknown',web_update_ready:false};
let updateCheck = null;
let statusTimer;
let currentView = 'tasks';
let toastTimer;
let scanDetailSourceId = '';

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
    const [nextCfg, nextBuild] = await Promise.all([
      api('/api/config'),
      api('/api/version?ts=' + Date.now()).catch(() => buildInfo)
    ]);
    cfg = nextCfg;
    buildInfo = nextBuild || buildInfo;
    cfg.providers ||= [];
    cfg.sources ||= [];
    cfg.targets ||= [];
    renderVersionBadge();
    renderAll();
    await refreshStatus();
    clearInterval(statusTimer);
    statusTimer = setInterval(refreshStatus, 3000);
  } catch (e) {
    toast(`加载失败：${e.message}`, true);
  }
}

function shortCommit(v) {
  v = String(v || 'unknown');
  return v.length > 8 ? v.slice(0,8) : v;
}

function compactError(v) {
  let s = String(v || '').replace(/\r/g,' ').replace(/\n+/g,' ').replace(/\s+/g,' ').trim();
  const helpAt = s.indexOf('参数：');
  if (helpAt > 0) s = s.slice(0, helpAt).trim();
  if (s.length > 220) s = s.slice(0,220) + '…';
  return s;
}

function renderVersionBadge() {
  const eyebrow = document.querySelector('.eyebrow');
  if (!eyebrow) return;
  eyebrow.innerHTML = `BESTIP MANAGER <span class="version-badge">${esc(buildInfo.version || 'dev')} · ${esc(shortCommit(buildInfo.commit))}</span>`;
}

function renderAll() {
  renderTasks();
  renderProviders();
  renderSettings();
}

function sourceById(id) { return (cfg.sources || []).find(s => s.id === id); }
function providerById(id) { return (cfg.providers || []).find(p => p.id === id); }
function normalizeZoneDomain(v) {
  v = String(v || '').trim().toLowerCase().replace(/^https?:\/\//,'').replace(/^\/+|\/+$/g,'').replace(/\.+$/,'');
  if (v.includes('/')) v = v.split('/')[0];
  return v;
}
function providerAuthMode(p={}) {
  const m = String(p.auth_mode || '').toLowerCase();
  if (['global_api_key','global','api_key'].includes(m)) return 'global_api_key';
  if (!p.api_token && p.email && p.api_key) return 'global_api_key';
  return 'api_token';
}
function composeHostname(provider, prefix) {
  const domain = normalizeZoneDomain(provider?.zone_domain || '');
  let p = String(prefix || '').trim().toLowerCase().replace(/^\.+|\.+$/g,'');
  if (p === '@') return domain;
  if (!domain) return p;
  if (!p) return '';
  if (p === domain || p.endsWith('.' + domain)) return p;
  return `${p}.${domain}`;
}
function taskPrefixValue(t, provider) {
  if (String(t?.prefix || '').trim()) return String(t.prefix).trim().toLowerCase();
  const host = String(t?.hostname || '').trim().toLowerCase().replace(/\.+$/,'');
  const domain = normalizeZoneDomain(provider?.zone_domain || '');
  if (!host) return '';
  if (domain && host === domain) return '@';
  if (domain && host.endsWith('.' + domain)) return host.slice(0, -(domain.length + 1));
  return host;
}
function taskHostname(t) {
  const p = providerById(t?.provider_id);
  const prefix = taskPrefixValue(t, p);
  return composeHostname(p, prefix) || String(t?.hostname || '').trim().toLowerCase();
}
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
function requiredCountForSource(sourceId) {
  const counts = [];
  for (const t of (cfg.targets || [])) {
    for (const r of (t.sources || [])) if (r.source_id === sourceId) counts.push(Number(r.count || 1));
  }
  return counts.length ? Math.max(...counts) : 1;
}
function taskRunning(t) { return taskLines(t).some(x => runtime.sources?.[x.source.id]?.running); }
function taskError(t) {
  for (const x of taskLines(t)) {
    const e = runtime.sources?.[x.source.id]?.error;
    if (e) return compactError(e);
  }
  return runtime.targets?.[t.id]?.ok === false ? compactError(runtime.targets[t.id].error) : '';
}

function phaseLabel(st={}) {
  const stage = String(st.stage || '').trim();
  if (stage) return stage;
  const p = st.progress?.phase;
  return ({probe:'延迟 / 丢包初筛',region:'地区筛选',speed:'速度决赛',done:'严选完成'})[p] || '等待';
}
function progressText(st={}) {
  const p = st.progress || {};
  if (!st.running) return `${st.results?.length || 0} 条`;
  const bits = [phaseLabel(st)];
  if (p.total > 0) bits.push(`${p.percent || 0}%`, `${p.current || 0}/${p.total}`);
  return bits.join(' · ');
}
function taskProgress(t) {
  const running = taskLines(t).map(x => runtime.sources?.[x.source.id]).filter(st => st?.running);
  if (!running.length) return 0;
  return Math.max(...running.map(st => Number(st.progress?.percent || 0)));
}
function fmtNum(v, digits=1) {
  const n = Number(v || 0);
  return Number.isFinite(n) ? n.toFixed(digits) : '0';
}
function fmtLoss(v) { return `${Math.round(Number(v || 0) * 100)}%`; }
function observedStatus(r={}, selected=false, running=false) {
  if (r.qualified && selected) return [running ? '暂列入选' : '入选','selected'];
  if (r.qualified) return ['合格','ok'];
  if (!r.speed_tested) return ['待测速','waiting'];
  return ['未达标','bad'];
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
    const pct = taskProgress(t);
    const err = taskError(t);
    const synced = runtime.targets?.[t.id];
    let stateClass = runningNow ? 'running' : err ? 'bad' : synced?.ok ? 'ok' : '';
    let stateText = runningNow ? `优选中${pct ? ` ${pct}%` : ''}` : err ? '有错误' : synced?.ok ? '已同步' : '待运行';
    const chips = [];
    [...new Set(lines.map(x => x.source.family))].forEach(f => chips.push(`<span class="chip ${f==='ipv6'?'v6':'v4'}">${recordType(f)} ${fmtFamily(f)}</span>`));
    [...new Set(lines.flatMap(x => x.source.cfst?.colo || []))].slice(0,3).forEach(c => chips.push(`<span class="chip colo">${esc(c)}</span>`));
    const p = providerById(t.provider_id);
    if (p) chips.push(`<span class="chip">${esc(p.name || 'Cloudflare')}</span>`);

    const lineRows = lines.map(x => {
      const src = x.source;
      const st = runtime.sources?.[src.id] || {};
      const pg = st.progress || {};
      const meta = [
        (src.cfst?.colo || []).join(','),
        `${x.ref.count || 1} 个`,
        fmtInterval(src.interval_minutes)
      ].filter(Boolean).join(' · ');
      return `<div class="line-status-block" data-source-detail="${esc(src.id)}">
        <div class="line-row clickable">
          <span class="line-family">${recordType(src.family)}</span>
          <span class="line-meta">${esc(src.name || lineTitle(src))}${meta ? ` · ${esc(meta)}` : ''}</span>
          <span class="line-result">${esc(progressText(st))}</span>
        </div>
        ${st.running ? `<div class="progress-track"><i style="width:${Math.max(2,Math.min(100,Number(pg.percent || 0)))}%"></i></div>
          <div class="progress-mini"><span>${esc(phaseLabel(st))} ${pg.current || 0}${pg.total ? ` / ${pg.total}` : ''}</span><span>合格 ${st.funnel?.speed_passed || 0}</span><span>点此看详情</span></div>` :
          (st.observed_total ? `<div class="progress-mini"><span>决赛 ${st.funnel?.region_passed || st.observed_total} 个</span><span>速度合格 ${st.funnel?.speed_passed || st.results?.length || 0}</span><span>点此看详情</span></div>` : '')}
      </div>`;
    }).join('') || `<div class="line-row"><span class="line-meta">尚未配置 IP 线路</span></div>`;

    const d = document.createElement('article');
    d.className = `task-card ${t.enabled ? '' : 'disabled'}`;
    d.innerHTML = `
      <div class="task-top">
        <div>
          <div class="task-domain">${esc(taskHostname(t))}</div>
          <div class="task-name">${t.name ? `备注 · ${esc(t.name)}` : esc(providerById(t.provider_id)?.name || '')}</div>
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
    d.querySelectorAll('[data-source-detail]').forEach(el => el.onclick = () => openScanDetail(el.dataset.sourceDetail));
    list.appendChild(d);
  });
}

function openScanDetail(sourceId) {
  scanDetailSourceId = sourceId;
  const src = sourceById(sourceId);
  const root = $('#modalRoot');
  root.innerHTML = `<div class="overlay" id="scanOverlay"><div class="sheet scan-sheet">
    <div class="sheet-head"><div><h3>${esc(src?.name || '扫描详情')}</h3><div class="help">${esc(src?.family || '')} · ${esc((src?.cfst?.colo || []).join(',') || '不限地区')}</div></div><button class="sheet-close" data-close>×</button></div>
    <div class="sheet-body" id="scanDetailBody"></div>
  </div></div>`;
  root.querySelectorAll('[data-close]').forEach(b => b.onclick = closeModal);
  $('#scanOverlay').onclick = e => { if (e.target.id === 'scanOverlay') closeModal(); };
  renderScanDetail();
}

function renderScanDetail() {
  if (!scanDetailSourceId) return;
  const body = $('#scanDetailBody');
  if (!body) return;
  const src = sourceById(scanDetailSourceId);
  const st = runtime.sources?.[scanDetailSourceId] || {};
  const pg = st.progress || {};
  const f = st.funnel || {};
  const need = requiredCountForSource(scanDetailSourceId);
  const logs = st.logs || [];
  const pct = Number(pg.percent || (st.running ? 0 : st.results?.length ? 100 : 0));

  // Engine already sorts live results, but sort again defensively:
  // qualified by speed desc -> pending -> speed failures by speed desc.
  const rows = [...(st.observed || [])].sort((a,b) => {
    const group = r => r.qualified ? 0 : (!r.speed_tested ? 1 : 2);
    const ga=group(a), gb=group(b);
    if (ga !== gb) return ga-gb;
    if ((ga===0 || ga===2) && Number(a.speed_mb||0)!==Number(b.speed_mb||0)) return Number(b.speed_mb||0)-Number(a.speed_mb||0);
    return Number(a.latency_ms||999999)-Number(b.latency_ms||999999);
  });

  let qualifiedIndex = 0;
  const resultRows = rows.length ? rows.slice(0,500).map(r => {
    let selected = false;
    if (r.qualified) {
      selected = qualifiedIndex < need;
      qualifiedIndex++;
    }
    const [label, cls] = observedStatus(r, selected, !!st.running);
    return `<div class="scan-result-row ${r.qualified?'is-qualified':''} ${selected?'is-selected':''}">
      <div class="scan-ip">${esc(r.ip || '')}<span class="scan-badge ${cls}">${label}</span></div>
      <div class="scan-metrics">
        <span>${r.latency_ms ? `${fmtNum(r.latency_ms)} ms` : '— ms'}</span>
        <span>丢包 ${fmtLoss(r.loss)}</span>
        <span>${r.speed_tested ? `${fmtNum(r.speed_mb,2)} MB/s` : '待测速'}</span>
        <span>${esc(r.colo || '—')}</span>
      </div>
      ${r.reject_reason ? `<div class="scan-reason">${esc(r.reject_reason)}</div>` : ''}
    </div>`;
  }).join('') : `<div class="empty mini"><b>还没有决赛 IP</b>${st.running ? '正在做延迟、丢包和地区初筛；淘汰项不会铺满列表，进入速度决赛后会实时显示。' : '点击“立即优选”开始严选。'}</div>`;

  body.innerHTML = `
    <div class="scan-live-card">
      <div class="scan-live-head"><div><div class="mini-label">当前阶段</div><b>${esc(phaseLabel(st))}</b></div><span class="scan-live-state ${st.running?'running':st.error?'bad':'ok'}">${st.running?'运行中':st.error?'失败':'完成'}</span></div>
      <div class="progress-track large"><i style="width:${Math.max(0,Math.min(100,pct))}%"></i></div>
      <div class="funnel-grid">
        <div><b>${f.total_candidates || pg.total || 0}</b><span>总候选</span></div>
        <div><b>${f.responsive || 0}</b><span>可连通</span></div>
        <div><b>${f.latency_passed || 0}</b><span>延迟通过</span></div>
        <div><b>${f.loss_passed || 0}</b><span>丢包通过</span></div>
        <div><b>${f.region_passed || 0}</b><span>地区通过 / 决赛</span></div>
        <div><b>${f.speed_tested || 0}/${f.region_passed || 0}</b><span>速度已测</span></div>
        <div><b>${f.speed_passed || 0}</b><span>速度合格</span></div>
        <div><b>${Math.min(need, Number(f.speed_passed || 0))}/${need}</b><span>${st.running?'暂列入选':'最终入选'}</span></div>
      </div>
      <div class="strict-note">严选流程：全量延迟/丢包 → 地区筛选 → 所有幸存 IP 进入速度决赛。延迟、丢包或地区不合格的 IP 直接淘汰，不占用下面列表；速度不达标会保留显示。最终按速度从高到低排序。</div>
      ${st.error ? `<div class="scan-error">${esc(compactError(st.error))}</div>` : ''}
    </div>

    <div class="scan-section-head"><b>速度决赛明细</b><span>合格实时置顶 · 按速度降序 · 目标 ${need} 个</span></div>
    <div class="scan-results">${resultRows}</div>

    <details class="scan-log-box" open>
      <summary>实时日志 · 最近 ${logs.length} 条</summary>
      <pre>${esc(logs.join('\n') || '等待日志…')}</pre>
    </details>`;
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
    if (scanDetailSourceId) renderScanDetail();
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
      <div><b>${esc(p.name || 'Cloudflare')}</b><div class="provider-meta">${esc(normalizeZoneDomain(p.zone_domain) || '未设置主域名')} · ${providerAuthMode(p)==='global_api_key'?'Email + Global API Key':'API Token'} · ${used} 个任务</div></div>
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
  const updateReady = !!buildInfo.web_update_ready;
  const available = !!updateCheck?.update_available;
  const latest = updateCheck?.latest_commit ? shortCommit(updateCheck.latest_commit) : '未检查';
  panel.innerHTML = `
    <div class="settings-card update-card">
      <div class="update-head">
        <div>
          <div class="mini-label">当前版本</div>
          <div class="version-title">${esc(buildInfo.version || 'dev')} <span>${esc(shortCommit(buildInfo.commit))}</span></div>
          <div class="help">构建：${esc(buildInfo.built_at || 'unknown')} · 最新 main：${esc(latest)}</div>
        </div>
        <span class="update-state ${available?'available':''}">${available?'有新版本':'版本状态'}</span>
      </div>
      <div id="updateMessage" class="modal-note">${updateReady ? '已启用 Web 一键更新。更新时页面会短暂断开并自动恢复。' : '当前未挂载 Docker Socket，只能检查版本；换用新版 Compose 后即可一键更新。'}</div>
      <div class="grid2 update-actions">
        <button id="checkUpdateBtn" class="ghost">检查更新</button>
        <button id="applyUpdateBtn" class="primary" ${updateReady?'':'disabled'}>${available?'更新到最新版':'重新拉取最新版'}</button>
      </div>
    </div>
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
  $('#checkUpdateBtn').onclick = checkForUpdate;
  $('#applyUpdateBtn').onclick = applyWebUpdate;
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

async function checkForUpdate() {
  const msg = $('#updateMessage');
  if (msg) msg.textContent = '正在检查 GitHub main…';
  try {
    updateCheck = await api('/api/update/check?ts=' + Date.now());
    buildInfo = updateCheck.current || buildInfo;
    renderVersionBadge();
    renderSettings();
    toast(updateCheck.update_available ? '发现新版本' : '当前已是最新 main');
  } catch (e) {
    if (msg) msg.textContent = `检查失败：${e.message}`;
    toast(`检查更新失败：${e.message}`, true);
  }
}

async function applyWebUpdate() {
  if (!buildInfo.web_update_ready) return toast('请先换用启用 Docker Socket 的新版 Compose', true);
  if (!confirm('更新会拉取 latest 镜像并重建 BestIP 容器，页面会短暂断开。继续吗？')) return;
  const oldCommit = String(buildInfo.commit || '');
  const btn = $('#applyUpdateBtn');
  if (btn) { btn.disabled = true; btn.textContent = '正在拉取…'; }
  try {
    await api('/api/update/apply', {method:'POST'});
    toast('已启动更新，等待服务重新上线');
    waitForUpdatedService(oldCommit);
  } catch (e) {
    toast(`更新失败：${e.message}`, true);
    renderSettings();
  }
}

function waitForUpdatedService(oldCommit) {
  let tries = 0;
  const timer = setInterval(async () => {
    tries++;
    try {
      const next = await api('/api/version?ts=' + Date.now());
      if (next?.commit && (next.commit !== oldCommit || tries > 5)) {
        clearInterval(timer);
        buildInfo = next;
        renderVersionBadge();
        toast(`更新完成：${next.version} · ${shortCommit(next.commit)}`);
        setTimeout(() => location.reload(), 900);
      }
    } catch (_) {}
    if (tries >= 60) {
      clearInterval(timer);
      toast('更新等待超时，请刷新页面确认版本', true);
      renderSettings();
    }
  }, 2500);
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
    provider_id:cfg.providers[0].id, prefix:'', hostname:'', ttl:60, proxied:false, sources:[]
  };
  t.prefix = taskPrefixValue(t, providerById(t.provider_id));
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
      <div class="grid2">
        <label class="field"><span>DNS 账号</span><select id="taskProvider">${providerOptions(t.provider_id)}</select></label>
        <label class="field"><span>域名前缀</span><input id="taskPrefix" value="${esc(t.prefix || '')}" placeholder="例如 v4 / nrtv4 / @" autocomplete="off"></label>
      </div>
      <div id="taskDomainPreview" class="domain-preview"></div>
      <label class="field"><span>备注（可选）</span><input id="taskName" value="${esc(t.name || '')}" placeholder="例如 日本 NRT 严选、移动线路"></label>
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
          <label class="field"><span>结果地区（可空）</span><input data-colo value="${esc((s.cfst.colo || []).join(','))}" placeholder="NRT"><small>先测速，再按实际 Colo 结果筛选；不会再用 HTTPing 提前淘汰。</small></label>
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
            <label class="field"><span>HTTPing（无地区筛选时）</span><select data-httping><option value="true" ${s.cfst.httping?'selected':''}>开启</option><option value="false" ${!s.cfst.httping?'selected':''}>关闭</option></select></label>
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
  const updateTaskDomainPreview = () => {
    const provider = providerById($('#taskProvider').value);
    const prefix = $('#taskPrefix').value.trim().toLowerCase();
    const hostname = composeHostname(provider, prefix);
    const domain = normalizeZoneDomain(provider?.zone_domain || '');
    $('#taskDomainPreview').innerHTML = hostname
      ? `<b>最终域名</b><span>${esc(hostname)}</span>`
      : `<b>最终域名</b><span>${domain ? '请输入前缀；根域名请填 @' : '请先在 DNS 账号中设置主域名'}</span>`;
  };
  $('#taskProvider').onchange = updateTaskDomainPreview;
  $('#taskPrefix').oninput = updateTaskDomainPreview;
  updateTaskDomainPreview();

  root.querySelectorAll('[data-close]').forEach(b => b.onclick = closeModal);
  $('#taskOverlay').onclick = e => { if (e.target.id === 'taskOverlay') closeModal(); };
  $('#saveTaskBtn').onclick = async () => {
    t.provider_id = $('#taskProvider').value;
    const provider = providerById(t.provider_id);
    t.prefix = $('#taskPrefix').value.trim().toLowerCase();
    t.hostname = composeHostname(provider, t.prefix);
    t.name = $('#taskName').value.trim();
    t.enabled = $('#taskEnabled').checked;
    t.ttl = Math.max(1, Number($('#taskTTL').value) || 60);
    t.proxied = $('#taskProxied').value === 'true';
    if (!t.provider_id) return toast('请选择 DNS 账号', true);
    if (!normalizeZoneDomain(provider?.zone_domain || '')) return toast('这个 DNS 账号还没有设置主域名 / Zone', true);
    if (!t.prefix) return toast('请填写域名前缀；根域名请填 @', true);
    if (!t.hostname) return toast('无法生成最终域名', true);
    if (!drafts.length) return toast('至少保留一条 IP 线路', true);
    if (drafts.some(d => !(d.source.inputs || []).length)) return toast('每条线路至少填写一个 IP 段或 IP 源', true);
    if (await persistTask(t, drafts, existing)) closeModal();
  };
  if (existing) $('#deleteTaskBtn').onclick = async () => {
    if (!confirm(`删除 ${taskHostname(existing)}？\n同时会删除仅属于这个任务的 IP 线路。`)) return;
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
  const p = existing ? clone(existing) : {
    id:uid('cf'), name:'Cloudflare', type:'cloudflare',
    zone_id:'', zone_domain:'', auth_mode:'api_token',
    api_token:'', email:'', api_key:''
  };
  p.auth_mode = providerAuthMode(p);

  const root = $('#modalRoot');
  root.innerHTML = `<div class="overlay" id="providerOverlay"><div class="sheet" style="max-width:580px">
    <div class="sheet-head"><h3>${existing?'编辑 DNS 账号':'添加 DNS 账号'}</h3><button class="sheet-close" data-close>×</button></div>
    <div class="sheet-body">
      <label class="field"><span>名称</span><input id="providerName" value="${esc(p.name || 'Cloudflare')}" placeholder="例如 Cloudflare 主账号"></label>
      <label class="field"><span>主域名 / Zone</span><input id="providerDomain" value="${esc(p.zone_domain || '')}" placeholder="例如 629717.xyz" autocomplete="off"></label>
      <label class="field"><span>Zone ID</span><input id="providerZone" value="${esc(p.zone_id || '')}" autocomplete="off"></label>

      <label class="field"><span>认证方式</span><select id="providerAuthMode">
        <option value="api_token" ${p.auth_mode==='api_token'?'selected':''}>API Token（推荐，不需要邮箱）</option>
        <option value="global_api_key" ${p.auth_mode==='global_api_key'?'selected':''}>Email + Global API Key（兼容旧 BestIP）</option>
      </select></label>

      <div id="providerTokenBox">
        <label class="field"><span>API Token</span><input id="providerToken" type="password" value="${esc(p.api_token || '')}" autocomplete="new-password" placeholder="Cloudflare API Token"></label>
        <div class="modal-note">API Token 不需要邮箱。建议权限至少包含该 Zone 的 <b>Zone:Read</b> 与 <b>DNS:Edit</b>。</div>
      </div>

      <div id="providerGlobalKeyBox">
        <label class="field"><span>Cloudflare 账号邮箱</span><input id="providerEmail" type="email" value="${esc(p.email || '')}" autocomplete="email" placeholder="your@email.com"></label>
        <label class="field"><span>Global API Key</span><input id="providerAPIKey" type="password" value="${esc(p.api_key || '')}" autocomplete="new-password" placeholder="Global API Key"></label>
        <div class="modal-note">这是原 BestIP 的认证方式：请求会使用 <b>X-Auth-Email + X-Auth-Key</b>。</div>
      </div>

      <div class="modal-note">主域名只在 DNS 账号里配置一次。以后任务只填前缀，例如 <b>v4</b> → <b>v4.${esc(p.zone_domain || '629717.xyz')}</b>；根域名使用 <b>@</b>。</div>
    </div>
    <div class="sheet-actions provider-sheet-actions">
      <button data-close>取消</button>
      <button id="testProviderBtn" class="ghost">测试连接</button>
      <button id="saveProviderBtn" class="primary">保存账号</button>
    </div>
  </div></div>`;

  function readProviderForm() {
    p.name = $('#providerName').value.trim() || 'Cloudflare';
    p.zone_domain = normalizeZoneDomain($('#providerDomain').value);
    p.zone_id = $('#providerZone').value.trim();
    p.auth_mode = $('#providerAuthMode').value;
    p.api_token = $('#providerToken').value.trim();
    p.email = $('#providerEmail').value.trim();
    p.api_key = $('#providerAPIKey').value.trim();
    return p;
  }
  function validateProviderForm() {
    readProviderForm();
    if (!p.zone_domain) return '请填写主域名 / Zone，例如 629717.xyz';
    if (!p.zone_id) return '请填写 Zone ID';
    if (p.auth_mode === 'global_api_key') {
      if (!p.email || !p.api_key) return 'Global API Key 模式需要同时填写邮箱和 Global API Key';
    } else if (!p.api_token) {
      return 'API Token 模式需要填写 API Token';
    }
    return '';
  }
  function drawAuthMode() {
    const legacy = $('#providerAuthMode').value === 'global_api_key';
    $('#providerTokenBox').style.display = legacy ? 'none' : '';
    $('#providerGlobalKeyBox').style.display = legacy ? '' : 'none';
  }

  $('#providerAuthMode').onchange = drawAuthMode;
  drawAuthMode();

  root.querySelectorAll('[data-close]').forEach(b => b.onclick = closeModal);
  $('#providerOverlay').onclick = e => { if (e.target.id === 'providerOverlay') closeModal(); };

  $('#testProviderBtn').onclick = async () => {
    const err = validateProviderForm();
    if (err) return toast(err, true);
    const btn = $('#testProviderBtn');
    btn.disabled = true;
    btn.textContent = '测试中…';
    try {
      const r = await api('/api/provider/test', {method:'POST', body:JSON.stringify(readProviderForm())});
      toast(r.message || 'Cloudflare 连接正常');
    } catch (e) {
      toast(e.message, true);
    } finally {
      btn.disabled = false;
      btn.textContent = '测试连接';
    }
  };

  $('#saveProviderBtn').onclick = async () => {
    const err = validateProviderForm();
    if (err) return toast(err, true);
    readProviderForm();
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

function closeModal() { scanDetailSourceId = ''; $('#modalRoot').innerHTML = ''; }

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
