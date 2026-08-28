let cfg;
let runtime = {sources:{},targets:{},health:{},period:'day'};
let buildInfo = {version:'dev',commit:'unknown',built_at:'unknown',web_update_ready:false,real_test_ready:false};
let updateCheck = null;
let currentView = 'tasks';
let statusTimer = null;
let toastTimer = null;
let scanDetailSourceId = '';
let furnaceData = {profiles:[],rules:[],period:'day'};
let furnaceSourceFilter = 'all';
let furnaceShowGray = false;
let furnaceLoadedAt = 0;
let updateRunTimer = null;
let updateRunState = null;

const $ = s => document.querySelector(s);
const $$ = s => [...document.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, m => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]));
const clone = v => JSON.parse(JSON.stringify(v));
const uid = p => `${p}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2,7)}`;

const UI_SCROLL_SELECTOR = '.sheet-body,.result-table,.logs,.raw,.task-list,.stack,#furnaceList,.filter-row,[data-preserve-scroll]';

function uiNodeKey(el,index){
  if(el.dataset?.uiKey)return`data:${el.dataset.uiKey}`;
  if(el.id)return`id:${el.id}`;
  const source=el.closest?.('[data-source]')?.dataset?.source||'';
  const ip=el.closest?.('[data-ip]')?.dataset?.ip||'';
  const cls=[...el.classList].slice(0,3).join('.');
  return`${el.tagName}:${source}:${ip}:${cls}:${index}`;
}
function captureUIState(){
  const state={x:window.scrollX,y:window.scrollY,details:{},scrolls:{}};
  [...document.querySelectorAll('details')].forEach((el,i)=>{
    state.details[uiNodeKey(el,i)]=!!el.open;
  });
  [...document.querySelectorAll(UI_SCROLL_SELECTOR)].forEach((el,i)=>{
    const key=uiNodeKey(el,i);
    state.scrolls[key]={
      top:el.scrollTop,
      left:el.scrollLeft,
      nearBottom:(el.scrollHeight-el.clientHeight-el.scrollTop)<36
    };
  });
  return state;
}
function restoreUIState(state){
  if(!state)return;
  requestAnimationFrame(()=>{
    [...document.querySelectorAll('details')].forEach((el,i)=>{
      const key=uiNodeKey(el,i);
      if(Object.prototype.hasOwnProperty.call(state.details,key))el.open=state.details[key];
    });
    [...document.querySelectorAll(UI_SCROLL_SELECTOR)].forEach((el,i)=>{
      const key=uiNodeKey(el,i),v=state.scrolls[key];
      if(!v)return;
      el.scrollLeft=v.left||0;
      el.scrollTop=v.nearBottom?Math.max(0,el.scrollHeight-el.clientHeight):v.top||0;
    });
    window.scrollTo(state.x||0,state.y||0);
  });
}
function renderWithUIState(fn){
  const state=captureUIState();
  fn();
  restoreUIState(state);
}

const DEFAULT_SPEED_URL = 'https://cf.090227.xyz/__down?bytes=99999999';
const DEFAULT_REAL_TEST_URL = 'https://www.google.com/generate_204';
const DEFAULT_PROBE_URL = 'https://speed.cloudflare.com/cdn-cgi/trace';
const DEFAULT_CFST = {
  binary:'cfst',url:'',probe_url:'',port:443,threads:200,ping_count:4,download_count:10,download_time:10,
  latency_max_ms:200,latency_min_ms:0,loss_max:.2,speed_min_mb:5,httping:false,colo:[],all_ip:false
};

async function api(url,opt={}){
  const r=await fetch(url,{headers:{'Content-Type':'application/json'},...opt});
  const j=await r.json().catch(()=>({}));
  if(!r.ok) throw new Error(j.error||r.statusText||`HTTP ${r.status}`);
  return j;
}
function toast(msg,bad=false){const el=$('#toast');if(!el)return;el.textContent=msg;el.classList.toggle('bad',bad);el.classList.add('show');clearTimeout(toastTimer);toastTimer=setTimeout(()=>el.classList.remove('show'),2600)}
function shortCommit(v){v=String(v||'unknown');return v.length>8?v.slice(0,8):v}
function compactError(v){let s=String(v||'').replace(/\r/g,' ').replace(/\n+/g,' ').replace(/\s+/g,' ').trim();if(s.length>180)s=s.slice(0,180)+'…';return s}
function fmtNum(v,d=1){const n=Number(v||0);return Number.isFinite(n)?n.toFixed(d):'0'}
function fmtPercent(v){return `${Math.round(Number(v||0)*100)}%`}
function fmtFamily(f){return f==='ipv6'?'IPv6':'IPv4'}
function recordType(f){return f==='ipv6'?'AAAA':'A'}
function fmtInterval(n){n=Number(n||0);if(!n)return'手动';if(n%1440===0)return`${n/1440}天`;if(n%60===0)return`${n/60}小时`;return`${n}分钟`}
function fmtTime(v){if(!v)return'—';const d=new Date(v);if(Number.isNaN(d.getTime()))return'—';return d.toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false})}
function relativeTime(ms){if(ms<=0)return'即将';const m=Math.ceil(ms/60000);if(m<60)return`${m}分钟后`;const h=Math.floor(m/60),rm=m%60;return rm?`${h}小时${rm}分后`:`${h}小时后`}
function fmtSeconds(v){v=Math.max(0,Math.round(Number(v||0)));if(v<60)return`${v}秒`;const m=Math.floor(v/60),s=v%60;if(m<60)return s?`${m}分${s}秒`:`${m}分钟`;const h=Math.floor(m/60),rm=m%60;return rm?`${h}小时${rm}分`:`${h}小时`}
function sourceRequiredCount(id){let n=0;for(const t of (cfg.targets||[])){if(!t.enabled)continue;for(const r of (t.sources||[]))if(r.source_id===id)n=Math.max(n,Number(r.count||0))}return n||1}
function sourceRunLabel(id){const st=runtime.sources?.[id]||{},hs=runtime.health?.[id]||{};if(hs.running&&hs.phase==='refill')return'自动补位';if(st.running&&st.progress?.phase==='health')return'健康检查';return st.running?'严选中':''}
function normalizeZoneDomain(v){return String(v||'').trim().toLowerCase().replace(/^https?:\/\//,'').split('/')[0].replace(/^\.+|\.+$/g,'')}
function providerById(id){return (cfg.providers||[]).find(p=>p.id===id)}
function sourceById(id){return (cfg.sources||[]).find(s=>s.id===id)}
function sourceUseCount(id,exceptTargetId=''){return (cfg.targets||[]).reduce((n,t)=>n+(t.id===exceptTargetId?0:(t.sources||[]).filter(r=>r.source_id===id).length),0)}
function composeHostname(provider,prefix){const domain=normalizeZoneDomain(provider?.zone_domain||'');let p=String(prefix||'').trim().toLowerCase().replace(/^\.+|\.+$/g,'');if(!domain)return'';if(!p||p==='@')return domain;if(p===domain||p.endsWith('.'+domain))return p;return`${p}.${domain}`}
function taskPrefixValue(t,p){if(String(t?.prefix||'').trim())return String(t.prefix).trim().toLowerCase();const host=String(t?.hostname||'').trim().toLowerCase();const domain=normalizeZoneDomain(p?.zone_domain||'');if(!host)return'';if(domain&&host===domain)return'@';if(domain&&host.endsWith('.'+domain))return host.slice(0,-domain.length-1);return host}
function taskHostname(t){const p=providerById(t.provider_id);return composeHostname(p,taskPrefixValue(t,p))||String(t.hostname||'').trim()}
function taskLines(t){return (t.sources||[]).map(ref=>({ref,source:sourceById(ref.source_id)})).filter(x=>x.source)}
function lineTitle(s){const c=(s.cfst?.colo||[]).join('/');return c?`${fmtFamily(s.family)} · ${c}`:fmtFamily(s.family)}
function providerAuthMode(p){const m=String(p?.auth_mode||'').toLowerCase();if(['global_api_key','global','api_key'].includes(m))return'global_api_key';if(p?.email&&p?.api_key&&!p?.api_token)return'global_api_key';return'api_token'}

function initConfig(){
  cfg.providers ||= [];cfg.sources ||= [];cfg.targets ||= [];cfg.furnace_rules ||= [];cfg.real_profiles ||= [];
  cfg.real_test_url ||= DEFAULT_REAL_TEST_URL;cfg.real_test_attempts ||=2;cfg.real_speed_url ||= cfg.speed_url||DEFAULT_SPEED_URL;cfg.real_speed_bytes_mb ||=5;cfg.real_speed_top_n ||=10;
  cfg.probe_url ||= DEFAULT_PROBE_URL;cfg.speed_url ||= DEFAULT_SPEED_URL;cfg.health_check_minutes ??=60;cfg.max_sample_count ||=10000;cfg.furnace_retention_days ||=45;
  if(cfg.furnace_auto_rank===undefined)cfg.furnace_auto_rank=true;
  cfg.sources.forEach(s=>{s.cfst={...clone(DEFAULT_CFST),...(s.cfst||{})};s.sample_count=Number(s.sample_count|| (s.cfst.all_ip?cfg.max_sample_count:256));s.cfst.all_ip=false;s.cfst.httping=false;s.keep_results=Number(s.keep_results||50)});
}
async function load(){
  try{
    const [nextCfg,nextBuild]=await Promise.all([api('/api/config'),api('/api/version?ts='+Date.now()).catch(()=>buildInfo)]);
    cfg=nextCfg;buildInfo=nextBuild||buildInfo;initConfig();renderVersionBadge();renderAll();await refreshStatus(true);
  }catch(e){toast(`加载失败：${e.message}`,true)}
}
function renderVersionBadge(){const e=$('.eyebrow');if(e)e.innerHTML=`BESTIP MANAGER <span class="version-badge">${esc(buildInfo.version||'dev')} · ${esc(shortCommit(buildInfo.commit))}</span>`}
function renderAll(){renderTasks();renderProviders();renderSettings();if(currentView==='furnace')loadFurnace(true)}

function resultCountFor(id){return runtime.sources?.[id]?.results?.length||0}
function taskRunning(t){return taskLines(t).some(x=>runtime.sources?.[x.source.id]?.running)}
function taskError(t){for(const x of taskLines(t)){const e=runtime.sources?.[x.source.id]?.error;if(e)return compactError(e)}const ts=runtime.targets?.[t.id];return ts?.ok===false?compactError(ts.error):''}
function nextRunForTask(t){
  let best=Infinity,manual=true;
  for(const {source:s} of taskLines(t)){
    const iv=Number(s.interval_minutes||0);if(!iv)continue;manual=false;
    const st=runtime.sources?.[s.id]||{};const base=new Date(st.ended_at||st.started_at||Date.now()).getTime();const left=base+iv*60000-Date.now();if(left<best)best=left;
  }
  return manual?'仅手动':relativeTime(best===Infinity?0:best);
}
function taskState(t){
  if(!t.enabled)return{cls:'disabled',badge:'',text:'已关闭'};
  const labels=taskLines(t).map(x=>sourceRunLabel(x.source.id)).filter(Boolean);
  if(labels.length)return{cls:'state-running',badge:'running',text:labels.includes('自动补位')?'自动补位':(labels.includes('健康检查')?'健康检查':'严选中')};
  const err=taskError(t);if(err)return{cls:'state-error',badge:'bad',text:'异常'};
  const ran=taskLines(t).some(x=>runtime.sources?.[x.source.id]?.ended_at);
  if(ran)return{cls:'state-waiting',badge:'ok',text:'等待下次'};
  return{cls:'state-pending',badge:'',text:'待首次运行'};
}
function renderTasks(){
  const list=$('#taskList');if(!list||!cfg)return;
  const targets=cfg.targets||[];const running=targets.filter(taskRunning).length;const errors=targets.filter(t=>!!taskError(t)).length;
  $('#summary').innerHTML=`<div class="summary-box"><b>${targets.length}</b><span>域名任务</span></div><div class="summary-box"><b>${running}</b><span>正在严选</span></div><div class="summary-box"><b>${errors}</b><span>异常任务</span></div>`;
  if(!targets.length){list.innerHTML='<div class="empty"><b>还没有任务</b>先在“我的”配置 Cloudflare，再建立第一个域名。</div>';return}
  list.innerHTML='';
  targets.forEach(t=>{
    const state=taskState(t),err=taskError(t),lines=taskLines(t),p=providerById(t.provider_id),healthRows=[];
    for(const x of lines){const hs=runtime.health?.[x.source.id];if(hs)healthRows.push(hs)}
    const chips=[];[...new Set(lines.map(x=>x.source.family))].forEach(f=>chips.push(`<span class="chip ${f==='ipv6'?'v6':'v4'}">${recordType(f)} ${fmtFamily(f)}</span>`));
    [...new Set(lines.flatMap(x=>x.source.cfst?.colo||[]))].slice(0,3).forEach(c=>chips.push(`<span class="chip colo">${esc(c)}</span>`));
    if(p)chips.push(`<span class="chip">${esc(p.name||'Cloudflare')}</span>`);
    if(healthRows.length){const busy=healthRows.find(h=>h.running),good=healthRows.every(h=>h.ok);let text='健康正常',cls='health-good';if(busy){text=busy.phase==='refill'?'自动补位中':'健康检测中';cls='health-warn'}else if(!good){text=healthRows.some(h=>h.phase==='failed')?'健康异常':'待自动补位';cls='health-bad'}chips.push(`<span class="chip ${cls}">${esc(text)}</span>`)}
    const rows=lines.map(x=>{
      const s=x.source,st=runtime.sources?.[s.id]||{},pr=st.progress||{};const pct=Number(pr.percent||0);
      const rp=realProfileById(s.real_profile_id);const meta=[(s.cfst?.colo||[]).join(','),rp?`真:${rp.name}`:'',`扫 ${s.sample_count||256}`,`写 ${x.ref.count||1}`,fmtInterval(s.interval_minutes)].filter(Boolean).join(' · ');
      const eta=Number(pr.eta_seconds||0)>0?` · 剩${fmtSeconds(pr.eta_seconds)}`:'';
      return `<div class="line-row" data-source="${esc(s.id)}"><span class="line-family">${recordType(s.family)}</span><span class="line-meta">${esc(s.name||lineTitle(s))} · ${esc(meta)}${st.running?`<div class="mini-progress"><i style="width:${Math.min(100,pct)}%"></i></div>`:''}</span><span class="line-result">${st.running?`${esc(sourceRunLabel(s.id)||st.stage||'扫描')} ${pct}%${eta}`:`${st.results?.length||0} 条`}</span></div>`;
    }).join('');
    const wrap=document.createElement('div');wrap.className='task-unit';
    const busy=taskRunning(t);
    const runButton=busy?`<button class="run run-busy" data-run><span class="run-normal">严选中…</span><span class="run-stop">停止严选</span></button>`:`<button class="run" data-run>立即严选</button>`;
    wrap.innerHTML=`<div class="task-unit-bar"><button class="task-power ${t.enabled?'on':'off'}" data-toggle>${t.enabled?'● 已开启':'○ 已关闭'}</button><span class="next-run">${t.enabled?`下次：${nextRunForTask(t)}`:'不会自动扫描'}</span></div><article class="task-card ${state.cls}"><div class="task-top"><div><div class="task-domain">${esc(taskHostname(t))}</div>${t.name?`<div class="task-name">${esc(t.name)}</div>`:''}</div><div class="status-dot ${state.badge}">${state.text}</div></div><div class="chips">${chips.join('')}</div><div class="line-preview">${rows||'<div class="line-row"><span class="line-meta">尚未配置线路</span></div>'}</div>${err?`<div class="task-error">${esc(err)}</div>`:''}<div class="card-actions">${runButton}<button class="sync" data-sync>同步 DNS</button><button class="more" data-edit>•••</button></div></article>`;
    wrap.querySelector('[data-toggle]').onclick=()=>toggleTask(t.id);
    wrap.querySelector('[data-run]').onclick=()=>busy?stopTask(t):runTask(t);
    wrap.querySelector('[data-sync]').onclick=()=>syncTask(t);
    wrap.querySelector('[data-edit]').onclick=()=>openTaskEditor(t.id);
    wrap.querySelectorAll('[data-source]').forEach(el=>el.onclick=()=>openScanDetail(el.dataset.source));
    list.appendChild(wrap);
  })
}
async function toggleTask(id){const t=cfg.targets.find(x=>x.id===id);if(!t)return;t.enabled=!t.enabled;recomputeSourceEnabled();await saveConfig(t.enabled?'任务已开启':'任务已关闭')}
function recomputeSourceEnabled(){for(const s of cfg.sources)s.enabled=cfg.targets.some(t=>t.enabled&&(t.sources||[]).some(r=>r.source_id===s.id))}
async function runTask(t){if(!t.enabled)return toast('先开启这个任务',true);const lines=taskLines(t);if(!lines.length)return toast('没有 IP 线路',true);try{for(const x of lines)await api('/api/run/source?id='+encodeURIComponent(x.source.id),{method:'POST'});toast(`已启动 ${lines.length} 条严选线路`);setTimeout(()=>refreshStatus(true),400)}catch(e){toast(e.message,true)}}
async function stopTask(t){
  const running=taskLines(t).filter(x=>runtime.sources?.[x.source.id]?.running);
  if(!running.length)return toast('任务已经停止');
  try{
    await Promise.all(running.map(x=>api('/api/stop/source?id='+encodeURIComponent(x.source.id),{method:'POST'})));
    toast(`正在停止 ${running.length} 条任务`);
    setTimeout(()=>refreshStatus(true),250);
  }catch(e){toast(`停止失败：${e.message}`,true)}
}
async function syncTask(t){try{await api('/api/sync/target?id='+encodeURIComponent(t.id),{method:'POST'});toast('DNS 同步成功');refreshStatus(true)}catch(e){toast(`同步失败：${e.message}`,true)}}

function openScanDetail(sourceId){scanDetailSourceId=sourceId;drawScanDetail()}
function drawScanDetail(){
  if(!scanDetailSourceId)return;
  const s=sourceById(scanDetailSourceId),st=runtime.sources?.[scanDetailSourceId]||{},hs=runtime.health?.[scanDetailSourceId]||{};if(!s)return;
  const p=st.progress||{},f=st.funnel||{},obs=st.observed||[],required=sourceRequiredCount(s.id);
  const active=((runtime.active_dns?.[scanDetailSourceId]||[]).length?runtime.active_dns[scanDetailSourceId]:(st.results||[])).slice(0,required);
  const freshRows=(hs.rows||[]).length?hs.rows:((p.phase==='health')?obs:[]),freshMap=new Map(freshRows.map(r=>[r.ip,r]));
  const activeRows=active.map((old,i)=>{const r=freshMap.get(old.ip)||old;const checking=st.running&&p.phase==='health'&&!freshMap.has(old.ip);const cls=checking?'wait':(freshMap.has(old.ip)?(r.qualified?'good':'bad'):'active');const state=checking?'等待检测':(freshMap.has(old.ip)?(r.qualified?'健康':'需替换'):'当前在用');return `<div class="active-ip-row ${cls}"><div><b>${esc(r.ip)}</b><span>${esc(r.colo||(!s.cfst?.colo?.length?'不限地区':'未识别'))}</span></div><div><span>${Number(r.latency_ms)>0?fmtNum(r.latency_ms,0)+'ms':'—'}${r.real_tested?` / 真${fmtNum(r.real_latency_ms,0)}ms`:''}</span><span>${r.speed_tested?fmtNum(r.speed_mb,1)+'MB/s':'—'}</span><span class="active-state">${esc(state)}</span></div></div>`}).join('');

  const qualified=obs.filter(r=>r.speed_tested&&r.qualified).sort((a,b)=>Number(b.speed_mb||0)-Number(a.speed_mb||0));
  const waiting=obs.filter(r=>!r.speed_tested&&!r.reject_reason?.includes('不匹配')&&!r.reject_reason?.includes('失败'));
  const failed=obs.filter(r=>(r.speed_tested&&!r.qualified)||r.reject_reason?.includes('不匹配')||r.reject_reason?.includes('失败'));
  const rowHtml=(r,kind)=>`<div class="result-row ${kind}"><span class="ip">${esc(r.ip)}</span><span>${esc(r.colo||(!s.cfst?.colo?.length?'不限地区':'未识别'))}</span><span>${Number(r.latency_ms)>0?'TCP '+fmtNum(r.latency_ms,0)+'ms':'—'}${r.real_tested?`<small>真 ${fmtNum(r.real_latency_ms,0)}ms</small>`:''}</span><span>${r.speed_tested?'直 '+fmtNum(r.speed_mb,1)+'M':(r.reject_reason?esc(compactError(r.reject_reason)):'待测速')}${r.real_speed_tested?`<small>真 ${fmtNum(r.real_speed_mb,1)}M</small>`:''}</span></div>`;
  const group=(title,rows,kind)=>rows.length?`<div class="result-group-title ${kind}"><b>${title}</b><span>${rows.length}</span></div>${rows.slice(0,240).map(r=>rowHtml(r,kind)).join('')}`:'';
  const resultRows=group('合格 · 按速度排序',qualified,'good')+group('待测 · 已过前置筛选',waiting,'wait')+group('不合格 / 失败',failed,'bad');
  const good=qualified.length,tested=obs.filter(r=>r.speed_tested).length;

  const phase=String(p.phase||'');
  const flowDefs=[['candidate','候选生成'],['probe','延迟 / 丢包'],['region','地区识别']];
  if(s.real_profile_id)flowDefs.push(['real','真连接']);
  flowDefs.push(['speed','速度决赛']);
  if(s.real_profile_id&&s.real_speed_enabled)flowDefs.push(['real_speed','真测速']);
  const phaseOrder=flowDefs.map(x=>x[0]),phasePos=phase==='done'?phaseOrder.length:Math.max(0,phaseOrder.indexOf(phase));
  const flowSteps=flowDefs.map((x,i)=>`<span class="flow-step ${phase==='done'||phasePos>i?'done':phasePos===i?'active':''}">${phase==='done'||phasePos>i?'✓ ':''}${x[1]}</span>`).join('');
  const eta=Number(p.eta_seconds||0)>0?`预计剩余 ${fmtSeconds(p.eta_seconds)}`:'正在估算剩余时间';
  const elapsed=Number(p.elapsed_seconds||0)>0?`已用 ${fmtSeconds(p.elapsed_seconds)}`:'';
  const isHealth=phase==='health'||hs.running||['checking','refill'].includes(hs.phase);
  const healthPct=isHealth?(phase==='health'?Number(p.percent||0):(hs.phase==='refill'?Math.min(95,Math.round(Number(hs.healthy||0)*100/Math.max(1,Number(hs.required||required)))):100)):0;
  const healthText=hs.phase==='refill'?`当前 ${hs.healthy||0}/${hs.required||required} 达标，缺 ${hs.need||0} 个 · ${hs.sample?`本轮候选 ${hs.sample}`:'准备补位'}`:(phase==='health'?`正在逐个复核当前 ${required} 个 DNS IP · ${p.current||0}/${p.total||required}`:(hs.message||''));
  const logs=(st.logs||[]).join('\n');

  $('#modalRoot').innerHTML=`<div class="overlay" id="scanOverlay"><div class="sheet"><div class="sheet-head"><h3>${esc(s.name||lineTitle(s))}</h3><button class="sheet-close" data-close>×</button></div><div class="sheet-body" data-ui-key="scan-sheet-body">
    <section class="active-ip-card" data-ui-key="active-ip-card"><div class="active-ip-head"><div><b>当前域名正在使用的 IP</b><span>${active.length}/${required} · 严选期间保持不变</span></div><span class="chip ${hs.ok?'health-good':hs.running?'health-warn':''}">${hs.running?(hs.phase==='refill'?'自动补位中':'健康检测中'):(hs.ok?'健康正常':'当前在用')}</span></div>${activeRows||'<div class="empty compact-empty">正在读取 Cloudflare 当前 DNS IP；读取失败时不会因此自动覆盖 DNS</div>'}</section>
    <div class="scan-head"><div><div class="scan-stage">${esc(isHealth?(hs.phase==='refill'?'自动补位':(st.stage||'健康检查')):(st.stage||'等待运行'))}</div><div class="help">候选 ${st.candidate_count||s.sample_count||0} · ${fmtFamily(s.family)} ${(s.cfst?.colo||[]).join('/')||'不限地区'}</div></div><button id="healthNowBtn" class="ghost compact" ${st.running?'disabled':''}>健康检查</button></div>
    ${isHealth?`<div class="health-progress-card"><div class="health-progress-head"><b>${esc(healthText||'健康检查')}</b><span>${healthPct}%</span></div><div class="progress-bar"><i style="width:${healthPct}%"></i></div><div class="help">${phase==='health'?`${eta}${elapsed?' · '+elapsed:''}`:(hs.phase==='refill'?'健康 IP 保留不动，只补缺失位置':'')}</div></div>`:`<div class="flow-steps">${flowSteps}</div><div class="progress-bar"><i style="width:${Math.min(100,Number(p.percent||0))}%"></i></div><div class="progress-meta"><span>${p.current||0}/${p.total||0} · ${Math.min(100,Number(p.percent||0))}%</span><span>${esc(eta)}${elapsed?' · '+esc(elapsed):''}</span></div>`}
    <div class="scan-stats"><div class="metric"><b>${f.total_candidates||st.candidate_count||0}</b><span>候选</span></div><div class="metric"><b>${f.loss_passed||0}</b><span>初筛</span></div><div class="metric"><b>${tested}</b><span>决赛已测</span></div><div class="metric"><b>${good}</b><span>速度合格</span></div></div>
    ${st.error?`<div class="task-error">${esc(compactError(st.error))}</div>`:''}${hs.error?`<div class="task-error">${esc(compactError(hs.error))}</div>`:''}
    <div class="mini-label">候选明细 · 分区显示</div><div class="result-table" data-ui-key="scan-result-table">${resultRows||'<div class="empty"><b>还没有候选结果</b>运行后会实时分成“合格 / 待测 / 不合格”。</div>'}</div>
    <details class="advanced" data-ui-key="scan-live-log" ${st.running?'open':''}><summary>流程日志</summary><pre class="logs" data-ui-key="scan-log-scroll">${esc(logs||'暂无日志')}</pre></details>
  </div><div class="sheet-actions"><button data-close>关闭</button><button class="primary" id="runThisLine" ${st.running?'disabled':''}>立即严选</button></div></div></div>`;
  $('#scanOverlay').onclick=e=>{if(e.target.id==='scanOverlay')closeModal()};$$('#modalRoot [data-close]').forEach(b=>b.onclick=closeModal);
  $('#runThisLine').onclick=async()=>{try{await api('/api/run/source?id='+encodeURIComponent(s.id),{method:'POST'});toast('已启动完整严选');refreshStatus(true)}catch(e){toast(e.message,true)}};
  $('#healthNowBtn').onclick=async()=>{try{await api('/api/health/source?id='+encodeURIComponent(s.id),{method:'POST'});toast('健康检查已启动；不达标会自动缺几补几');refreshStatus(true)}catch(e){toast(e.message,true)}};
}

function renderProviders(){const box=$('#providerList');if(!box||!cfg)return;const arr=cfg.providers||[];if(!arr.length){box.innerHTML='<div class="empty"><b>还没有 DNS 账号</b>添加 Cloudflare Zone 后，任务只需要填写域名前缀。</div>';return}box.innerHTML='';arr.forEach(p=>{const used=cfg.targets.filter(t=>t.provider_id===p.id).length;const d=document.createElement('div');d.className='provider-card';d.innerHTML=`<div class="provider-head"><div><b>${esc(p.name||'Cloudflare')}</b><div class="provider-meta">${esc(normalizeZoneDomain(p.zone_domain)||'未设置主域名')} · ${providerAuthMode(p)==='global_api_key'?'Email + Global API Key':'API Token'} · ${used} 个任务</div></div><div class="provider-actions"><button data-edit>编辑</button><button data-del>删除</button></div></div>`;d.querySelector('[data-edit]').onclick=()=>openProviderEditor(p.id);d.querySelector('[data-del]').onclick=()=>deleteProvider(p.id);box.appendChild(d)})}


function realProfileById(id){return (cfg.real_profiles||[]).find(p=>p.id===id)}
function realProfileOptions(selected){
  return `<option value="">关闭真连接测试</option>`+(cfg.real_profiles||[]).map(p=>`<option value="${esc(p.id)}" ${p.id===selected?'selected':''}>${esc(p.name||p.id)} · ${esc((p.network||'ws').toUpperCase())}+${esc((p.security||'tls').toUpperCase())}</option>`).join('')
}
function realProfilesHTML(){
  const arr=cfg.real_profiles||[];
  if(!arr.length)return'<div class="empty compact-empty">还没有真连接节点。可以直接粘贴 vless:// 链接自动识别。</div>';
  return arr.map(p=>`<div class="real-profile-card" data-real-id="${esc(p.id)}"><div><b>${esc(p.name||p.id)}</b><span>${esc(p.server)}:${Number(p.port||443)} · ${esc((p.network||'ws').toUpperCase())}+${esc((p.security||'tls').toUpperCase())}${p.ech_query_name?' · ECH':''}</span><small>SNI ${esc(p.sni||'—')} · Host ${esc(p.host||'—')}</small></div><div class="real-profile-actions"><button data-real-test>测试</button><button data-real-edit>编辑</button><button data-real-del>删除</button></div></div>`).join('')
}

function openRealProfileEditor(profile=null){
  const p=profile?clone(profile):{id:uid('real'),name:'',protocol:'vless',server:'',port:443,uuid:'',encryption:'none',flow:'',network:'ws',security:'tls',sni:'',host:'',path:'/',fingerprint:'chrome',alpn:'',insecure:false,ech:'',ech_query_name:'',ech_doh:'',raw_uri:''};
  const existing=(cfg.real_profiles||[]).some(x=>x.id===p.id);
  const root=$('#modalRoot');
  root.innerHTML=`<div class="overlay" id="realProfileOverlay"><div class="sheet"><div class="sheet-head"><h3>${existing?'编辑真连接节点':'新增真连接节点'}</h3><button class="sheet-close" data-close>×</button></div><div class="sheet-body">
    <div class="modal-note">测试候选 IP 时只会替换下面的“原始地址”；UUID、端口、SNI、Host、Path、TLS、指纹和 ECH 都保持不变。</div>
    <label class="field"><span>节点名称</span><input id="rpName" value="${esc(p.name||'')}" placeholder="JP / TW / KR / US"></label>
    <div class="grid2"><label class="field"><span>原始地址</span><input id="rpServer" value="${esc(p.server||'')}"></label><label class="field"><span>端口</span><input id="rpPort" type="number" min="1" max="65535" value="${Number(p.port||443)}"></label></div>
    <label class="field"><span>UUID</span><input id="rpUUID" value="${esc(p.uuid||'')}"></label>
    <div class="grid3"><label class="field"><span>Encryption</span><input id="rpEncryption" value="${esc(p.encryption||'none')}" placeholder="none"></label><label class="field"><span>传输</span><select id="rpNetwork"><option value="ws" ${(p.network||'ws')==='ws'?'selected':''}>WebSocket</option></select></label><label class="field"><span>TLS</span><select id="rpSecurity"><option value="tls" ${(p.security||'tls')==='tls'?'selected':''}>TLS</option><option value="none" ${p.security==='none'?'selected':''}>无</option></select></label></div>
    <div class="grid2"><label class="field"><span>SNI</span><input id="rpSNI" value="${esc(p.sni||'')}"></label><label class="field"><span>WS Host</span><input id="rpHost" value="${esc(p.host||'')}"></label></div>
    <div class="grid2"><label class="field"><span>Path</span><input id="rpPath" value="${esc(p.path||'/')}"></label><label class="field"><span>Fingerprint</span><input id="rpFP" value="${esc(p.fingerprint||'chrome')}"></label></div>
    <div class="grid2"><label class="field"><span>Flow（可空）</span><input id="rpFlow" value="${esc(p.flow||'')}"></label><label class="field"><span>ALPN（可空）</span><input id="rpALPN" value="${esc(p.alpn||'')}"></label></div>
    <div class="switch-line"><div><b>允许不安全证书</b><span class="help">对应 insecure / allowInsecure。</span></div><input id="rpInsecure" class="switch" type="checkbox" ${p.insecure?'checked':''}></div>
    <details class="advanced" ${p.ech_query_name||p.ech_doh?'open':''}><summary>ECH</summary><div class="grid2"><label class="field"><span>ECH 查询域名</span><input id="rpECHName" value="${esc(p.ech_query_name||'')}" placeholder="cloudflare-ech.com"></label><label class="field"><span>ECH DoH</span><input id="rpECHDoH" value="${esc(p.ech_doh||'')}" placeholder="https://dns.alidns.com/dns-query"></label></div></details>
  </div><div class="sheet-actions"><button data-close>取消</button><button id="saveRealProfile" class="primary">保存节点</button></div></div></div>`;
  $$('#modalRoot [data-close]').forEach(b=>b.onclick=closeModal);
  $('#realProfileOverlay').onclick=e=>{if(e.target.id==='realProfileOverlay')closeModal()};
  $('#saveRealProfile').onclick=async()=>{
    p.name=$('#rpName').value.trim()||'VLESS 节点';p.server=$('#rpServer').value.trim();p.port=Math.max(1,Number($('#rpPort').value)||443);p.uuid=$('#rpUUID').value.trim();p.protocol='vless';p.encryption=$('#rpEncryption').value.trim()||'none';p.network=$('#rpNetwork').value;p.security=$('#rpSecurity').value;p.sni=$('#rpSNI').value.trim();p.host=$('#rpHost').value.trim();p.path=$('#rpPath').value.trim()||'/';p.fingerprint=$('#rpFP').value.trim()||'chrome';p.flow=$('#rpFlow').value.trim();p.alpn=$('#rpALPN').value.trim();p.insecure=$('#rpInsecure').checked;p.ech_query_name=$('#rpECHName').value.trim();p.ech_doh=$('#rpECHDoH').value.trim();p.ech=p.ech_query_name?(p.ech_query_name+(p.ech_doh?'+'+p.ech_doh:'')):'';
    if(!p.server||!p.uuid)return toast('地址和 UUID 不能为空',true);
    const i=(cfg.real_profiles||[]).findIndex(x=>x.id===p.id);if(i>=0)cfg.real_profiles[i]=p;else cfg.real_profiles.push(p);
    if(await saveConfig(existing?'真连接节点已更新':'真连接节点已添加'))closeModal();
  };
}

function renderSettings(){
  const panel=$('#settingsPanel');if(!panel||!cfg)return;
  const ready=!!buildInfo.web_update_ready,available=!!updateCheck?.update_available;
  const checkOK=updateCheck?.check_ok!==false;
  const latest=updateCheck?.latest_commit?shortCommit(updateCheck.latest_commit):(updateCheck?.latest_version||'未检查');
  const source=updateCheck?.check_source&&updateCheck.check_source!=='unavailable'?` · ${updateCheck.check_source}`:'';
  const running=!!updateRunState?.running;
  const stateText=running?'更新中':(!updateCheck?'版本状态':(checkOK?(available?'有新版本':'已检查'):'网络受限'));
  const baseNote=!updateCheck
    ?(ready?'Web 一键更新已启用。检查失败时仍可直接强制拉取 latest。':'未挂载 Docker Socket，只能检查版本。')
    :(updateCheck.check_error
      ?`无法连接版本源，但不会影响“强制拉取 latest”。${updateCheck.check_error}`
      :(updateCheck.check_warning|| (ready?'版本检查完成。':'版本检查完成；未启用 Web 更新。')));
  const note=running?(updateRunState.message||'更新进行中…'):(updateRunState?.error?`更新失败：${updateRunState.error}`:baseNote);
  const updateButtonText=running?'正在更新…':(available?'更新到最新版':'强制拉取 latest');
  panel.innerHTML=`<div class="settings-card"><div class="update-head"><div><div class="mini-label">当前版本</div><div class="version-title">${esc(buildInfo.version||'dev')} <span>${esc(shortCommit(buildInfo.commit))}</span></div><div class="help">构建 ${esc(buildInfo.built_at||'unknown')} · 最新 ${esc(latest)}${esc(source)}</div></div><span class="update-state ${available?'available':''}">${esc(stateText)}</span></div><div id="updateMessage" class="modal-note">${esc(note)}</div>${running?`<div class="progress-bar" style="margin:9px 0"><i class="indeterminate-progress"></i></div><div class="help">阶段：${esc(updateRunState.stage||'准备')} · 已用时 ${Math.max(0,Math.floor((Date.now()-new Date(updateRunState.started_at||Date.now()).getTime())/1000))} 秒</div>`:''}<div class="grid2 update-actions"><button id="checkUpdateBtn" class="ghost" ${running?'disabled':''}>检查更新</button><button id="applyUpdateBtn" class="primary" ${ready&&!running?'':'disabled'}>${esc(updateButtonText)}</button></div></div>
  <div class="settings-card"><h3>全局测速网络</h3><div class="modal-note"><b>初筛固定 TCPing 443</b>：不再使用 HTTPing，避免 Host / SNI / HEAD 状态码造成“256 个全部不可用”的假失败。</div><label class="field"><span>Colo 地区探测地址</span><input id="setProbeURL" value="${esc(cfg.probe_url||DEFAULT_PROBE_URL)}"><small>只用于 NRT/HKG 等地区识别；如果没有真正返回 colo，程序会自动回退 Cloudflare 官方 trace。</small></label><label class="field"><span>下载测速地址</span><input id="setSpeedURL" value="${esc(cfg.speed_url||DEFAULT_SPEED_URL)}"><small>默认 CM：cf.090227.xyz，避免把 Cloudflare 官方下载源作为长期高频测速源。线路高级设置可单独覆盖。</small></label><div class="grid2"><label class="field"><span>健康检查周期 / 分钟</span><input id="setHealth" type="number" min="1" value="${Number(cfg.health_check_minutes||60)}"></label><label class="field"><span>单线路最大采样数</span><input id="setMaxSample" type="number" min="1" max="100000" value="${Number(cfg.max_sample_count||10000)}"></label></div><div class="modal-note">健康检查会同时复核当前在用 IP 的 <b>延迟 + 丢包 + 网速 + 地区</b>。任一不达标会保留健康 IP，并自动“缺几补几”；只有到正常严选周期或手动严选才完整扫描。</div></div>
  <div class="settings-card"><h3>真连接 / 真测速</h3><div class="modal-note"><b>${buildInfo.real_test_ready?'sing-box 核心已就绪':'当前镜像没有 sing-box 核心'}</b>。真连接会完整走 VLESS → TLS/ECH → WebSocket → 测试网址，比 TCPing 更接近真实使用；只替换候选 IP，不改节点其他参数。</div>
    <label class="field"><span>真连接测试地址</span><input id="setRealTestURL" value="${esc(cfg.real_test_url||DEFAULT_REAL_TEST_URL)}"><small>默认 ${esc(DEFAULT_REAL_TEST_URL)}；响应很小，主要测完整代理握手与 HTTP 建连。</small></label>
    <div class="grid3"><label class="field"><span>真连接次数</span><input id="setRealAttempts" type="number" min="1" max="5" value="${Number(cfg.real_test_attempts||2)}"></label><label class="field"><span>真测速每 IP 上限 / MB</span><input id="setRealSpeedBytes" type="number" min="1" max="100" value="${Number(cfg.real_speed_bytes_mb||5)}"></label><label class="field"><span>真测速最多候选</span><input id="setRealSpeedTopN" type="number" min="1" max="100" value="${Number(cfg.real_speed_top_n||10)}"></label></div>
    <label class="field"><span>真测速地址</span><input id="setRealSpeedURL" value="${esc(cfg.real_speed_url||cfg.speed_url||DEFAULT_SPEED_URL)}"><small>只有线路开启“真测速”才使用。默认 5MB × 最多 10 个 = 最多约 50MB/完整严选/线路；如果线路设置了“真速度≥”，只有实际完成真测速并达标的候选才算合格。</small></label>
    <div class="mini-label">节点库 · JP / TW / KR / US 等</div><div class="real-paste"><textarea id="realUriPaste" rows="2" placeholder="粘贴 vless://...，自动识别 UUID / SNI / Host / Path / ECH"></textarea><button id="parseRealUri" class="primary">识别节点</button></div>
    <div id="realProfileList">${realProfilesHTML()}</div><button id="addRealManual" class="add-line">＋ 手动添加节点</button>
  </div>
  <div class="settings-card"><h3>熔炉学习</h3><div class="switch-line"><div><b style="font-size:10px">分时段历史加权</b><span class="help">成熟后只在本轮新鲜合格 IP 中按日/夜历史表现重排，不会复活失效 IP。</span></div><input id="setFurnaceRank" class="switch" type="checkbox" ${cfg.furnace_auto_rank?'checked':''}></div><div class="grid2" style="margin-top:9px"><label class="field"><span>历史保留 / 天</span><input id="setRetention" type="number" min="7" max="365" value="${Number(cfg.furnace_retention_days||45)}"></label><label class="field"><span>最大并发任务</span><input id="setConcurrency" type="number" min="1" max="16" value="${Number(cfg.max_concurrency||2)}"></label></div></div>
  <button id="saveGlobal" class="primary" style="width:100%;padding:10px;margin-bottom:9px">保存全局设置</button>
  <div class="settings-card"><details><summary>高级 / 调试</summary><label class="field" style="margin-top:10px"><span>监听地址</span><input id="setListen" value="${esc(cfg.listen||':8080')}"></label><textarea id="rawConfig" class="raw">${esc(JSON.stringify(cfg,null,2))}</textarea><div class="grid2" style="margin-top:7px"><button id="copyRaw" class="ghost">复制 JSON</button><button id="applyRaw" class="danger-btn">应用 JSON</button></div></details></div>`;
  $('#checkUpdateBtn').onclick=checkForUpdate;$('#applyUpdateBtn').onclick=applyWebUpdate;
  $('#parseRealUri').onclick=async()=>{const uri=$('#realUriPaste').value.trim();if(!uri)return toast('先粘贴 VLESS 链接',true);try{const r=await api('/api/reallink/parse',{method:'POST',body:JSON.stringify({uri})});const p=r.profile||{};p.id=uid('real');openRealProfileEditor(p)}catch(e){toast(`识别失败：${e.message}`,true)}};
  $('#addRealManual').onclick=()=>openRealProfileEditor();
  $$('#realProfileList [data-real-id]').forEach(card=>{const p=realProfileById(card.dataset.realId);card.querySelector('[data-real-edit]').onclick=()=>openRealProfileEditor(p);card.querySelector('[data-real-test]').onclick=async()=>{try{toast(`正在测试 ${p.name}…`);const r=await api('/api/reallink/test',{method:'POST',body:JSON.stringify({profile:p})});toast(`${p.name} 真连接 ${fmtNum(r.latency_ms,0)}ms`)}catch(e){toast(`真连接失败：${e.message}`,true)}};card.querySelector('[data-real-del]').onclick=async()=>{if(!confirm(`删除真连接节点 ${p.name}？`))return;cfg.real_profiles=cfg.real_profiles.filter(x=>x.id!==p.id);cfg.sources.forEach(s=>{if(s.real_profile_id===p.id)s.real_profile_id=''});await saveConfig('真连接节点已删除')}}); 
  $('#saveGlobal').onclick=async()=>{cfg.probe_url=$('#setProbeURL').value.trim()||DEFAULT_PROBE_URL;cfg.speed_url=$('#setSpeedURL').value.trim()||DEFAULT_SPEED_URL;cfg.real_test_url=$('#setRealTestURL').value.trim()||DEFAULT_REAL_TEST_URL;cfg.real_test_attempts=Math.max(1,Math.min(5,Number($('#setRealAttempts').value)||2));cfg.real_speed_url=$('#setRealSpeedURL').value.trim()||cfg.speed_url;cfg.real_speed_bytes_mb=Math.max(1,Math.min(100,Number($('#setRealSpeedBytes').value)||5));cfg.real_speed_top_n=Math.max(1,Math.min(100,Number($('#setRealSpeedTopN').value)||10));cfg.health_check_minutes=Math.max(1,Number($('#setHealth').value)||60);cfg.max_sample_count=Math.max(1,Math.min(100000,Number($('#setMaxSample').value)||10000));cfg.furnace_retention_days=Math.max(7,Number($('#setRetention').value)||45);cfg.furnace_auto_rank=$('#setFurnaceRank').checked;cfg.max_concurrency=Math.max(1,Number($('#setConcurrency').value)||2);cfg.listen=$('#setListen')?.value.trim()||cfg.listen||':8080';cfg.sources.forEach(s=>s.sample_count=Math.min(Number(s.sample_count||256),cfg.max_sample_count));await saveConfig('全局设置已保存')};
  $('#copyRaw').onclick=async()=>{try{await navigator.clipboard.writeText($('#rawConfig').value);toast('JSON 已复制')}catch{toast('浏览器不允许复制',true)}};
  $('#applyRaw').onclick=async()=>{try{cfg=JSON.parse($('#rawConfig').value);initConfig();await saveConfig('JSON 已应用')}catch(e){toast(e.message,true)}};
}

async function checkForUpdate(){const m=$('#updateMessage');if(m)m.textContent='正在检查版本源…';try{updateCheck=await api('/api/update/check?ts='+Date.now());buildInfo=updateCheck.current||buildInfo;renderVersionBadge();renderSettings();if(updateCheck.check_ok===false)toast('版本源暂时不可达，可直接强制拉取 latest',true);else toast(updateCheck.update_available?'发现新版本':`已完成检查 · ${updateCheck.check_source||'版本源'}`)}catch(e){toast(`检查更新异常：${e.message}；仍可强制拉取 latest`,true)}}

async function applyWebUpdate(){
  if(!buildInfo.web_update_ready)return toast('当前未启用 Web 更新',true);
  if(!confirm('将直接从 GHCR 拉取 latest 并重建容器。页面会短暂断开，继续吗？'))return;
  const old=String(buildInfo.commit||'');
  updateRunState={running:true,stage:'submitting',message:'正在提交更新请求…',started_at:new Date().toISOString()};
  renderSettings();
  toast('更新请求已提交');
  try{
    const r=await api('/api/update/apply',{method:'POST'});
    updateRunState={...(r.state||updateRunState),running:true,stage:r.state?.stage||'pulling',message:r.message||'后台正在拉取 latest 镜像'};
    renderSettings();
    monitorWebUpdate(old);
  }catch(e){
    updateRunState={running:false,stage:'failed',error:e.message,message:'更新启动失败'};
    renderSettings();
    toast(`更新启动失败：${e.message}`,true);
  }
}

function monitorWebUpdate(old){
  clearInterval(updateRunTimer);
  let misses=0;
  updateRunTimer=setInterval(async()=>{
    try{
      const st=await api('/api/update/status?ts='+Date.now());
      misses=0;
      updateRunState=st||updateRunState;
      if(currentView==='settings')renderSettings();
      if(st?.error){
        clearInterval(updateRunTimer);
        toast(`更新失败：${st.error}`,true);
        return;
      }
      if(st?.stage==='restarting'){
        // The helper will replace this container in about 2 seconds.
        clearInterval(updateRunTimer);
        waitForUpdatedService(old);
      }
    }catch{
      misses++;
      // A disconnect after the pull usually means the helper is replacing us.
      if(misses>=2){
        clearInterval(updateRunTimer);
        waitForUpdatedService(old);
      }
    }
  },1200);
}

function waitForUpdatedService(old){
  let n=0;
  updateRunState={running:true,stage:'restarting',message:'容器正在重启，等待 Web 恢复…',started_at:updateRunState?.started_at||new Date().toISOString()};
  if(currentView==='settings')renderSettings();
  const timer=setInterval(async()=>{
    n++;
    try{
      const x=await api('/api/version?ts='+Date.now());
      if(x?.commit&&(x.commit!==old||n>5)){
        clearInterval(timer);
        buildInfo=x;
        updateRunState={running:false,stage:'done',message:`更新完成：${x.version} · ${shortCommit(x.commit)}`};
        renderVersionBadge();
        toast(updateRunState.message);
        setTimeout(()=>location.reload(),700);
      }
    }catch{}
    if(n>=120){
      clearInterval(timer);
      updateRunState={running:false,stage:'timeout',error:'等待新容器上线超时，请查看 Docker 日志'};
      if(currentView==='settings')renderSettings();
      toast('更新等待超时，请查看 Docker 日志',true);
    }
  },2000);
}
async function saveConfig(okText='已保存'){try{await api('/api/config',{method:'PUT',body:JSON.stringify(cfg)});toast(okText);renderWithUIState(()=>renderAll());return true}catch(e){toast(`保存失败：${e.message}`,true);return false}}

function openTaskEditor(targetId=''){
  if(!cfg.providers.length){toast('请先在“我的”添加 DNS 账号',true);switchView('account');return}
  const existing=targetId?cfg.targets.find(t=>t.id===targetId):null;
  const t=existing?clone(existing):{id:uid('task'),name:'',enabled:true,provider_id:cfg.providers[0].id,prefix:'',hostname:'',ttl:60,proxied:false,sources:[]};
  t.prefix=taskPrefixValue(t,providerById(t.provider_id));
  const drafts=existing?taskLines(existing).map(x=>({originalSourceId:x.source.id,ref:clone(x.ref),source:clone(x.source),shared:sourceUseCount(x.source.id,existing.id)>0})):[newLineDraft('ipv4')];
  const root=$('#modalRoot');root.innerHTML=`<div class="overlay" id="taskOverlay"><div class="sheet"><div class="sheet-head"><h3>${existing?'编辑域名任务':'新建域名任务'}</h3><button class="sheet-close" data-close>×</button></div><div class="sheet-body"><label class="field"><span>DNS 账号</span><select id="taskProvider">${providerOptions(t.provider_id)}</select></label><div class="mini-label">最终域名 · 按实际书写顺序</div><div class="domain-builder"><input id="taskPrefix" value="${esc(t.prefix||'')}" placeholder="v4 / nrtv4 / @"><span class="domain-dot">.</span><div id="taskDomainFixed" class="domain-fixed"></div></div><div id="taskDomainPreview" class="domain-preview"></div><label class="field"><span>备注（可选）</span><input id="taskName" value="${esc(t.name||'')}" placeholder="例如 日本 NRT 严选"></label><div class="mini-label">IP 线路</div><div id="lineEditors"></div><button id="addLineBtn" class="add-line">＋ 添加一条线路</button><details class="advanced"><summary>DNS 高级设置</summary><div class="grid2"><label class="field"><span>TTL</span><input id="taskTTL" type="number" min="1" value="${Number(t.ttl||60)}"></label><label class="field"><span>Cloudflare 代理</span><select id="taskProxied"><option value="false" ${!t.proxied?'selected':''}>关闭</option><option value="true" ${t.proxied?'selected':''}>开启</option></select></label></div></details>${existing?'<div class="danger-zone"><button id="deleteTaskBtn" class="danger-btn">删除这个任务</button></div>':''}</div><div class="sheet-actions"><button data-close>取消</button><button id="saveTaskBtn" class="primary">保存任务</button></div></div></div>`;
  function drawLines(){const box=$('#lineEditors');box.innerHTML='';drafts.forEach((d,i)=>{d.source.cfst={...clone(DEFAULT_CFST),...(d.source.cfst||{})};d.source.sample_count=Number(d.source.sample_count||256);const s=d.source,ref=d.ref,e=document.createElement('div');e.className='line-editor';e.innerHTML=`<div class="line-editor-head"><div class="line-editor-title">线路 ${i+1}</div><button class="remove-line" data-remove>删除</button></div><div class="grid2"><label class="field"><span>线路名称</span><input data-name value="${esc(s.name||'')}" placeholder="NRT IPv4"></label><label class="field"><span>协议</span><select data-family><option value="ipv4" ${s.family==='ipv4'?'selected':''}>IPv4 / A</option><option value="ipv6" ${s.family==='ipv6'?'selected':''}>IPv6 / AAAA</option></select></label></div><label class="field"><span>IP 段 / IP 源</span><textarea data-inputs placeholder="每行一个 CIDR、IP、IP:443#速度-地区 或 URL">${esc((s.inputs||[]).join('\n'))}</textarea></label><div class="grid2"><label class="field"><span>本轮扫描数量</span><input data-sample type="number" min="1" max="${Number(cfg.max_sample_count||10000)}" value="${Number(s.sample_count||256)}"></label><label class="field"><span>结果地区（可空）</span><input data-colo value="${esc((s.cfst.colo||[]).join(','))}" placeholder="NRT"></label></div><div class="capacity-note" data-capacity></div><div class="thresholds"><label class="field"><span>延迟≤ms</span><input data-latency type="number" min="0" value="${Number(s.cfst.latency_max_ms??200)}"></label><label class="field"><span>丢包≤</span><input data-loss type="number" step="0.01" min="0" max="1" value="${Number(s.cfst.loss_max??.2)}"></label><label class="field"><span>速度≥MB/s</span><input data-speed type="number" step="0.1" min="0" value="${Number(s.cfst.speed_min_mb??5)}"></label></div><div class="grid2"><label class="field"><span>写入数量</span><input data-count type="number" min="1" max="100" value="${Number(ref.count||5)}"></label><label class="field"><span>自动周期</span><select data-interval>${intervalOptions(s.interval_minutes)}</select></label></div><div class="real-line-box"><label class="field"><span>真连接节点（可选）</span><select data-real-profile>${realProfileOptions(s.real_profile_id||'')}</select><small>选择后，会在 TCPing/地区初筛后增加真实 VLESS 建连。候选 IP 只替换节点 address，其余参数保持不变。</small></label><div class="grid3"><label class="field"><span>真延迟≤ms</span><input data-real-latency type="number" min="0" value="${Number(s.real_latency_max_ms||0)}" placeholder="0=不限"></label><label class="field"><span>真测速</span><select data-real-speed><option value="false" ${!s.real_speed_enabled?'selected':''}>关闭</option><option value="true" ${s.real_speed_enabled?'selected':''}>开启</option></select></label><label class="field"><span>真速度≥MB/s</span><input data-real-speed-min type="number" step="0.1" min="0" value="${Number(s.real_speed_min_mb||0)}" placeholder="0=只排序"></label></div></div><details class="advanced"><summary>严选高级参数</summary><div class="grid3"><label class="field"><span>测速秒数/IP</span><input data-dt type="number" min="1" max="60" value="${Number(s.cfst.download_time||10)}"></label><label class="field"><span>初筛并发</span><input data-threads type="number" min="1" max="1000" value="${Number(s.cfst.threads||200)}"></label><label class="field"><span>延迟次数</span><input data-ping type="number" min="1" max="10" value="${Number(s.cfst.ping_count||4)}"></label></div><label class="field"><span>结果缓存数量</span><input data-keep type="number" min="1" max="500" value="${Number(s.keep_results||50)}"></label><div class="modal-note">延迟 / 丢包初筛固定使用 <b>TCPing</b>；地区识别和下载测速独立执行。</div><label class="field"><span>测速地址覆盖（留空=全局）</span><input data-url value="${esc(s.cfst.url||'')}" placeholder="${esc(cfg.speed_url||DEFAULT_SPEED_URL)}"></label><label class="field"><span>探测地址覆盖（留空=全局）</span><input data-probe value="${esc(s.cfst.probe_url||'')}" placeholder="${esc(cfg.probe_url||DEFAULT_PROBE_URL)}"></label></details>`;
    const sampleInput=e.querySelector('[data-sample]'),cap=e.querySelector('[data-capacity]'),inputArea=e.querySelector('[data-inputs]');
    const updateCap=()=>{const raw=splitLines(inputArea.value);s.family=e.querySelector('[data-family]').value;let req=Math.max(1,Number(sampleInput.value)||1);req=Math.min(req,Number(cfg.max_sample_count||10000));const info=estimateInputCapacity(raw,s.family,req);s.inputs=info.normalized;if(info.finite&&info.capacityNum>0&&info.capacityNum<req){req=Math.max(1,info.capacityNum);sampleInput.value=req}s.sample_count=req;d.inputInvalid=info.invalid;d.inputDecorated=info.decorated;cap.innerHTML=capacityText(info,req)};
    e.querySelector('[data-remove]').onclick=()=>{drafts.splice(i,1);drawLines()};e.querySelector('[data-name]').oninput=ev=>s.name=ev.target.value;e.querySelector('[data-family]').onchange=updateCap;inputArea.oninput=updateCap;inputArea.onblur=()=>{const info=analyzeInputsUI(splitLines(inputArea.value),s.family);if(info.invalid===0&&info.decorated>0){inputArea.value=info.normalized.join('\n');updateCap();toast(`已自动整理 ${info.decorated} 行 IP 源`)}};sampleInput.oninput=updateCap;e.querySelector('[data-colo]').oninput=ev=>s.cfst.colo=splitComma(ev.target.value).map(x=>x.toUpperCase());e.querySelector('[data-count]').oninput=ev=>ref.count=Math.max(1,Number(ev.target.value)||1);e.querySelector('[data-interval]').onchange=ev=>s.interval_minutes=Number(ev.target.value);e.querySelector('[data-latency]').oninput=ev=>s.cfst.latency_max_ms=Number(ev.target.value)||0;e.querySelector('[data-loss]').oninput=ev=>s.cfst.loss_max=Number(ev.target.value);e.querySelector('[data-speed]').oninput=ev=>s.cfst.speed_min_mb=Number(ev.target.value)||0;e.querySelector('[data-dt]').oninput=ev=>s.cfst.download_time=Math.max(1,Number(ev.target.value)||10);e.querySelector('[data-threads]').oninput=ev=>s.cfst.threads=Math.max(1,Number(ev.target.value)||200);e.querySelector('[data-ping]').oninput=ev=>s.cfst.ping_count=Math.max(1,Number(ev.target.value)||4);e.querySelector('[data-keep]').oninput=ev=>s.keep_results=Math.max(1,Number(ev.target.value)||50);s.cfst.httping=false;e.querySelector('[data-url]').oninput=ev=>s.cfst.url=ev.target.value.trim();e.querySelector('[data-probe]').oninput=ev=>s.cfst.probe_url=ev.target.value.trim();e.querySelector('[data-real-profile]').onchange=ev=>s.real_profile_id=ev.target.value;e.querySelector('[data-real-latency]').oninput=ev=>s.real_latency_max_ms=Math.max(0,Number(ev.target.value)||0);e.querySelector('[data-real-speed]').onchange=ev=>s.real_speed_enabled=ev.target.value==='true';e.querySelector('[data-real-speed-min]').oninput=ev=>s.real_speed_min_mb=Math.max(0,Number(ev.target.value)||0);updateCap();box.appendChild(e)})}
  drawLines();$('#addLineBtn').onclick=()=>{drafts.push(newLineDraft(drafts.some(d=>d.source.family==='ipv4')?'ipv6':'ipv4'));drawLines()};
  const updateDomain=()=>{const p=providerById($('#taskProvider').value),domain=normalizeZoneDomain(p?.zone_domain||''),prefix=$('#taskPrefix').value.trim().toLowerCase();$('#taskDomainFixed').textContent=domain||'未设置主域名';const h=composeHostname(p,prefix);$('#taskDomainPreview').innerHTML=`<b>最终域名</b><span>${esc(h||'请输入前缀；根域名用 @')}</span>`};$('#taskProvider').onchange=updateDomain;$('#taskPrefix').oninput=updateDomain;updateDomain();
  $$('#modalRoot [data-close]').forEach(b=>b.onclick=closeModal);$('#taskOverlay').onclick=e=>{if(e.target.id==='taskOverlay')closeModal()};
  $('#saveTaskBtn').onclick=async()=>{t.provider_id=$('#taskProvider').value;const p=providerById(t.provider_id);t.prefix=$('#taskPrefix').value.trim().toLowerCase();t.hostname=composeHostname(p,t.prefix);t.name=$('#taskName').value.trim();t.ttl=Math.max(1,Number($('#taskTTL').value)||60);t.proxied=$('#taskProxied').value==='true';if(!normalizeZoneDomain(p?.zone_domain))return toast('这个 DNS 账号没有主域名',true);if(!t.prefix)return toast('请填写域名前缀；根域名用 @',true);if(drafts.some(d=>Number(d.inputInvalid||0)>0))return toast('IP 源中还有无法识别的行，请先检查提示',true);if(drafts.some(d=>!(d.source.inputs||[]).length))return toast('每条线路至少要有一个可识别的 IP、CIDR 或来源',true);if(await persistTask(t,drafts,existing))closeModal()};
  if(existing)$('#deleteTaskBtn').onclick=async()=>{if(!confirm(`删除 ${taskHostname(existing)}？`))return;deleteTask(existing.id);if(await saveConfig('任务已删除'))closeModal()}
}
function newLineDraft(family='ipv4'){return{originalSourceId:'',shared:false,ref:{source_id:'',count:5},source:{id:uid('line'),name:family==='ipv6'?'IPv6':'IPv4',enabled:true,family,inputs:[],interval_minutes:360,keep_results:50,sample_count:256,real_profile_id:'',real_latency_max_ms:0,real_speed_enabled:false,real_speed_min_mb:0,cfst:clone(DEFAULT_CFST)}}}
async function persistTask(t,drafts,existing){const oldIds=existing?(existing.sources||[]).map(r=>r.source_id):[],nextRefs=[],nextIds=new Set();for(const d of drafts){const s=clone(d.source);s.cfst={...clone(DEFAULT_CFST),...(s.cfst||{})};s.inputs=(s.inputs||[]).map(x=>x.trim()).filter(Boolean);s.cfst.colo=(s.cfst.colo||[]).map(x=>String(x).trim().toUpperCase()).filter(Boolean);s.cfst.all_ip=false;s.cfst.httping=false;s.sample_count=Math.max(1,Math.min(Number(cfg.max_sample_count||10000),Number(s.sample_count||256)));if(!s.name.trim())s.name=lineTitle(s);let id=d.originalSourceId||s.id||uid('line');if(d.shared)id=uid('line');s.id=id;nextIds.add(id);const idx=cfg.sources.findIndex(x=>x.id===id);if(idx>=0)cfg.sources[idx]=s;else cfg.sources.push(s);nextRefs.push({source_id:id,count:Math.max(1,Number(d.ref.count)||1)})}for(const old of oldIds){if(!nextIds.has(old)&&sourceUseCount(old,existing?.id||'')===0){cfg.sources=cfg.sources.filter(s=>s.id!==old);cfg.furnace_rules=cfg.furnace_rules.filter(r=>r.source_id!==old)}}t.sources=nextRefs;const ti=cfg.targets.findIndex(x=>x.id===t.id);if(ti>=0)cfg.targets[ti]=t;else cfg.targets.push(t);recomputeSourceEnabled();return await saveConfig(existing?'任务已更新':'任务已创建')}
function deleteTask(id){const t=cfg.targets.find(x=>x.id===id);if(!t)return;const refs=(t.sources||[]).map(r=>r.source_id);cfg.targets=cfg.targets.filter(x=>x.id!==id);for(const sid of refs){if(!cfg.targets.some(x=>(x.sources||[]).some(r=>r.source_id===sid))){cfg.sources=cfg.sources.filter(s=>s.id!==sid);cfg.furnace_rules=cfg.furnace_rules.filter(r=>r.source_id!==sid)}}recomputeSourceEnabled()}
function providerOptions(selected){return cfg.providers.map(p=>`<option value="${esc(p.id)}" ${p.id===selected?'selected':''}>${esc(p.name||p.id)} · ${esc(normalizeZoneDomain(p.zone_domain))}</option>`).join('')}
function intervalOptions(selected){const opts=[[0,'手动'],[60,'每 1 小时'],[180,'每 3 小时'],[360,'每 6 小时'],[720,'每 12 小时'],[1440,'每天']];if(selected&&!opts.some(x=>x[0]===Number(selected)))opts.push([Number(selected),`${selected} 分钟`]);return opts.map(([v,n])=>`<option value="${v}" ${Number(selected)===v?'selected':''}>${n}</option>`).join('')}
function splitLines(v){return String(v||'').split(/\n+/).map(x=>x.trim()).filter(Boolean)}function splitComma(v){return String(v||'').split(',').map(x=>x.trim()).filter(Boolean)}

function validIPv4(v){
  const p=String(v||'').split('.');
  return p.length===4&&p.every(x=>/^\d{1,3}$/.test(x)&&Number(x)>=0&&Number(x)<=255);
}
function normalizeInputLineUI(raw,family){
  const original=String(raw||'').trim();
  if(!original)return{ok:false,empty:true,raw:original};
  if(/^https?:\/\//i.test(original))return{ok:true,value:original,remote:true,decorated:false};
  if(original.startsWith('#')||original.startsWith(';'))return{ok:false,empty:true,raw:original};

  let v=original.split('#')[0].split(';')[0].trim();
  if(!v)return{ok:false,empty:true,raw:original};

  const cidr=v.match(/^(.+)\/(\d{1,3})$/);
  if(cidr){
    const prefix=Number(cidr[2]),bits=family==='ipv6'?128:32;
    if(prefix>=0&&prefix<=bits){
      if(family==='ipv4'&&validIPv4(cidr[1]))return{ok:true,value:v,range:true,decorated:v!==original};
      if(family==='ipv6'&&cidr[1].includes(':'))return{ok:true,value:v,range:true,decorated:v!==original};
    }
  }

  const bracket=v.match(/^\[([0-9a-fA-F:]+)\](?::(\d{1,5}))?$/);
  if(bracket&&family==='ipv6'&&(!bracket[2]||Number(bracket[2])<=65535))
    return{ok:true,value:bracket[1],fixed:true,decorated:true};

  const v4=v.match(/^(\d{1,3}(?:\.\d{1,3}){3})(?::(\d{1,5}))?$/);
  if(v4&&family==='ipv4'&&validIPv4(v4[1])&&(!v4[2]||Number(v4[2])<=65535))
    return{ok:true,value:v4[1],fixed:true,decorated:v4[1]!==original};

  if(family==='ipv6'&&v.includes(':')&&!/\s/.test(v))
    return{ok:true,value:v,fixed:true,decorated:v!==original};

  const first=v.split(/[\s,|\t]+/)[0];
  if(first&&first!==v){
    const x=normalizeInputLineUI(first,family);
    if(x.ok){x.decorated=true;return x}
  }
  return{ok:false,raw:original};
}
function analyzeInputsUI(inputs,family){
  const normalized=[],seen=new Set();
  let decorated=0,invalid=0,remote=0,fixed=0,ranges=0;
  for(const raw of inputs){
    const x=normalizeInputLineUI(raw,family);
    if(x.empty)continue;
    if(!x.ok){invalid++;continue}
    if(x.decorated)decorated++;
    if(x.remote)remote++;
    if(x.fixed)fixed++;
    if(x.range)ranges++;
    if(!seen.has(x.value)){seen.add(x.value);normalized.push(x.value)}
  }
  return{normalized,decorated,invalid,remote,fixed,ranges};
}
function estimateInputCapacity(inputs,family,requested){
  const parsed=analyzeInputsUI(inputs,family);
  let total=0n,unknown=parsed.remote>0,huge=false;
  for(const v of parsed.normalized){
    if(/^https?:\/\//i.test(v))continue;
    let prefix;
    if(v.includes('/'))prefix=Number(v.split('/').pop());
    else prefix=family==='ipv6'?128:32;
    if(!Number.isFinite(prefix))continue;
    const bits=family==='ipv6'?128:32,host=bits-prefix;
    if(host>52){huge=true;total+=BigInt(Number.MAX_SAFE_INTEGER)}
    else total+=1n<<BigInt(host);
  }
  const max=BigInt(Number(cfg.max_sample_count||10000));
  const finite=!unknown&&!huge&&parsed.invalid===0;
  let capNum=finite?Number(total>max?max:total):Number(cfg.max_sample_count||10000);
  if(finite&&total<BigInt(capNum))capNum=Number(total);
  return{...parsed,unknown,huge,finite,capacity:total,capacityNum:capNum,requested};
}
function capacityText(info,req){
  if(info.invalid)return`<b class="bad-text">${info.invalid} 行无法识别</b> · 支持 IP、CIDR、IP:端口#速度-地区、[IPv6]:端口#备注、URL。`;
  if(!info.normalized.length)return'请填写 CIDR、IP、带端口/备注的 IP 列表或远程 URL。';
  const parts=[];
  if(info.fixed)parts.push(`${info.fixed} 个固定 IP`);
  if(info.ranges)parts.push(`${info.ranges} 个 CIDR`);
  if(info.remote)parts.push(`${info.remote} 个远程源`);
  if(info.decorated)parts.push(`<b>${info.decorated} 行将自动清洗</b>`);
  parts.push(info.huge?'IPv6 巨大地址池':(info.unknown?'远程容量运行时确认':`可扫描容量 ${info.capacity.toString()}`));
  parts.push(`本轮 ${req}`);
  return parts.join(' · ');
}

function openProviderEditor(providerId=''){const existing=providerId?cfg.providers.find(p=>p.id===providerId):null,p=existing?clone(existing):{id:uid('cf'),name:'Cloudflare',type:'cloudflare',zone_id:'',zone_domain:'',auth_mode:'api_token',api_token:'',email:'',api_key:''};p.auth_mode=providerAuthMode(p);$('#modalRoot').innerHTML=`<div class="overlay" id="providerOverlay"><div class="sheet"><div class="sheet-head"><h3>${existing?'编辑 DNS 账号':'添加 DNS 账号'}</h3><button class="sheet-close" data-close>×</button></div><div class="sheet-body"><label class="field"><span>名称</span><input id="providerName" value="${esc(p.name||'Cloudflare')}"></label><label class="field"><span>主域名 / Zone</span><input id="providerDomain" value="${esc(p.zone_domain||'')}" placeholder="629717.xyz"></label><label class="field"><span>Zone ID</span><input id="providerZone" value="${esc(p.zone_id||'')}"></label><label class="field"><span>认证方式</span><select id="providerAuthMode"><option value="api_token" ${p.auth_mode==='api_token'?'selected':''}>API Token（推荐）</option><option value="global_api_key" ${p.auth_mode==='global_api_key'?'selected':''}>Email + Global API Key</option></select></label><div id="providerTokenBox"><label class="field"><span>API Token</span><input id="providerToken" type="password" value="${esc(p.api_token||'')}"></label></div><div id="providerGlobalKeyBox"><label class="field"><span>Cloudflare 邮箱</span><input id="providerEmail" type="email" value="${esc(p.email||'')}"></label><label class="field"><span>Global API Key</span><input id="providerAPIKey" type="password" value="${esc(p.api_key||'')}"></label></div><div class="modal-note">任务里只填写前缀。例如 <b>nrtv4</b> + <b>${esc(p.zone_domain||'629717.xyz')}</b> → 完整域名。</div></div><div class="sheet-actions provider-sheet-actions"><button data-close>取消</button><button id="testProviderBtn" class="ghost">测试连接</button><button id="saveProviderBtn" class="primary">保存</button></div></div></div>`;const read=()=>{p.name=$('#providerName').value.trim()||'Cloudflare';p.zone_domain=normalizeZoneDomain($('#providerDomain').value);p.zone_id=$('#providerZone').value.trim();p.auth_mode=$('#providerAuthMode').value;p.api_token=$('#providerToken').value.trim();p.email=$('#providerEmail').value.trim();p.api_key=$('#providerAPIKey').value.trim();return p};const validate=()=>{read();if(!p.zone_domain)return'请填写主域名';if(!p.zone_id)return'请填写 Zone ID';if(p.auth_mode==='global_api_key'&&(!p.email||!p.api_key))return'需要邮箱和 Global API Key';if(p.auth_mode==='api_token'&&!p.api_token)return'请填写 API Token';return''};const draw=()=>{const legacy=$('#providerAuthMode').value==='global_api_key';$('#providerTokenBox').style.display=legacy?'none':'';$('#providerGlobalKeyBox').style.display=legacy?'':'none'};$('#providerAuthMode').onchange=draw;draw();$$('#modalRoot [data-close]').forEach(b=>b.onclick=closeModal);$('#providerOverlay').onclick=e=>{if(e.target.id==='providerOverlay')closeModal()};$('#testProviderBtn').onclick=async()=>{const er=validate();if(er)return toast(er,true);try{await api('/api/provider/test',{method:'POST',body:JSON.stringify(read())});toast('Cloudflare 认证和 Zone 正常')}catch(e){toast(e.message,true)}};$('#saveProviderBtn').onclick=async()=>{const er=validate();if(er)return toast(er,true);read();const i=cfg.providers.findIndex(x=>x.id===p.id);if(i>=0)cfg.providers[i]=p;else cfg.providers.push(p);if(await saveConfig(existing?'DNS 账号已更新':'DNS 账号已添加'))closeModal()}}
async function deleteProvider(id){const p=providerById(id),used=cfg.targets.filter(t=>t.provider_id===id);if(used.length)return toast(`还有 ${used.length} 个任务在使用`,true);if(!confirm(`删除 ${p?.name||id}？`))return;cfg.providers=cfg.providers.filter(x=>x.id!==id);await saveConfig('DNS 账号已删除')}

async function loadFurnace(force=false){if(!force&&Date.now()-furnaceLoadedAt<5000)return;const ui=captureUIState();try{furnaceData=await api('/api/furnace?limit=1000&ts='+Date.now());furnaceLoadedAt=Date.now();renderFurnace();restoreUIState(ui)}catch(e){if(currentView==='furnace')toast(`熔炉读取失败：${e.message}`,true)}}
function renderFurnace(){const list=$('#furnaceList'),hero=$('#furnaceHero'),filters=$('#furnaceFilters');if(!list||!cfg)return;const profiles=furnaceData.profiles||[],admitted=profiles.filter(p=>p.admitted),mature=admitted.filter(p=>p.maturity>=100),period=furnaceData.period==='night'?'夜间':'日间';hero.innerHTML=`<div class="furnace-hero"><div><div class="big">${admitted.length}</div><div class="sub">已取得炼丹资格 · ${mature.length} 个学习成熟</div></div><div class="period"><span class="sub">当前分时段</span><b>${period}</b><span class="sub">成熟 IP 自动按当前时段历史加权</span></div></div>`;const sourceIds=[...new Set(profiles.map(p=>p.source_id))];filters.innerHTML=`<div class="filter-row"><button class="filter-pill ${furnaceSourceFilter==='all'?'active':''}" data-filter="all">全部</button>${sourceIds.map(id=>`<button class="filter-pill ${furnaceSourceFilter===id?'active':''}" data-filter="${esc(id)}">${esc(sourceById(id)?.name||id)}</button>`).join('')}<button class="filter-pill ${furnaceShowGray?'active':''}" id="grayToggle">${furnaceShowGray?'隐藏灰名单':'显示灰名单'}</button></div>`;filters.querySelectorAll('[data-filter]').forEach(b=>b.onclick=()=>{furnaceSourceFilter=b.dataset.filter;renderFurnace()});$('#grayToggle').onclick=()=>{furnaceShowGray=!furnaceShowGray;renderFurnace()};let rows=profiles.filter(p=>(furnaceSourceFilter==='all'||p.source_id===furnaceSourceFilter)&&(furnaceShowGray||p.admitted));if(!rows.length){list.innerHTML='<div class="empty"><b>熔炉还在升温</b>启用炼丹规则后，达到资格的 IP 会入册；灰名单记录长期未达标候选。</div>';return}list.innerHTML=rows.map(p=>{const cls=phenotypeClass(p.phenotype);return`<div class="furnace-card ${p.admitted?'':'gray'}" data-source="${esc(p.source_id)}" data-ip="${esc(p.ip)}"><div class="furnace-top"><div><div class="furnace-ip">${esc(p.ip)}</div><div class="help">${esc(sourceById(p.source_id)?.name||p.source_id)} · ${esc(p.colo||'—')}</div></div><span class="phenotype ${cls}">${p.admitted?esc(p.phenotype):p.attempts>=28?'长期淘汰':'观察中'}</span></div><div class="furnace-grid"><div><b>${Math.round(p.hit_rate*100)}%</b><span>达成率</span></div><div><b>${fmtNum(p.avg_speed_mb,1)}M</b><span>均速</span></div><div><b>${fmtNum(p.avg_latency_ms,0)}ms</b><span>均延迟</span></div><div><b>${Math.round(p.current_score)}</b><span>当前时段分</span></div></div><div class="maturity"><i style="width:${Math.min(100,p.maturity)}%"></i></div></div>`}).join('');list.querySelectorAll('[data-ip]').forEach(el=>el.onclick=()=>openFurnaceDetail(el.dataset.source,el.dataset.ip))}
function phenotypeClass(v){if(v==='日间型')return'day';if(v==='夜间型')return'night';if(v==='全天稳定')return'stable';return''}
function openFurnaceRules(){
  // Furnace rules are source-level learning rules, but only sources that are
  // actually referenced by current domain tasks belong here. Group each active
  // source with its task hostname(s) so stale/orphan lines can never look like
  // an extra task.
  const rules=new Map((cfg.furnace_rules||[]).map(r=>[r.source_id,clone(r)]));
  const active=new Map();
  for(const t of (cfg.targets||[])){
    for(const ref of (t.sources||[])){
      const s=sourceById(ref.source_id);if(!s)continue;
      let row=active.get(s.id);
      if(!row){row={source:s,targets:[]};active.set(s.id,row)}
      row.targets.push(t);
    }
  }
  const sourceRows=[...active.values()].map(({source:s,targets})=>{
    const r=rules.get(s.id)||{source_id:s.id,enabled:false,latency_max_ms:50,loss_max:0,speed_min_mb:50,auto_rank:true};rules.set(s.id,r);
    const hosts=[...new Set(targets.map(t=>taskHostname(t)).filter(Boolean))];
    const taskLabel=hosts.length===1?hosts[0]:`共享 ${hosts.length} 个任务 · ${hosts.join(' / ')}`;
    return`<div class="line-editor furnace-rule-card" data-rule="${esc(s.id)}"><div class="furnace-rule-task"><span class="mini-label">关联任务</span><b>${esc(taskLabel)}</b></div><div class="switch-line"><div><b style="font-size:11px">${esc(s.name||lineTitle(s))}</b><span class="help">${fmtFamily(s.family)} · ${(s.cfst?.colo||[]).join('/')||'不限地区'} · 写入 ${targets.map(t=>(t.sources||[]).find(x=>x.source_id===s.id)?.count||0).join('/')}</span></div><input class="switch" data-enable type="checkbox" ${r.enabled?'checked':''}></div><div class="thresholds" style="margin-top:8px"><label class="field"><span>炼丹延迟≤ms</span><input data-lat type="number" value="${Number(r.latency_max_ms??50)}"></label><label class="field"><span>炼丹丢包≤</span><input data-loss type="number" step="0.01" min="0" max="1" value="${Number(r.loss_max??0)}"></label><label class="field"><span>炼丹速度≥MB/s</span><input data-speed type="number" step="0.1" value="${Number(r.speed_min_mb??50)}"></label></div><div class="switch-line"><span class="help">允许成熟历史参与日/夜 DNS 排序</span><input class="switch" data-rank type="checkbox" ${r.auto_rank?'checked':''}></div></div>`
  }).join('');
  const count=active.size;
  $('#modalRoot').innerHTML=`<div class="overlay" id="furnaceRuleOverlay"><div class="sheet"><div class="sheet-head"><h3>熔炉 · 炼丹资格</h3><button class="sheet-close" data-close>×</button></div><div class="sheet-body"><div class="modal-note">当前显示 <b>${count}</b> 条正在被域名任务使用的线路。规则跟线路学习数据绑定，并明确显示关联域名；历史孤儿线路不会再出现。第一次满足资格即“入册”，约 28 次观察达到 100% 学习成熟度。</div>${sourceRows||'<div class="empty"><b>暂无可配置线路</b>请先创建域名任务和 IP 线路。</div>'}</div><div class="sheet-actions"><button data-close>取消</button><button id="saveFurnaceRules" class="primary">保存规则</button></div></div></div>`;
  $$('#modalRoot [data-rule]').forEach(el=>{const r=rules.get(el.dataset.rule);el.querySelector('[data-enable]').onchange=e=>r.enabled=e.target.checked;el.querySelector('[data-lat]').oninput=e=>r.latency_max_ms=Math.max(0,Number(e.target.value)||0);el.querySelector('[data-loss]').oninput=e=>r.loss_max=Math.max(0,Math.min(1,Number(e.target.value)||0));el.querySelector('[data-speed]').oninput=e=>r.speed_min_mb=Math.max(0,Number(e.target.value)||0);el.querySelector('[data-rank]').onchange=e=>r.auto_rank=e.target.checked});
  $$('#modalRoot [data-close]').forEach(b=>b.onclick=closeModal);$('#furnaceRuleOverlay').onclick=e=>{if(e.target.id==='furnaceRuleOverlay')closeModal()};
  $('#saveFurnaceRules').onclick=async()=>{const activeIDs=new Set(active.keys());cfg.furnace_rules=[...rules.values()].filter(r=>r.enabled&&activeIDs.has(r.source_id));if(await saveConfig('炼丹规则已保存')){closeModal();loadFurnace(true)}}
}

async function openFurnaceDetail(sourceId,ip){try{const d=await api(`/api/furnace/detail?source_id=${encodeURIComponent(sourceId)}&ip=${encodeURIComponent(ip)}`),s=d.summary,samples=d.samples||[];$('#modalRoot').innerHTML=`<div class="overlay" id="furnaceDetailOverlay"><div class="sheet"><div class="sheet-head"><h3>${esc(ip)}</h3><button class="sheet-close" data-close>×</button></div><div class="sheet-body"><div class="furnace-detail-summary"><div class="metric"><b>${Math.round(s.hit_rate*100)}%</b><span>达成率 ${s.hits}/${s.attempts}</span></div><div class="metric"><b>${esc(s.phenotype)}</b><span>分型</span></div><div class="metric"><b>${s.maturity}%</b><span>学习成熟度</span></div></div>${chartCard('速度 MB/s',samples,'speed_mb','speed')}${chartCard('延迟 ms',samples,'latency_ms','latency')}${chartCard('丢包 %',samples,'loss','loss',100)}<div class="modal-note">日间分 ${fmtNum(s.day_score,0)} · 夜间分 ${fmtNum(s.night_score,0)} · 最好时段 ${s.best_hour>=0?s.best_hour+':00':'数据不足'} · 最差时段 ${s.worst_hour>=0?s.worst_hour+':00':'数据不足'}</div></div><div class="sheet-actions"><button data-close>关闭</button><button class="primary" id="scanProfileSource">严选该来源</button></div></div></div>`;$$('#modalRoot [data-close]').forEach(b=>b.onclick=closeModal);$('#furnaceDetailOverlay').onclick=e=>{if(e.target.id==='furnaceDetailOverlay')closeModal()};$('#scanProfileSource').onclick=async()=>{await api('/api/run/source?id='+encodeURIComponent(sourceId),{method:'POST'});toast('已启动严选');closeModal()}}catch(e){toast(e.message,true)}}
function chartCard(title,samples,key,cls,mul=1){const vals=samples.map(x=>Number(x[key]||0)*mul);const times=samples.map(x=>x.time);const svg=sparkSVG(vals,cls);const last=vals.length?vals[vals.length-1]:0;return`<div class="chart-card"><div class="chart-title"><span>${title}</span><span>当前 ${fmtNum(last,key==='loss'?0:1)}</span></div>${svg}<div class="help">${times.length?fmtTime(times[0])+' → '+fmtTime(times[times.length-1]):'暂无历史'}</div></div>`}
function sparkSVG(vals,cls){if(!vals.length)return'<div class="empty">暂无曲线数据</div>';const w=320,h=92,pad=7,min=Math.min(...vals),max=Math.max(...vals),span=max-min||1;const pts=vals.map((v,i)=>`${pad+(w-2*pad)*(vals.length===1?.5:i/(vals.length-1))},${h-pad-(h-2*pad)*(v-min)/span}`).join(' ');return`<svg class="spark ${cls}" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none"><line class="grid" x1="0" y1="23" x2="320" y2="23"/><line class="grid" x1="0" y1="46" x2="320" y2="46"/><line class="grid" x1="0" y1="69" x2="320" y2="69"/><polyline class="line" points="${pts}"/></svg>`}

function closeModal(){scanDetailSourceId='';$('#modalRoot').innerHTML=''}
function switchView(view){currentView=view;const titles={tasks:'域名任务',furnace:'熔炉记录',account:'我的',settings:'设置'};$('#pageTitle').textContent=titles[view]||'BestIP';$$('.view').forEach(v=>v.classList.remove('active'));$$('.nav-item').forEach(v=>v.classList.remove('active'));$(`#view${view[0].toUpperCase()+view.slice(1)}`)?.classList.add('active');$(`.nav-item[data-view="${view}"]`)?.classList.add('active');$('#addTaskBtn').style.display=view==='tasks'?'':'none';if(view==='tasks')renderTasks();if(view==='furnace')loadFurnace(true);if(view==='account')renderProviders();if(view==='settings')renderSettings()}
async function refreshStatus(immediate=false){clearTimeout(statusTimer);const ui=captureUIState();try{runtime=await api('/api/status?ts='+Date.now());if(currentView==='tasks')renderTasks();if(scanDetailSourceId&&$('#scanOverlay'))drawScanDetail();restoreUIState(ui);if(currentView==='furnace'&&Date.now()-furnaceLoadedAt>8000)loadFurnace(true)}catch{restoreUIState(ui)}const running=Object.values(runtime.sources||{}).some(s=>s.running);statusTimer=setTimeout(()=>refreshStatus(),running?1000:4000)}

$$('.nav-item').forEach(b=>b.onclick=()=>switchView(b.dataset.view));$('#addTaskBtn').onclick=()=>openTaskEditor();$('#addProviderBtn').onclick=()=>openProviderEditor();$('#furnaceRuleBtn').onclick=openFurnaceRules;$('#refreshBtn').onclick=async()=>{await refreshStatus(true);if(currentView==='furnace')await loadFurnace(true);toast('已刷新')};
load();
