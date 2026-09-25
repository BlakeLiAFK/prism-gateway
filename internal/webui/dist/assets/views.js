// 各页面与编辑抽屉的渲染。
import {rpc} from './api.js';
import {loadPage,renderPage} from './app.js';
import {$,$$,E,compact,dateTime,dateTimeSec,json,money,ms,number,state} from './core.js';
import {icon} from './icons.js';
import {avatar,badge,btn,chart,check,checked,closeDialog,confirm,copy,empty,field,getModel,getProvider,head,headerValue,modelName,nval,pillStatus,quotaBars,rangeTools,save,selectField,selectRow,showDialog,sourceTag,spark,tag,toast,val} from './ui.js';

// 客户端名称：认得出的给正式名，认不出的退回 UA 第一段，至少看得出是什么在调。
const agentNames=[['claude-code','Claude Code'],['opencode','OpenCode'],['codex','Codex'],['cursor','Cursor'],['openai-python','OpenAI Python'],['openai/python','OpenAI Python'],['openai-node','OpenAI Node'],['openai/node','OpenAI Node'],['anthropic-sdk','Anthropic SDK'],['python-requests','Python requests'],['node-fetch','node-fetch'],['axios','axios'],['curl','curl'],['go-http-client','Go HTTP'],['postman','Postman'],['prism-gateway','Prism 调试台']];

export function agentName(ua){
  if(!ua)return '';
  const l=ua.toLowerCase();
  for(const[m,name]of agentNames)if(l.includes(m))return name;
  return ua.split(/[\s/]/)[0].slice(0,18);
}

function sourceCell(r){
  if(!r.client_ip&&!r.user_agent)return '<span class="muted">—</span>';
  const name=agentName(r.user_agent);
  const role=roleName(r.agent_role);
  return `<div class="cell-title mono tiny">${E(r.client_ip||'—')}</div>${name?`<div class="cell-sub" title="${E(r.user_agent)}">${E(name)}${role?` <span class="role-tag" title="${E(r.agent_role)}">${E(role)}</span>`:''}</div>`:''}`;
}

// Claude Code 声明的请求角色，形如 subagent:Explore
const roleNames={main:'主线程',subagent:'子代理',auxiliary:'后台',compaction:'压缩',workflow:'工作流'};
function roleName(v){
  if(!v)return '';
  const[c,t]=v.split(':');
  return [roleNames[c]||c,t].filter(Boolean).join(' · ');
}

// 输出速度 = 输出 tokens / 总耗时，含首字延迟；没有输出或耗时的记录不显示
function tps(r){
  const n=Number(r.output_tokens),d=Number(r.duration_ms);
  return n>0&&d>0?(n*1000/d).toFixed(1)+' tok/s':'';
}

// 输入输出分开看才有意义：输入贵在量大、输出贵在单价，混成一个数就都看不出来了。
function tokenCell(main,sub,label){
  return `<td class="mono">${compact(Number(main))}${Number(sub)>0?`<div class="cell-sub">${label} ${compact(Number(sub))}</div>`:''}</td>`;
}

export function requestRows(items,short=false){
  if(!items?.length)return empty('请求记录会出现在这里','每次真实转发或本地演示都会记录元数据；不保存你的提示词和回答。','go-playground','发起测试','request');
  return `<div class="table-scroll"><table><thead><tr><th>时间</th><th>模型 / 请求</th>${short?'':'<th>协议</th>'}<th>状态</th><th>耗时</th>${short?'':'<th>输入</th><th>输出</th><th>估算价值</th><th class="col-source">来源</th>'}</tr></thead><tbody>${items.map(r=>`<tr data-action="request-detail" data-id="${E(r.id)}"><td class="mono tiny nowrap">${E(dateTimeSec(r.started_at))}</td><td><div class="cell-title flex">${E(modelName(r.model_id))}${sourceTag(r.provider_id)}${r.is_demo?badge('DEMO'):''}</div><div class="cell-sub mono">${E(r.parent_id.slice(0,23))}…</div></td>${short?'':`<td>${badge(r.protocol)}</td>`}<td>${pillStatus(r.status)}</td><td class="mono">${ms(r.duration_ms)}${tps(r)?`<div class="cell-sub">${tps(r)}</div>`:''}</td>${short?'':`${tokenCell(r.input_tokens,r.cache_tokens,'缓存')}${tokenCell(r.output_tokens,r.write_tokens,'写入')}<td class="mono">${r.cost_known?money(r.cost_nano/1e9):r.usage_mode==='rejected'?'—':'待确认'}</td><td class="col-source">${sourceCell(r)}</td>`}</tr>`).join('')}</tbody></table></div>`;

}

// 请求详情：原来直接打一坨 JSON，字段名是英文、时间是毫秒数，看一眼还得自己翻译。
export function requestDetail(r){
  const rows=[
    ['来源 IP',r.client_ip||'—',true],
    ['客户端',r.user_agent?`${agentName(r.user_agent)} · ${r.user_agent}`:'—'],
    ['开始时间',dateTimeSec(r.started_at),true],
    ['耗时',ms(r.duration_ms),true],
    ['输出速度',tps(r)?tps(r)+'（含首字延迟）':'—',true],
    ['客户端请求名',r.requested_model,true],
    ['实际模型',modelName(r.model_id),false],
    ['供应商',getProvider(r.provider_id)?.name||r.provider_id],
    ['协议',r.protocol===r.upstream_protocol?`${r.protocol}（原生）`:`${r.protocol} → ${r.upstream_protocol}（转换）`,true],
    ['输入 Tokens',number(r.input_tokens),true],
    ['输出 Tokens',number(r.output_tokens),true],
    ['缓存 / 写入',`${number(r.cache_tokens)} / ${number(r.write_tokens)}`,true],
    ['估算价值',r.cost_known?money(r.cost_nano/1e9):'待确认',true],
    ['计价来源',r.usage_mode,true],
    ['HTTP 状态',r.http_status||'—',true],
    ['错误码',r.error_code||'—',true],
    ['选择原因',(r.reason||'').replace(/^;\s*/,'')||'—'],
    ['会话',r.session_id,true],
    ['客户端 Key',r.key_id,true],
    ['请求 ID',r.parent_id,true],
  ];
  return `<div class="detail-grid">${rows.map(([k,v,mono])=>`<div><small>${E(k)}</small><span class="${mono?'mono ':''}">${E(v)}</span></div>`).join('')}</div><details style="margin-top:18px"><summary class="small muted">原始记录</summary><pre style="margin-top:10px">${E(json(r))}</pre></details>`;

}

export function overview(){
  const d=state.data,s=d.summary;
  const total=Number(s.requests),success=total?((s.success/total)*100).toFixed(1)+'%':'—';
  const tokens=Number(s.input_tokens)+Number(s.output_tokens);
  const cache=s.input_tokens?Math.round(s.cache_tokens/s.input_tokens*100)+'%':'—';
  const realProviders=state.config.providers.filter(p=>p.kind!=='mock');
  const enabled=state.config.models.filter(m=>m.enabled);
  const hasDemo=state.config.providers.some(p=>p.kind==='mock');

 return head('WORKSPACE OVERVIEW','你的 AI 流量，一目了然。','统一接入、稳定路由、成本可见。所有数据来自当前工作空间。',rangeTools())+
 (!realProviders.length?`<div class="banner">${icon('info')}<span>工作空间已就绪。添加真实供应商开始接入，或先用本地演示验证完整链路。</span><span class="right">${btn(hasDemo?'进入调试台':'启用演示',hasDemo?'go-playground':'enable-demo','play','','small')}</span></div>`:'')+
 (!total?`<div class="quickstart">${[['连接供应商','填入上游地址与凭证','add-provider',realProviders.length>0],['选择模型与路由','同步、确认能力后启用','go-models',enabled.length>0],['创建客户端 Key','隔离管理员与调用凭证','new-key',false]].map((x,i)=>`<div class="quickstep ${x[3]?'done':''}"><span class="quickstep-num">${x[3]?icon('check'):String(i+1).padStart(2,'0')}</span><div><h3>${x[0]}</h3><p>${x[1]}</p><br><button class="action-link" data-action="${x[2]}">${x[3]?'管理':'开始设置'} ${icon('arrow')}</button></div></div>`).join('')}</div>`:'')+
 `<section class="stats-grid"><article class="stat"><div class="stat-top">模型请求 <span class="stat-icon">${icon('route')}</span></div><div class="stat-value">${number(total)}</div><div class="stat-foot"><span><span class="positive">${success}</span> 成功率</span>${spark(d.series)}</div></article><article class="stat"><div class="stat-top">Tokens 用量 <span class="stat-icon">${icon('bolt')}</span></div><div class="stat-value">${compact(tokens)}</div><div class="stat-foot"><span><span class="positive">${cache}</span> 输入缓存占比</span>${spark(d.series)}</div></article><article class="stat"><div class="stat-top">估算用量价值 <span class="stat-icon">${icon('usage')}</span></div><div class="stat-value">${money(s.cost_nano/1e9)}</div><div class="stat-foot"><span>不等于订阅实付或官方账单</span></div></article><article class="stat"><div class="stat-top">平均请求耗时 <span class="stat-icon">${icon('clock')}</span></div><div class="stat-value">${s.latency_ms>=1000?(s.latency_ms/1000).toFixed(2):Math.round(s.latency_ms)}<span class="unit">${s.latency_ms>=1000?'s':'ms'}</span></div><div class="stat-foot"><span>完整响应 · 非首 Token 延迟</span></div></article></section>
 <div class="overview-grid"><div class="stack"><section class="card"><div class="card-head"><div><h2>请求趋势</h2><p class="card-sub">24 个时间桶 · 包含重试尝试与本地演示</p></div><div class="chart-legend"><span class="flex" style="gap:6px"><span class="legend-key"></span>请求量</span>${badge('LIVE')}</div></div>${chart(d.series)}<div class="chart-summary"><div><strong>${number(s.success)}</strong><small>成功尝试</small></div><div><strong>${number(s.errors)}</strong><small>失败 / 待确认</small></div><div><strong>${number(s.demo_requests)}</strong><small>本地演示</small></div><div><strong>${d.runtime.active}</strong><small>当前并发</small></div></div></section><section class="card"><div class="card-head"><div><h2>最近请求</h2><p class="card-sub">仅记录路由、耗时和用量元数据</p></div><a class="action-link" href="#requests">查看全部 ${icon('arrow')}</a></div>${requestRows(d.recent,true)}</section></div><div class="stack">${attentionCard()}<section class="card"><div class="card-head"><div><h2>本地预算水位</h2><p class="card-sub">包含在途预留 · 不是上游剩余额度</p></div><a class="icon-btn" href="#usage" aria-label="查看预算">${icon('arrow')}</a></div><div class="card-body">${pickQuotas(d.quotas).map(q=>`<div class="quota-item"><div class="quota-name"><span class="ellipsis">${E(q.name||q.id)}</span><span class="tiny muted mono">${money(q.used_30d)}</span></div>${quotaBars(q)}</div>`).join('')||`<div class="small muted" style="padding:12px 0 25px">添加模型并确认计价后，可以设置滚动安全预算。</div>`}<div class="quota-note">5 小时 / 7 天 / 30 天滚动统计。上游账单与重置时间以供应商为准。</div></div></section></div></div>`;

}

// 总览「本地预算水位」优先展示有信息量的模型：已启用且有用量或设了限额的，
// 没有的话退而展示已启用模型，再退而展示原始顺序，不让新装的空模型占位。
function pickQuotas(quotas){
  const enabledIds=new Set(state.config.models.filter(m=>m.enabled).map(m=>m.id));
  const active=q=>q.used_5h>0||q.used_7d>0||q.used_30d>0||q.limit_5h>0||q.limit_7d>0||q.limit_30d>0;
  const relevant=quotas.filter(q=>enabledIds.has(q.id)&&active(q));
  if(relevant.length)return relevant.slice(0,4);
  const enabled=quotas.filter(q=>enabledIds.has(q.id));
  return (enabled.length?enabled:quotas).slice(0,4);
}

// 上游额度：只有少数供应商提供按 Key 可查的接口，不支持的直接写明原因，
// 避免和总览里的本地估算预算混为一谈。
function providerUsageBlock(p){
  if(p.kind==='mock')return '';
  const u=state.providerUsage?.[p.id];
  const wrap=(t,extra='')=>`<div class="provider-usage"><div class="provider-usage-row"><span class="tiny muted">上游额度</span>${t}</div>${extra}</div>`;
  if(!u)return wrap('<span class="tiny muted">查询中…</span>');
  // 额度接口查不到时，用上游最近一次在响应头里声明的限额兜底
  const limits=(u.limits||[]).flatMap(x=>Object.entries(x.limits).map(([k,v])=>`<span class="tiny muted" title="${E(modelName(x.model))}">${E(k)} <b class="mono">${E(v)}</b></span>`)).slice(0,4).join('');
  const fallback=limits?`<div class="provider-usage-fields">${limits}</div>`:'';
  if(!u.supported)return wrap(`<span class="tiny muted ellipsis" title="${E(u.note)}">${E(u.note)}</span>`,fallback);
  if(u.error)return wrap(`<span class="tiny negative ellipsis" title="${E(u.error)}">${E(u.error)}</span>`,fallback);
  const fields=(u.fields||[]).map(f=>`<span class="tiny muted">${E(f.label)} <b class="mono">${E(f.value)}</b></span>`).join('');
  // 等宽字体只给纯数值用，中文混进去字距会被撑开
  const h=u.headline||'—',mono=/^[\d$¥%.,\s/+-]+$/.test(h)?' class="mono"':'';
  return wrap(`<strong${mono}>${E(h)}</strong>`,fields?`<div class="provider-usage-fields">${fields}</div>`:'');
}

export function providers(){
  const ps=state.config.providers;
  return head('PROVIDERS','把你的模型，连接进来。','凭证加密保存在 SQLite；只有网关会向上游发送真实 API Key。拖动卡片调整顺序。',btn('添加供应商','add-provider','plus','','primary'))+`<div class="provider-grid">${ps.map((p,i)=>{const models=state.config.models.filter(m=>m.provider_id===p.id);return `<article class="card provider-card" draggable="true" data-drag-index="${i}" data-drag-kind="provider"><div class="provider-card-head">${avatar(p)}<div style="min-width:0"><h3 class="ellipsis">${E(p.name)}</h3><span class="tiny muted mono">${E(p.kind)}</span></div>${tag(p.enabled?'已启用':'已停用',p.enabled?'':'neutral')}</div><p class="small sub">${E(p.description||'独立凭证、协议和网络边界。')}</p><div class="provider-url">${E(p.kind==='mock'?'LOCAL ONLY · NO CLOUD TRAFFIC':p.base_url)}</div><div class="provider-meta"><div><strong>${models.length} <span class="tiny muted">models</span></strong><small>${models.filter(m=>m.enabled).length} 个已启用</small></div><div><strong>${p.kind==='mock'?'本地演示':p.has_key?'已加密':'无凭证'}</strong><small>${p.kind==='mock'?'非真实推理':`鉴权方式 ${E(p.auth)}`}</small></div></div>${providerUsageBlock(p)}<div class="provider-buttons"><button class="switch ${p.enabled?'on':''}" role="switch" aria-checked="${p.enabled}" aria-label="启用供应商 ${E(p.name)}" data-action="toggle-provider" data-id="${E(p.id)}"></button>${btn('编辑','edit-provider','edit',`data-id="${E(p.id)}"`,'small')}${btn('测试连接','test-provider','bolt',`data-id="${E(p.id)}"`,'small')}${p.kind!=='mock'?btn('同步模型','sync-provider','refresh',`data-id="${E(p.id)}"`,'small'):''}<button class="icon-btn" data-action="delete-provider" data-id="${E(p.id)}" aria-label="删除供应商">${icon('trash')}</button></div></article>`;}).join('')}<button class="add-card" data-action="add-provider">${icon('plus')}<span>连接新的供应商</span><small class="tiny">OpenCode / Command Code / Z.AI / DeepSeek / OpenAI / Anthropic / 自定义</small></button></div><div class="banner" style="margin-top:22px">${icon('shield')}<span>默认禁止访问私有网络。连接 Ollama、内网 vLLM 等服务时，需在供应商设置中明确授权。</span></div>`;

}

// 每模型安全预算的相关模型：已启用，或者有用量/限额历史——632 个模型全列出来
// 大多是从没调用过的空行，找不到重点。
function relevantQuotas(quotas){
  const enabledIds=new Set(state.config.models.filter(m=>m.enabled).map(m=>m.id));
  const active=q=>q.used_5h>0||q.used_7d>0||q.used_30d>0||q.limit_5h>0||q.limit_7d>0||q.limit_30d>0;
  return quotas.filter(q=>enabledIds.has(q.id)||active(q));
}

export function usage(){
  const d=state.data;
  const list=state.usageShowAll?d.quotas:relevantQuotas(d.quotas);
  const hiddenCount=d.quotas.length-list.length;
  return head('BUDGET & USAGE','看清消耗，不盲目刷量。','本地滚动窗口用于安全预算；不能代表供应商的账单周期、剩余额度或实际重置时间。',rangeTools())+`<div class="banner warning">${icon('info')}<span>预算包含在途预留和异常请求的保守预留。计费价格为手动配置；分层价格、活动和额外工具费用需要自行核对。</span></div><section class="card"><div class="card-head usage-card-head"><div><h2>每模型安全预算</h2><p class="card-sub">5 小时 / 7 天 / 30 天滚动窗口 · 0 表示本地不设金额上限</p></div><div class="flex usage-head-tools">${d.quotas.length?btn(state.usageShowAll?'只看相关':`显示全部 ${d.quotas.length} 个`,'toggle-usage-all','','','small'):''}${badge('LOCAL ESTIMATE')}</div></div>${list.length?`<div class="table-scroll"><table><thead><tr><th>模型</th><th>5 小时 · 已用 / 限额</th><th>7 天 · 已用 / 限额</th><th>30 天 · 已用 / 限额</th><th>计价</th><th></th></tr></thead><tbody>${list.map(q=>`<tr><td><div class="cell-title">${E(q.name||q.id)}</div><div style="width:165px;margin-top:10px">${quotaBars(q)}</div></td>${['5h','7d','30d'].map(k=>`<td class="mono">${money(q['used_'+k])} <span class="muted">/ ${q['limit_'+k]?money(q['limit_'+k]):'不限'}</span></td>`).join('')}<td>${q.pricing_set?tag('已配置'):tag('需确认','warning')}</td><td>${btn('编辑预算','edit-model','settings',`data-id="${E(q.id)}"`,'small')}</td></tr>`).join('')}</tbody></table></div>${!state.usageShowAll&&hiddenCount>0?`<div class="usage-hidden-note small muted">已隐藏 ${hiddenCount} 个未启用且无用量的模型 · ${btn('显示全部','toggle-usage-all','','','ghost small')}</div>`:''}`:empty('还没有模型预算','添加模型、确认 token 单价后，再设置本地安全预算。','add-model','添加模型','usage')}</section><section class="card" style="margin-top:22px"><div class="card-head"><div><h2>模型用量分布</h2><p class="card-sub">所选范围内的实际尝试记录，含演示流量；估算金额不等于实付费用。</p></div></div>${d.top_models.length?`<div class="table-scroll"><table><thead><tr><th>模型</th><th>请求数</th><th>Tokens</th><th>估算 / 预留价值</th><th>平均耗时</th></tr></thead><tbody>${d.top_models.map(m=>`<tr><td class="cell-title">${E(modelName(m.model_id))}</td><td class="mono">${number(m.requests)}</td><td class="mono">${compact(m.tokens)}</td><td class="mono">${money(m.cost_nano/1e9)}</td><td class="mono">${ms(m.latency_ms)}</td></tr>`).join('')}</tbody></table></div>`:empty('暂无用量','成功、失败与在途请求都会留下元数据记录。','','','usage')}</section>`;

}

export function requests(){
  const d=state.data;
  return head('REQUEST EXPLORER','每一次调用，都有迹可循。','查看真实选模、协议转换、用量与调用来源。日志记录来源 IP 与客户端标识，不保存 Prompt、模型回答和 API Key。',btn('刷新','refresh','refresh'))+`<form id="request-filter" class="toolbar"><div class="search-field">${icon('search')}<input name="q" placeholder="搜索模型、请求 ID 或来源 IP" value="${E(state.requestQ)}" aria-label="搜索请求"></div><select name="status" aria-label="筛选请求状态"><option value="">全部状态</option>${[['success','成功'],['error','失败'],['unknown','待确认'],['running','进行中']].map(([v,l])=>`<option value="${v}" ${state.requestStatus===v?'selected':''}>${l}</option>`).join('')}</select><select name="provider" aria-label="筛选供应商"><option value="">全部供应商</option>${state.config.providers.map(p=>`<option value="${E(p.id)}" ${state.requestProvider===p.id?'selected':''}>${E(p.name||p.id)}</option>`).join('')}</select><button class="btn" type="submit">筛选</button><span class="small muted" style="margin-left:auto">${number(d.total)} 条尝试记录</span></form><section class="card">${requestRows(d.items)}<div class="list-foot"><span>第 ${d.page} 页 · 每页 ${d.page_size} 条</span><div class="flex">${btn('上一页','request-prev','',d.page<=1?'disabled':'','small')}${btn('下一页','request-next','',d.page*d.page_size>=d.total?'disabled':'','small')}</div></div></section>`;

}

export function sessions(){
  const d=state.data;
  const counts={};
  d.forEach(x=>{counts[x.model_id]=(counts[x.model_id]||0)+1;});
  const modelIds=Object.keys(counts).sort((a,b)=>counts[b]-counts[a]);
  const filtered=state.sessionModel?d.filter(x=>x.model_id===state.sessionModel):d;
  const filterBar=modelIds.length?`<div class="toolbar"><select id="session-model" aria-label="按绑定模型筛选"><option value="">全部绑定模型（${d.length}）</option>${modelIds.map(id=>`<option value="${E(id)}" ${state.sessionModel===id?'selected':''}>${E(modelName(id))}（${counts[id]}）</option>`).join('')}</select>${state.sessionModel?btn(`解除该模型的全部 ${counts[state.sessionModel]} 个绑定`,'unbind-session-model','link',`data-id="${E(state.sessionModel)}"`,'danger small'):''}</div>`:'';
  return head('SESSION AFFINITY','让同一段对话，保持连续。','只保存匿名会话映射。稳定 session 有助于上游亲和路由，但不保证缓存命中或永久记忆。',btn('刷新','refresh','refresh'))+filterBar+`<section class="card">${filtered.length?`<div class="table-scroll"><table><thead><tr><th>会话 ID（匿名）</th><th>绑定模型</th><th>供应商</th><th>成功请求</th><th>最近活动</th><th></th></tr></thead><tbody>${filtered.map(x=>`<tr><td class="mono tiny">${E(x.id.slice(0,25))}…</td><td><span class="cell-title flex" style="gap:6px">${E(modelName(x.model_id))}${sourceTag(x.provider_id)}</span></td><td>${E(getProvider(x.provider_id)?.name||x.provider_id)}</td><td class="mono">${number(x.requests)}</td><td class="mono tiny">${dateTime(x.updated_at)}</td><td>${btn('解除绑定','unbind-session','link',`data-id="${E(x.id)}"`,'small')}</td></tr>`).join('')}</tbody></table></div>`:empty('暂无会话绑定','请求中保留 x-opencode-session 或发送 X-Prism-Session，后续请求使用相同值。','','','session')}</section><div class="banner" style="margin-top:20px">${icon('info')}<span>解除绑定仅删除本地路由亲和记录；不会删除或重置上游的对话、缓存、额度。</span></div>`;

}

export function jobs(){
  const d=state.data;
  return head('BACKGROUND JOBS','耗时工作，有序进行。','模型同步在数据库事务外执行网络请求；进程中断的任务不会静默重放。',btn('刷新','refresh','refresh'))+`<section class="card">${d.length?`<div class="table-scroll"><table><thead><tr><th>任务</th><th>状态</th><th>结果</th><th>创建时间</th></tr></thead><tbody>${d.map(j=>`<tr><td><div class="cell-title mono">${E(j.action)}</div><div class="cell-sub mono">${E(j.id.slice(0,22))}…</div></td><td>${pillStatus(j.status)}</td><td style="white-space:normal;max-width:400px">${E(j.error||j.result||'等待执行结果')}</td><td class="mono tiny">${dateTime(j.created_at)}</td></tr>`).join('')}</tbody></table></div>`:empty('任务队列很安静','在供应商页面点击“同步模型”，任务将在这里显示。','','','job')}</section>`;

}

export function keys(){
  const d=state.data;
  return head('ACCESS KEYS','访问有边界，密钥有归属。','客户端使用网关 Key；上游 Key 不会下发给客户端。完整网关 Key 仅在创建时显示一次。',btn('创建 API Key','new-key','plus','','primary'))+`<section class="card">${d.length?`<div class="table-scroll"><table><thead><tr><th>名称 / 前缀</th><th>权限范围</th><th>状态</th><th>最近使用</th><th>创建时间</th><th></th></tr></thead><tbody>${d.map(k=>{let allowed=[];try{allowed=JSON.parse(k.allowed)}catch{};return `<tr><td><div class="cell-title">${E(k.name)}</div><div class="cell-sub mono">${E(k.prefix)}…</div></td><td>${allowed.length?allowed.slice(0,3).map(badge).join(' ')+(allowed.length>3?' …':''):tag('全部模型 / 路由','neutral')}</td><td>${k.enabled?tag('有效'):tag('已撤销','neutral')}</td><td class="mono tiny">${dateTime(k.last_used)}</td><td class="mono tiny">${dateTime(k.created_at)}</td><td>${k.enabled?btn('撤销','revoke-key','',`data-id="${E(k.id)}"`,'danger small'):''}</td></tr>`;}).join('')}</tbody></table></div>`:empty('创建第一个客户端密钥','建议按客户端或项目分别创建 Key，方便追踪、控制与单独撤销。','new-key','创建 API Key','key')}</section>${connectionGuide()}`;

}

export function connectionGuide(){
  const base=location.origin;
  return `<div class="connection-guide"><section class="card guide"><div class="between"><h3>OpenAI-compatible</h3>${badge('DATA PLANE')}</div><pre>Base URL: ${E(base)}/openai/v1
Authorization: Bearer prism_sk_…

POST /chat/completions
POST /responses
GET  /models</pre><p>Chat / Responses 原生转发或显式子集转换。服务端状态、内建工具和私有推理字段优先使用同协议模型。</p></section><section class="card guide"><div class="between"><h3>Anthropic-compatible</h3>${badge('DATA PLANE')}</div><pre>Base URL: ${E(base)}/anthropic
x-api-key: prism_sk_…
anthropic-version: 2023-06-01

POST /v1/messages
POST /v1/messages/count_tokens</pre><p>token counting 没有原生接口时会明确标注 estimated；可以在系统设置中禁止本地估算。</p></section><section class="card guide"><div class="between"><h3>TypeSafe System One</h3>${badge('DATA PLANE')}</div><pre>Base URL: ${E(base)}/typesafe/v1
Authorization: Bearer prism_sk_…

POST /systemone</pre><p>返回类型化决策与概率，不产生文本，也没有流式。它与三种对话协议之间不做转换：两边没有等价语义，转换只能靠编造内容。</p></section></div>`;

}

export function playground(){
  const options=[...state.config.routes.filter(r=>r.enabled).map(r=>[r.id,r.name||r.id]),...state.config.models.filter(m=>m.enabled).map(m=>[m.id,m.name||m.id]),...state.config.aliases.filter(a=>a.enabled).map(a=>[a.id,a.id])];
  const result=state.playResult;
  return head('PROTOCOL PLAYGROUND','从这里，发出第一条请求。','统一 /api.json 发起非流式诊断；标准模型入口另支持实时 SSE。真实供应商请求可能产生费用。',btn('接入指南','guide','code'))+`<div class="playground-grid"><section class="card"><div class="card-head"><div><h2>请求编辑器</h2><p class="card-sub">选择客户端协议，验证路由与最终响应。</p></div>${badge('LIVE REQUEST')}</div><form id="playground-form" class="playground-form"><div class="protocol-tabs">${[['chat','Chat Completions'],['messages','Anthropic'],['responses','Responses'],['systemone','System One']].map(([v,l])=>`<button type="button" class="${state.playProtocol===v?'active':''}" data-action="play-protocol" data-value="${v}">${l}</button>`).join('')}</div>${selectField('模型 / 路由','model',state.lastPlayModel||options.find(x=>x[0]===state.config.settings.default_route)?.[0]||options[0]?.[0],options,'只展示启用项；实际能力仍由所选模型决定。')}${state.playProtocol==='systemone'?systemOneEditor():`<div class="field"><label for="play-prompt">Prompt</label><textarea id="play-prompt" name="prompt" placeholder="例如：用 Go 写一个带超时的 HTTP 请求示例。" required>${E(state.lastPrompt||'请用 Go 写一个并发安全的计数器，并给出单元测试。')}</textarea></div>`}<div class="between"><span class="tiny muted">${state.playProtocol==='systemone'?'单次请求 · 返回类型化决策，不产生文本':'单次请求 · 输出上限 512 tokens'}</span><button type="submit" class="btn primary" ${state.playBusy||!options.length?'disabled':''}>${icon('play')}${state.playBusy?'正在请求…':'发送测试请求'}</button></div></form></section><section class="card"><div class="card-head"><div><h2>响应与诊断</h2><p class="card-sub">请求走真实网关核心，不是界面模拟。</p></div>${result?tag('HTTP '+result.status,result.status>=400?'error':''):badge('READY')}</div><div class="playground-result">${state.playBusy?'<div class="progress-indicator"><i></i><i></i><i></i><span style="margin-left:9px">等待网关与供应商返回…</span></div>':result?renderPlayResult(result):empty('等待你的第一条请求','可以先启用本地 Sandbox。演示回复会明确标注，不冒充真实模型输出。','','','terminal')}</div></section></div>${connectionGuide()}`;

}

// System One 的载荷是状态加一组带类型的问题，和对话协议完全不同，
// 所以调试台给它一套单独的编辑器：手拼这种 JSON 太容易出错。
const QUESTION_TYPES=[['choice','Choice · 选一个'],['score','Score · 分级打分'],['noul','Noul · 是非概率']];

export function defaultQuestions(){
  return [{key:'department',type:'choice',instructions:'这条工单应该交给哪个部门处理',
    criteria:'billing = 账单、扣款与退款\ntechnical = 功能故障与报错\nsales = 询价与合作'}];
}

// 把编辑器里的文本形态转成上游要的 criteria 结构。
// choice 每行 `键 = 描述`；score 每行一个分级，顺序即从低到高；noul 不需要。
function parseCriteria(q){
  const lines=(q.criteria||'').split('\n').map(x=>x.trim()).filter(Boolean);
  if(q.type==='choice'){
    const out={};
    lines.forEach(line=>{
      const i=line.indexOf('=');
      if(i>0)out[line.slice(0,i).trim()]=line.slice(i+1).trim();
      else out[line]=line;
    });
    return out;
  }
  if(q.type==='score')return lines;
  return null;
}

export function buildQuestions(list){
  const out={};
  list.forEach((q,i)=>{
    const key=(q.key||'').trim()||('q'+(i+1));
    const item={type:q.type,instructions:q.instructions||''};
    const c=parseCriteria(q);
    if(c&&(Array.isArray(c)?c.length:Object.keys(c).length))item.criteria=c;
    out[key]=item;
  });
  return out;
}

function questionCard(q,i){
  const hint=q.type==='choice'?'每行一个选项，格式 `键 = 描述`'
    :q.type==='score'?'每行一个分级，从低到高排列':'是非题不需要选项';
  return `<div class="question-card" data-question="${i}">
    <div class="question-head">
      <input class="question-key mono" name="q-key" value="${E(q.key||'')}" placeholder="答案键名" aria-label="答案键名">
      <select name="q-type" aria-label="题型">${QUESTION_TYPES.map(([v,l])=>`<option value="${v}" ${q.type===v?'selected':''}>${l}</option>`).join('')}</select>
      <button type="button" class="icon-btn" data-action="remove-question" data-index="${i}" aria-label="删除这道题">${icon('trash')}</button>
    </div>
    <input name="q-instructions" value="${E(q.instructions||'')}" placeholder="要模型判断什么" aria-label="问题说明">
    ${q.type==='noul'?'':`<textarea name="q-criteria" rows="3" placeholder="${E(hint)}" aria-label="选项或分级">${E(q.criteria||'')}</textarea><small class="tiny muted">${E(hint)}</small>`}
  </div>`;
}

// 概率分布画成条，比一串小数好读；最高的一项用强调色。
function probBars(probs,picked,labels){
  const entries=Object.entries(probs||{}).sort((a,b)=>b[1]-a[1]);
  if(!entries.length)return '';
  return `<div class="prob-list">${entries.map(([k,v])=>`<div class="prob-row ${k===String(picked)?'picked':''}">
    <span class="prob-name ellipsis">${E(labels?.[k]??k)}</span>
    <span class="prob-track"><i style="width:${Math.max(1,Math.round(v*100))}%"></i></span>
    <span class="prob-value mono">${(v*100).toFixed(1)}%</span></div>`).join('')}</div>`;
}

function answerCard(key,a){
  let body='';
  if(a.type==='noul'){
    const pct=Math.round((a.noul||0)*100);
    body=`<div class="answer-headline"><strong class="mono">${pct}%</strong><span class="tiny muted">判定为「是」的概率</span></div>
      <span class="prob-track"><i style="width:${Math.max(1,pct)}%"></i></span>`;
  }else if(a.type==='choice'){
    body=`<div class="answer-headline"><strong>${E(a.choice)}</strong>${a.confidence!=null?badge('置信度 '+(a.confidence*100).toFixed(0)+'%'):''}</div>${probBars(a.probabilities,a.choice)}`;
  }else if(a.type==='score'){
    // legend 是 {级别序号: 文字} 的对象，序号从 0 开始；
    // score 是落在这些级别上的连续值（例如 1.3），不是整数下标。
    const legend=a.legend||{};
    const levels=Object.keys(legend).length;
    const nearest=String(Math.round(Number(a.score)||0));
    const label=legend[nearest];
    body=`<div class="answer-headline"><strong>${label?E(label):E(a.score)}</strong><span class="mono">${E(a.score)}${levels?` · 共 ${levels} 级`:''}</span>${a.confidence!=null?badge('置信度 '+(a.confidence*100).toFixed(0)+'%'):''}</div>${probBars(a.probabilities,nearest,legend)}`;
  }else{
    body=`<pre>${E(json(a))}</pre>`;
  }
  return `<div class="answer-card"><div class="answer-key"><span class="mono">${E(key)}</span>${badge(a.type||'?')}</div>${body}</div>`;
}

export function renderAnswers(o){
  const answers=o.answers||{};
  const keys=Object.keys(answers);
  if(!keys.length)return '';
  return `<div class="answer-list">${keys.map(k=>answerCard(k,answers[k]||{})).join('')}</div>`;
}

function systemOneEditor(){
  const list=state.playQuestions||defaultQuestions();
  return `<div class="field"><label for="play-state">状态（state）</label><textarea id="play-state" name="state" rows="4" placeholder="要评估的内容。可以是一段话、一条工单、一段日志。" required>${E(state.playState||'用户反馈：本月账单被扣了两次款，希望尽快退回多扣的金额。')}</textarea><small>System One 评估这段内容，然后回答下面每一道题。</small></div>
  <div class="question-section"><div class="between" style="margin-bottom:12px"><strong class="small">问题（questions）</strong>${btn('添加问题','add-question','plus','','small')}</div>
  ${list.length?list.map(questionCard).join(''):'<p class="small muted">至少要有一道题。</p>'}</div>`;
}

export function renderPlayResult(r){
  const o=r.response||{

  };
  let text='';
  if(o.choices)text=o.choices[0]?.message?.content||'';
  else if(o.content)text=o.content.map(x=>x.text||'').join('');
  else if(o.output)text=o.output.map(x=>(x.content||[]).map(x=>x.text||'').join('')).join('\n');
  const answers=o.answers?renderAnswers(o):'';
  return `<div class="response-meta">${badge(ms(r.duration_ms))}${badge(headerValue(r.headers,'X-Prism-Model')||'—')}${badge(headerValue(r.headers,'X-Prism-Protocol-Mode')||'—')}${headerValue(r.headers,'X-Prism-Demo')?tag('本地演示','warning'):''}</div>${answers}${text?`<div class="playground-text">${E(text)}</div>`:''}<details ${r.status>=400?'open':''}><summary class="small muted" style="cursor:pointer;margin:16px 0">原始协议响应与诊断</summary><pre>${E(json(r))}</pre></details>`;

}

// 上游限额快照：各家的头名字与单位都不一样，这里原样列出，
// 先看清楚谁给了什么，再决定要不要据此调度。
function upstreamLimits(runtime){
  const models=Object.entries(runtime?.models||{}).filter(([,v])=>v.limits&&Object.keys(v.limits).length);
  if(!models.length)return `<section class="card setting-card" style="margin-top:21px"><h2>上游限额</h2><p class="small muted" style="margin-top:10px">还没有上游在响应头里声明过限额。有真实调用之后，这里会列出各家声明的剩余量与重置时间。</p></section>`;
  return `<section class="card setting-card" style="margin-top:21px"><h2>上游限额</h2><p class="small muted" style="margin-top:10px">最近一次调用时上游声明的限额，不是账户余额。剩余归零且给出重置时间时，该模型会自动冷却到重置时刻。</p><div class="table-scroll" style="margin-top:16px"><table><thead><tr><th>模型</th><th>响应头</th><th>值</th><th>采集时间</th></tr></thead><tbody>${models.flatMap(([id,v])=>Object.entries(v.limits).map(([k,val],i)=>`<tr>${i===0?`<td rowspan="${Object.keys(v.limits).length}" class="cell-title">${E(modelName(id))}${Number(v.cooldown_until)>Date.now()?badge('冷却中'):''}</td>`:''}<td class="mono tiny">${E(k)}</td><td class="mono">${E(val)}</td>${i===0?`<td rowspan="${Object.keys(v.limits).length}" class="mono tiny">${E(dateTimeSec(v.limits_at))}</td>`:''}</tr>`)).join('')}</tbody></table></div></section>`;

}

export function settings(){
  const s=state.config.settings,info=state.data;
  return head('WORKSPACE SETTINGS','安静运行，清晰可控。','所有运行配置持久化到 SQLite，保存即生效；启动参数只负责数据库路径与救援覆盖。',btn('查看审计记录','audit','shield'))+`<form id="settings-form" data-version="${state.config.version}"><section class="card setting-card"><div class="between" style="margin-bottom:9px"><h2>运行设置</h2>${badge('SQLITE CONFIG')}</div>${[
 ['工作空间名称','后台识别名称','app_name',s.app_name,'text',''],['默认路由名称','用于配置指引；标准调用仍须显式传 model','default_route',s.default_route,'text',''],['全局最大并发','达到限制时返回 429；不会无限排队','global_concurrency',s.global_concurrency,'number','min="1" max="512"'],['请求体上限 · MB','限制请求内存占用，范围 1–32MB','max_body_mb',s.max_body_mb,'number','min="1" max="32"'],['记录保留天数','至少 31 天，覆盖 30 天滚动预算','retention_days',s.retention_days,'number','min="31" max="3650"'],['会话亲和保留 · 小时','只保留本地路由映射，不保存提示词','session_ttl_hours',s.session_ttl_hours,'number','min="1" max="720"'],['监听地址','host:port，保存后立即切换。新地址占不住时配置与服务都不变；对外监听需进程带 --allow-remote 启动','listen',s.listen||'127.0.0.1:8080','text','']].map(([label,help,name,value,type,extra])=>`<div class="setting-row"><div><h3>${label}</h3><p>${help}</p></div><input name="${name}" type="${type}" value="${E(value)}" ${extra} aria-label="${label}" required></div>`).join('')}<div class="setting-row"><div><h3>允许估算 Token Count</h3><p>原生计数不可用时返回本地估算，并附 estimated 响应头。</p></div>${check('允许本地估算','allow_estimated_count',s.allow_estimated_count)}</div>${selectRow('日志级别','debug 会额外记录管理错误详情，用于临时排障；详情可能包含刚提交的配置片段，排障完请调回 info。','log_level',s.log_level||'info',['info','debug','warn','error'])}${selectRow('日志格式','text 便于终端阅读，json 便于日志采集器解析。改动立即生效，无需重启。','log_format',s.log_format||'text',['text','json'])}<div class="setting-row"><div><h3>开放 Prometheus 指标</h3><p>开启后 GET /metrics 可用，仍需管理员令牌抓取。指标含模型名、调用量与费用估算，关闭时端点返回 404。</p></div>${check('开放 /metrics','metrics_enabled',s.metrics_enabled)}</div><div class="between" style="padding-top:20px"><span class="tiny muted">配置 v${state.config.version} · 保存后新请求生效</span><button class="btn primary" type="submit">${icon('check')}保存设置</button></div></section></form><section class="card setting-card" style="margin-top:21px"><h2>运行环境</h2><div class="form-grid" style="margin-top:20px">${[['网关版本',info.version],['Go 运行时',info.go],['SQLite 版本',info.sqlite],['运行时长',Math.round(info.uptime_ms/60000)+' 分钟'],['前端资源','embed.FS · 不加载 CDN'],['管理入口','POST /api.json']].map(([k,v])=>`<div><div class="tiny muted">${k}</div><div class="small mono" style="margin-top:6px">${E(v)}</div></div>`).join('')}</div><div class="dialog-note" style="margin-top:22px">备份请先停止进程，再复制整个 data 目录，至少保留 gateway.db 与配套 gateway.db.key。主密钥丢失后，上游凭证无法解密。不要仅复制运行中的 .db 而忽略 WAL。</div></section>${upstreamLimits(info.runtime)}`;

}

// Editors use the same RPC service and optimistic config version as every other client.
export function providerEditor(id=''){

 const old=getProvider(id);
  const p=old||{
    name:'',kind:'opencode',base_url:'https://opencode.ai/zen/go/v1',auth:'auto',enabled:true,allow_private:false,timeout_sec:300,description:''
  };

 const d=showDialog(old?'编辑供应商':'连接新的供应商','设置上游地址与独立凭证；密钥不会返回给浏览器。',`<div class="form-section"><h3>连接信息</h3>${field('显示名称','name',p.name,'text','','required placeholder="例如：OpenCode Go"')}${selectField('供应商类型','kind',p.kind,[['opencode','OpenCode Go'],['commandcode','Command Code'],['zai','Z.AI'],['deepseek','DeepSeek'],['openai','OpenAI'],['anthropic','Anthropic'],['custom','自定义兼容接口'],...(p.kind==='mock'?[['mock','本地演示']]:[])])}${field('API Base URL','base_url',p.base_url,'url','包含上游 /v1 前缀；不要包含 /chat/completions 等操作路径。',p.kind==='mock'?'':'required')}${field('上游 API Key','api_key','','password',p.has_key?'已保存加密凭证。留空保持原密钥；勾选下方选项可清除。':'只会加密持久化，不在日志与配置接口中回显。','autocomplete="new-password" placeholder="sk-…"')}${p.has_key?check('清除现有上游凭证','clear_key',false):''}<div id="admin-key-row" class="${['openai','anthropic'].includes(p.kind)?'':'hidden'}">${field('组织 Admin Key · 可选','admin_key','','password',p.has_admin_key?'已保存加密凭证。留空保持原密钥。只用于查询本月花费，不参与推理转发。':'只用于在供应商卡片上查询本月官方花费，不参与推理转发。','autocomplete="new-password" placeholder="sk-admin-…"')}${p.has_admin_key?check('清除现有 Admin Key','clear_admin_key',false):''}</div>${selectField('鉴权方式','auth',p.auth,[['auto','自动（根据目标协议）'],['bearer','Authorization: Bearer'],['x-api-key','x-api-key'],['none','不使用凭证']])}</div><div class="form-section"><h3>行为与安全</h3>${field('超时 · 秒','timeout_sec',p.timeout_sec,'number','包含完整响应和流式生成时间。','min="5" max="1800" required')}${field('备注','description',p.description)}${check('启用供应商','enabled',p.enabled)}${check('允许私有网络 / 明文 HTTP','allow_private',p.allow_private,'仅连接你信任的本地或内网模型服务时开启。开启后可访问回环与私网 IP。')}<div class="dialog-note">连接测试只调用 GET /models。同步的新模型默认启用，能力取自上游声明；价格与额度需在模型库确认。</div></div>`,async f=>{
  const v={name:val(f,'name').trim(),kind:val(f,'kind'),base_url:val(f,'base_url').trim(),auth:val(f,'auth'),timeout_sec:nval(f,'timeout_sec'),enabled:checked(f,'enabled'),allow_private:checked(f,'allow_private'),description:val(f,'description')};
  if(val(f,'api_key'))v.api_key=val(f,'api_key');else if(checked(f,'clear_key'))v.api_key='';
  if(val(f,'admin_key'))v.admin_key=val(f,'admin_key');else if(checked(f,'clear_admin_key'))v.admin_key='';
  await save('provider.save',{id,provider:v},f);
 });

 $('[name=kind]',d).addEventListener('change',ev=>{const preset={opencode:'https://opencode.ai/zen/go/v1',commandcode:'https://api.commandcode.ai/provider/v1',zai:'https://api.z.ai/api/coding/paas/v4',deepseek:'https://api.deepseek.com',openai:'https://api.openai.com/v1',anthropic:'https://api.anthropic.com/v1'}[ev.target.value];if(preset&&!old)$('[name=base_url]',d).value=preset;$('#admin-key-row',d).classList.toggle('hidden',!['openai','anthropic'].includes(ev.target.value));});

}

export function keyEditor(){
  const opts=[...state.config.routes.map(x=>[x.id,x.name||x.id]),...state.config.models.map(x=>[x.id,x.name||x.id]),...state.config.aliases.map(x=>[x.id,x.id])];
  showDialog('创建客户端 API Key','按项目或客户端独立创建，可随时撤销。',`${field('密钥名称','name','','text','','required placeholder="例如：我的 Claude Code"')}<div class="form-section"><h3>可请求模型 / 路由</h3><p class="small muted" style="margin:10px 0">不选表示允许全部。范围限制针对客户端请求的模型、路由或别名 ID。</p>${opts.map(([v,l])=>`<label class="checkline"><input type="checkbox" name="allowed" value="${E(v)}"><span>${E(l)}<small class="mono">${E(v)}</small></span></label>`).join('')}</div>`,async f=>{const k=await rpc('apikey.create',{name:val(f,'name'),allowed:$$('[name=allowed]:checked',f).map(x=>x.value)});closeDialog();showDialog('密钥已创建','这是唯一一次显示完整密钥。关闭前请安全保存。',`<div class="banner">请不要将此 Key 放进公开仓库、截图或前端源代码。</div><pre class="secret-box">${E(k.key)}</pre>${btn('复制完整密钥','copy-secret','copy','','primary')}${connectionGuide()}`);state.secretValue=k.key;state.data=await rpc('apikey.list');if(state.page==='keys')renderPage(false);},'创建密钥');

}

export function preservePlayground(){
  const f=$('#playground-form');
  if(!f)return;
  state.lastPlayModel=val(f,'model');
  if(state.playProtocol==='systemone'){
    state.playState=val(f,'state');
    // 每次重绘前把编辑器里的题目读回 state，否则改完题型就丢了正在写的内容
    state.playQuestions=$$('.question-card',f).map(card=>({
      key:$('[name=q-key]',card)?.value||'',
      type:$('[name=q-type]',card)?.value||'choice',
      instructions:$('[name=q-instructions]',card)?.value||'',
      criteria:$('[name=q-criteria]',card)?.value||''
    }));
  }else{
    state.lastPrompt=val(f,'prompt');
  }
}

// ===== 总览、额度、会话页的增强 =====

// 把毫秒时间戳写成「今天/明天 HH:mm」，更晚的退回完整日期；比一串数字更看得懂恢复时间。
function untilText(ts){
  const d=new Date(ts),base=new Date();
  const days=Math.round((new Date(d.getFullYear(),d.getMonth(),d.getDate())-new Date(base.getFullYear(),base.getMonth(),base.getDate()))/86400000);
  const hm=d.toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',hour12:false});
  if(days===0)return `今天 ${hm}`;
  if(days===1)return `明天 ${hm}`;
  return dateTime(ts);
}

// 总览「需要关注」：state.providerUsage（异步加载，可能为 null）、state.data.runtime、state.config
// 条目：额度用完的供应商、冷却中的模型（按供应商聚合）、可用候选不足 2 个的已启用路由。
export function attentionItems(){
  const items=[],now=Date.now();
  const runtimeModels=state.data?.runtime?.models||{};
  // 额度用完的供应商
  if(state.providerUsage){
    for(const p of state.config.providers){
      const u=state.providerUsage[p.id];
      if(!u)continue;
      const until=Number(u.exhausted_until)||0;
      if(!(until>now)&&!/已用完/.test(u.headline||''))continue;
      items.push({level:'error',action:'go-providers',
        text:`${p.name||p.id} 额度已用完${until>now?`，${untilText(until)}恢复`:''}`});
    }
  }
  // 冷却中的模型，同一供应商聚合成一条，避免刷屏
  const cooling={};
  for(const[id,v]of Object.entries(runtimeModels)){
    if(!(Number(v.cooldown_until)>now))continue;
    const m=getModel(id);
    if(!m)continue;
    (cooling[m.provider_id]=cooling[m.provider_id]||[]).push({m,until:Number(v.cooldown_until)});
  }
  for(const[pid,list]of Object.entries(cooling)){
    const name=getProvider(pid)?.name||pid,until=Math.max(...list.map(x=>x.until));
    const text=list.length>3
      ?`${name} 的 ${list.length} 个模型冷却中，${untilText(until)}恢复`
      :`${name} 的 ${list.map(x=>x.m.name||x.m.id).join('、')} 冷却中，${untilText(until)}恢复`;
    items.push({level:'warning',action:'go-models',text});
  }
  // 可用候选不足 2 个的已启用路由：模型与供应商都启用、且未冷却才算可用候选
  for(const r of state.config.routes){
    if(!r.enabled)continue;
    const viable=(r.candidates||[]).filter(c=>{
      const m=getModel(c.model_id);
      if(!m?.enabled)return false;
      const p=getProvider(m.provider_id);
      if(!p?.enabled)return false;
      return!(Number(runtimeModels[m.id]?.cooldown_until)>now);
    }).length;
    if(viable<2)items.push({level:viable===0?'error':'warning',action:'go-routes',
      text:`路由「${r.name||r.id}」可用候选仅 ${viable} 个，请求可能失败或无法切换`});
  }
  return items;
}

function attentionCard(){
  const items=attentionItems();
  const body=items.length
    ?`<div class="attention-list">${items.map(it=>`<button type="button" class="attention-item ${it.level}" data-action="${it.action}">${icon(it.level==='error'?'warning':'clock')}<span>${E(it.text)}</span>${icon('arrow')}</button>`).join('')}</div>`
    :`<div class="attention-calm">${icon('check')}<div><strong>一切正常</strong><p class="small muted">供应商额度充足，模型未冷却，路由候选充分。</p></div></div>`;
  return `<section class="card"><div class="card-head"><div><h2>需要关注</h2><p class="card-sub">额度、冷却与路由候选的实时检查</p></div>${icon('shield')}</div><div class="card-body">${body}</div><div class="card-foot between"><span>${items.length?`${items.length} 项待处理`:'常规巡检'}</span><a class="action-link" href="#routes">管理路由 ${icon('arrow')}</a></div></section>`;
}

export const pageActions={
  'toggle-usage-all':()=>{
    state.usageShowAll=!state.usageShowAll;
    renderPage(false);
  },
  'unbind-session-model':async(el,id)=>{
    const n=(state.data||[]).filter(x=>x.model_id===id).length;
    if(!await confirm('解除该模型的全部绑定？',`将解除 ${modelName(id)} 的 ${n} 个会话绑定，不会清除供应商缓存，也不会重置上游额度。`,'解除绑定'))return;
    const r=await rpc('session.delete',{model_id:id});
    state.sessionModel='';
    toast(`已解除 ${r.unbound} 个绑定`);
    await loadPage(false);
  }
};

export function bindPages(){
  $('#session-model')?.addEventListener('change',ev=>{state.sessionModel=ev.target.value;renderPage(false);});
}
