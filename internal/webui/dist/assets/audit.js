// 管理操作审计页：只展示操作元数据，不读取或呈现任何密钥正文。
import {loadPage} from './app.js';
import {$,E,dateTimeSec,state} from './core.js';
import {btn,empty,head,report,val} from './ui.js';

const filters={page:1,page_size:30,q:'',action:'',target:'',from:0,to:0};

const localValue=ms=>ms?new Date(ms-new Date(ms).getTimezoneOffset()*60000).toISOString().slice(0,16):'';
const timeValue=value=>value?new Date(value).getTime():0;

export const auditParams=()=>({...filters});

export function renderAudit(data=state.data||{}){
  const items=data.items||[],page=Number(data.page||1),pages=Number(data.pages||0),total=Number(data.total||0);
  const form=`<form class="toolbar" id="audit-filter"><div class="search-field"><input name="q" value="${E(filters.q)}" maxlength="120" placeholder="搜索操作或目标" aria-label="搜索操作或目标"></div><input name="action" value="${E(filters.action)}" maxlength="120" placeholder="操作，例如 provider" aria-label="操作筛选" style="width:170px"><input name="target" value="${E(filters.target)}" maxlength="240" placeholder="目标 ID" aria-label="目标筛选" style="width:170px"><input name="from" type="datetime-local" value="${E(localValue(filters.from))}" aria-label="开始时间" style="width:190px"><input name="to" type="datetime-local" value="${E(localValue(filters.to))}" aria-label="结束时间" style="width:190px"><button class="btn primary small" type="submit">筛选</button>${btn('清除','audit-clear','','type="button"','ghost small')}</form>`;
  const body=items.length?`<div class="table-scroll"><table><thead><tr><th>时间</th><th>操作</th><th>目标</th><th>配置版本</th><th>记录 ID</th></tr></thead><tbody>${items.map(row=>`<tr><td class="mono tiny nowrap">${E(dateTimeSec(row.created_at))}</td><td class="cell-title mono">${E(row.action)}</td><td class="mono">${E(row.target||'—')}</td><td class="mono">v${E(row.version)}</td><td class="mono tiny">${E(row.id)}</td></tr>`).join('')}</tbody></table></div>`:empty('没有匹配的审计记录','调整筛选条件后重试。','','','shield');
  const foot=`<div class="list-foot"><span>共 ${total} 条${pages?` · 第 ${page} / ${pages} 页`:''}</span><div class="flex">${btn('上一页','audit-prev','','type="button" '+(page<=1?'disabled':''),'small')}${btn('下一页','audit-next','','type="button" '+(pages===0||page>=pages?'disabled':''),'small')}</div></div>`;
  return head('SECURITY AUDIT','管理操作审计','记录配置与安全相关管理操作；不包含密钥正文。',btn('刷新','audit-refresh','refresh'))+form+`<section class="card">${body}${foot}</section>`;
}

export function bindAudit(){
  $('#audit-filter')?.addEventListener('submit',ev=>{
    ev.preventDefault();
    const form=ev.currentTarget;
    Object.assign(filters,{page:1,q:val(form,'q').trim(),action:val(form,'action').trim(),target:val(form,'target').trim(),from:timeValue(val(form,'from')),to:timeValue(val(form,'to'))});
    loadPage(false).catch(report);
  });
}

export const auditActions={
  'audit-clear':async()=>{Object.assign(filters,{page:1,q:'',action:'',target:'',from:0,to:0});await loadPage(false);},
  'audit-prev':async()=>{if(filters.page>1){filters.page--;await loadPage(false);}},
  'audit-next':async()=>{if(filters.page<Number(state.data?.pages||0)){filters.page++;await loadPage(false);}},
  'audit-refresh':async()=>{await loadPage(false);}
};
