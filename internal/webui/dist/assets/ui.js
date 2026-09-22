// 通用界面构件：按钮、徽章、表单、对话框、图表。
import {rpc} from './api.js';
import {loadPage,login,shell} from './app.js';
import {$,$$,E,compact,dateTime,state} from './core.js';
import {icon} from './icons.js';

export function btn(text,action,ico='',extra='',type=''){
  return `<button class="btn ${type}" data-action="${action}" ${extra}>${ico?icon(ico):''}${E(text)}</button>`;

}

export function tag(text,cls=''){
  return `<span class="status-pill ${cls}"><span class="dot"></span>${E(text)}</span>`;

}

export function badge(text){
  return `<span class="badge">${E(text)}</span>`;

}

export function empty(title,desc,action='',label='',ico='model'){
  return `<div class="empty"><div class="empty-icon">${icon(ico)}</div><h3>${E(title)}</h3><p>${E(desc)}</p>${action?btn(label,action,'plus','','primary'):''}</div>`;

}

export function avatar(p){
  const k=p?.kind||'custom';
  return `<span class="provider-avatar ${E(k)}">${({opencode:'OC',commandcode:'CC',zai:'ZA',deepseek:'DS',openai:'OA',anthropic:'AN',mock:'LO',custom:'API'})[k]||'API'}</span>`;

}

export function pillStatus(s){
  return tag(({success:'成功',error:'失败',unknown:'用量待确认',running:'处理中',queued:'排队中',succeeded:'完成',failed:'失败',interrupted:'已中断'})[s]||s,(['error','failed'].includes(s)?'error':['unknown','interrupted'].includes(s)?'warning':['running','queued'].includes(s)?'neutral':''));

}

export function toast(text,error=false,requestID=''){
  const el=document.createElement('div');
  el.className='toast'+(error?' error':'');
  el.innerHTML=`${icon(error?'warning':'check')}<div>${E(text)}${requestID?`<small>${E(requestID)}</small>`:''}</div><button class="icon-btn" aria-label="关闭">${icon('close')}</button>`;
  el.querySelector('button').addEventListener('click',()=>el.remove());
  $('#toasts').append(el);
  setTimeout(()=>el.remove(),error?10000:4800);

}

export function report(e){
  toast(e.message||'网络连接失败',true,e.requestID||'');
  if(e.status===401&&!document.querySelector('.login-page')){
    clearInterval(state.refreshTimer);
    login();

  }
}

export function field(label,name,value='',type='text',help='',extra=''){
  return `<div class="field"><label for="f-${E(name)}">${E(label)}</label><input id="f-${E(name)}" name="${E(name)}" type="${type}" value="${E(value)}" ${extra}>${help?`<small>${E(help)}</small>`:''}</div>`;

}

export function selectField(label,name,value,options,help=''){
  return `<div class="field"><label for="f-${E(name)}">${E(label)}</label><select id="f-${E(name)}" name="${E(name)}">${options.map(([v,l])=>`<option value="${E(v)}" ${v===value?'selected':''}>${E(l)}</option>`).join('')}</select>${help?`<small>${E(help)}</small>`:''}</div>`;

}

export function check(label,name,value,help=''){
  return `<label class="checkline"><input type="checkbox" name="${E(name)}" ${value?'checked':''}><span>${E(label)}${help?`<small>${E(help)}</small>`:''}</span></label>`;

}

export function val(f,n){
  return f.elements.namedItem(n)?.value??'';

}

export function checked(f,n){
  return !!f.elements.namedItem(n)?.checked;

}

export function nval(f,n){
  return Number(val(f,n));

}

export function getProvider(id){
  return state.config.providers.find(p=>p.id===id);

}

export function getModel(id){
  return state.config.models.find(m=>m.id===id);

}

export function modelName(id){
  return getModel(id)?.name||id;

}

export function showDialog(title,sub,body,onSubmit=null,save='保存配置',wide=false){

 closeDialog();
  const d=document.createElement('dialog');
  d.className='drawer';
  if(wide)d.style.width='730px';

 d.innerHTML=`<form method="dialog" id="drawer-form" class="drawer-shell" data-version="${state.config?.version||0}"><div class="drawer-head"><div><h2>${E(title)}</h2><p>${E(sub)}</p></div><button class="icon-btn" type="button" data-action="close-dialog" aria-label="关闭">${icon('close')}</button></div><div class="drawer-body"><div class="form-error hidden" id="form-error"></div>${body}</div><div class="drawer-footer">${btn(onSubmit?'取消':'关闭','close-dialog','','type="button"')}${onSubmit?`<button type="submit" class="btn primary">${icon('check')}${E(save)}</button>`:''}</div></form>`;

 $('#overlay-root').append(d);
  state.dialogSubmit=onSubmit;
  d.addEventListener('close',()=>{d.remove();state.secretValue='';state.dialogSubmit=null;});
  d.addEventListener('click',ev=>{if(ev.target===d&&ev.clientX<d.getBoundingClientRect().left)d.close();});
  d.showModal();

 $('#drawer-form',d).addEventListener('submit',async ev=>{ev.preventDefault();if(!onSubmit){d.close();return};const b=$('button[type=submit]',d);b.disabled=true;$('#form-error',d).classList.add('hidden');try{await onSubmit(ev.currentTarget);}catch(e){$('#form-error',d).textContent=e.message;$('#form-error',d).classList.remove('hidden');}finally{if(b.isConnected)b.disabled=false;}});
  return d;

}

export function closeDialog(){
  for(const d of $$('dialog')){
    d.close();
    d.remove();

  }
  state.dialogSubmit=null;
  state.secretValue='';

}

export function confirm(title,desc,yes='确认'){
  return new Promise(resolve=>{closeDialog();const d=document.createElement('dialog');d.className='confirm-dialog';d.innerHTML=`<h2>${E(title)}</h2><p>${E(desc)}</p><div class="flex" style="justify-content:flex-end"><button class="btn" data-no>取消</button><button class="btn danger" data-yes>${E(yes)}</button></div>`;$('#overlay-root').append(d);let answer=false;$('[data-yes]',d).onclick=()=>{answer=true;d.close();};$('[data-no]',d).onclick=()=>d.close();d.addEventListener('close',()=>{d.remove();resolve(answer);});d.showModal();});

}

export async function save(action,params,form){
  const version=Number(form?.dataset.version||state.config.version);
  const result=await rpc(action,{version,...params});
  state.config=result;
  closeDialog();
  state.routeDraft=null;
  state.simulation=null;
  toast(`配置已保存 · v${result.version}`);
  await loadPage(false);

}

export async function copy(text){
  try{
    await navigator.clipboard.writeText(text);

  }
  catch{
    const a=document.createElement('textarea');
    a.value=text;
    document.body.append(a);
    a.select();
    document.execCommand('copy');
    a.remove();

  }
  toast('已复制到剪贴板');

}

export function head(eyebrow,title,desc,tools=''){
  return `<div class="page-head"><div><div class="eyebrow">${E(eyebrow)}</div><h1>${E(title)}</h1><p class="page-desc">${E(desc)}</p></div><div class="page-tools">${tools}</div></div>`;

}

export function rangeTools(){
  return `<div class="segmented">${[['24h','24 小时'],['7d','7 天'],['30d','30 天']].map(([v,l])=>`<button class="${state.range===v?'active':''}" data-action="range" data-value="${v}">${l}</button>`).join('')}</div>${btn('刷新','refresh','refresh')}`;

}

export function footer(){
  return `<footer class="footer"><span class="footer-left"><span><span class="dot"></span> <b>管理连接正常</b></span><span>配置 v${state.config?.version||1}</span><span class="mono">/api.json</span></span><span>${state.paused?'自动刷新已暂停':`更新于 ${new Date(state.lastSync||Date.now()).toLocaleTimeString('zh-CN',{hour12:false})}`} · ${btn(state.paused?'恢复':'暂停','pause',state.paused?'play':'pause','','ghost small')}</span></footer>`;

}

export function spark(series){
  const values=series.map(x=>Number(x.requests));
  const max=Math.max(1,...values);
  return `<svg class="stat-spark" viewBox="0 0 70 20" aria-hidden="true"><polyline points="${values.map((v,i)=>`${i*3},${18-v/max*15}`).join(' ')}" fill="none" stroke="currentColor" stroke-width="1.6"/></svg>`;

}

export function chart(series){
  const W=640,H=207,left=40,right=15,top=20,bottom=26;
  const maxValue=Math.max(4,...series.map(x=>Number(x.requests)));
  const height=H-top-bottom;
  const points=series.map((x,i)=>[left+i*(W-left-right)/23,top+height-Number(x.requests)/maxValue*height]);
  const path=points.map((p,i)=>(i?'L':'M')+p.map(x=>x.toFixed(2)).join(',')).join(' ');
  const hasData=series.some(x=>x.requests>0);
  return `<div class="chart-wrap"><svg class="chart-svg" viewBox="0 0 ${W} ${H}" role="img" aria-label="所选时间范围的模型请求趋势"><defs><linearGradient id="chart-fill" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#8fc19b" stop-opacity=".25"/><stop offset="100%" stop-color="#8fc19b" stop-opacity=".01"/></linearGradient></defs>${[0,.25,.5,.75,1].map(v=>{const y=top+height-v*height;return `<line x1="${left}" x2="${W-right}" y1="${y}" y2="${y}" class="chart-grid"/><text x="${left-11}" y="${y+3}" class="chart-label" text-anchor="end">${compact(Math.round(v*maxValue))}</text>`;}).join('')}<path d="${path} L${W-right},${top+height} L${left},${top+height}Z" fill="url(#chart-fill)"/><path d="${path}" class="chart-path"/>${series.map((x,i)=>`<circle cx="${points[i][0]}" cy="${points[i][1]}" r="${x.requests?2.5:0}" class="chart-dot"><title>${dateTime(x.time)} · ${x.requests} 次请求</title></circle>`).join('')}${[0,5,11,17,23].map(i=>`<text x="${points[i][0]}" y="${H-4}" class="chart-label" text-anchor="middle">${state.range==='24h'?new Date(series[i].time).toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',hour12:false}):new Date(series[i].time).toLocaleDateString('zh-CN',{month:'2-digit',day:'2-digit'})}</text>`).join('')}</svg>${!hasData?'<div class="chart-empty">还没有模型请求<br><span class="tiny">添加供应商，或在调试台运行本地演示。</span></div>':''}</div>`;

}

export function flowMini(){
  const n=state.config.providers.filter(x=>x.enabled).length;
  return `<div class="route-mini"><svg viewBox="0 0 360 190" aria-label="客户端经网关连接供应商"><path class="flow-line" d="M89 96H155M202 96H250Q264 96 264 51H292M202 96H292M202 96H250Q264 96 264 141H292"/><rect class="flow-box" x="16" y="77" width="77" height="37" rx="8"/><text x="54" y="99" text-anchor="middle" class="flow-label">CLIENTS</text><rect class="flow-router" x="148" y="67" width="67" height="57" rx="12"/><path d="m170 106 11-24 12 24Z" stroke="#669a76" fill="none" stroke-width="1.5"/><rect class="flow-box" x="282" y="34" width="62" height="30" rx="7"/><rect class="flow-box" x="282" y="80" width="62" height="30" rx="7"/><rect class="flow-box" x="282" y="128" width="62" height="30" rx="7"/><text x="313" y="53" text-anchor="middle" class="flow-label">CHAT</text><text x="313" y="99" text-anchor="middle" class="flow-label">MESSAGES</text><text x="313" y="147" text-anchor="middle" class="flow-label" style="font-size:8px">RESPONSES</text><text x="181" y="146" text-anchor="middle" class="flow-label" style="font-size:8px">PRISM ROUTER</text></svg></div><div class="route-mini-legend"><span><span class="tiny-dot"></span>${n} 个启用供应商</span><span><span class="tiny-dot"></span>${state.config.models.filter(x=>x.enabled).length} 个启用模型</span></div>`;

}

export function quotaBars(q){
  return `<div class="quota-mini-bars">${[['5h','5h'],['7d','7d'],['30d','30d']].map(([k,label])=>{const lim=q['limit_'+k],used=q['used_'+k];const pct=lim?Math.min(100,used/lim*100):0;return `<div><div class="quota-bar ${pct>95?'high':pct>80?'warn':''}"><i style="width:${pct}%"></i></div><div class="quota-bar-label"><span>${label}</span><span>${lim?Math.round(pct)+'%':'未设限'}</span></div></div>`;}).join('')}</div>`;

}

export function headerValue(h,k){
  return h?.[k]?.[0]||h?.[k.toLowerCase()]?.[0]||'';

}

export function selectRow(label,help,name,value,options){
  return `<div class="setting-row"><div><h3>${label}</h3><p>${help}</p></div><select name="${name}" aria-label="${label}">${options.map(v=>`<option value="${v}"${v===value?' selected':''}>${E(v)}</option>`).join('')}</select></div>`;

}
