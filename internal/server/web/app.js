let cfg;
let statusTimer;
const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"']/g, m => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]));

async function api(url, opt={}) {
  const r = await fetch(url, {headers:{'Content-Type':'application/json'}, ...opt});
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

async function load() {
  cfg = await api('/api/config');
  render();
  await refreshStatus();
  clearInterval(statusTimer);
  statusTimer = setInterval(refreshStatus, 3000);
}

function render() {
  renderGlobal();
  renderProviders();
  renderSources();
  renderTargets();
  syncRaw();
}

function renderGlobal() {
  $('#global').innerHTML = `<div class="row">
    <label class="f3">监听地址<input id="listen" value="${esc(cfg.listen || ':8080')}"></label>
    <label class="f3">最大并发测速任务<input id="maxConcurrency" type="number" min="1" max="64" value="${cfg.max_concurrency || 2}"></label>
    <div class="f6 muted">最大并发修改后建议重启容器，使任务槽数量同步更新。</div>
  </div>`;
  $('#listen').onchange = e => { cfg.listen = e.target.value.trim(); syncRaw(); };
  $('#maxConcurrency').onchange = e => { cfg.max_concurrency = Math.max(1, Number(e.target.value) || 1); syncRaw(); };
}

function renderProviders() {
  const box = $('#providers');
  box.innerHTML = '';
  (cfg.providers || []).forEach((p, i) => {
    const d = document.createElement('div');
    d.className = 'item';
    d.innerHTML = `<div class="item-head"><b>${esc(p.name || p.id)}</b><button data-del class="danger">删除</button></div>
      <div class="row">
        <label class="f3">ID<input data-k="id" value="${esc(p.id)}"></label>
        <label class="f3">名称<input data-k="name" value="${esc(p.name)}"></label>
        <label class="f2">类型<select data-k="type"><option value="cloudflare" selected>cloudflare</option></select></label>
        <label class="f4">Zone ID<input data-k="zone_id" value="${esc(p.zone_id || '')}"></label>
        <label class="f12">API Token<input type="password" data-k="api_token" value="${esc(p.api_token || '')}" autocomplete="new-password"></label>
      </div>`;
    d.querySelectorAll('[data-k]').forEach(el => el.onchange = () => {
      const oldID = p.id;
      p[el.dataset.k] = el.value.trim();
      if (el.dataset.k === 'id' && oldID !== p.id) {
        (cfg.targets || []).forEach(t => { if (t.provider_id === oldID) t.provider_id = p.id; });
      }
      renderTargets(); syncRaw();
    });
    d.querySelector('[data-del]').onclick = () => {
      if ((cfg.targets || []).some(t => t.provider_id === p.id)) return alert('仍有 Target 引用此 Provider，请先修改对应 Target。');
      cfg.providers.splice(i, 1); render();
    };
    box.appendChild(d);
  });
}

function renderSources() {
  const box = $('#sources');
  box.innerHTML = '';
  (cfg.sources || []).forEach((s, i) => {
    s.cfst = s.cfst || {};
    const d = document.createElement('div');
    d.className = 'item';
    d.innerHTML = `<div class="item-head"><b>${esc(s.name || s.id)}</b><div><button data-run>立即测速</button> <button data-del class="danger">删除</button></div></div>
      <div class="row">
        <label class="f3">ID<input data-k="id" value="${esc(s.id)}"></label>
        <label class="f3">名称<input data-k="name" value="${esc(s.name)}"></label>
        <label class="f2">协议<select data-k="family"><option ${s.family==='ipv4'?'selected':''}>ipv4</option><option ${s.family==='ipv6'?'selected':''}>ipv6</option></select></label>
        <label class="f2">周期(分钟)<input type="number" min="0" data-k="interval_minutes" value="${s.interval_minutes || 0}"></label>
        <label class="f2">启用<select data-k="enabled"><option value="true" ${s.enabled?'selected':''}>是</option><option value="false" ${!s.enabled?'selected':''}>否</option></select></label>
        <label class="f12">IP 源（每行一个：URL / 文件 / CIDR / IP）<textarea data-inputs>${esc((s.inputs || []).join('\n'))}</textarea></label>
        <label class="f3">测速 URL<input data-c="url" value="${esc(s.cfst.url || '')}"></label>
        <label class="f2">最低速度 MB/s<input type="number" step="0.1" data-c="speed_min_mb" value="${s.cfst.speed_min_mb || 0}"></label>
        <label class="f2">延迟上限 ms<input type="number" step="0.1" data-c="latency_max_ms" value="${s.cfst.latency_max_ms || 0}"></label>
        <label class="f2">丢包上限 0-1<input type="number" step="0.01" min="0" max="1" data-c="loss_max" value="${s.cfst.loss_max || 0}"></label>
        <label class="f3">Colo（逗号）<input data-colo value="${esc((s.cfst.colo || []).join(','))}" placeholder="NRT,HKG"></label>
        <label class="f2">结果缓存数量<input type="number" min="1" data-k="keep_results" value="${s.keep_results || 50}"></label>
        <label class="f2">HTTPing<select data-c-bool="httping"><option value="true" ${s.cfst.httping?'selected':''}>是</option><option value="false" ${!s.cfst.httping?'selected':''}>否</option></select></label>
        <label class="f2">All IP<select data-c-bool="all_ip"><option value="true" ${s.cfst.all_ip?'selected':''}>是</option><option value="false" ${!s.cfst.all_ip?'selected':''}>否</option></select></label>
      </div>`;
    bindSource(d, s, i);
    box.appendChild(d);
  });
}

function bindSource(d, s, i) {
  d.querySelectorAll('[data-k]').forEach(el => el.onchange = () => {
    const key = el.dataset.k;
    const oldID = s.id;
    let v = el.value;
    if (key === 'enabled') v = v === 'true';
    else if (el.type === 'number') v = Number(v);
    s[key] = v;
    if (key === 'id' && oldID !== s.id) (cfg.targets || []).forEach(t => (t.sources || []).forEach(r => { if (r.source_id === oldID) r.source_id = s.id; }));
    renderTargets(); syncRaw();
  });
  d.querySelector('[data-inputs]').onchange = e => { s.inputs = e.target.value.split(/\n+/).map(x=>x.trim()).filter(Boolean); syncRaw(); };
  d.querySelectorAll('[data-c]').forEach(el => el.onchange = () => { s.cfst[el.dataset.c] = el.type === 'number' ? Number(el.value) : el.value; syncRaw(); });
  d.querySelectorAll('[data-c-bool]').forEach(el => el.onchange = () => { s.cfst[el.dataset.cBool] = el.value === 'true'; syncRaw(); });
  d.querySelector('[data-colo]').onchange = e => { s.cfst.colo = e.target.value.split(',').map(x=>x.trim().toUpperCase()).filter(Boolean); syncRaw(); };
  d.querySelector('[data-run]').onclick = async () => { try { await api('/api/run/source?id='+encodeURIComponent(s.id), {method:'POST'}); } catch(e) { alert(e.message); } refreshStatus(); };
  d.querySelector('[data-del]').onclick = () => {
    cfg.sources.splice(i,1);
    (cfg.targets || []).forEach(t => t.sources = (t.sources || []).filter(r => r.source_id !== s.id));
    render();
  };
}

function renderTargets() {
  const box = $('#targets');
  box.innerHTML = '';
  (cfg.targets || []).forEach((t, i) => {
    const sourceOptions = (cfg.sources || []).map(s => `<option value="${esc(s.id)}">${esc(s.name || s.id)} (${s.family})</option>`).join('');
    const providerOptions = (cfg.providers || []).map(p => `<option value="${esc(p.id)}" ${p.id===t.provider_id?'selected':''}>${esc(p.name || p.id)}</option>`).join('');
    const refs = (t.sources || []).map((r, ri) => `<div class="ref"><select data-ref-source="${ri}">${sourceOptions.replace(`value="${esc(r.source_id)}"`,`value="${esc(r.source_id)}" selected`)}</select><input type="number" min="1" data-ref-count="${ri}" value="${r.count || 1}"><button data-ref-del="${ri}">×</button></div>`).join('');
    const d = document.createElement('div');
    d.className = 'item';
    d.innerHTML = `<div class="item-head"><b>${esc(t.name || t.hostname || t.id)}</b><div><button data-sync>同步 DNS</button> <button data-del class="danger">删除</button></div></div>
      <div class="row">
        <label class="f3">ID<input data-k="id" value="${esc(t.id)}"></label>
        <label class="f3">名称<input data-k="name" value="${esc(t.name)}"></label>
        <label class="f4">Hostname<input data-k="hostname" value="${esc(t.hostname)}"></label>
        <label class="f2">启用<select data-k="enabled"><option value="true" ${t.enabled?'selected':''}>是</option><option value="false" ${!t.enabled?'selected':''}>否</option></select></label>
        <label class="f4">DNS Provider<select data-k="provider_id">${providerOptions}</select></label>
        <label class="f2">TTL<input type="number" min="1" data-k="ttl" value="${t.ttl || 60}"></label>
        <label class="f2">Cloudflare 代理<select data-k="proxied"><option value="false" ${!t.proxied?'selected':''}>关闭</option><option value="true" ${t.proxied?'selected':''}>开启</option></select></label>
        <div class="f12"><span class="muted">引用 IP 池（同一 hostname 可同时引用 IPv4 + IPv6，自动写 A + AAAA）</span>${refs}<button data-add-ref>+ 引用 IP 池</button></div>
      </div>`;
    bindTarget(d, t, i);
    box.appendChild(d);
  });
}

function bindTarget(d, t, i) {
  d.querySelectorAll('[data-k]').forEach(el => el.onchange = () => {
    const key = el.dataset.k;
    let v = el.value;
    if (key === 'enabled' || key === 'proxied') v = v === 'true';
    else if (el.type === 'number') v = Number(v);
    t[key] = v; syncRaw();
  });
  d.querySelectorAll('[data-ref-source]').forEach(el => el.onchange = () => { t.sources[Number(el.dataset.refSource)].source_id = el.value; syncRaw(); });
  d.querySelectorAll('[data-ref-count]').forEach(el => el.onchange = () => { t.sources[Number(el.dataset.refCount)].count = Number(el.value); syncRaw(); });
  d.querySelectorAll('[data-ref-del]').forEach(el => el.onclick = () => { t.sources.splice(Number(el.dataset.refDel),1); renderTargets(); syncRaw(); });
  d.querySelector('[data-add-ref]').onclick = () => { if (!cfg.sources.length) return alert('请先创建 IP 池'); t.sources = t.sources || []; t.sources.push({source_id:cfg.sources[0].id,count:5}); renderTargets(); syncRaw(); };
  d.querySelector('[data-sync]').onclick = async () => { try { await api('/api/sync/target?id='+encodeURIComponent(t.id), {method:'POST'}); } catch(e) { alert(e.message); } refreshStatus(); };
  d.querySelector('[data-del]').onclick = () => { cfg.targets.splice(i,1); render(); };
}

function syncRaw() { $('#raw').value = JSON.stringify(cfg, null, 2); }

async function refreshStatus() {
  let s;
  try { s = await api('/api/status'); } catch (_) { return; }
  const box = $('#status'); box.innerHTML = '';
  for (const src of (cfg.sources || [])) {
    const st = s.sources[src.id] || {};
    const d = document.createElement('div'); d.className='card';
    d.innerHTML = `<b>${esc(src.name || src.id)}</b><div class="muted">${esc(st.stage || '尚未运行')}</div><div>${st.running?'<span class="pill">运行中</span>':''}<span class="pill">结果 ${st.results?.length || 0}</span></div>${st.error?`<div class="bad">${esc(st.error)}</div>`:''}`;
    box.appendChild(d);
  }
  for (const t of (cfg.targets || [])) {
    const st = s.targets[t.id]; const d=document.createElement('div'); d.className='card';
    d.innerHTML = `<b>${esc(t.hostname)}</b><div class="muted">DNS Target</div>${st?`<div class="${st.ok?'ok':'bad'}">${st.ok?'最近同步成功':esc(st.error || '同步失败')}</div>`:'<div class="muted">尚未同步</div>'}`;
    box.appendChild(d);
  }
}

$('#save').onclick = async () => { try { await api('/api/config',{method:'PUT',body:JSON.stringify(cfg)}); alert('配置已保存'); } catch(e) { alert(e.message); } };
$('#runAll').onclick = async () => { try { await api('/api/run/all',{method:'POST'}); } catch(e) { alert(e.message); } };
$('#applyRaw').onclick = () => { try { cfg=JSON.parse($('#raw').value); render(); } catch(e) { alert(e.message); } };
$('#addProvider').onclick = () => { cfg.providers = cfg.providers || []; cfg.providers.push({id:'cf-'+Date.now(),name:'Cloudflare',type:'cloudflare',zone_id:'',api_token:''}); render(); };
$('#addSource').onclick = () => { cfg.sources = cfg.sources || []; cfg.sources.push({id:'source-'+Date.now(),name:'新 IP 池',enabled:true,family:'ipv4',inputs:[],interval_minutes:360,keep_results:50,cfst:{binary:'cfst',url:'https://speed.cloudflare.com/__down?bytes=200000000',port:443,threads:4,ping_count:200,download_count:10,download_time:10,latency_max_ms:200,latency_min_ms:0,loss_max:0.2,speed_min_mb:5,httping:true,colo:[],all_ip:false}}); render(); };
$('#addTarget').onclick = () => { cfg.targets = cfg.targets || []; cfg.targets.push({id:'target-'+Date.now(),name:'新域名',enabled:true,provider_id:cfg.providers?.[0]?.id || '',hostname:'',ttl:60,proxied:false,sources:[]}); render(); };
load();
