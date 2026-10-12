// 智能路由页：路由列表与画布、路由编辑器、别名、策略定义。
import {rpc} from './api.js';
import {renderPage} from './app.js';
import {$,$$,E,compact,dateTime,dateTimeSec,json,money,ms,number,state} from './core.js';
import {icon} from './icons.js';
import {avatar,badge,btn,chart,check,checked,closeDialog,copy,empty,field,flowMini,getModel,getProvider,head,headerValue,modelName,nval,pillStatus,quotaBars,rangeTools,save,selectField,selectRow,showDialog,sourceTag,spark,tag,toast,val} from './ui.js';
import {modelEditor} from './models.js';
import {affinityText,bindCandidateDrag,flatCandidates,groupSections,groupsOf,moveGroupCandidate,routeSummary} from './route-groups.js';
import {openRouteEditor,routeEditorDirty} from './route-editor.js';

// 路由策略：[取值, 编辑器里的完整说明, 列表里的简称]
const STRATEGIES=[
  ['priority','优先级：按候选顺序，失败再试下一个','优先级'],
  ['balanced','权重优先：权重最高的先试；设了本地预算时按用量降权。不做随机分流','权重优先'],
  ['weighted','按权重分流：按权重比例随机抽签','权重分流'],
  ['latency','最快响应：按上游响应头耗时，未测过的先试','最快响应'],
  ['cost','最低价格：按输入 + 输出单价，未确认计价的排最后','最低价格'],
  ['least_busy','最空闲：按当前并发占用比例','最空闲']
];
const strategyName=v=>STRATEGIES.find(x=>x[0]===v)?.[2]||v;
const routeCacheText=id=>{
  const u=state.data?.cache?.[id];
  if(!u?.cache_samples)return '暂无可靠样本';
  const hit=u.cache_hit_rate==null?'—':Math.round(u.cache_hit_rate*100)+'%';
  const nulls=u.cache_null_rate?` · null ${Math.round(u.cache_null_rate*100)}%`:'';
  return `${hit} · ${number(u.cache_samples)} 个样本${nulls}`;
};

export function routes(){
  const rs=state.config.routes;
  let r=rs.find(r=>r.id===state.routeID)||rs[0];
  if(r)state.routeID=r.id;
  const groups=groupsOf(r);
  const cand=flatCandidates(groups);
  const attempts=Object.values(state.data?.share?.[r?.id]||{}).reduce((s,v)=>s+(v.attempts||0),0);
  const modelMap=new Map(state.config.models.map(m=>[m.id,m]));
  const protocols=[...new Set(cand.flatMap(c=>nativeProtocols(modelMap.get(c.model_id))||[]))];
  return head('ROUTING STUDIO','让请求，找到合适的模型。','按优先级、权重、响应速度、价格或负载选择候选；会话优先保持亲和，不会在半截流中切换模型。',btn('创建路由','add-route','plus','','primary'))+(r?`<div class="route-layout"><div class="route-list"><p class="tiny muted" style="padding:0 2px 4px">拖动卡片调整顺序</p>${rs.map((x,i)=>`<button class="route-select ${x.id===r.id?'active':''}" draggable="true" data-drag-index="${i}" data-drag-kind="route" data-action="select-route" data-id="${E(x.id)}"><div class="between"><strong>${E(x.name||x.id)}</strong><span class="dot ${x.enabled?'positive':'muted'}"></span></div><p class="mono">${E(x.id)}</p><p>${routeSummary(x,strategyName)}</p></button>`).join('')}</div><section class="card"><div class="card-head"><div><h2>${E(r.name||r.id)}</h2><p class="card-sub">${E(r.description||'拖动候选可在组内或跨组排序；上下箭头也可跨组移动。')}</p></div><div class="flex">${state.routeDraft?btn('应用修改','save-route-order','check','','mint small')+btn('放弃修改','discard-route-draft','close','','ghost small'):''}<button class="switch ${r.enabled?'on':''}" role="switch" aria-checked="${r.enabled}" aria-label="启用路由 ${E(r.name||r.id)}" data-action="toggle-route" data-id="${E(r.id)}"></button>${btn('编辑','edit-route','settings',`data-id="${E(r.id)}"`,'small')}<button class="icon-btn" data-action="delete-route" data-id="${E(r.id)}" aria-label="删除路由">${icon('trash')}</button></div></div>${state.routeDraft?'<div class="route-draft-note" role="status">候选编排尚未保存，请应用修改或放弃修改。</div>':''}${routeLiveBar(r.id)}<div class="route-canvas"><div class="router-node">${icon('prism')}<strong class="mono">${E(r.id)}</strong><small>CLIENT REQUEST</small>${badge(r.groups.length>1?`${r.groups.length} 组`:r.groups[0].strategy)}</div><div class="route-connector"></div><div class="route-candidates">${groupSections(groups,(x,i,gi)=>candidateCard(r,protocols,x,i,gi,groups,modelMap.get(x.model_id)||null,attempts),strategyName)}</div></div><p class="route-canvas-note tiny muted">并发、速度等实时指标每 5 秒刷新；流量占比统计近 ${state.data?.minutes||60} 分钟该路由的实际尝试，不是额度。「新选」是按策略挑中的，「亲和」是会话沿用上次的模型。</p><div class="route-properties"><div><small>缓存命中 · 近 ${state.data?.minutes||60} 分钟</small><strong>${routeCacheText(r.id)}</strong></div><div><small>会话亲和</small><strong>${affinityText(r)}</strong></div><div><small>重试边界</small><strong>安全请求 + 上游 429 / 503 / 400</strong></div><div><small>流开始之后</small><strong>禁止中途切模型</strong></div><div class="flex" style="margin-left:auto;gap:8px"><div class="segmented">${['claude-code','simple'].map(v=>`<button type="button" class="${state.simulateMode===v?'active':''}" data-action="set-simulate-mode" data-value="${v}">${v==='claude-code'?'Claude Code 请求':'简单请求'}</button>`).join('')}</div>${btn('模拟选择','simulate-route','play',`data-id="${E(r.id)}"`,'small')}</div></div>${state.simulation?`<div class="simulation"><h3>路由模拟结果 · ${state.simulateMode==='claude-code'?'Claude Code 请求':'简单请求'}</h3><p class="tiny muted" style="margin:6px 0 14px">${E(state.simulation.note)}</p>${(state.simulation.checks||[]).filter(c=>!c.eligible).map(c=>`<p class="sim-out"><b class="mono">${E(modelName(c.model_id))}</b> 淘汰：${E(c.reason)}</p>`).join('')}${state.simulation.ranked.map((x,i)=>`<div class="sim-line"><span class="flex">${badge(String(i+1).padStart(2,'0'))}<b>${E(modelName(x.model_id))}</b>${sourceTag(getModel(x.model_id)?.provider_id)}${badge(x.protocol)}${r.groups.length>1&&x.group?badge(x.group):''}</span><span class="mono">${x.score.toFixed(1)}</span></div>`).join('')||'<p class="small negative">没有兼容候选。检查下方原因。</p>'}<details style="margin-top:14px"><summary class="small muted">查看候选判定依据</summary><pre style="margin-top:10px">${E(json(state.simulation.checks))}</pre></details></div>`:''}</section></div>`:empty('创建你的第一个路由','把多个模型组成一个稳定的客户端入口，例如 auto-coding。候选只在本账户的授权范围内使用。','add-route','创建路由','route'))+`<section class="card aliases-grid"><div class="card-head"><div><h2>模型别名</h2><p class="card-sub">客户端名称映射到真实模型或路由；实际解析结果记录在请求元数据中。</p></div>${btn('添加别名','add-alias','plus','','small')}</div>${state.config.aliases.length?`<div class="table-scroll"><table><thead><tr><th>客户端请求名称</th><th>指向目标</th><th>状态</th><th></th></tr></thead><tbody>${state.config.aliases.map(a=>`<tr><td class="mono">${E(a.id)}</td><td class="mono">${E(a.target)}</td><td>${tag(a.enabled?'已启用':'停用',a.enabled?'':'neutral')}</td><td><div class="table-actions"><button class="icon-btn" data-action="delete-alias" data-id="${E(a.id)}" aria-label="删除别名">${icon('trash')}</button></div></td></tr>`).join('')}</tbody></table></div>`:empty('别名不是必需项','你可以直接使用模型 ID 或路由 ID，也可以在这里增加一个更好记的名字。','','','link')}</section>`;

}

// 画布候选：拖拽与上下移均可跨组，整条路由用于会话切入提示。
function candidateCard(r,protocols,x,i,gi,groups,m,attempts){
  const cs=candidateState(x.model_id,m);const flags=(x.disabled?tag('已暂停 · 本路由不再选用','warning'):'')+candidateBadges(cs)+switchHint(protocols,m);return `<div class="candidate${x.disabled||cs.disabled||cs.cooling?' muted':''}" draggable="true" data-drag-index="${i}" data-drag-kind="candidate" data-group="${gi}">${icon('drag','drag-handle')}<span class="candidate-order">${String(i+1).padStart(2,'0')}</span><div class="candidate-info"><strong class="candidate-title"><span class="ellipsis">${E(m?.name||x.model_id)}</span>${sourceTag(m?.provider_id)}</strong><small><span class="mono">${E(x.model_id)}</span> · weight ${x.weight}</small>${flags?`<div class="candidate-flags">${flags}</div>`:''}${candidateLive(x.model_id)}${shareBar(r.id,x.model_id,attempts)}</div>${badge(m?.protocol||'?')}<button class="switch ${x.disabled?'':'on'}" role="switch" aria-checked="${!x.disabled}" aria-label="在本路由启用 ${E(m?.name||x.model_id)}" title="${x.disabled?'恢复此候选':'临时关闭：新请求与亲和会话都不再选用'}" data-action="toggle-candidate" data-id="${E(x.model_id)}"></button><div><button class="icon-btn" data-action="candidate-up" data-group="${gi}" data-index="${i}" ${i===0&&gi===0?'disabled':''} aria-label="${i===0?'移到上一组末尾':'上移'}">${icon('up')}</button><button class="icon-btn" data-action="candidate-down" data-group="${gi}" data-index="${i}" ${i===groups[gi].candidates.length-1&&gi===groups.length-1?'disabled':''} aria-label="${i===groups[gi].candidates.length-1?'移到下一组开头':'下移'}">${icon('down')}</button></div></div>`;
}

export function routeEditor(id='',group=0){
  openRouteEditor(id,STRATEGIES,group);
}

export const routeHasChanges=()=>!!state.routeDraft||routeEditorDirty();

export function aliasEditor(){
  const opts=[...state.config.routes.map(x=>[x.id,`${x.name||x.id} · 路由`]),...state.config.models.map(x=>[x.id,x.name||x.id])];
  if(!opts.length){
    toast('请先添加模型或路由',true);
    return;

  }
  showDialog('添加模型别名','别名必须指向现有模型或路由，不允许多级别名环。',`${field('客户端名称','id','','text','','required placeholder="coding"')}${selectField('目标','target',opts[0][0],opts)}${check('启用别名','enabled',true)}`,f=>save('alias.save',{alias:{id:val(f,'id').trim(),target:val(f,'target'),enabled:checked(f,'enabled')}},f));

}

export function moveCandidate(fromGroup,from,to,toGroup=fromGroup){
  if(state.routeDraftSaving)return;
  const r=state.config.routes.find(x=>x.id===state.routeID);
  if(!r)return;
  const groups=structuredClone(groupsOf(r));
  if(!moveGroupCandidate(groups,fromGroup,from,toGroup,to))return;
  state.routeDraftVersion??=state.config.version;
  state.routeDraft=groups;
  state.simulation=null;
  renderPage(false);
  const moved=$$('.candidate').find(el=>Number(el.dataset.group)===toGroup&&Number(el.dataset.dragIndex)===to);
  moved?.querySelector('[data-action="candidate-up"]:not(:disabled),[data-action="candidate-down"]:not(:disabled),button')?.focus();
}

function nudgeCandidate(el,step){
  const gi=Number(el.dataset.group),i=Number(el.dataset.index);
  const groups=groupsOf(state.config.routes.find(r=>r.id===state.routeID));
  let group=gi,index=i+step;
  if(index<0&&gi>0){group--;index=groups[group].candidates.length;}
  else if(index>=groups[gi].candidates.length&&gi<groups.length-1){group++;index=0;}
  moveCandidate(gi,i,index,group);
}

// ===== 路由画布实时状态与模拟 =====
// 路由页的 state.data 来自 route.stats：
//   {minutes, now, share:{路由ID:{模型ID:{attempts,success}}}, runtime:{active, models:{模型ID:{active,cooldown_until,last_status,latency_ms,ttfb_ms,limits,limits_at}}}}

// 候选此刻的状态：{disabled, cooling(恢复时刻毫秒，0 为未冷却), active, concurrency, full}
export function candidateState(modelId,m=getModel(modelId)){
  const p=m?getProvider(m.provider_id):null;
  const disabled=!m?.enabled||!p?.enabled;
  const rt=state.data?.runtime?.models?.[modelId];
  const serverNow=state.data?.now||Date.now();
  const cooling=rt?.cooldown_until>serverNow?rt.cooldown_until:0;
  const active=rt?.active||0;
  const concurrency=m?.concurrency||0;
  return {disabled,cooling,active,concurrency,full:concurrency>0&&active>=concurrency};
}

// 候选在该路由最近窗口内的落点占比（0~1）与尝试次数：{share, attempts, success, affinity}；affinity 是会话亲和命中的次数
export function candidateShare(routeId,modelId,total=Object.values(state.data?.share?.[routeId]||{}).reduce((s,v)=>s+(v.attempts||0),0)){
  const bucket=state.data?.share?.[routeId]||{};
  const x=bucket[modelId]||{};
  return {share:total?(x.attempts||0)/total:0,attempts:x.attempts||0,success:x.success||0,affinity:x.affinity||0};
}

// 模拟请求体。mode：'simple' 简单文本请求；'claude-code' 带工具定义、thinking、max_tokens=64000 的 messages 请求，
// 并带一轮推理历史：进行中的会话才是切换候选时真正会遇到的形态
// 返回 {protocol, request}，直接作为 route.test 的参数
export function simulationRequest(routeId,mode){
  if(mode==='claude-code')return {protocol:'messages',request:{model:routeId,max_tokens:64000,thinking:{type:'enabled',budget_tokens:10000},messages:[{role:'user',content:'hi'},{role:'assistant',content:[{type:'thinking',thinking:'simulated',signature:'simulated'},{type:'text',text:'hello'}]},{role:'user',content:'continue'}],tools:[{name:'Read',description:'read file',input_schema:{type:'object',properties:{}}}]}};
  return {protocol:'chat',request:null};
}

// 恢复时刻的展示：今天只显示 HH:MM，否则带上月日；附加一个相对时间方便一眼判断还要等多久
function coolLabel(ts){
  const serverNow=state.data?.now||Date.now();
  const d=new Date(ts),ref=new Date(serverNow);
  const sameDay=d.getFullYear()===ref.getFullYear()&&d.getMonth()===ref.getMonth()&&d.getDate()===ref.getDate();
  const clock=d.toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',hour12:false});
  const abs=sameDay?clock:`${String(d.getMonth()+1).padStart(2,'0')}-${String(d.getDate()).padStart(2,'0')} ${clock}`;
  const diffMin=Math.max(1,Math.round((ts-serverNow)/60000));
  return `${abs} · ${diffMin<60?diffMin+' 分钟后':Math.round(diffMin/60)+' 小时后'}`;
}

// 候选状态徽章：未启用 / 冷却中；并发由每 5 秒刷新的实时指标行显示，这里不重复（页面快照会过时）
function candidateBadges(cs){
  const badges=[];
  if(cs.disabled)badges.push(tag('未启用','neutral'));
  else if(cs.cooling)badges.push(tag(`冷却中 · 恢复于 ${coolLabel(cs.cooling)}`,'warning'));
  return badges.join('');
}

// 模型原生支持的入口协议；显式空列表表示协议尚未确认，不参与判断
const nativeProtocols=m=>!m?null:m.supported_protocols==null?[m.protocol]:m.supported_protocols.length?m.supported_protocols:null;

// 会话切入提示：本路由其他候选原生支持、而此候选要跨协议才能接的入口。会话从它们切过来时，
// 历史里的推理内容或文档会让此候选被淘汰（HTTP 400），除非模型开启了对应开关。只提示，不拦截保存。
function switchHint(protocols,m){
  const mine=nativeProtocols(m);
  if(!mine||mine.includes('systemone'))return '';
  const cross=new Set();
  for(const p of protocols)if(p!=='systemone'&&!mine.includes(p))cross.add(p);
  if(!cross.size)return '';
  const missing=[];
  if(!m.drop_reasoning)missing.push('推理历史');
  if(cross.has('messages')&&!m.drop_documents)missing.push('文档');
  if(!missing.length)return '';
  const tip=`本路由其他候选原生支持 ${[...cross].join(' / ')}，此候选需跨协议。带${missing.join('或')}的会话切到它会返回 400；可在模型库为它开启对应的跨协议开关。`;
  return `<span class="status-pill ${m.drop_reasoning?'neutral':'warning'} switch-hint" title="${E(tip)}"><span class="dot"></span>会话切入受限 · ${missing.join(' / ')}</span>`;
}

// 近窗口落点占比条：0 次时弱化显示，避免看起来像“均匀分布”
function shareBar(routeId,modelId,attempts){
  const sh=candidateShare(routeId,modelId,attempts);
  const minutes=state.data?.minutes||60;
  const pct=Math.round(sh.share*100);
  return `<div class="cand-share${sh.attempts?'':' zero'}"><span class="cand-share-bar"><i style="width:${sh.attempts?Math.max(pct,3):0}%"></i></span><small>流量占比 ${pct}% · ${sh.attempts} 次${sh.attempts?`（新选 ${sh.attempts-sh.affinity} · 亲和 ${sh.affinity}）`:''}</small></div>`;
}

// ===== 路由画布实时数据 =====
// 数据来自 route.live（与页面数据的 route.stats 不同源），放在 state.live，每 5 秒刷新一次：
//   {now, global:{active,limit},
//    models:{模型ID:{active,concurrency,rpm,rpm_limit,ttfb_ms,cooldown_until,limits,sessions,requests_5m,success_5m,tok_s}},
//    routes:{路由ID:{active,capacity,rpm,tok_s,requests_5m,success_5m,sessions,ttfb_ms}}}
// 轮询每个页面模块各自持有，这里只存自己的 interval，避免重复启动。

let liveTimer=null;

const LIVE_INTERVAL=5000;

// 数字缺省显示 —，0 是有效值，所以只认 null/undefined
const lv=(v,fmt=number)=>v==null?'—':fmt(v);

// 输出速度：路由的 tok/s 可能是 0.17 这种小数，直接用 number() 会抹成 0
const tokRate=v=>Number(v)>=10?number(v):Number(v).toFixed(1);

// 成功率：没有请求时显示 —，不显示 0%
function rateText(requests,success){
  if(!requests)return '—';
  return Math.round((success||0)/requests*100)+'%';
}

// 单个指标格
function liveItem(value,label,lead='',sub=''){
  return `<div class="live-item${lead?' '+lead:''}"><div class="live-val">${value}</div><small class="live-label">${label}</small>${sub?`<small class="live-sub">${sub}</small>`:''}</div>`;
}

// 画布上方的路由实时指标；state.live 还没有数据时用同样布局显示占位
export function routeLiveBar(routeId){
  const g=state.live?.global;
  const r=state.live?.routes?.[routeId];
  const pct=r?.capacity?Math.round((r.active||0)/r.capacity*100):0;
  const lead=r?.capacity&&pct>=80?(pct>=100?'error':'warn'):'';
  const sub=`全局 ${g?number(g.active||0):'—'} / ${g?number(g.limit||0):'—'}`;
  return `<div class="route-live"><div class="live-grid">${liveItem(`${lv(r?.active)} <span class="live-sep">/</span> ${lv(r?.capacity)}`,'并发',lead,sub)}${r?.live_tok_s>0?liveItem(`${tokRate(r.live_tok_s)} <span class="live-unit">tok/s</span>`,'输出速度 · 实时估算','',`已完成请求 ${lv(r.tok_s,tokRate)} tok/s`):liveItem(`${lv(r?.tok_s,tokRate)} <span class="live-unit">tok/s</span>`,'输出速度 · 近 60 秒')}${liveItem(lv(r?.rpm),'RPM')}${liveItem(rateText(r?.requests_5m,r?.success_5m),'成功率')}${liveItem(lv(r?.sessions),'会话')}${liveItem(lv(r?.ttfb_ms,v=>v>0?ms(v):'—'),'首字节')}</div><span class="live-tag" title="每 ${LIVE_INTERVAL/1000} 秒自动刷新路由与候选的实时数据"><i class="dot"></i>实时 · ${LIVE_INTERVAL/1000}s</span></div>`;
}

// 候选卡片上的一行实时指标：并发条 + 并发 / RPM / 速度 / 首字节 / 会话。
// 冷却倒计时已由 candidateBadges 显示，这里不重复；没有数据的项直接省略。
const REJECT_NAMES={CONCURRENCY_LIMIT:'并发满',UPSTREAM_COOLDOWN:'冷却',RPM_LIMIT:'RPM 满',LOCAL_QUOTA_LIMIT:'预算不足'};
function candidateLive(modelId){
  const m=state.live?.models?.[modelId];
  if(!m)return '';
  const parts=[];
  if(m.active!=null&&m.concurrency)parts.push(`并发 ${number(m.active)}/${number(m.concurrency)}`);
  if(m.rpm!=null)parts.push(m.rpm_limit?`RPM ${number(m.rpm)} / ${number(m.rpm_limit)}`:`RPM ${number(m.rpm)}`);
  if(m.tok_s>0)parts.push(`${tokRate(m.tok_s)} tok/s`);
  if(m.ttfb_ms>0)parts.push(`TTFB ${ms(m.ttfb_ms)}`);
  if(m.sessions!=null)parts.push(`${number(m.sessions)} 会话`);
  // 近 5 分钟准入被拒：并发满、冷却中、RPM 或本地预算
  const rj=Object.entries(m.rejected_5m||{});
  if(rj.length)parts.push('5 分钟内被拒 '+rj.map(([c,n])=>`${REJECT_NAMES[c]||c} ${number(n)}`).join('、'));
  if(!parts.length)return '';
  const bar=m.concurrency?`<span class="cand-live-bar ${liveLevel(m.active,m.concurrency)}"><i style="width:${Math.min(100,Math.round(m.active/m.concurrency*100))}%"></i></span>`:'';
  return `<div class="cand-live">${bar}<small>${E(parts.join(' · '))}</small></div>`;
}

// 并发占用达到 80% 提醒、满了标红
function liveLevel(active,capacity){
  if(!capacity)return '';
  if(active>=capacity)return 'full';
  return active/capacity>=.8?'high':'';
}

export const routeActions={
  'save-route-order':async()=>{
    if(!state.routeDraft||state.routeDraftSaving)return;
    state.routeDraftSaving=true;
    const main=$('#main');
    if(main){main.inert=true;main.setAttribute('aria-busy','true');}
    try{await save('route.save',{version:state.routeDraftVersion??state.config.version,id:state.routeID,route:{groups:state.routeDraft}});}
    finally{state.routeDraftSaving=false;if(main){main.inert=false;main.removeAttribute('aria-busy');}}
  },
  'edit-route-group':el=>routeEditor(state.routeID,Number(el.dataset.group)),
  'candidate-up':el=>nudgeCandidate(el,-1),
  'candidate-down':el=>nudgeCandidate(el,1),
  'discard-route-draft':()=>{state.routeDraft=null;state.routeDraftVersion=null;state.simulation=null;renderPage(false);},
  'simulate-route':async(el,id)=>{
    const {protocol,request}=simulationRequest(id,state.simulateMode);
    state.simulation=await rpc('route.test',request?{id,protocol,request}:{id,protocol});
    renderPage(false);
  },
  'set-simulate-mode':(el,id)=>{
    state.simulateMode=el.dataset.value;
    state.simulation=null;
    renderPage(false);
  }
};

// 实时数据轮询：只在路由页且没有对话框、没有拖动、页面可见时刷新。
// 离开路由页清掉 interval，回来时 bindRoutes 立刻取一次；已有 interval 就不再启动，避免叠加。
export function bindRoutes(){
  if(state.page!=='routes'){stopLive();return;}
  bindCandidateDrag(moveCandidate);
  if(liveTimer)return;
  refreshLive();
  liveTimer=setInterval(refreshLive,LIVE_INTERVAL);
}

function stopLive(){
  clearInterval(liveTimer);
  liveTimer=null;
}

function refreshLive(){
  // #main 消失说明已经退出登录，路由页不会再回来，继续轮询只是白打接口
  if(state.page!=='routes'||!$('#main')){stopLive();return;}
  if($('dialog')||state.routeDraft||state.routeDragging||document.visibilityState!=='visible')return;
  // 失败静默：下一次轮询再试，不打断页面上的操作
  rpc('route.live').then(d=>{
    state.live=d;
    if(state.page==='routes'&&!$('dialog')&&!state.routeDraft&&!state.routeDragging&&document.visibilityState==='visible')renderPage(false);
  }).catch(()=>{});
}
