// 请求记录筛选：草稿与已应用条件分离，自动刷新不会覆盖正在输入的内容。
import {$,$$,E,number,state} from './core.js';
import {icon} from './icons.js';

const DAY=24*60*60*1000;
const MAX_TOKEN='9007199254740991';
const MAX_COST='9223372036.854775807';
const DEFAULTS={q:'',status:'',provider:'',range:'24h',from:'',to:'',min_input_tokens:'',max_input_tokens:'',min_output_tokens:'',max_output_tokens:'',min_cost:'',max_cost:'',sort:'newest'};
const RANGES={
  '24h':['过去 24 小时',DAY],
  '7d':['过去 7 天',7*DAY],
  '30d':['过去 30 天',30*DAY],
  custom:['自定义时间',0]
};
const SORTS=[['newest','最新优先'],['input_desc','输入最多'],['output_desc','输出最多'],['cost_desc','估算价值最高']];
const NUMERIC_PAIRS=[
  ['min_input_tokens','max_input_tokens','输入 Token','整数'],
  ['min_output_tokens','max_output_tokens','输出 Token','整数'],
  ['min_cost','max_cost','估算价值 · USD','金额']
];

function copy(v){return {...DEFAULTS,...v};}

export function ensureRequestFilters(){
  if(!state.requestFilterDraft)state.requestFilterDraft=copy();
  if(!state.requestFilterApplied)state.requestFilterApplied={...copy(),from_ms:null,to_ms:null,anchored:false};
}

function selected(v,current){return v===current?'selected':'';}

function timezoneLabel(){
  const zone=Intl.DateTimeFormat().resolvedOptions().timeZone||'本地时区';
  const offset=-new Date().getTimezoneOffset(),sign=offset>=0?'+':'-';
  const hours=String(Math.floor(Math.abs(offset)/60)).padStart(2,'0');
  const minutes=String(Math.abs(offset)%60).padStart(2,'0');
  return `${zone} · UTC${sign}${hours}:${minutes}`;
}

function rangeField(min,max,label,hint){
  const d=state.requestFilterDraft,cost=min.includes('cost');
  return `<fieldset class="request-filter-range"><legend>${E(label)}</legend><div><label><span>最少</span><input name="${min}" value="${E(d[min])}" inputmode="${cost?'decimal':'numeric'}" autocomplete="off" placeholder="不限" aria-label="${E(label)}最小值"></label><span class="request-filter-sep">至</span><label><span>最多</span><input name="${max}" value="${E(d[max])}" inputmode="${cost?'decimal':'numeric'}" autocomplete="off" placeholder="不限" aria-label="${E(label)}最大值"></label></div><small>${hint}，留空表示不限</small></fieldset>`;
}

export function requestFilterMarkup(total,data){
  ensureRequestFilters();
  const d=state.requestFilterDraft,custom=d.range==='custom';
  const windowText=data?.from&&data?.to?`${new Date(data.from).toLocaleString('zh-CN',{hour12:false})} 至 ${new Date(data.to).toLocaleString('zh-CN',{hour12:false})}`:'过去 24 小时';
  return `<form id="request-filter" class="request-filter" novalidate>
    <div class="request-filter-primary">
      <div class="search-field">${icon('search')}<input name="q" placeholder="搜索模型、请求 ID 或来源 IP" value="${E(d.q)}" aria-label="搜索请求"></div>
      <select name="status" aria-label="筛选请求状态"><option value="">全部状态</option>${[['success','成功'],['error','失败'],['unknown','待确认'],['running','进行中']].map(([v,l])=>`<option value="${v}" ${selected(v,d.status)}>${l}</option>`).join('')}</select>
      <select name="provider" aria-label="筛选供应商"><option value="">全部供应商</option>${state.config.providers.map(p=>`<option value="${E(p.id)}" ${selected(p.id,d.provider)}>${E(p.name||p.id)}</option>`).join('')}</select>
      <select name="sort" aria-label="请求排序">${SORTS.map(([v,l])=>`<option value="${v}" ${selected(v,d.sort)}>${l}</option>`).join('')}</select>
    </div>
    <div class="request-filter-grid">
      <fieldset class="request-filter-time"><legend>时间范围</legend><div class="request-filter-time-row"><label><span>范围</span><select name="range" aria-label="时间范围">${Object.entries(RANGES).map(([v,[l]])=>`<option value="${v}" ${selected(v,d.range)}>${l}</option>`).join('')}</select></label><div class="request-custom-time" ${custom?'':'hidden'}><label><span>开始</span><input name="from" type="datetime-local" value="${E(d.from)}" aria-label="开始时间"></label><label><span>结束</span><input name="to" type="datetime-local" value="${E(d.to)}" aria-label="结束时间"></label></div></div><small>本地时间：${E(timezoneLabel())} · 含开始，不含结束 · 单次最多 30 天</small></fieldset>
      ${rangeField('min_input_tokens','max_input_tokens','输入 Token','非负整数')}
      ${rangeField('min_output_tokens','max_output_tokens','输出 Token','非负整数')}
      ${rangeField('min_cost','max_cost','估算价值 · USD','最多 9 位小数')}
    </div>
    <p class="request-filter-note">未知用量、未知金额和预算预留不参与对应数值筛选；估算价值不等于供应商实付账单。</p>
    <div id="request-filter-error" class="form-error" role="alert" hidden></div>
    <div class="request-filter-actions"><span class="small muted">${number(total)} 条尝试记录 · 查询窗口 ${E(windowText)}</span><div class="flex"><button id="request-filter-clear" class="btn ghost small" type="button">清空条件</button><button class="btn primary small" type="submit">应用筛选</button></div></div>
  </form>`;
}

function normalizeInteger(v){return v.replace(/^0+(?=\d)/,'');}

function compareInteger(a,b){
  a=normalizeInteger(a);b=normalizeInteger(b);
  return a.length===b.length?a.localeCompare(b):a.length-b.length;
}

function costParts(v){
  const [whole,fraction='']=v.split('.');
  return [normalizeInteger(whole),fraction.padEnd(9,'0')];
}

function compareCost(a,b){
  const [aw,af]=costParts(a),[bw,bf]=costParts(b);
  const whole=compareInteger(aw,bw);
  return whole||af.localeCompare(bf);
}

function readDraft(form){
  const next={};
  for(const key of Object.keys(DEFAULTS))next[key]=form.elements[key]?.value.trim()??DEFAULTS[key];
  return copy(next);
}

function validate(draft){
  const integer=/^\d+$/,cost=/^\d+(?:\.\d{1,9})?$/;
  for(const [min,max,label,kind] of NUMERIC_PAIRS){
    const pattern=kind==='整数'?integer:cost,compare=kind==='整数'?compareInteger:compareCost;
    if(draft[min]&&!pattern.test(draft[min]))return `${label}最小值必须是${kind==='整数'?'非负整数':'非负 USD 十进制，最多 9 位小数'}。`;
    if(draft[max]&&!pattern.test(draft[max]))return `${label}最大值必须是${kind==='整数'?'非负整数':'非负 USD 十进制，最多 9 位小数'}。`;
    const ceiling=kind==='整数'?MAX_TOKEN:MAX_COST;
    if(draft[min]&&compare(draft[min],ceiling)>0||draft[max]&&compare(draft[max],ceiling)>0)return `${label}不能超过 ${ceiling}。`;
    if(draft[min]&&draft[max]&&compare(draft[min],draft[max])>0)return `${label}最小值不能大于最大值。`;
  }
  if(draft.range!=='custom')return '';
  if(!draft.from||!draft.to)return '自定义时间必须同时填写开始和结束时间。';
  const from=new Date(draft.from).getTime(),to=new Date(draft.to).getTime();
  if(!Number.isFinite(from)||!Number.isFinite(to))return '自定义时间格式无效。';
  if(from<0||to<0)return '自定义时间不能早于 1970 年 1 月 1 日。';
  if(from>=to)return '开始时间必须早于结束时间。';
  if(to-from>30*DAY)return '单次查询跨度不能超过 30 天，请缩小时间范围。';
  return '';
}

function applyDraft(draft){
  const now=Date.now()+1,duration=RANGES[draft.range]?.[1]||0;
  state.requestFilterApplied={...draft,from_ms:duration?now-duration:new Date(draft.from).getTime(),to_ms:duration?now:new Date(draft.to).getTime(),anchored:true};
  state.requestPage=1;
}

function setBusy(form,busy){
  form.setAttribute('aria-busy',String(busy));
  $$('button',form).forEach(button=>button.disabled=busy);
}

function showError(form,message){
  const box=$('#request-filter-error',form);
  box.textContent=message;
  box.hidden=!message;
}

export function bindRequestFilters(loadPage,report){
  ensureRequestFilters();
  const form=$('#request-filter');
  if(!form)return;
  form.addEventListener('input',()=>{state.requestFilterDraft=readDraft(form);});
  form.addEventListener('change',()=>{state.requestFilterDraft=readDraft(form);});
  $('[name=range]',form)?.addEventListener('change',ev=>{
    state.requestFilterDraft=readDraft(form);
    $('.request-custom-time',form).hidden=ev.target.value!=='custom';
  });
  form.addEventListener('submit',async ev=>{
    ev.preventDefault();
    if(form.getAttribute('aria-busy')==='true')return;
    const draft=readDraft(form),error=validate(draft);
    state.requestFilterDraft=draft;
    showError(form,error);
    if(error)return;
    applyDraft(draft);
    setBusy(form,true);
    try{await loadPage(false);}catch(e){report(e);}finally{if(form.isConnected)setBusy(form,false);}
  });
  $('#request-filter-clear',form)?.addEventListener('click',async()=>{
    state.requestFilterDraft=copy();
    state.requestFilterApplied={...copy(),from_ms:null,to_ms:null,anchored:false};
    state.requestPage=1;
    setBusy(form,true);
    try{await loadPage(false);}catch(e){report(e);}finally{if(form.isConnected)setBusy(form,false);}
  });
  $('[data-request-retry]')?.addEventListener('click',async()=>{
    refreshRequestWindow();
    try{await loadPage(false);}catch(e){report(e);}
  });
}

export function requestListParams(){
  ensureRequestFilters();
  const f=state.requestFilterApplied;
  const params={page:state.requestPage,page_size:30,q:f.q,status:f.status,provider_id:f.provider,sort:f.sort};
  for(const key of ['min_input_tokens','max_input_tokens','min_output_tokens','max_output_tokens','min_cost','max_cost'])if(f[key])params[key]=f[key];
  if(f.anchored){params.from=f.from_ms;params.to=f.to_ms;}
  return params;
}

export function captureRequestWindow(data){
  ensureRequestFilters();
  if(data?.from!=null&&data?.to!=null&&Number.isFinite(Number(data.from))&&Number.isFinite(Number(data.to))){
    state.requestFilterApplied.from_ms=Number(data.from);
    state.requestFilterApplied.to_ms=Number(data.to);
    state.requestFilterApplied.anchored=true;
  }
}

export function refreshRequestWindow(){
  ensureRequestFilters();
  const f=state.requestFilterApplied;
  if(f.range==='custom')return;
  const duration=RANGES[f.range]?.[1]||DAY,to=Date.now()+1;
  f.from_ms=to-duration;
  f.to_ms=to;
  f.anchored=true;
  state.requestPage=1;
}
