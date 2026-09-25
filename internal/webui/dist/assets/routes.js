// 智能路由页：路由列表与画布、路由编辑器、别名、策略定义。
import {rpc} from './api.js';
import {renderPage} from './app.js';
import {$,$$,E,compact,dateTime,dateTimeSec,json,money,ms,number,state} from './core.js';
import {icon} from './icons.js';
import {avatar,badge,btn,chart,check,checked,closeDialog,copy,empty,field,flowMini,getModel,getProvider,head,headerValue,modelName,nval,pillStatus,quotaBars,rangeTools,save,selectField,selectRow,showDialog,sourceTag,spark,tag,toast,val} from './ui.js';
import {modelEditor} from './models.js';

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

export function routes(){
  const rs=state.config.routes;
  let r=rs.find(r=>r.id===state.routeID)||rs[0];
  if(r)state.routeID=r.id;
  const cand=state.routeDraft||r?.candidates||[];
  return head('ROUTING STUDIO','让请求，找到合适的模型。','按优先级、权重、响应速度、价格或负载选择候选；会话优先保持亲和，不会在半截流中切换模型。',btn('创建路由','add-route','plus','','primary'))+(r?`<div class="route-layout"><div class="route-list"><p class="tiny muted" style="padding:0 2px 4px">拖动卡片调整顺序</p>${rs.map((x,i)=>`<button class="route-select ${x.id===r.id?'active':''}" draggable="true" data-drag-index="${i}" data-drag-kind="route" data-action="select-route" data-id="${E(x.id)}"><div class="between"><strong>${E(x.name||x.id)}</strong><span class="dot ${x.enabled?'positive':'muted'}"></span></div><p class="mono">${E(x.id)}</p><p>${x.candidates.length} 个候选 · ${strategyName(x.strategy)}</p></button>`).join('')}</div><section class="card"><div class="card-head"><div><h2>${E(r.name||r.id)}</h2><p class="card-sub">${E(r.description||'拖动候选调整优先级，或用上下箭头操作。')}</p></div><div class="flex">${state.routeDraft?btn('应用排序','save-route-order','check','','mint small'):''}<button class="switch ${r.enabled?'on':''}" role="switch" aria-checked="${r.enabled}" aria-label="启用路由 ${E(r.name||r.id)}" data-action="toggle-route" data-id="${E(r.id)}"></button>${btn('编辑','edit-route','settings',`data-id="${E(r.id)}"`,'small')}<button class="icon-btn" data-action="delete-route" data-id="${E(r.id)}" aria-label="删除路由">${icon('trash')}</button></div></div>${routeLiveBar(r.id)}<div class="route-canvas"><div class="router-node">${icon('prism')}<strong class="mono">${E(r.id)}</strong><small>CLIENT REQUEST</small>${badge(r.strategy)}</div><div class="route-connector"></div><div class="route-candidates">${cand.map((x,i)=>{const m=getModel(x.model_id);const cs=candidateState(x.model_id);const flags=candidateBadges(cs);return `<div class="candidate${cs.disabled||cs.cooling?' muted':''}" draggable="true" data-drag-index="${i}" data-drag-kind="candidate">${icon('drag','drag-handle')}<span class="candidate-order">${String(i+1).padStart(2,'0')}</span><div class="candidate-info"><strong class="candidate-title"><span class="ellipsis">${E(m?.name||x.model_id)}</span>${sourceTag(m?.provider_id)}</strong><small><span class="mono">${E(x.model_id)}</span> · weight ${x.weight}</small>${flags?`<div class="candidate-flags">${flags}</div>`:''}${candidateLive(x.model_id)}${shareBar(r.id,x.model_id)}</div>${badge(m?.protocol||'?')}<div><button class="icon-btn" data-action="candidate-up" data-index="${i}" ${i===0?'disabled':''} aria-label="上移">${icon('up')}</button><button class="icon-btn" data-action="candidate-down" data-index="${i}" ${i===cand.length-1?'disabled':''} aria-label="下移">${icon('down')}</button></div></div>`;}).join('')||'<div class="small muted">没有候选，请编辑路由添加模型。</div>'}</div></div><p class="route-canvas-note tiny muted">并发、速度等实时指标每 5 秒刷新；流量占比统计近 ${state.data?.minutes||60} 分钟该路由的实际尝试，不是额度。「新选」是按策略挑中的，「亲和」是会话沿用上次的模型。</p><div class="route-properties"><div><small>会话亲和</small><strong>${r.affinity?'优先保持同一模型':'已关闭'}</strong></div><div><small>重试边界</small><strong>安全请求 + 上游 429 / 503</strong></div><div><small>流开始之后</small><strong>禁止中途切模型</strong></div><div class="flex" style="margin-left:auto;gap:8px"><div class="segmented">${['claude-code','simple'].map(v=>`<button type="button" class="${state.simulateMode===v?'active':''}" data-action="set-simulate-mode" data-value="${v}">${v==='claude-code'?'Claude Code 请求':'简单请求'}</button>`).join('')}</div>${btn('模拟选择','simulate-route','play',`data-id="${E(r.id)}"`,'small')}</div></div>${state.simulation?`<div class="simulation"><h3>路由模拟结果 · ${state.simulateMode==='claude-code'?'Claude Code 请求':'简单请求'}</h3><p class="tiny muted" style="margin:6px 0 14px">${E(state.simulation.note)}</p>${(state.simulation.checks||[]).filter(c=>!c.eligible).map(c=>`<p class="sim-out"><b class="mono">${E(modelName(c.model_id))}</b> 淘汰：${E(c.reason)}</p>`).join('')}${state.simulation.ranked.map((x,i)=>`<div class="sim-line"><span class="flex">${badge(String(i+1).padStart(2,'0'))}<b>${E(modelName(x.model_id))}</b>${sourceTag(getModel(x.model_id)?.provider_id)}${badge(x.protocol)}</span><span class="mono">${x.score.toFixed(1)}</span></div>`).join('')||'<p class="small negative">没有兼容候选。检查下方原因。</p>'}<details style="margin-top:14px"><summary class="small muted">查看候选判定依据</summary><pre style="margin-top:10px">${E(json(state.simulation.checks))}</pre></details></div>`:''}</section></div>`:empty('创建你的第一个路由','把多个模型组成一个稳定的客户端入口，例如 auto-coding。候选只在本账户的授权范围内使用。','add-route','创建路由','route'))+`<section class="card aliases-grid"><div class="card-head"><div><h2>模型别名</h2><p class="card-sub">客户端名称映射到真实模型或路由；实际解析结果记录在请求元数据中。</p></div>${btn('添加别名','add-alias','plus','','small')}</div>${state.config.aliases.length?`<div class="table-scroll"><table><thead><tr><th>客户端请求名称</th><th>指向目标</th><th>状态</th><th></th></tr></thead><tbody>${state.config.aliases.map(a=>`<tr><td class="mono">${E(a.id)}</td><td class="mono">${E(a.target)}</td><td>${tag(a.enabled?'已启用':'停用',a.enabled?'':'neutral')}</td><td><div class="table-actions"><button class="icon-btn" data-action="delete-alias" data-id="${E(a.id)}" aria-label="删除别名">${icon('trash')}</button></div></td></tr>`).join('')}</tbody></table></div>`:empty('别名不是必需项','你可以直接使用模型 ID 或路由 ID，也可以在这里增加一个更好记的名字。','','','link')}</section>`;

}

// 候选池可能有几百个模型，没有搜索就翻不动。
// 已勾选的行任何时候都保持可见：搜索把自己已经选中的东西藏起来，
// 会让人以为选择丢了。
export function bindCandidateSearch(){
  const box=$('#candidate-search');
  if(!box)return;
  const rows=$$('.candidate-row');
  const count=$('#candidate-count');
  const providerSel=$('#candidate-provider');
  const protocolSel=$('#candidate-protocol');
  const filterBar=$('.candidate-filters');
  const clearBtn=filterBar?$('[data-cand-clear]',filterBar):null;

  // 缓存每行的复选框，避免重复查询
  const items=rows.map(row=>({row,cb:$('input[name=candidate]',row)}));

  const filters={provider:'',protocol:'',chips:{}};
  const hasFilters=()=>filters.provider||filters.protocol||box.value.trim()||Object.values(filters.chips).some(v=>v);

  // 通用匹配逻辑
  const matches=(item,f)=>{
    const q=f.q||'';
    const matchSearch=!q||item.row.dataset.search.includes(q);
    const matchProvider=!f.provider||item.row.dataset.provider===f.provider;
    const matchProto=!f.protocol||item.row.dataset.protocol===f.protocol;
    let matchChips=true;
    for(const [k,v] of Object.entries(f.chips||{})){
      if(!v)continue;
      if(k==='checked'){if(!item.cb.checked)matchChips=false;}
      else if(item.row.dataset[k]!=='1')matchChips=false;
    }
    return matchSearch&&matchProvider&&matchProto&&matchChips;
  };

  // 计数：只计匹配的行，不含已勾选的常显行
  const countWith=(applyChip)=>{
    const testFilters={...filters,chips:{...filters.chips,...applyChip},q:box.value.trim().toLowerCase()};
    let c=0;
    items.forEach(item=>{
      if(matches(item,testFilters))c++;
    });
    return c;
  };

  const apply=()=>{
    const q=box.value.trim().toLowerCase();
    const f={provider:filters.provider,protocol:filters.protocol,chips:filters.chips,q};
    let shown=0;
    items.forEach(item=>{
      const matched=matches(item,f);
      item.row.hidden=!(matched||item.cb.checked);
      if(!item.row.hidden)shown++;
    });

    if(count){
      const active=hasFilters();
      count.textContent=active?`显示 ${shown} / ${rows.length} 个模型（已选中的始终显示）`:`共 ${rows.length} 个模型`;
    }

    if(filterBar){
      const chips=$$('[data-cand-filter]',filterBar);
      chips.forEach(chip=>{
        const key=chip.dataset.candFilter;
        chip.classList.toggle('active',!!filters.chips[key]);
        const cnt=chip.querySelector('.filter-count');
        if(cnt)cnt.textContent=countWith({[key]:true});
      });
      if(clearBtn)clearBtn.hidden=!hasFilters();
    }

    const off=items.filter(({row,cb})=>row.classList.contains('off')&&cb.checked).length;
    const note=$('#candidate-enable-note');
    if(note){note.hidden=!off;note.textContent=`已选 ${off} 个未启用的模型，保存路由时会一并启用。`;}
  };

  box.addEventListener('input',apply);
  if(providerSel)providerSel.addEventListener('change',e=>{filters.provider=e.target.value;apply();});
  if(protocolSel)protocolSel.addEventListener('change',e=>{filters.protocol=e.target.value;apply();});

  if(filterBar){
    const chips=$$('[data-cand-filter]',filterBar);
    chips.forEach(chip=>{
      chip.addEventListener('click',()=>{
        const key=chip.dataset.candFilter;
        filters.chips[key]=!filters.chips[key];
        apply();
      });
    });
    if(clearBtn){
      clearBtn.addEventListener('click',()=>{
        box.value='';
        filters.provider='';
        filters.protocol='';
        filters.chips={};
        if(providerSel)providerSel.value='';
        if(protocolSel)protocolSel.value='';
        apply();
      });
    }
  }

  items.forEach(({cb})=>cb?.addEventListener('change',apply));
  apply();
}

export function routeEditor(id=''){

 if(!state.config.models.length){
    toast('请先添加模型',true);
    modelEditor();
    return;

  }

 const old=state.config.routes.find(x=>x.id===id);
  const r=old||{
    id:'',name:'',strategy:'priority',enabled:true,affinity:true,candidates:[],description:''
  };

 const order=[...r.candidates.map(x=>getModel(x.model_id)).filter(Boolean),...state.config.models.filter(m=>!r.candidates.some(c=>c.model_id===m.id))];

 showDialog(old?'编辑路由':'创建智能路由','明确候选池；会话亲和优先，安全失败后才尝试备用。',`<div class="form-grid">${field('路由 ID','id',r.id,'text','客户端可直接将其作为 model。',`${old?'readonly':''} required placeholder="auto-coding"`)}${field('显示名称','name',r.name,'text','','required')}${selectField('选择策略','strategy',r.strategy,STRATEGIES.map(x=>[x[0],x[1]]))}</div>${field('路由描述','description',r.description)}${check('启用路由','enabled',r.enabled)}${check('启用会话亲和','affinity',r.affinity)}<div class="form-section"><h3>候选模型</h3><p class="small muted" style="margin:8px 0 14px">选中加入路由。权重用于「按权重分流」的抽签比例和「权重优先」的排序；冷却中或并发已满的候选会自动排到最后。保存后可在路由画布拖动排序。</p><div class="candidate-tools"><div class="search-field">${icon('search')}<input id="candidate-search" placeholder="搜索模型、ID、协议或供应商" autocomplete="off" aria-label="搜索候选模型"></div><select id="candidate-provider" aria-label="按供应商过滤"><option value="">全部供应商</option>${state.config.providers.map(p=>`<option value="${E(p.id)}">${E(p.name)}</option>`).join('')}</select><select id="candidate-protocol" aria-label="按协议过滤"><option value="">全部协议</option>${['chat','messages','responses','systemone'].map(v=>`<option value="${v}">${v}</option>`).join('')}</select></div><div class="filter-bar candidate-filters"><button type="button" class="filter-chip" data-cand-filter="checked">已选中<span class="filter-count">0</span></button><button type="button" class="filter-chip" data-cand-filter="enabled">已启用<span class="filter-count">0</span></button><button type="button" class="filter-chip" data-cand-filter="tools">支持工具<span class="filter-count">0</span></button><button type="button" class="filter-chip" data-cand-filter="vision">支持图像<span class="filter-count">0</span></button><button type="button" class="filter-chip" data-cand-filter="out64k" title="Claude Code 请求要求 64000 的输出上限">输出 ≥ 64K<span class="filter-count">0</span></button><button type="button" class="filter-chip" data-cand-filter="priced">已计价<span class="filter-count">0</span></button><button type="button" class="filter-chip" data-cand-filter="free">免费<span class="filter-count">0</span></button><button type="button" class="btn ghost small" data-cand-clear hidden>清除筛选</button></div><p class="tiny muted" id="candidate-count"></p><p class="enable-note" id="candidate-enable-note" hidden></p><div class="candidate-list">${order.map(m=>{const c=r.candidates.find(x=>x.model_id===m.id);const p=getProvider(m.provider_id);const hay=[m.id,m.name,m.upstream,m.protocol,m.provider_id,p?.name].filter(Boolean).join(' ').toLowerCase();return `<div class="candidate-row${m.enabled?'':' off'}" data-search="${E(hay)}" data-provider="${E(m.provider_id)}" data-protocol="${E(m.protocol)}" data-enabled="${m.enabled?'1':'0'}" data-tools="${m.tools?'1':'0'}" data-vision="${m.vision?'1':'0'}" data-out64k="${m.max_output_tokens>=64000?'1':'0'}" data-priced="${m.pricing_set?'1':'0'}" data-free="${m.pricing_set&&!(m.input_price||m.output_price||m.cache_price||m.write_price)?'1':'0'}"><label class="checkline" style="flex:1"><input name="candidate" type="checkbox" value="${E(m.id)}" ${c?'checked':''}><span><span class="candidate-title">${E(m.name||m.id)}${sourceTag(m.provider_id)}${m.enabled?'':'<span class="off-tag">未启用</span>'}</span><small><span class="mono">${E(m.id)}</span> · ${E(m.protocol)}</small></span></label><input class="weight-field" type="number" min="1" max="1000" value="${c?.weight||10}" data-weight="${E(m.id)}" style="width:80px" aria-label="候选权重"></div>`}).join('')}</div></div>`,async f=>{const candidates=$$('input[name=candidate]:checked',f).map(el=>({model_id:el.value,weight:Number($$('[data-weight]',f).find(x=>x.dataset.weight===el.value).value)}));const enable_models=candidates.some(c=>!getModel(c.model_id)?.enabled);await save('route.save',{id,enable_models,route:{id:val(f,'id').trim(),name:val(f,'name').trim(),strategy:val(f,'strategy'),description:val(f,'description'),enabled:checked(f,'enabled'),affinity:checked(f,'affinity'),sort:r.sort||0,candidates}},f);});
  bindCandidateSearch();

}

export function aliasEditor(){
  const opts=[...state.config.routes.map(x=>[x.id,`${x.name||x.id} · 路由`]),...state.config.models.map(x=>[x.id,x.name||x.id])];
  if(!opts.length){
    toast('请先添加模型或路由',true);
    return;

  }
  showDialog('添加模型别名','别名必须指向现有模型或路由，不允许多级别名环。',`${field('客户端名称','id','','text','','required placeholder="coding"')}${selectField('目标','target',opts[0][0],opts)}${check('启用别名','enabled',true)}`,f=>save('alias.save',{alias:{id:val(f,'id').trim(),target:val(f,'target'),enabled:checked(f,'enabled')}},f));

}

export function moveCandidate(from,to){
  const r=state.config.routes.find(x=>x.id===state.routeID);
  if(!r)return;
  const xs=structuredClone(state.routeDraft||r.candidates);
  if(from<0||to<0||from>=xs.length||to>=xs.length)return;
  const [x]=xs.splice(from,1);
  xs.splice(to,0,x);
  state.routeDraft=xs;
  state.simulation=null;
  renderPage(false);

}

// ===== 路由画布实时状态与模拟 =====
// 路由页的 state.data 来自 route.stats：
//   {minutes, now, share:{路由ID:{模型ID:{attempts,success}}}, runtime:{active, models:{模型ID:{active,cooldown_until,last_status,latency_ms,ttfb_ms,limits,limits_at}}}}

// 候选此刻的状态：{disabled, cooling(恢复时刻毫秒，0 为未冷却), active, concurrency, full}
export function candidateState(modelId){
  const m=getModel(modelId);
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
export function candidateShare(routeId,modelId){
  const bucket=state.data?.share?.[routeId]||{};
  const total=Object.values(bucket).reduce((s,v)=>s+(v.attempts||0),0);
  const x=bucket[modelId]||{};
  return {share:total?(x.attempts||0)/total:0,attempts:x.attempts||0,success:x.success||0,affinity:x.affinity||0};
}

// 模拟请求体。mode：'simple' 简单文本请求；'claude-code' 带工具定义、thinking、max_tokens=64000 的 messages 请求
// 返回 {protocol, request}，直接作为 route.test 的参数
export function simulationRequest(routeId,mode){
  if(mode==='claude-code')return {protocol:'messages',request:{model:routeId,max_tokens:64000,thinking:{type:'enabled',budget_tokens:10000},messages:[{role:'user',content:'hi'}],tools:[{name:'Read',description:'read file',input_schema:{type:'object',properties:{}}}]}};
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

// 近窗口落点占比条：0 次时弱化显示，避免看起来像“均匀分布”
function shareBar(routeId,modelId){
  const sh=candidateShare(routeId,modelId);
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
  return `<div class="route-live"><div class="live-grid">${liveItem(`${lv(r?.active)} <span class="live-sep">/</span> ${lv(r?.capacity)}`,'并发',lead,sub)}${liveItem(`${lv(r?.tok_s,tokRate)} <span class="live-unit">tok/s</span>`,'输出速度 · 近 60 秒')}${liveItem(lv(r?.rpm),'RPM')}${liveItem(rateText(r?.requests_5m,r?.success_5m),'成功率')}${liveItem(lv(r?.sessions),'会话')}${liveItem(lv(r?.ttfb_ms,v=>v>0?ms(v):'—'),'首字节')}</div><span class="live-tag" title="每 ${LIVE_INTERVAL/1000} 秒自动刷新路由与候选的实时数据"><i class="dot"></i>实时 · ${LIVE_INTERVAL/1000}s</span></div>`;
}

// 候选卡片上的一行实时指标：并发条 + 并发 / RPM / 速度 / 首字节 / 会话。
// 冷却倒计时已由 candidateBadges 显示，这里不重复；没有数据的项直接省略。
function candidateLive(modelId){
  const m=state.live?.models?.[modelId];
  if(!m)return '';
  const parts=[];
  if(m.active!=null&&m.concurrency)parts.push(`并发 ${number(m.active)}/${number(m.concurrency)}`);
  if(m.rpm!=null)parts.push(m.rpm_limit?`RPM ${number(m.rpm)} / ${number(m.rpm_limit)}`:`RPM ${number(m.rpm)}`);
  if(m.tok_s>0)parts.push(`${tokRate(m.tok_s)} tok/s`);
  if(m.ttfb_ms>0)parts.push(`TTFB ${ms(m.ttfb_ms)}`);
  if(m.sessions!=null)parts.push(`${number(m.sessions)} 会话`);
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
  if($('dialog')||state.routeDraft||document.visibilityState!=='visible')return;
  // 失败静默：下一次轮询再试，不打断页面上的操作
  rpc('route.live').then(d=>{
    state.live=d;
    if(state.page==='routes'&&!$('dialog')&&!state.routeDraft&&document.visibilityState==='visible')renderPage(false);
  }).catch(()=>{});
}
