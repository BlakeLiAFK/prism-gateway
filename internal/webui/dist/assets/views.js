// 各页面与编辑抽屉的渲染。
import {rpc} from './api.js';
import {renderPage} from './app.js';
import {$,$$,E,compact,dateTime,json,money,ms,number,state} from './core.js';
import {icon} from './icons.js';
import {avatar,badge,btn,chart,check,checked,closeDialog,copy,empty,field,flowMini,getModel,getProvider,head,headerValue,modelName,nval,pillStatus,quotaBars,rangeTools,save,selectField,selectRow,showDialog,spark,tag,toast,val} from './ui.js';

export function requestRows(items,short=false){
  if(!items?.length)return empty('请求记录会出现在这里','每次真实转发或本地演示都会记录元数据；不保存你的提示词和回答。','go-playground','发起测试','request');
  return `<div class="table-scroll"><table><thead><tr><th>模型 / 请求</th><th>协议</th><th>状态</th><th>耗时</th>${short?'':'<th>Tokens</th><th>估算价值</th>'}<th>时间</th></tr></thead><tbody>${items.map(r=>`<tr data-action="request-detail" data-id="${E(r.id)}"><td><div class="cell-title flex">${E(modelName(r.model_id))}${r.is_demo?badge('DEMO'):''}</div><div class="cell-sub mono">${E(r.parent_id.slice(0,23))}…</div></td><td>${badge(r.protocol)}</td><td>${pillStatus(r.status)}</td><td class="mono">${ms(r.duration_ms)}</td>${short?'':`<td class="mono">${compact(Number(r.input_tokens)+Number(r.output_tokens))}</td><td class="mono">${r.cost_known?money(r.cost_nano/1e9):r.usage_mode==='rejected'?'—':'待确认'}</td>`}<td class="mono tiny">${dateTime(r.started_at)}</td></tr>`).join('')}</tbody></table></div>`;

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
 <div class="overview-grid"><div class="stack"><section class="card"><div class="card-head"><div><h2>请求趋势</h2><p class="card-sub">24 个时间桶 · 包含重试尝试与本地演示</p></div><div class="chart-legend"><span class="flex" style="gap:6px"><span class="legend-key"></span>请求量</span>${badge('LIVE')}</div></div>${chart(d.series)}<div class="chart-summary"><div><strong>${number(s.success)}</strong><small>成功尝试</small></div><div><strong>${number(s.errors)}</strong><small>失败 / 待确认</small></div><div><strong>${number(s.demo_requests)}</strong><small>本地演示</small></div><div><strong>${d.runtime.active}</strong><small>当前并发</small></div></div></section><section class="card"><div class="card-head"><div><h2>最近请求</h2><p class="card-sub">仅记录路由、耗时和用量元数据</p></div><a class="action-link" href="#requests">查看全部 ${icon('arrow')}</a></div>${requestRows(d.recent,true)}</section></div><div class="stack"><section class="card"><div class="card-head"><div><h2>一个入口，多种能力</h2><p class="card-sub">管理面与模型调用面分离</p></div>${icon('route')}</div>${flowMini()}<div class="card-foot between"><span>原生优先 · 显式协议转换</span><a class="action-link" href="#routes">管理路由 ${icon('arrow')}</a></div></section><section class="card"><div class="card-head"><div><h2>本地预算水位</h2><p class="card-sub">包含在途预留 · 不是上游剩余额度</p></div><a class="icon-btn" href="#usage" aria-label="查看预算">${icon('arrow')}</a></div><div class="card-body">${d.quotas.slice(0,4).map(q=>`<div class="quota-item"><div class="quota-name"><span class="ellipsis">${E(q.name||q.id)}</span><span class="tiny muted mono">${money(q.used_30d)}</span></div>${quotaBars(q)}</div>`).join('')||`<div class="small muted" style="padding:12px 0 25px">添加模型并确认计价后，可以设置滚动安全预算。</div>`}<div class="quota-note">5 小时 / 7 天 / 30 天滚动统计。上游账单与重置时间以供应商为准。</div></div></section></div></div>`;

}

export function providers(){
  const ps=state.config.providers;
  return head('PROVIDERS','把你的模型，连接进来。','凭证加密保存在 SQLite；只有网关会向上游发送真实 API Key。',btn('添加供应商','add-provider','plus','','primary'))+`<div class="provider-grid">${ps.map(p=>{const models=state.config.models.filter(m=>m.provider_id===p.id);return `<article class="card provider-card"><div class="provider-card-head">${avatar(p)}<div style="min-width:0"><h3 class="ellipsis">${E(p.name)}</h3><span class="tiny muted mono">${E(p.kind)}</span></div>${tag(p.enabled?'已启用':'已停用',p.enabled?'':'neutral')}</div><p class="small sub">${E(p.description||'独立凭证、协议和网络边界。')}</p><div class="provider-url">${E(p.kind==='mock'?'LOCAL ONLY · NO CLOUD TRAFFIC':p.base_url)}</div><div class="provider-meta"><div><strong>${models.length} <span class="tiny muted">models</span></strong><small>${models.filter(m=>m.enabled).length} 个已启用</small></div><div><strong>${p.kind==='mock'?'本地演示':p.has_key?'已加密':'无凭证'}</strong><small>${p.kind==='mock'?'非真实推理':`鉴权方式 ${E(p.auth)}`}</small></div></div><div class="provider-buttons"><button class="switch ${p.enabled?'on':''}" role="switch" aria-checked="${p.enabled}" aria-label="启用供应商 ${E(p.name)}" data-action="toggle-provider" data-id="${E(p.id)}"></button>${btn('编辑','edit-provider','edit',`data-id="${E(p.id)}"`,'small')}${btn('测试连接','test-provider','bolt',`data-id="${E(p.id)}"`,'small')}${p.kind!=='mock'?btn('同步模型','sync-provider','refresh',`data-id="${E(p.id)}"`,'small'):''}<button class="icon-btn" data-action="delete-provider" data-id="${E(p.id)}" aria-label="删除供应商">${icon('trash')}</button></div></article>`;}).join('')}<button class="add-card" data-action="add-provider">${icon('plus')}<span>连接新的供应商</span><small class="tiny">OpenCode / Command Code / Z.AI / OpenAI / Anthropic / 自定义</small></button></div><div class="banner" style="margin-top:22px">${icon('shield')}<span>默认禁止访问私有网络。连接 Ollama、内网 vLLM 等服务时，需在供应商设置中明确授权。</span></div>`;

}

export function models(){
  let models=state.config.models.filter(m=>(!state.modelQ||(m.id+' '+m.name+' '+m.upstream).toLowerCase().includes(state.modelQ.toLowerCase()))&&(!state.modelProtocol||m.protocol===state.modelProtocol)&&(!state.providerFilter||m.provider_id===state.providerFilter));
  // 选中某个供应商时，直接在这里同步它的模型；否则同步入口只在供应商页面，找起来绕
  const syncable=state.config.providers.find(p=>p.id===state.providerFilter&&p.kind!=='mock');
  return head('MODEL LIBRARY','每个模型，各司其职。','按模型指定上游协议、能力、价格和限额；新同步模型默认禁用，确认后再投入使用。',(syncable?btn(`同步 ${syncable.name} 的模型`,'sync-provider','refresh',`data-id="${E(syncable.id)}"`,'small'):'')+btn('添加模型','add-model','plus','','primary'))+`<div class="toolbar"><div class="search-field">${icon('search')}<input id="model-search" placeholder="搜索模型、ID 或上游名称" value="${E(state.modelQ)}" aria-label="搜索模型"></div><select id="model-protocol" aria-label="协议过滤"><option value="">全部协议</option>${['chat','messages','responses','systemone'].map(v=>`<option value="${v}" ${v===state.modelProtocol?'selected':''}>${v}</option>`).join('')}</select><select id="model-provider" aria-label="供应商过滤"><option value="">全部供应商</option>${state.config.providers.map(p=>`<option value="${E(p.id)}" ${p.id===state.providerFilter?'selected':''}>${E(p.name)}</option>`).join('')}</select><span class="small muted" style="margin-left:auto">${models.length} 个模型</span></div><section class="card">${models.length?`<div class="table-scroll"><table><thead><tr><th>模型 / 上游 ID</th><th>供应商</th><th>原生协议</th><th>能力</th><th>Input / Output · $/M</th><th>并发</th><th>启用</th><th></th></tr></thead><tbody>${models.map(m=>`<tr><td><div class="cell-title">${E(m.name||m.id)}</div><div class="cell-sub mono">${E(m.upstream)}</div></td><td class="small">${E(getProvider(m.provider_id)?.name||m.provider_id)}</td><td>${badge(m.protocol)}</td><td><div class="flex" style="gap:4px">${m.tools?badge('TOOLS'):''}${m.vision?badge('VISION'):''}${badge(compact(m.context_window))}</div></td><td class="mono">${m.pricing_set?`${money(m.input_price)} / ${money(m.output_price)}`:'未确认价格'}</td><td class="mono">${m.concurrency}${m.rpm?` <span class="tiny muted">/ ${m.rpm} RPM</span>`:''}</td><td><button class="switch ${m.enabled?'on':''}" role="switch" aria-checked="${m.enabled}" aria-label="启用 ${E(m.name)}" data-action="toggle-model" data-id="${E(m.id)}"></button></td><td><div class="table-actions"><button class="icon-btn" data-action="edit-model" data-id="${E(m.id)}" aria-label="编辑模型">${icon('edit')}</button><button class="icon-btn" data-action="delete-model" data-id="${E(m.id)}" aria-label="删除模型">${icon('trash')}</button></div></td></tr>`).join('')}</tbody></table></div>`:empty('让模型库准备就绪','先添加供应商，再同步或手动添加模型。供应商模型列表不会自动确认协议能力与计价。','add-model','添加第一个模型')}</section>`;

}

export function routes(){
  const rs=state.config.routes;
  let r=rs.find(r=>r.id===state.routeID)||rs[0];
  if(r)state.routeID=r.id;
  const cand=state.routeDraft||r?.candidates||[];
  return head('ROUTING STUDIO','让请求，找到合适的模型。','按优先级或预算压力选择候选；会话优先保持亲和，不会在半截流中切换模型。',btn('创建路由','add-route','plus','','primary'))+(r?`<div class="route-layout"><div class="route-list"><p class="tiny muted" style="padding:0 2px 4px">拖动卡片调整顺序</p>${rs.map((x,i)=>`<button class="route-select ${x.id===r.id?'active':''}" draggable="true" data-drag-index="${i}" data-drag-kind="route" data-action="select-route" data-id="${E(x.id)}"><div class="between"><strong>${E(x.name||x.id)}</strong><span class="dot ${x.enabled?'positive':'muted'}"></span></div><p class="mono">${E(x.id)}</p><p>${x.candidates.length} 个候选 · ${x.strategy==='balanced'?'预算均衡':'优先级'}</p></button>`).join('')}</div><section class="card"><div class="card-head"><div><h2>${E(r.name||r.id)}</h2><p class="card-sub">${E(r.description||'拖动候选调整优先级，或用上下箭头操作。')}</p></div><div class="flex">${state.routeDraft?btn('应用排序','save-route-order','check','','mint small'):''}<button class="switch ${r.enabled?'on':''}" role="switch" aria-checked="${r.enabled}" aria-label="启用路由 ${E(r.name||r.id)}" data-action="toggle-route" data-id="${E(r.id)}"></button>${btn('编辑','edit-route','settings',`data-id="${E(r.id)}"`,'small')}<button class="icon-btn" data-action="delete-route" data-id="${E(r.id)}" aria-label="删除路由">${icon('trash')}</button></div></div><div class="route-canvas"><div class="router-node">${icon('prism')}<strong class="mono">${E(r.id)}</strong><small>CLIENT REQUEST</small>${badge(r.strategy)}</div><div class="route-connector"></div><div class="route-candidates">${cand.map((x,i)=>{const m=getModel(x.model_id);return `<div class="candidate" draggable="true" data-drag-index="${i}" data-drag-kind="candidate">${icon('drag','drag-handle')}<span class="candidate-order">${String(i+1).padStart(2,'0')}</span><div class="candidate-info"><strong class="ellipsis">${E(m?.name||x.model_id)}</strong><small>${E(getProvider(m?.provider_id)?.name||'')} · weight ${x.weight}</small></div>${badge(m?.protocol||'?')}<div><button class="icon-btn" data-action="candidate-up" data-index="${i}" ${i===0?'disabled':''} aria-label="上移">${icon('up')}</button><button class="icon-btn" data-action="candidate-down" data-index="${i}" ${i===cand.length-1?'disabled':''} aria-label="下移">${icon('down')}</button></div></div>`;}).join('')||'<div class="small muted">没有候选，请编辑路由添加模型。</div>'}</div></div><div class="route-properties"><div><small>会话亲和</small><strong>${r.affinity?'优先保持同一模型':'已关闭'}</strong></div><div><small>重试边界</small><strong>安全请求 + 上游 429 / 503</strong></div><div><small>流开始之后</small><strong>禁止中途切模型</strong></div><div style="margin-left:auto">${btn('模拟选择','simulate-route','play',`data-id="${E(r.id)}"`,'small')}</div></div>${state.simulation?`<div class="simulation"><h3>路由模拟结果</h3><p class="tiny muted" style="margin:6px 0 14px">${E(state.simulation.note)}</p>${state.simulation.ranked.map((x,i)=>`<div class="sim-line"><span class="flex">${badge(String(i+1).padStart(2,'0'))}<b>${E(modelName(x.model_id))}</b>${badge(x.protocol)}</span><span class="mono">${x.score.toFixed(1)}</span></div>`).join('')||'<p class="small negative">没有兼容候选。检查下方原因。</p>'}<details style="margin-top:14px"><summary class="small muted">查看候选判定依据</summary><pre style="margin-top:10px">${E(json(state.simulation.checks))}</pre></details></div>`:''}</section></div>`:empty('创建你的第一个路由','把多个模型组成一个稳定的客户端入口，例如 auto-coding。候选只在本账户的授权范围内使用。','add-route','创建路由','route'))+`<section class="card aliases-grid"><div class="card-head"><div><h2>模型别名</h2><p class="card-sub">客户端名称映射到真实模型或路由；实际解析结果记录在请求元数据中。</p></div>${btn('添加别名','add-alias','plus','','small')}</div>${state.config.aliases.length?`<div class="table-scroll"><table><thead><tr><th>客户端请求名称</th><th>指向目标</th><th>状态</th><th></th></tr></thead><tbody>${state.config.aliases.map(a=>`<tr><td class="mono">${E(a.id)}</td><td class="mono">${E(a.target)}</td><td>${tag(a.enabled?'已启用':'停用',a.enabled?'':'neutral')}</td><td><div class="table-actions"><button class="icon-btn" data-action="delete-alias" data-id="${E(a.id)}" aria-label="删除别名">${icon('trash')}</button></div></td></tr>`).join('')}</tbody></table></div>`:empty('别名不是必需项','你可以直接使用模型 ID 或路由 ID，也可以在这里增加一个更好记的名字。','','','link')}</section>`;

}

export function usage(){
  const d=state.data;
  return head('BUDGET & USAGE','看清消耗，不盲目刷量。','本地滚动窗口用于安全预算；不能代表供应商的账单周期、剩余额度或实际重置时间。',rangeTools())+`<div class="banner warning">${icon('info')}<span>预算包含在途预留和异常请求的保守预留。计费价格为手动配置；分层价格、活动和额外工具费用需要自行核对。</span></div><section class="card"><div class="card-head"><div><h2>每模型安全预算</h2><p class="card-sub">5 小时 / 7 天 / 30 天滚动窗口 · 0 表示本地不设金额上限</p></div>${badge('LOCAL ESTIMATE')}</div>${d.quotas.length?`<div class="table-scroll"><table><thead><tr><th>模型</th><th>5 小时 · 已用 / 限额</th><th>7 天 · 已用 / 限额</th><th>30 天 · 已用 / 限额</th><th>计价</th><th></th></tr></thead><tbody>${d.quotas.map(q=>`<tr><td><div class="cell-title">${E(q.name||q.id)}</div><div style="width:165px;margin-top:10px">${quotaBars(q)}</div></td>${['5h','7d','30d'].map(k=>`<td class="mono">${money(q['used_'+k])} <span class="muted">/ ${q['limit_'+k]?money(q['limit_'+k]):'不限'}</span></td>`).join('')}<td>${q.pricing_set?tag('已配置'):tag('需确认','warning')}</td><td>${btn('编辑预算','edit-model','settings',`data-id="${E(q.id)}"`,'small')}</td></tr>`).join('')}</tbody></table></div>`:empty('还没有模型预算','添加模型、确认 token 单价后，再设置本地安全预算。','add-model','添加模型','usage')}</section><section class="card" style="margin-top:22px"><div class="card-head"><div><h2>模型用量分布</h2><p class="card-sub">所选范围内的实际尝试记录，含演示流量；估算金额不等于实付费用。</p></div></div>${d.top_models.length?`<div class="table-scroll"><table><thead><tr><th>模型</th><th>请求数</th><th>Tokens</th><th>估算 / 预留价值</th><th>平均耗时</th></tr></thead><tbody>${d.top_models.map(m=>`<tr><td class="cell-title">${E(modelName(m.model_id))}</td><td class="mono">${number(m.requests)}</td><td class="mono">${compact(m.tokens)}</td><td class="mono">${money(m.cost_nano/1e9)}</td><td class="mono">${ms(m.latency_ms)}</td></tr>`).join('')}</tbody></table></div>`:empty('暂无用量','成功、失败与在途请求都会留下元数据记录。','','','usage')}</section>`;

}

export function requests(){
  const d=state.data;
  return head('REQUEST EXPLORER','每一次调用，都有迹可循。','查看真实选模、协议转换、状态与耗时。日志不保存 Prompt、模型回答和 API Key。',btn('刷新','refresh','refresh'))+`<form id="request-filter" class="toolbar"><div class="search-field">${icon('search')}<input name="q" placeholder="搜索模型或请求 ID" value="${E(state.requestQ)}" aria-label="搜索请求"></div><select name="status" aria-label="筛选请求状态"><option value="">全部状态</option>${[['success','成功'],['error','失败'],['unknown','待确认'],['running','进行中']].map(([v,l])=>`<option value="${v}" ${state.requestStatus===v?'selected':''}>${l}</option>`).join('')}</select><button class="btn" type="submit">筛选</button><span class="small muted" style="margin-left:auto">${number(d.total)} 条尝试记录</span></form><section class="card">${requestRows(d.items)}<div class="list-foot"><span>第 ${d.page} 页 · 每页 ${d.page_size} 条</span><div class="flex">${btn('上一页','request-prev','',d.page<=1?'disabled':'','small')}${btn('下一页','request-next','',d.page*d.page_size>=d.total?'disabled':'','small')}</div></div></section>`;

}

export function sessions(){
  const d=state.data;
  return head('SESSION AFFINITY','让同一段对话，保持连续。','只保存匿名会话映射。稳定 session 有助于上游亲和路由，但不保证缓存命中或永久记忆。',btn('刷新','refresh','refresh'))+`<section class="card">${d.length?`<div class="table-scroll"><table><thead><tr><th>会话 ID（匿名）</th><th>绑定模型</th><th>供应商</th><th>成功请求</th><th>最近活动</th><th></th></tr></thead><tbody>${d.map(x=>`<tr><td class="mono tiny">${E(x.id.slice(0,25))}…</td><td class="cell-title">${E(modelName(x.model_id))}</td><td>${E(getProvider(x.provider_id)?.name||x.provider_id)}</td><td class="mono">${number(x.requests)}</td><td class="mono tiny">${dateTime(x.updated_at)}</td><td>${btn('解除绑定','unbind-session','link',`data-id="${E(x.id)}"`,'small')}</td></tr>`).join('')}</tbody></table></div>`:empty('暂无会话绑定','请求中保留 x-opencode-session 或发送 X-Prism-Session，后续请求使用相同值。','','','session')}</section><div class="banner" style="margin-top:20px">${icon('info')}<span>解除绑定仅删除本地路由亲和记录；不会删除或重置上游的对话、缓存、额度。</span></div>`;

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

export function settings(){
  const s=state.config.settings,info=state.data;
  return head('WORKSPACE SETTINGS','安静运行，清晰可控。','所有运行配置持久化到 SQLite，保存即生效；启动参数只负责数据库路径与救援覆盖。',btn('查看审计记录','audit','shield'))+`<form id="settings-form" data-version="${state.config.version}"><section class="card setting-card"><div class="between" style="margin-bottom:9px"><h2>运行设置</h2>${badge('SQLITE CONFIG')}</div>${[
 ['工作空间名称','后台识别名称','app_name',s.app_name,'text',''],['默认路由名称','用于配置指引；标准调用仍须显式传 model','default_route',s.default_route,'text',''],['全局最大并发','达到限制时返回 429；不会无限排队','global_concurrency',s.global_concurrency,'number','min="1" max="512"'],['请求体上限 · MB','限制请求内存占用，范围 1–32MB','max_body_mb',s.max_body_mb,'number','min="1" max="32"'],['记录保留天数','至少 31 天，覆盖 30 天滚动预算','retention_days',s.retention_days,'number','min="31" max="3650"'],['会话亲和保留 · 小时','只保留本地路由映射，不保存提示词','session_ttl_hours',s.session_ttl_hours,'number','min="1" max="720"'],['监听地址','host:port，保存后立即切换。新地址占不住时配置与服务都不变；对外监听需进程带 --allow-remote 启动','listen',s.listen||'127.0.0.1:8080','text','']].map(([label,help,name,value,type,extra])=>`<div class="setting-row"><div><h3>${label}</h3><p>${help}</p></div><input name="${name}" type="${type}" value="${E(value)}" ${extra} aria-label="${label}" required></div>`).join('')}<div class="setting-row"><div><h3>允许估算 Token Count</h3><p>原生计数不可用时返回本地估算，并附 estimated 响应头。</p></div>${check('允许本地估算','allow_estimated_count',s.allow_estimated_count)}</div>${selectRow('日志级别','debug 会额外记录管理错误详情，用于临时排障；详情可能包含刚提交的配置片段，排障完请调回 info。','log_level',s.log_level||'info',['info','debug','warn','error'])}${selectRow('日志格式','text 便于终端阅读，json 便于日志采集器解析。改动立即生效，无需重启。','log_format',s.log_format||'text',['text','json'])}<div class="setting-row"><div><h3>开放 Prometheus 指标</h3><p>开启后 GET /metrics 可用，仍需管理员令牌抓取。指标含模型名、调用量与费用估算，关闭时端点返回 404。</p></div>${check('开放 /metrics','metrics_enabled',s.metrics_enabled)}</div><div class="between" style="padding-top:20px"><span class="tiny muted">配置 v${state.config.version} · 保存后新请求生效</span><button class="btn primary" type="submit">${icon('check')}保存设置</button></div></section></form><section class="card setting-card" style="margin-top:21px"><h2>运行环境</h2><div class="form-grid" style="margin-top:20px">${[['网关版本',info.version],['Go 运行时',info.go],['SQLite 版本',info.sqlite],['运行时长',Math.round(info.uptime_ms/60000)+' 分钟'],['前端资源','embed.FS · 不加载 CDN'],['管理入口','POST /api.json']].map(([k,v])=>`<div><div class="tiny muted">${k}</div><div class="small mono" style="margin-top:6px">${E(v)}</div></div>`).join('')}</div><div class="dialog-note" style="margin-top:22px">备份请先停止进程，再复制整个 data 目录，至少保留 gateway.db 与配套 gateway.db.key。主密钥丢失后，上游凭证无法解密。不要仅复制运行中的 .db 而忽略 WAL。</div></section>`;

}

// Editors use the same RPC service and optimistic config version as every other client.
export function providerEditor(id=''){

 const old=getProvider(id);
  const p=old||{
    name:'',kind:'opencode',base_url:'https://opencode.ai/zen/go/v1',auth:'auto',enabled:true,allow_private:false,timeout_sec:300,description:''
  };

 const d=showDialog(old?'编辑供应商':'连接新的供应商','设置上游地址与独立凭证；密钥不会返回给浏览器。',`<div class="form-section"><h3>连接信息</h3>${field('显示名称','name',p.name,'text','','required placeholder="例如：OpenCode Go"')}${selectField('供应商类型','kind',p.kind,[['opencode','OpenCode Go'],['commandcode','Command Code'],['zai','Z.AI'],['openai','OpenAI'],['anthropic','Anthropic'],['custom','自定义兼容接口'],...(p.kind==='mock'?[['mock','本地演示']]:[])])}${field('API Base URL','base_url',p.base_url,'url','包含上游 /v1 前缀；不要包含 /chat/completions 等操作路径。',p.kind==='mock'?'':'required')}${field('上游 API Key','api_key','','password',p.has_key?'已保存加密凭证。留空保持原密钥；勾选下方选项可清除。':'只会加密持久化，不在日志与配置接口中回显。','autocomplete="new-password" placeholder="sk-…"')}${p.has_key?check('清除现有上游凭证','clear_key',false):''}${selectField('鉴权方式','auth',p.auth,[['auto','自动（根据目标协议）'],['bearer','Authorization: Bearer'],['x-api-key','x-api-key'],['none','不使用凭证']])}</div><div class="form-section"><h3>行为与安全</h3>${field('超时 · 秒','timeout_sec',p.timeout_sec,'number','包含完整响应和流式生成时间。','min="5" max="1800" required')}${field('备注','description',p.description)}${check('启用供应商','enabled',p.enabled)}${check('允许私有网络 / 明文 HTTP','allow_private',p.allow_private,'仅连接你信任的本地或内网模型服务时开启。开启后可访问回环与私网 IP。')}<div class="dialog-note">连接测试只调用 GET /models。同步模型不会自动确认价格、额度与全部能力，新模型默认禁用。</div></div>`,async f=>{
  const v={name:val(f,'name').trim(),kind:val(f,'kind'),base_url:val(f,'base_url').trim(),auth:val(f,'auth'),timeout_sec:nval(f,'timeout_sec'),enabled:checked(f,'enabled'),allow_private:checked(f,'allow_private'),description:val(f,'description')};
  if(val(f,'api_key'))v.api_key=val(f,'api_key');else if(checked(f,'clear_key'))v.api_key='';
  await save('provider.save',{id,provider:v},f);
 });

 $('[name=kind]',d).addEventListener('change',ev=>{const preset={opencode:'https://opencode.ai/zen/go/v1',commandcode:'https://api.commandcode.ai/provider/v1',zai:'https://api.z.ai/api/coding/paas/v4',openai:'https://api.openai.com/v1',anthropic:'https://api.anthropic.com/v1'}[ev.target.value];if(preset&&!old)$('[name=base_url]',d).value=preset;});

}

export function modelEditor(id=''){

 if(!state.config.providers.length){
    toast('请先添加一个供应商',true);
    providerEditor();
    return;

  }

 const old=getModel(id);
  const m=old||{
    id:'',name:'',provider_id:state.config.providers[0].id,upstream:'',protocol:'chat',enabled:true,tools:true,vision:false,native_count:false,drop_reasoning:false,context_window:128000,max_output_tokens:4096,concurrency:2,rpm:0,pricing_set:false,input_price:0,output_price:0,cache_price:0,write_price:0,limit_5h:0,limit_7d:0,limit_30d:0
  };

 showDialog(old?'编辑模型':'添加模型','配置真实能力；不把同一家供应商的所有模型假定为相同协议。',`<div class="form-section"><h3>身份与协议</h3><div class="form-grid">${field('客户端模型 ID','id',m.id,'text','由你指定，客户端请求 model 使用此值。',`${old?'readonly':''} required placeholder="my-coding-model"`)}${field('显示名称','name',m.name,'text','','required')}${selectField('供应商','provider_id',m.provider_id,state.config.providers.map(p=>[p.id,p.name]))}${selectField('上游原生协议','protocol',m.protocol,[['chat','OpenAI Chat Completions'],['messages','Anthropic Messages'],['responses','OpenAI Responses'],['systemone','TypeSafe System One']])}</div>${field('上游模型 ID','upstream',m.upstream,'text','与供应商接受的 model 字段完全一致。','required')}<div class="form-grid">${field('上下文窗口','context_window',m.context_window,'number','能力元数据，不是精确 tokenizer 验证。','min="128" required')}${field('最大输出 Tokens','max_output_tokens',m.max_output_tokens,'number','','min="1" required')}</div>${check('启用模型','enabled',m.enabled)}${check('支持工具调用','tools',m.tools)}${check('支持图像输入','vision',m.vision)}${check('有原生 Messages Token Count 接口','native_count',m.native_count,'只适用于消息协议且上游确实提供 /messages/count_tokens。')}${check('跨协议时丢弃推理内容','drop_reasoning',m.drop_reasoning,'仅当上游返回 reasoning 且你需要跨协议调用时开启。丢弃会在响应头 X-Prism-Dropped 标注；关闭时这类响应被明确拒绝，而不是悄悄截断。')}</div><div class="form-section"><h3>速率与并发</h3><div class="form-grid">${field('最大并发','concurrency',m.concurrency,'number','网关本地限制，不代表上游允许并发。','min="1" max="128" required')}${field('每分钟请求数 RPM','rpm',m.rpm,'number','0 = 不额外限制；仍受上游限制。','min="0" required')}</div></div><div class="form-section"><h3>计价 · USD / 百万 Tokens</h3><div class="dialog-note">费用仅按你填写的单价与上游 usage 计算，不代表订阅实付。价格未确认时不显示为真实已知费用。</div>${check('我已确认以下计价','pricing_set',m.pricing_set)}<div class="form-grid">${[['普通输入','input_price'],['输出','output_price'],['缓存读取','cache_price'],['缓存写入','write_price']].map(([label,k])=>field(label,k,m[k],'number','','min="0" max="1000000" step="any" required')).join('')}</div></div><div class="form-section"><h3>本地滚动预算 · USD</h3><p class="small muted" style="margin-bottom:14px">不是供应商的账期额度。0 = 不额外限制；启用金额限制必须先确认计价。</p><div class="form-grid">${[['过去 5 小时','limit_5h'],['过去 7 天','limit_7d'],['过去 30 天','limit_30d']].map(([label,k])=>field(label,k,m[k],'number','','min="0" step="any" required')).join('')}</div></div>`,async f=>{const v={};for(const k of ['id','name','provider_id','upstream','protocol'])v[k]=val(f,k).trim();for(const k of ['enabled','tools','vision','native_count','drop_reasoning','pricing_set'])v[k]=checked(f,k);for(const k of ['context_window','max_output_tokens','concurrency','rpm','input_price','output_price','cache_price','write_price','limit_5h','limit_7d','limit_30d'])v[k]=nval(f,k);await save('model.save',{id,model:v},f);},'保存模型',true);

}

// 候选池可能有几百个模型，没有搜索就翻不动。
// 已勾选的行任何时候都保持可见：搜索把自己已经选中的东西藏起来，
// 会让人以为选择丢了。
export function bindCandidateSearch(){
  const box=$('#candidate-search');
  if(!box)return;
  const rows=$$('.candidate-row');
  const count=$('#candidate-count');
  const apply=()=>{
    const q=box.value.trim().toLowerCase();
    let shown=0;
    rows.forEach(row=>{
      const checked=$('input[name=candidate]',row)?.checked;
      const hit=!q||row.dataset.search.includes(q);
      row.hidden=!(hit||checked);
      if(!row.hidden)shown++;
    });
    if(count)count.textContent=q?`显示 ${shown} / ${rows.length} 个（含已选中）`:`共 ${rows.length} 个模型`;
  };
  box.addEventListener('input',apply);
  // 勾选状态变化后要重算，否则取消勾选的行在搜索状态下不会隐藏
  rows.forEach(row=>$('input[name=candidate]',row)?.addEventListener('change',apply));
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

 showDialog(old?'编辑路由':'创建智能路由','明确候选池；会话亲和优先，安全失败后才尝试备用。',`<div class="form-grid">${field('路由 ID','id',r.id,'text','客户端可直接将其作为 model。',`${old?'readonly':''} required placeholder="auto-coding"`)}${field('显示名称','name',r.name,'text','','required')}${selectField('选择策略','strategy',r.strategy,[['priority','优先级（按候选顺序）'],['balanced','本地预算压力均衡']])}</div>${field('路由描述','description',r.description)}${check('启用路由','enabled',r.enabled)}${check('启用会话亲和','affinity',r.affinity)}<div class="form-section"><h3>候选模型</h3><p class="small muted" style="margin:8px 0 14px">选中加入路由，权重参与均衡评分。保存后可在路由画布拖动排序。</p><div class="search-field" style="margin-bottom:12px">${icon('search')}<input id="candidate-search" placeholder="搜索模型、ID、协议或供应商" autocomplete="off" aria-label="搜索候选模型"></div><p class="tiny muted" id="candidate-count"></p><div class="candidate-list">${order.map(m=>{const c=r.candidates.find(x=>x.model_id===m.id);const hay=[m.id,m.name,m.upstream,m.protocol,getProvider(m.provider_id)?.name].filter(Boolean).join(' ').toLowerCase();return `<div class="candidate-row" data-search="${E(hay)}"><label class="checkline" style="flex:1"><input name="candidate" type="checkbox" value="${E(m.id)}" ${c?'checked':''}><span>${E(m.name||m.id)}<small>${E(m.protocol)} · ${E(getProvider(m.provider_id)?.name)}${m.enabled?'':' · 未启用'}</small></span></label><input class="weight-field" type="number" min="1" max="1000" value="${c?.weight||10}" data-weight="${E(m.id)}" style="width:80px" aria-label="候选权重"></div>`}).join('')}</div></div>`,async f=>{const candidates=$$('input[name=candidate]:checked',f).map(el=>({model_id:el.value,weight:Number($$('[data-weight]',f).find(x=>x.dataset.weight===el.value).value)}));await save('route.save',{id,route:{id:val(f,'id').trim(),name:val(f,'name').trim(),strategy:val(f,'strategy'),description:val(f,'description'),enabled:checked(f,'enabled'),affinity:checked(f,'affinity'),sort:Number(val(f,'sort'))||0,candidates}},f);});
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

export function keyEditor(){
  const opts=[...state.config.routes.map(x=>[x.id,x.name||x.id]),...state.config.models.map(x=>[x.id,x.name||x.id]),...state.config.aliases.map(x=>[x.id,x.id])];
  showDialog('创建客户端 API Key','按项目或客户端独立创建，可随时撤销。',`${field('密钥名称','name','','text','','required placeholder="例如：我的 Claude Code"')}<div class="form-section"><h3>可请求模型 / 路由</h3><p class="small muted" style="margin:10px 0">不选表示允许全部。范围限制针对客户端请求的模型、路由或别名 ID。</p>${opts.map(([v,l])=>`<label class="checkline"><input type="checkbox" name="allowed" value="${E(v)}"><span>${E(l)}<small class="mono">${E(v)}</small></span></label>`).join('')}</div>`,async f=>{const k=await rpc('apikey.create',{name:val(f,'name'),allowed:$$('[name=allowed]:checked',f).map(x=>x.value)});closeDialog();showDialog('密钥已创建','这是唯一一次显示完整密钥。关闭前请安全保存。',`<div class="banner">请不要将此 Key 放进公开仓库、截图或前端源代码。</div><pre class="secret-box">${E(k.key)}</pre>${btn('复制完整密钥','copy-secret','copy','','primary')}${connectionGuide()}`);state.secretValue=k.key;state.data=await rpc('apikey.list');if(state.page==='keys')renderPage(false);},'创建密钥');

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
