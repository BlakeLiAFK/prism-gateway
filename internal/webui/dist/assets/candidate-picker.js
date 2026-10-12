// 路由候选选择器：已选项编排与大量模型筛选保持在本地草稿中。
import {$,$$,E,formatTokens,state} from './core.js';
import {sourceTag} from './ui.js';

export const CANDIDATE_PAGE_SIZE=50;
const CONTEXTS=[[128000,'128K'],[200000,'200K'],[256000,'256K'],[512000,'512K'],[1000000,'1M'],[2000000,'2M']];
const FLAGS=[['enabled','已启用'],['tools','支持工具'],['vision','支持图像'],['out64k','输出 ≥ 64K'],['priced','已计价'],['free','免费'],['stealth','Stealth']];

export function routeCandidateIDs(groups){
  return new Set(groups.flatMap(g=>(g.candidates||[]).map(c=>c.model_id)));
}

export function candidateMatches(m,providerName,filters){
  const q=String(filters.q||'').trim().toLowerCase();
  if(q&&![m.id,m.name,m.upstream,m.provider_id,providerName].some(v=>String(v||'').toLowerCase().includes(q)))return false;
  if(filters.provider&&m.provider_id!==filters.provider)return false;
  const protocols=m.supported_protocols==null?[m.protocol]:m.supported_protocols;
  if(filters.protocol&&!protocols.includes(filters.protocol))return false;
  if(filters.enabled&&!m.enabled)return false;
  if(filters.tools&&(!m.tools||m.tools_unknown))return false;
  if(filters.vision&&(!m.vision||m.vision_unknown))return false;
  if(filters.out64k&&!(m.max_output_tokens>=64000))return false;
  if(filters.priced&&!m.pricing_set)return false;
  if(filters.free&&(!m.pricing_set||m.input_price||m.output_price||m.cache_price||m.write_price))return false;
  if(filters.stealth&&![m.id,m.name,m.upstream].some(v=>String(v||'').toLowerCase().includes('stealth')))return false;
  if(filters.minContext&&!(m.context_window>=filters.minContext))return false;
  return true;
}

export function availableCandidates(models,providers,groups,filters){
  const used=routeCandidateIDs(groups);
  const names=new Map(providers.map(p=>[p.id,p.name||p.id]));
  return models.filter(m=>!used.has(m.id)&&candidateMatches(m,names.get(m.provider_id),filters));
}

export function candidatePage(items,page){
  const pages=Math.max(1,Math.ceil(items.length/CANDIDATE_PAGE_SIZE));
  const current=Math.min(Math.max(1,Number(page)||1),pages);
  return {items:items.slice((current-1)*CANDIDATE_PAGE_SIZE,current*CANDIDATE_PAGE_SIZE),page:current,pages,total:items.length};
}

export function availableCandidateSelection(models,providers,groups,filters,ids){
  const wanted=new Set(ids);
  return availableCandidates(models,providers,groups,filters).filter(m=>wanted.has(m.id));
}

export function appendCandidates(group,models){
  const current=new Set((group.candidates||=[]).map(c=>c.model_id));
  const added=[];
  for(const m of models)if(!current.has(m.id)){
    const candidate={model_id:m.id,weight:10,disabled:false};
    group.candidates.push(candidate);
    current.add(m.id);
    added.push(candidate);
  }
  return added;
}

export function moveCandidate(groups,fromGroup,modelID,toGroup){
  if(fromGroup===toGroup||!groups[fromGroup]||!groups[toGroup])return false;
  const index=(groups[fromGroup].candidates||[]).findIndex(c=>c.model_id===modelID);
  if(index<0||routeCandidateIDs([groups[toGroup]]).has(modelID))return false;
  const [candidate]=groups[fromGroup].candidates.splice(index,1);
  (groups[toGroup].candidates||=[]).push(candidate);
  return true;
}

export function moveCandidateWithin(group,modelID,offset){
  const list=group?.candidates||[];
  const from=list.findIndex(c=>c.model_id===modelID),to=from+offset;
  if(from<0||to<0||to>=list.length)return false;
  [list[from],list[to]]=[list[to],list[from]];
  return true;
}

export function setCandidateWeight(candidate,value){
  candidate.weight=Math.min(1000,Math.max(1,Math.round(Number(value)||1)));
  return candidate.weight;
}

export function applyCandidateWeightInput(groups,groupIndex,target){
  const candidate=groups[groupIndex]?.candidates?.find(c=>c.model_id===target.dataset.weight);
  if(!candidate)return false;
  candidate.weight=target.value;
  return true;
}

const activeFilters=f=>!!(String(f.q||'').trim()||f.provider||f.protocol||f.minContext||FLAGS.some(([key])=>f[key]));
const pageControls=(part,page)=>page.pages<2?'':`<div class="candidate-picker-pages"><button type="button" class="btn ghost small" data-cp="${part}-prev" data-focus="${part}-prev" ${page.page===1?'disabled':''}>上一页</button><span>${page.page} / ${page.pages}</span><button type="button" class="btn ghost small" data-cp="${part}-next" data-focus="${part}-next" ${page.page===page.pages?'disabled':''}>下一页</button></div>`;

export function mountCandidatePicker(root,{groups,groupIndex,onChange=()=>{}}){
  if(!root||!groups?.[groupIndex])return;
  root._candidatePickerAbort?.abort();
  const controller=new AbortController();
  root._candidatePickerAbort=controller;
  const models=state.config?.models||[];
  const providers=state.config?.providers||[];
  const modelByID=new Map(models.map(m=>[m.id,m]));
  const view={selectedQ:'',selectedPage:1,availablePage:1,confirmBulk:null,filters:{q:'',provider:'',protocol:'',minContext:0}};
  const currentGroup=()=>groups[groupIndex];
  const totalCount=()=>groups.reduce((n,g)=>n+(g.candidates||[]).length,0);

  const rememberFocus=()=>{
    const el=document.activeElement?.closest?.('[data-focus]');
    return el&&root.contains(el)?{key:el.dataset.focus,start:el.selectionStart,end:el.selectionEnd}:null;
  };
  const restoreFocus=(saved,fallback='')=>{
    const focus=typeof saved==='string'?{key:saved}:saved;
    let target=focus?.key?$$('[data-focus]',root).find(el=>el.dataset.focus===focus.key&&!el.disabled):null;
    if(!target&&fallback)target=$$('[data-focus]',root).find(el=>el.dataset.focus===fallback&&!el.disabled);
    if(!target)return;
    target.focus();
    if(Number.isInteger(focus?.start)&&target?.setSelectionRange)target.setSelectionRange(focus.start,focus.end);
  };
  const changed=()=>onChange();
  const selectedModels=()=>{
    const q=view.selectedQ.trim().toLowerCase();
    return (currentGroup().candidates||[]).filter(c=>{
      const m=modelByID.get(c.model_id);
      return !q||[c.model_id,m?.name,m?.upstream].some(v=>String(v||'').toLowerCase().includes(q));
    });
  };
  const available=()=>availableCandidates(models,providers,groups,view.filters);
  const modelTitle=m=>`<span class="candidate-picker-name">${E(m?.name||m?.id||'已删除模型')}</span>${m?sourceTag(m.provider_id):''}${m&&!m.enabled?'<span class="off-tag">未启用</span>':''}`;
  const modelMeta=m=>m?`<span class="mono">${E(m.id)}</span> · ${E((m.supported_protocols||[m.protocol]).join(' / ')||'协议待确认')} · ${E(formatTokens(m.context_window))}`:'';

  const selectedHTML=()=>{
    const page=candidatePage(selectedModels(),view.selectedPage);
    view.selectedPage=page.page;
    const rows=page.items.map(c=>{
      const m=modelByID.get(c.model_id);
      const index=currentGroup().candidates.indexOf(c);
      const targets=groups.map((g,i)=>i===groupIndex?'':`<option value="${i}">${i+1} · ${E(g.name||'未命名')}</option>`).join('');
      return `<div class="candidate-picker-row" data-model="${E(c.model_id)}"><div class="candidate-picker-info"><div class="candidate-title">${m?modelTitle(m):`<span class="candidate-picker-name">${E(c.model_id)}</span><span class="off-tag">模型已删除</span>`}${c.disabled?'<span class="off-tag">已暂停</span>':''}</div><small>${m?modelMeta(m):`<span class="mono">${E(c.model_id)}</span>`}</small></div><label class="candidate-picker-weight">权重<input type="number" min="1" max="1000" value="${E(c.weight)}" data-weight="${E(c.model_id)}" data-focus="weight-${E(c.model_id)}" aria-label="${E(c.model_id)} 权重"></label><span class="candidate-picker-order"><button type="button" class="icon-btn" data-cp="selected-up" data-id="${E(c.model_id)}" data-focus="up-${E(c.model_id)}" aria-label="上移 ${E(c.model_id)}" ${index===0?'disabled':''}>↑</button><button type="button" class="icon-btn" data-cp="selected-down" data-id="${E(c.model_id)}" data-focus="down-${E(c.model_id)}" aria-label="下移 ${E(c.model_id)}" ${index===currentGroup().candidates.length-1?'disabled':''}>↓</button></span>${groups.length>1?`<span class="candidate-picker-move"><select data-move-target="${E(c.model_id)}" data-focus="move-target-${E(c.model_id)}" aria-label="${E(c.model_id)} 目标组">${targets}</select><button type="button" class="btn ghost small" data-cp="move" data-id="${E(c.model_id)}" data-focus="move-${E(c.model_id)}">移动</button></span>`:''}<button type="button" class="btn ghost small danger" data-cp="remove" data-id="${E(c.model_id)}" data-focus="remove-${E(c.model_id)}">移除</button></div>`;
    }).join('');
    return `<section class="candidate-picker-section"><header><div><h4>当前组已选 <span>${currentGroup().candidates.length}</span></h4><p>保留原有顺序、权重和暂停状态。</p></div><div class="search-field"><input data-selected-search data-focus="selected-search" value="${E(view.selectedQ)}" placeholder="搜索已选模型" aria-label="搜索已选模型"></div></header><div class="candidate-picker-list">${rows||'<p class="candidate-picker-empty">当前组没有匹配的已选模型。</p>'}</div>${pageControls('selected',page)}</section>`;
  };

  const filterChip=(key,label)=>`<button type="button" class="filter-chip ${view.filters[key]?'active':''}" aria-pressed="${!!view.filters[key]}" data-cp="filter" data-filter="${key}" data-focus="filter-${key}">${E(label)}</button>`;
  const availableHTML=()=>{
    const matches=available();
    const page=candidatePage(matches,view.availablePage);
    view.availablePage=page.page;
    const rows=page.items.map(m=>`<div class="candidate-picker-row"><div class="candidate-picker-info"><div class="candidate-title">${modelTitle(m)}</div><small>${modelMeta(m)}</small></div><button type="button" class="btn ghost small" data-cp="add" data-id="${E(m.id)}" data-focus="add-${E(m.id)}">添加</button></div>`).join('');
    const canAll=activeFilters(view.filters)&&matches.length>0;
    const pending=view.confirmBulk;
    const confirmation=pending?`<div class="candidate-picker-confirm" role="alert"><span>确认把 ${pending.ids.length} 个模型加入当前组？${pending.enableCount?`其中 ${pending.enableCount} 个将在保存时启用。`:''}</span><button type="button" class="btn primary small" data-cp="confirm-bulk" data-focus="confirm-bulk">确认添加</button><button type="button" class="btn ghost small" data-cp="cancel-bulk" data-focus="cancel-bulk">取消</button></div>`:'';
    return `<section class="candidate-picker-section"><header><div><h4>待添加 <span>${matches.length}</span></h4><p>已排除这条路由中的所有已选模型。</p></div></header><div class="candidate-picker-tools"><div class="search-field"><input data-available-search data-focus="available-search" value="${E(view.filters.q)}" placeholder="搜索 ID、名称、上游或供应商" aria-label="搜索待添加模型"></div><select data-provider data-focus="provider" aria-label="按渠道筛选"><option value="">全部渠道</option>${providers.map(p=>`<option value="${E(p.id)}" ${view.filters.provider===p.id?'selected':''}>${E(p.name||p.id)}</option>`).join('')}</select><select data-protocol data-focus="protocol" aria-label="按原生协议筛选"><option value="">全部原生协议</option>${['chat','messages','responses','systemone'].map(p=>`<option value="${p}" ${view.filters.protocol===p?'selected':''}>${p}</option>`).join('')}</select><select data-context data-focus="context" aria-label="按最小上下文筛选"><option value="0">不限上下文</option>${CONTEXTS.map(([v,l])=>`<option value="${v}" ${view.filters.minContext===v?'selected':''}>上下文 ≥ ${l}</option>`).join('')}</select></div><div class="filter-bar candidate-picker-filters">${FLAGS.map(([key,label])=>filterChip(key,label)).join('')}<button type="button" class="btn ghost small" data-cp="clear-filters" data-focus="clear-filters" ${activeFilters(view.filters)?'':'disabled'}>清除筛选</button></div><div class="candidate-picker-list">${rows||'<p class="candidate-picker-empty">没有匹配的可添加模型。</p>'}</div>${pageControls('available',page)}<footer class="candidate-picker-actions"><span>本页 ${page.items.length} 个 · 匹配 ${matches.length} 个</span><button type="button" class="btn ghost small" data-cp="add-page" data-focus="add-page" ${page.items.length?'':'disabled'}>添加本页 ${page.items.length} 个</button><button type="button" class="btn primary small" data-cp="add-all" data-focus="add-all" ${canAll?'':'disabled'} title="${canAll?'':'请先设置筛选，避免误加全部模型'}">添加匹配的 ${matches.length} 个</button></footer>${confirmation}</section>`;
  };

  const summaryHTML=()=>{
    const disabled=groups.flatMap(g=>g.candidates||[]).filter(c=>{const m=modelByID.get(c.model_id);return m&&!m.enabled;}).length;
    return `<div class="candidate-picker-summary" aria-live="polite"><span>当前组 ${currentGroup().candidates.length} 个候选</span><span>本路由 ${totalCount()} 个候选</span>${disabled?`<span class="candidate-picker-warning">${disabled} 个未启用模型将在保存后启用</span>`:''}</div>`;
  };
  const renderAll=()=>{
    const focus=rememberFocus();
    root.innerHTML=`<div class="candidate-picker">${summaryHTML()}<div data-selected-panel>${selectedHTML()}</div><div data-available-panel>${availableHTML()}</div><div class="sr-only" data-candidate-live aria-live="polite"></div></div>`;
    restoreFocus(focus);
  };
  const renderPanels=(part,fallback='')=>{
    const focus=rememberFocus();
    if(part!=='available')$('[data-selected-panel]',root).innerHTML=selectedHTML();
    if(part!=='selected')$('[data-available-panel]',root).innerHTML=availableHTML();
    $('.candidate-picker-summary',root).outerHTML=summaryHTML();
    restoreFocus(focus,fallback);
  };
  const announce=text=>{const live=$('[data-candidate-live]',root);if(live)live.textContent=text;};
  const addModels=(items,label,focus='available-search')=>{
    const safe=availableCandidateSelection(models,providers,groups,view.filters,items.map(m=>m.id));
    const added=appendCandidates(currentGroup(),safe);
    view.confirmBulk=null;
    if(added.length){changed();renderPanels(undefined,focus);announce(`${label}，已添加 ${added.length} 个模型`);}
  };
  const requestBulk=(kind,items)=>{
    const safe=availableCandidateSelection(models,providers,groups,view.filters,items.map(m=>m.id));
    const enableCount=safe.filter(m=>!m.enabled).length;
    if(safe.length>50||enableCount){
      view.confirmBulk={kind,ids:safe.map(m=>m.id),enableCount};
      renderPanels('available','confirm-bulk');
      restoreFocus('confirm-bulk');
    }else addModels(safe,'批量添加完成');
  };

  root.addEventListener('input',ev=>{
    if(ev.isComposing)return;
    if(ev.target.matches('[data-weight]')){if(applyCandidateWeightInput(groups,groupIndex,ev.target))changed();}
    else if(ev.target.matches('[data-selected-search]')){view.selectedQ=ev.target.value;view.selectedPage=1;renderPanels('selected');}
    else if(ev.target.matches('[data-available-search]')){view.filters.q=ev.target.value;view.availablePage=1;view.confirmBulk=null;renderPanels('available');}
  },{signal:controller.signal});
  root.addEventListener('compositionend',ev=>ev.target.dispatchEvent(new Event('input',{bubbles:true})),{signal:controller.signal});
  root.addEventListener('change',ev=>{
    if(ev.target.matches('[data-provider]'))view.filters.provider=ev.target.value;
    else if(ev.target.matches('[data-protocol]'))view.filters.protocol=ev.target.value;
    else if(ev.target.matches('[data-context]'))view.filters.minContext=Number(ev.target.value);
    else if(ev.target.matches('[data-weight]')){
      const c=currentGroup().candidates.find(x=>x.model_id===ev.target.dataset.weight);
      if(c){ev.target.value=setCandidateWeight(c,ev.target.value);changed();announce(`已更新 ${c.model_id} 的权重`);}
      return;
    }else return;
    view.availablePage=1;view.confirmBulk=null;renderPanels('available');
  },{signal:controller.signal});
  root.addEventListener('click',ev=>{
    const button=ev.target.closest('[data-cp]');
    if(!button||button.disabled)return;
    const action=button.dataset.cp;
    if(action==='filter'){
      const key=button.dataset.filter;
      view.filters[key]=!view.filters[key];view.availablePage=1;view.confirmBulk=null;renderPanels('available');return;
    }
    if(action==='clear-filters'){
      view.filters={q:'',provider:'',protocol:'',minContext:0};view.availablePage=1;view.confirmBulk=null;renderPanels('available','available-search');return;
    }
    if(action==='selected-prev'||action==='selected-next'){
      view.selectedPage+=action.endsWith('next')?1:-1;renderPanels('selected','selected-search');return;
    }
    if(action==='available-prev'||action==='available-next'){
      view.availablePage+=action.endsWith('next')?1:-1;view.confirmBulk=null;renderPanels('available','available-search');return;
    }
    const matches=available();
    const page=candidatePage(matches,view.availablePage);
    if(action==='add'){
      const index=page.items.findIndex(m=>m.id===button.dataset.id),next=page.items[index+1];
      addModels(models.filter(m=>m.id===button.dataset.id),'添加完成',next?`add-${next.id}`:'available-search');
    }else if(action==='add-page')requestBulk('page',page.items);
    else if(action==='add-all')requestBulk('all',matches);
    else if(action==='cancel-bulk'){view.confirmBulk=null;renderPanels('available','available-search');}
    else if(action==='confirm-bulk'){
      const ids=view.confirmBulk?.ids||[];
      addModels(models.filter(m=>ids.includes(m.id)),'批量添加完成');
    }
    else if(action==='remove'){
      const visible=selectedModels(),shown=visible.findIndex(c=>c.model_id===button.dataset.id),next=visible[shown+1];
      const list=currentGroup().candidates,index=list.findIndex(c=>c.model_id===button.dataset.id);
      if(index>=0){list.splice(index,1);changed();renderPanels(undefined,next?`remove-${next.model_id}`:'selected-search');announce(`已移除 ${button.dataset.id}`);}
    }else if(action==='selected-up'||action==='selected-down'){
      if(moveCandidateWithin(currentGroup(),button.dataset.id,action.endsWith('down')?1:-1)){changed();renderPanels('selected','selected-search');}
    }else if(action==='move'){
      const select=$$('[data-move-target]',root).find(el=>el.dataset.moveTarget===button.dataset.id);
      const target=Number(select?.value);
      const visible=selectedModels(),shown=visible.findIndex(c=>c.model_id===button.dataset.id),next=visible[shown+1];
      if(moveCandidate(groups,groupIndex,button.dataset.id,target)){changed();renderPanels(undefined,next?`move-${next.model_id}`:'selected-search');announce(`已把 ${button.dataset.id} 移到第 ${target+1} 组`);}
    }
  },{signal:controller.signal});
  renderAll();
}
