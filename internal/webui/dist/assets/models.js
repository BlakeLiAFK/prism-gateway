// 模型库页：模型列表与筛选、模型编辑器。
import {rpc} from './api.js';
import {renderPage} from './app.js';
import {$,$$,E,compact,dateTime,dateTimeSec,json,money,ms,number,state} from './core.js';
import {icon} from './icons.js';
import {avatar,badge,btn,chart,check,checked,closeDialog,copy,empty,field,flowMini,getModel,getProvider,head,headerValue,modelName,nval,pillStatus,quotaBars,rangeTools,save,selectField,selectRow,showDialog,sourceTag,spark,tag,toast,val} from './ui.js';
import {providerEditor} from './views.js';

// 搜索 / 协议 / 供应商三个筛选先框出一个范围，模型库自己的筛选标签再在这个范围内叠加
function scopedModels(){
  return state.config.models.filter(m=>(!state.modelQ||(m.id+' '+m.name+' '+m.upstream).toLowerCase().includes(state.modelQ.toLowerCase()))&&(!state.modelProtocol||m.protocol===state.modelProtocol)&&(!state.providerFilter||m.provider_id===state.providerFilter));
}

// 表格里的路由徽章：路由多时只显示前两个，title 给全名
function routeCell(id){
  const rs=routesUsing(id);
  if(!rs.length)return '<span class="muted">—</span>';
  const shown=rs.slice(0,2);
  const rest=rs.length-shown.length;
  return `<div class="route-tags" title="${E(rs.map(r=>r.name||r.id).join('、'))}">${shown.map(r=>badge(r.name||r.id)).join('')}${rest>0?badge('+'+rest):''}</div>`;

}

export function models(){
  const scoped=scopedModels();
  const filtered=scoped.filter(modelMatches);
  const showAll=!!state.modelFilters.showAll;
  const visible=showAll?filtered:filtered.slice(0,100);
  const truncated=filtered.length>visible.length;
  // 选中某个供应商时，直接在这里同步它的模型；否则同步入口只在供应商页面，找起来绕
  const syncable=state.config.providers.find(p=>p.id===state.providerFilter&&p.kind!=='mock');
  return head('MODEL LIBRARY','每个模型，各司其职。','按模型指定上游协议、能力、价格和限额；同步的模型默认启用，能力取自上游声明；计价需确认后才计入预算。',(syncable?btn(`同步 ${syncable.name} 的模型`,'sync-provider','refresh',`data-id="${E(syncable.id)}"`,'small'):'')+btn('添加模型','add-model','plus','','primary'))
  +`<div class="toolbar"><div class="search-field">${icon('search')}<input id="model-search" placeholder="搜索模型、ID 或上游名称" value="${E(state.modelQ)}" aria-label="搜索模型"></div><select id="model-protocol" aria-label="协议过滤"><option value="">全部协议</option>${['chat','messages','responses','systemone'].map(v=>`<option value="${v}" ${v===state.modelProtocol?'selected':''}>${v}</option>`).join('')}</select><select id="model-provider" aria-label="供应商过滤"><option value="">全部供应商</option>${state.config.providers.map(p=>`<option value="${E(p.id)}" ${p.id===state.providerFilter?'selected':''}>${E(p.name)}</option>`).join('')}</select><span class="small muted" style="margin-left:auto">显示 ${visible.length} / ${filtered.length} 个模型</span></div>`
  +modelFilterBar()
  +`<section class="card">${!scoped.length?empty('让模型库准备就绪','先添加供应商，再同步或手动添加模型。供应商模型列表不会自动确认协议能力与计价。','add-model','添加第一个模型'):!filtered.length?`<div class="empty"><div class="empty-icon">${icon('model')}</div><h3>没有匹配的筛选条件</h3><p>放宽筛选条件，或清除当前筛选查看全部模型。</p>${btn('清除筛选','model-filter-clear','close','','ghost')}</div>`:`<div class="table-scroll"><table><thead><tr><th>模型 / 上游 ID</th><th>供应商</th><th>原生协议</th><th>能力</th><th>Input / Output · $/M</th><th>并发</th><th>路由</th><th>启用</th><th></th></tr></thead><tbody>${visible.map(m=>`<tr><td><div class="cell-title">${E(m.name||m.id)}</div><div class="cell-sub mono">${E(m.upstream)}</div></td><td class="small">${E(getProvider(m.provider_id)?.name||m.provider_id)}</td><td>${badge(m.protocol)}</td><td><div class="flex" style="gap:4px">${m.tools?badge('TOOLS'):''}${m.vision?badge('VISION'):''}${badge(compact(m.context_window))}</div></td><td class="mono">${m.pricing_set?`${money(m.input_price)} / ${money(m.output_price)}`:'未确认价格'}</td><td class="mono">${m.concurrency}${m.rpm?` <span class="tiny muted">/ ${m.rpm} RPM</span>`:''}</td><td>${routeCell(m.id)}</td><td><button class="switch ${m.enabled?'on':''}" role="switch" aria-checked="${m.enabled}" aria-label="启用 ${E(m.name)}" data-action="toggle-model" data-id="${E(m.id)}"></button></td><td><div class="table-actions"><button class="icon-btn" data-action="edit-model" data-id="${E(m.id)}" aria-label="编辑模型">${icon('edit')}</button><button class="icon-btn" data-action="delete-model" data-id="${E(m.id)}" aria-label="删除模型">${icon('trash')}</button></div></td></tr>`).join('')}</tbody></table></div>${truncated?`<div class="list-foot"><span>已显示前 ${visible.length} 个，共 ${filtered.length} 个匹配</span>${btn(`显示全部 ${filtered.length} 个`,'model-show-all','','','ghost small')}</div>`:''}`}</section>`;

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

// ===== 模型库筛选（骨架，业务由 models 模块实现） =====
// state.modelFilters 约定：{status:''|'enabled'|'disabled', inRoute:bool, unpriced:bool, tools:bool, vision:bool}
// 与已有的 state.modelQ / state.modelProtocol / state.providerFilter 叠加生效，切页后保留。

// 使用某模型的路由列表
export function routesUsing(id){
  return state.config.routes.filter(r=>r.candidates.some(c=>c.model_id===id));
}

// 模型是否满足给定的筛选条件；modelMatches 是它固定用 state.modelFilters 的特例
function matchesFilters(m,f){
  if(f.status==='enabled'&&!m.enabled)return false;
  if(f.status==='disabled'&&m.enabled)return false;
  if(f.inRoute&&!routesUsing(m.id).length)return false;
  if(f.unpriced&&m.pricing_set)return false;
  if(f.tools&&!m.tools)return false;
  if(f.vision&&!m.vision)return false;
  return true;

}

// 模型是否满足 state.modelFilters
export function modelMatches(m){
  return matchesFilters(m,state.modelFilters);

}

// 快捷筛选标签条：每个标签带当前数量，点击切换；返回 HTML
export function modelFilterBar(){
  const f=state.modelFilters;
  const base=scopedModels();
  const count=over=>base.filter(m=>matchesFilters(m,{...f,...over})).length;
  const statusChip=(value,label)=>`<button class="filter-chip ${(f.status||'')===value?'active':''}" data-action="model-filter-status" data-value="${value}">${E(label)}<span class="filter-count">${count({status:value})}</span></button>`;
  const toggleChip=(key,label,warn=false)=>`<button class="filter-chip ${warn?'warn':''} ${f[key]?'active':''}" data-action="model-filter-toggle" data-key="${key}">${E(label)}<span class="filter-count">${count({[key]:true})}</span></button>`;
  const active=!!(f.status||f.inRoute||f.unpriced||f.tools||f.vision);
  return `<div class="filter-bar"><div class="filter-group">${statusChip('','全部')}${statusChip('enabled','已启用')}${statusChip('disabled','未启用')}</div><div class="filter-group">${toggleChip('inRoute','在路由中')}${toggleChip('unpriced','计价未确认',true)}${toggleChip('tools','支持工具')}${toggleChip('vision','支持图像')}</div>${active?btn('清除筛选','model-filter-clear','close','','ghost small'):''}</div>`;

}

// 本模块的点击动作，键为 data-action
export const modelActions={
  'model-filter-status':(el)=>{
    state.modelFilters={...state.modelFilters,status:el.dataset.value,showAll:false};
    renderPage(false);

  },
  'model-filter-toggle':(el)=>{
    const key=el.dataset.key;
    state.modelFilters={...state.modelFilters,[key]:!state.modelFilters[key],showAll:false};
    renderPage(false);

  },
  'model-filter-clear':()=>{
    state.modelFilters={};
    renderPage(false);

  },
  'model-show-all':()=>{
    state.modelFilters={...state.modelFilters,showAll:true};
    renderPage(false);

  }
};

// 本模块的非点击事件绑定（每次渲染后调用，元素不存在时静默跳过）
export function bindModels(){}
