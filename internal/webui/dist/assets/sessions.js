// 会话亲和页：保留绑定操作，展示请求历史中的模型用量与费用。
import {E,dateTime,money,number,state,untilText} from './core.js';
import {icon} from './icons.js';
import {btn,empty,getProvider,head,modelName,sourceTag} from './ui.js';

function sessionCostCell(x){
  const pending=Number(x.cost_unknown_requests)||0;
  const known=Number(x.cost_known_requests)>0;
  const amount=known?money(x.cost_nano/1e9):pending?'待确认':'—';
  const note=pending?`${known?'已知部分 · ':''}${number(pending)} 次待确认`:known?'':'暂无费用记录';
  return `<div>${amount}</div>${note?`<div class="cell-sub">${note}</div>`:''}${x.demo_requests?`<div class="cell-sub">演示 ${number(x.demo_requests)} 次不计费</div>`:''}`;
}

export function sessions(){
  const d=state.data;
  const counts={};
  d.forEach(x=>{counts[x.model_id]=(counts[x.model_id]||0)+1;});
  const modelIds=Object.keys(counts).sort((a,b)=>counts[b]-counts[a]);
  const filtered=state.sessionModel?d.filter(x=>x.model_id===state.sessionModel):d;
  const filterBar=modelIds.length?`<div class="toolbar"><select id="session-model" aria-label="按绑定模型筛选"><option value="">全部绑定模型（${d.length}）</option>${modelIds.map(id=>`<option value="${E(id)}" ${state.sessionModel===id?'selected':''}>${E(modelName(id))}（${counts[id]}）</option>`).join('')}</select>${state.sessionModel?btn(`解除该模型的全部 ${counts[state.sessionModel]} 个绑定`,'unbind-session-model','link',`data-id="${E(state.sessionModel)}"`,'danger small'):''}</div>`:'';
  return head('SESSION AFFINITY','让同一段对话，保持连续。','只保存匿名会话映射。稳定 session 有助于上游亲和路由，但不保证缓存命中或永久记忆。',btn('刷新','refresh','refresh'))+filterBar+`<section class="card">${filtered.length?`<div class="table-scroll"><table><thead><tr><th>会话 ID（匿名）</th><th>绑定模型</th><th>供应商</th><th>使用模型 / 次数</th><th>成功请求</th><th>总费用 · USD</th><th>最近活动</th><th></th></tr></thead><tbody>${filtered.map(x=>`<tr><td class="mono tiny" title="${E(x.id)}">${E(x.id.slice(0,25))}…</td><td><span class="cell-title flex" style="gap:6px">${E(modelName(x.model_id))}${sourceTag(x.provider_id)}</span></td><td>${E(getProvider(x.provider_id)?.name||x.provider_id)}</td><td>${x.models?.length?x.models.map(m=>`<div class="cell-title flex" title="${E(m.model_id)} · 成功 ${number(m.success_requests)} 次">${E(modelName(m.model_id))}${sourceTag(m.provider_id)||`<span class="muted tiny">${E(m.provider_id)}</span>`}<span class="mono">${number(m.requests)} 次</span></div>`).join(''):'<span class="muted">暂无请求记录</span>'}</td><td class="mono">${number(x.requests)}</td><td class="mono">${sessionCostCell(x)}</td><td class="mono tiny">${dateTime(x.updated_at)}${x.expires_at?`<div class="cell-sub">${untilText(x.expires_at)} 过期</div>`:''}</td><td>${btn('解除绑定','unbind-session','link',`data-id="${E(x.id)}"`,'small')}</td></tr>`).join('')}</tbody></table></div>`:empty('暂无会话绑定','请求中保留 x-opencode-session 或发送 X-Prism-Session，后续请求使用相同值。','','','session')}</section><div class="banner" style="margin-top:20px">${icon('info')}<span>模型次数含失败记录；费用只计当前保留记录中的非演示已确认金额，有缺失时显示已知部分。同一会话的各组绑定显示相同汇总。解除绑定仅删除本地路由亲和记录；不会删除或重置上游的对话、缓存、额度。</span></div>`;

}
