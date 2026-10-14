// 路由分组：画布按组分节、编辑器里的组表、候选所属组的收集。
// 路由由若干组按顺序组成：请求先在第 1 组按该组策略选候选，本组全部不可用或都接不了时才轮到下一组。
import {$,$$,E,state} from './core.js';
import {icon} from './icons.js';
import {field,selectField,tag} from './ui.js';
import {mountCandidatePicker} from './candidate-picker.js';

// 画布与草稿都以组为单位：state.routeDraft 是未应用排序的组数组
export const groupsOf=r=>state.routeDraft||r?.groups||[];
export const flatCandidates=groups=>groups.flatMap(g=>g.candidates||[]);
export const MAX_GROUPS=5;

// 列表与路由节点里的一句话概述
export function routeSummary(r,strategyName){
  const gs=r.groups||[];
  const n=flatCandidates(gs).length;
  return `${n} 个候选 · ${gs.length>1?`${gs.length} 组`:strategyName(gs[0]?.strategy||'priority')}`;
}

// 会话亲和的解析：组显式设置用它，未设置（null）时继承路由默认值，路由也没设按 true 兜底。
// 与后端 route.affinity 一致，界面显示的就是运行时真正会用的值。
export const routeAffinityDefault=r=>r?.affinity_default!==false;
export const resolvedAffinity=(r,g)=>g?.affinity===true?true:g?.affinity===false?false:routeAffinityDefault(r);
const groupAffinityValue=g=>g?.affinity===true?'on':g?.affinity===false?'off':'inherit';

// 会话亲和一栏：单组沿用原来的说法，多组逐组说明
export function affinityText(r){
  const gs=r.groups||[];
  if(gs.length<2)return resolvedAffinity(r,gs[0])?'优先保持同一模型':'已关闭';
  return gs.map(g=>`${g.name}：${resolvedAffinity(r,g)?'开':'关'}`).join(' · ');
}

// 每组保留编辑入口与空组落点，单组也可直接进入候选编辑。
export function groupSections(groups,card,strategyName,affinityOf){
  return groups.map((g,gi)=>`${gi?'<div class="group-link" aria-hidden="true">↓ 上一组全部不可用，或都接不了这次请求时</div>':''}<section class="route-group" data-group="${gi}"><header class="route-group-head"><strong>${gi+1} · ${E(g.name)}</strong>${tag(strategyName(g.strategy),'neutral')}${tag(affinityOf?affinityOf(g):(g.affinity?'会话亲和':'无亲和'),'neutral')}<span class="tiny muted">${g.candidates.length} 个模型</span><button class="btn small ghost group-edit" data-action="edit-route-group" data-group="${gi}">编辑本组</button></header><div class="route-group-list" data-drop-group="${gi}">${(g.candidates||[]).map((x,i)=>card(x,i,gi)).join('')}<div class="route-drop-zone">${g.candidates.length?'拖到这里，放在本组末尾':'空组 · 拖入模型，或点击编辑本组添加'}</div></div></section>`).join('');
}

// 目标位置是移除源候选后的索引；只搬移整个对象，保留权重、暂停与未知字段。
export function moveGroupCandidate(groups,fromGroup,from,toGroup,to){
  if(![fromGroup,from,toGroup,to].every(Number.isInteger))return false;
  const source=groups[fromGroup]?.candidates,target=groups[toGroup]?.candidates;
  if(!source||!target||from<0||from>=source.length||to<0||to>target.length-(source===target?1:0))return false;
  if(source===target&&from===to)return false;
  target.splice(to,0,source.splice(from,1)[0]);
  return true;
}

export function bindCandidateDrag(move){
  let drag=null;
  const clear=()=>{
    state.routeDragging=false;drag=null;
    $$('.candidate.dragging,.route-group-list.drag-over').forEach(el=>el.classList.remove('dragging','drag-over'));
  };
  $$('.candidate[data-drag-kind="candidate"]').forEach(el=>{
    el.addEventListener('dragstart',ev=>{
      if(ev.target.closest('button,input,select,a')){ev.preventDefault();return;}
      drag={group:Number(el.dataset.group),index:Number(el.dataset.dragIndex)};
      state.routeDragging=true;
      ev.dataTransfer.effectAllowed='move';ev.dataTransfer.setData('text/plain',String(drag.index));
      el.classList.add('dragging');
    });
    el.addEventListener('dragend',clear);
  });
  $$('[data-drop-group]').forEach(list=>{
    list.addEventListener('dragover',ev=>{
      if(!drag)return;
      ev.preventDefault();ev.dataTransfer.dropEffect='move';list.classList.add('drag-over');
    });
    list.addEventListener('dragleave',ev=>{if(!list.contains(ev.relatedTarget))list.classList.remove('drag-over');});
    list.addEventListener('drop',ev=>{
      if(!drag)return;
      ev.preventDefault();ev.stopPropagation();
      const group=Number(list.dataset.dropGroup),card=ev.target.closest('.candidate');
      let index=$$('.candidate',list).length;
      if(card){const box=card.getBoundingClientRect();index=Number(card.dataset.dragIndex)+(ev.clientY>=box.top+box.height/2?1:0);}
      const from=drag;
      if(from.group===group&&from.index<index)index--;
      clear();move(from.group,from.index,index,group);
    });
  });
}

// 当前组以对象引用保持身份，排序后重新计算索引，候选不会跟错组。
export function mountGroupEditor(root,groups,strategies,initialGroup=0,route={}){
  let active=groups[initialGroup]||groups[0];
  root.innerHTML='<div class="group-editor-nav" role="group" aria-label="选择要编辑的分组"></div><div class="group-editor-panel"></div>';
  const nav=$('.group-editor-nav',root),panel=$('.group-editor-panel',root);
  const refreshNav=()=>{
    nav.innerHTML=groups.map((g,i)=>`<button type="button" class="filter-chip ${g===active?'active':''}" aria-pressed="${g===active}" data-group-select="${i}">${i+1} · ${E(g.name||'未命名')}<span class="filter-count">${g.candidates.length}</span></button>`).join('')+`<button type="button" class="btn small ghost" id="group-add" data-group-op="add" ${groups.length>=MAX_GROUPS?'disabled':''}>${icon('plus')}添加组</button>`;
  };
  const render=()=>{
    refreshNav();
    const index=groups.indexOf(active);
    const inherited=routeAffinityDefault(route)?'开':'关';
    panel.innerHTML=`<div class="group-editor-head"><h3>第 ${index+1} 组</h3><div class="flex"><button type="button" class="btn small ghost" data-group-op="up" ${index===0?'disabled':''}>上移组</button><button type="button" class="btn small ghost" data-group-op="down" ${index===groups.length-1?'disabled':''}>下移组</button><button type="button" class="btn small ghost" data-group-op="delete" ${groups.length===1||active.candidates.length?'disabled':''} title="只能删除空组，且至少保留一组">删除空组</button></div></div><div class="form-grid">${field('组名','group-name',active.name,'text','','required maxlength="40"')}${selectField('选择策略','group-strategy',active.strategy,strategies.map(([value,desc])=>[value,desc]))}${selectField('会话亲和','group-affinity',groupAffinityValue(active),[['inherit',`跟随路由（当前${inherited}）`],['on','强制开启'],['off','强制关闭']],'跟随路由时由路由的会话亲和开关统一决定。')}</div><p class="tiny muted">请求先尝试前面的组，本组不可用才轮到下一组。非空组请先移出或移除候选后再删除。</p><div class="group-candidate-picker"></div>`;
    mountCandidatePicker($('.group-candidate-picker',panel),{groups,groupIndex:index,onChange:()=>{
      refreshNav();$('[data-group-op="delete"]',panel).disabled=groups.length===1||active.candidates.length>0;
    }});
  };
  root.addEventListener('input',ev=>{
    if(ev.target.name==='group-name'){active.name=ev.target.value;refreshNav();}
  });
  root.addEventListener('change',ev=>{
    if(ev.target.name==='group-strategy')active.strategy=ev.target.value;
    // 三态：跟随路由写 null（继承），其余写明确的布尔值
    if(ev.target.name==='group-affinity')active.affinity=ev.target.value==='inherit'?null:ev.target.value==='on';
  });
  root.addEventListener('click',ev=>{
    const button=ev.target.closest('[data-group-select],[data-group-op]');
    if(!button||button.disabled)return;
    const index=groups.indexOf(active),op=button.dataset.groupOp;
    if(button.dataset.groupSelect!==undefined)active=groups[Number(button.dataset.groupSelect)];
    else if(op==='add'&&groups.length<MAX_GROUPS){active={name:`第 ${groups.length+1} 组`,strategy:'priority',affinity:null,candidates:[]};groups.push(active);}
    else if(op==='delete'&&groups.length>1&&!active.candidates.length){groups.splice(index,1);active=groups[Math.min(index,groups.length-1)];}
    else if(op==='up'&&index>0)groups.splice(index-1,0,groups.splice(index,1)[0]);
    else if(op==='down'&&index<groups.length-1)groups.splice(index+1,0,groups.splice(index,1)[0]);
    render();$(`[data-group-select="${groups.indexOf(active)}"]`,nav)?.focus();
  });
  render();
}
