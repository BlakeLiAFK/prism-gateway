// 模型与供应商的用量统计：模型库「用量」列、供应商卡片汇总、模型详情的每日趋势。
// 数据来自 model.stats（随页面加载）与 model.usage（打开模型详情时单独取）。
import {rpc} from './api.js';
import {loadPage,renderPage} from './app.js';
import {$,E,compact,dateTime,money,number,state} from './core.js';
import {btn,confirm,getModel,modelName,showDialog,toast} from './ui.js';
import {modelEditor} from './models.js';
import {keyEditor} from './views.js';

const RANGES=[[1,'24 小时'],[7,'7 天'],[30,'30 天']];
const ROLE_NAMES={'':'主线程 / 其他',main:'主线程',subagent:'子代理',auxiliary:'后台',compaction:'压缩',workflow:'工作流'};

export const usageDays=()=>state.usageDays||7;
const rangeLabel=()=>RANGES.find(r=>r[0]===usageDays())?.[1]||usageDays()+' 天';

// 模型在当前时间范围内的用量；没有记录时返回 null
export function modelUsageOf(id){
  return state.data?.models?.[id]||null;
}

// 时间范围切换，模型库与供应商页共用
export function rangeSwitch(){
  return `<div class="segmented" role="group" aria-label="用量时间范围">${RANGES.map(([d,l])=>`<button type="button" class="${usageDays()===d?'active':''}" data-action="usage-days" data-value="${d}">${l}</button>`).join('')}</div>`;
}

// 花费文字：有估算花费显示金额；只有未计价的请求时写明「未计价」，不显示成 $0
function costText(u,priced=true){
  if(u.cost>0)return money(u.cost);
  if(u.unpriced>0||!priced)return '未计价';
  return money(0);
}

const rate=u=>u.requests?Math.round(u.success/u.requests*100)+'%':'—';
const tokens=u=>(u.input_tokens||0)+(u.output_tokens||0);

// 模型库表格的「用量」单元格
export function usageCell(m){
  const u=modelUsageOf(m.id);
  if(!u?.requests)return '<span class="muted">—</span>';
  return `<div class="cell-title">${number(u.requests)} 次 <span class="tiny muted">· 成功 ${rate(u)}</span></div><div class="cell-sub" title="输入 ${number(u.input_tokens)} · 输出 ${number(u.output_tokens)} · 缓存 ${number(u.cache_tokens)}">${compact(tokens(u))} tokens · ${costText(u,m.pricing_set)}</div>`;
}

// 模型库表头的用量列：点击按请求数排序
export function usageHead(){
  const on=!!state.modelFilters.sortUsage;
  return `<th><button class="th-sort${on?' active':''}" data-action="model-sort-usage" title="按请求数排序">用量 · ${rangeLabel()}${on?' ↓':''}</button></th>`;
}

// 供应商卡片上的总体用量
export function providerStatsBlock(p){
  const u=state.data?.providers?.[p.id];
  const body=u?.requests
    ?`<div class="provider-stats-grid"><div><strong>${number(u.requests)}</strong><small>请求 · 成功 ${rate(u)}</small></div><div><strong>${compact(tokens(u))}</strong><small>tokens</small></div><div><strong>${costText(u)}</strong><small>${u.unpriced?`估算 · ${number(u.unpriced)} 次未计价`:'估算花费'}</small></div></div>`
    :'<p class="tiny muted">这段时间没有请求</p>';
  return `<div class="provider-stats"><div class="provider-usage-row"><span class="tiny muted">本地用量 · ${rangeLabel()}</span>${u?.last_used?`<span class="tiny muted">最近 ${dateTime(u.last_used)}</span>`:''}</div>${body}</div>`;
}

// 模型详情里的用量区块：先占位，loadModelUsage 取到数据后填充
export function modelUsagePanel(){
  return `<div class="form-section model-usage"><h3>用量 · 近 30 天</h3><div id="model-usage-body"><div class="skeleton skeleton-line"></div><div class="skeleton skeleton-line" style="width:60%"></div></div></div>`;
}

export async function loadModelUsage(id){
  const el=()=>$('#model-usage-body');
  try{
    const d=await rpc('model.usage',{id,days:30,tz_offset_min:new Date().getTimezoneOffset()});
    if(el())el().innerHTML=modelUsageBody(d,getModel(id));
  }catch(e){
    if(el())el().innerHTML=`<p class="small negative">${E(e.message||String(e))}</p>`;
  }
}

function modelUsageBody(d,m){
  const sum=d.daily.reduce((a,x)=>({requests:a.requests+x.requests,success:a.success+x.success,input_tokens:a.input_tokens+x.input_tokens,output_tokens:a.output_tokens+x.output_tokens,cost:a.cost+x.cost,unpriced:a.unpriced+(x.unpriced||0)}),{requests:0,success:0,input_tokens:0,output_tokens:0,cost:0,unpriced:0});
  if(!sum.requests)return '<p class="small muted">近 30 天没有请求。</p>';
  const total=d.roles.reduce((a,r)=>a+r.requests,0)||1;
  const fill=sum.unpriced&&m?.pricing_set?`<div class="reprice-note"><span class="small muted">${number(sum.unpriced)} 次请求发生在确认价格之前，还没有计入花费。</span>${btn('按当前价格补录','reprice-history','refresh',`data-model="${E(m.id)}"`,'small')}</div>`:'';
  return fill+`<div class="usage-summary"><div><strong>${number(sum.requests)}</strong><small>请求 · 成功 ${rate(sum)}</small></div><div><strong>${compact(tokens(sum))}</strong><small>tokens</small></div><div><strong>${costText(sum,m?.pricing_set)}</strong><small>估算花费</small></div></div>${bars(d.daily)}<div class="role-split">${d.roles.map(r=>{const pct=Math.round(r.requests/total*100);const [c,t]=(r.role||'').split(':');const name=[ROLE_NAMES[c]??c,t].filter(Boolean).join(' · ');return `<div class="role-line"><span>${E(name)}</span><span class="role-bar"><i style="width:${Math.max(pct,2)}%"></i></span><b>${number(r.requests)} 次 · ${pct}%</b></div>`;}).join('')}</div>`;
}

// 每日请求柱状图，悬停显示当天明细
function bars(daily){
  const W=640,H=120,gap=3,bw=(W-gap*(daily.length-1))/daily.length;
  const top=Math.max(1,...daily.map(x=>x.requests));
  const day=t=>new Date(t).toLocaleDateString('zh-CN',{month:'2-digit',day:'2-digit'});
  return `<svg class="usage-bars" viewBox="0 0 ${W} ${H+16}" role="img" aria-label="近 30 天每日请求数">${daily.map((x,i)=>{const h=x.requests?Math.max(2,x.requests/top*H):0;return `<rect x="${(i*(bw+gap)).toFixed(1)}" y="${(H-h).toFixed(1)}" width="${bw.toFixed(1)}" height="${h.toFixed(1)}" rx="2"><title>${day(x.time)} · ${number(x.requests)} 次 · ${compact(tokens(x))} tokens · ${money(x.cost)}</title></rect>`;}).join('')}<text x="0" y="${H+13}" class="chart-label">${day(daily[0].time)}</text><text x="${W}" y="${H+13}" class="chart-label" text-anchor="end">今天</text></svg>`;
}

// 补录历史花费：先预览条数与金额，确认后写入；data-model 为空时处理全部模型
async function repriceHistory(el){
  const model_id=el.dataset.model||'';
  const p=await rpc('request.reprice',{model_id,dry_run:true});
  if(!p.count){toast(p.unpriced_models.length?`没有可补录的记录；${p.unpriced_models.length} 个模型仍未确认价格`:'没有需要补录的记录');return;}
  const detail=p.models.map(x=>`${getModel(x.model_id)?.name||x.model_id} ${number(x.count)} 条 ${money(x.cost)}`).join('；');
  const skip=p.unpriced_models.length?`另有 ${p.unpriced_models.length} 个模型仍未确认价格，不处理。`:'';
  if(!await confirm(`按当前价格补录 ${number(p.count)} 条记录？`,`合计约 ${money(p.cost)}：${detail}。按现在的单价计算，不是请求当时的价格；用量未知与演示请求不受影响。${skip}`,'补录'))return;
  const r=await rpc('request.reprice',{model_id});
  toast(`已补录 ${number(r.count)} 条，合计 ${money(r.cost)}`);
  await loadPage(false);
  if(model_id)modelEditor(model_id);
}

// 访问密钥列表的用量单元格：u 是 apikey.list 附带的 usage，没有请求时为空
export function keyUsageCell(k){
  const u=k.usage;
  if(!u?.requests)return '<span class="muted">—</span>';
  return `<div class="cell-title">${number(u.requests)} 次 <span class="tiny muted">· 成功 ${rate(u)}</span></div><div class="cell-sub">${compact(tokens(u))} tokens · ${costText(u)}</div>`;
}
export const keyUsageHead=()=>`<th>用量 · ${rangeLabel()}</th>`;

// Key 用量详情：近 30 天每日请求、常用模型与路由、近 7 天来源
export function keyUsageDialog(id,name){
  showDialog(`${name||id} · 用量`,'按小时汇总的本地统计；花费按已确认的单价估算，不是上游账单。',`<div id="key-usage-body"><div class="skeleton skeleton-line"></div><div class="skeleton skeleton-line" style="width:60%"></div></div>`,null,'',true);
  rpc('key.usage',{id,days:30,tz_offset_min:new Date().getTimezoneOffset()}).then(d=>{
    const el=$('#key-usage-body');if(!el)return;
    const sum=d.daily.reduce((a,x)=>({requests:a.requests+x.requests,success:a.success+x.success,input_tokens:a.input_tokens+x.input_tokens,output_tokens:a.output_tokens+x.output_tokens,cost:a.cost+x.cost,unpriced:a.unpriced+(x.unpriced||0)}),{requests:0,success:0,input_tokens:0,output_tokens:0,cost:0,unpriced:0});
    const list=(title,rows,label)=>rows.length?`<h3 class="key-usage-title">${title}</h3><div class="role-split">${rows.map(r=>{const pct=Math.round(r.requests/(sum.requests||1)*100);return `<div class="role-line"><span class="ellipsis" title="${E(r.id)}">${E(label(r.id))}</span><span class="role-bar"><i style="width:${Math.max(pct,2)}%"></i></span><b>${number(r.requests)} 次 · ${compact(r.tokens)} tokens</b></div>`;}).join('')}</div>`:'';
    const src=d.sources.length?`<h3 class="key-usage-title">近 7 天来源</h3><div class="key-sources">${d.sources.map(s=>`<div><span class="mono">${E(s.ip||'—')}</span><span class="ellipsis muted" title="${E(s.agent)}">${E(s.agent||'—')}</span><b>${number(s.requests)} 次 · ${dateTime(s.last_used)}</b></div>`).join('')}</div>`:'';
    el.innerHTML=sum.requests?`<div class="usage-summary"><div><strong>${number(sum.requests)}</strong><small>请求 · 成功 ${rate(sum)}</small></div><div><strong>${compact(tokens(sum))}</strong><small>tokens</small></div><div><strong>${costText(sum)}</strong><small>估算花费</small></div></div>${bars(d.daily)}${list('常用模型',d.models,modelName)}${list('常用路由 / 请求名',d.routes,x=>x)}${src}`:'<p class="small muted">近 30 天没有请求。</p>';
  }).catch(e=>{const el=$('#key-usage-body');if(el)el.innerHTML=`<p class="small negative">${E(e.message||String(e))}</p>`;});
}

export const usageActions={
  'key-usage':el=>keyUsageDialog(el.dataset.id,el.dataset.name),
  'edit-key':el=>keyEditor(el.dataset.id),
  'reprice-history':repriceHistory,
  'usage-days':async el=>{state.usageDays=Number(el.dataset.value)||7;await loadPage(false);},
  'model-sort-usage':()=>{state.modelFilters={...state.modelFilters,sortUsage:!state.modelFilters.sortUsage};renderPage(false);}
};
