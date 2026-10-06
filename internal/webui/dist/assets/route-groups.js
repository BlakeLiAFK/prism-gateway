// 路由分组：画布按组分节、编辑器里的组表、候选所属组的收集。
// 路由由若干组按顺序组成：请求先在第 1 组按该组策略选候选，本组全部不可用或都接不了时才轮到下一组。
import {$,$$,E,state} from './core.js';
import {icon} from './icons.js';
import {tag} from './ui.js';

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

// 会话亲和一栏：单组沿用原来的说法，多组逐组说明
export function affinityText(r){
  const gs=r.groups||[];
  if(gs.length<2)return gs[0]?.affinity?'优先保持同一模型':'已关闭';
  return gs.map(g=>`${g.name}：${g.affinity?'开':'关'}`).join(' · ');
}

// 画布：每组一节；组与组之间一条「本组不行才轮到下一组」的连接线。只有一组时不画组头，与分组之前的画布一致
export function groupSections(groups,card,strategyName){
  return groups.map((g,gi)=>`${gi?'<div class="group-link" aria-hidden="true">↓ 上一组全部不可用，或都接不了这次请求时</div>':''}<section class="route-group" data-group="${gi}">${groups.length>1?`<header class="route-group-head"><strong>${E(g.name)}</strong>${tag(strategyName(g.strategy),'neutral')}${tag(g.affinity?'会话亲和':'无亲和','neutral')}${gi?tag('备用组','warning'):''}</header>`:''}<div class="route-group-list">${(g.candidates||[]).map((x,i)=>card(x,i,gi)).join('')||'<div class="small muted">本组没有候选，请编辑路由添加模型。</div>'}</div></section>`).join('');
}

function groupRow(g,gid,strategies,canDelete){
  return `<div class="group-row" data-gid="${E(gid)}"><input class="group-name" value="${E(g.name||'')}" maxlength="40" required aria-label="组名"><select class="group-strategy" aria-label="组策略">${strategies.map(x=>`<option value="${x[0]}" title="${E(x[1])}" ${x[0]===g.strategy?'selected':''}>${E(x[2])}</option>`).join('')}</select><label class="checkline tiny"><input type="checkbox" class="group-affinity" ${g.affinity?'checked':''}><span>会话亲和</span></label><span class="group-ops"><button type="button" class="icon-btn" data-grp="up" aria-label="上移该组">${icon('up')}</button><button type="button" class="icon-btn" data-grp="down" aria-label="下移该组">${icon('down')}</button><button type="button" class="icon-btn" data-grp="del" aria-label="删除该组" ${canDelete?'':'disabled'}>${icon('trash')}</button></span></div>`;
}

// 编辑器里的组表。每行的 data-gid 是稳定标识：候选的「所属组」下拉按它记录，组上移、下移、删除后仍然对得上
export function groupEditor(groups,strategies){
  return `<div class="form-section"><h3>候选分组</h3><p class="small muted" style="margin:8px 0 14px">请求先在第 1 组按该组的策略选候选；本组全部不可用，或都接不了这次请求时，才轮到下一组。每组有自己的策略与会话亲和，同一个模型只能放在一个组里。想要兜底，就把贵但什么都能接的模型放在后面的组。</p><div id="group-table">${groups.map((g,i)=>groupRow(g,'g'+i,strategies,groups.length>1)).join('')}</div><button type="button" class="btn ghost small" id="group-add">${icon('plus')}添加一组</button></div>`;
}

// 模型所在组在编辑器里的稳定标识，用作候选行下拉的初始值
export const initialGid=(groups,id)=>{
  const i=groups.findIndex(g=>(g.candidates||[]).some(c=>c.model_id===id));
  return 'g'+Math.max(i,0);
};

export function bindGroupEditor(strategies){
  const table=$('#group-table');
  if(!table)return;
  let seq=0;
  const rows=()=>$$('.group-row',table);
  // 组的增删与改名之后，候选行的下拉、各组按钮的可用状态都要跟着刷新
  const refresh=()=>{
    const rs=rows();
    $$('select.group-select').forEach(sel=>{
      const cur=sel.value||sel.dataset.initial;
      sel.innerHTML=rs.map((r,i)=>`<option value="${E(r.dataset.gid)}">${i+1} · ${E($('.group-name',r).value||'未命名')}</option>`).join('');
      sel.value=rs.some(r=>r.dataset.gid===cur)?cur:rs[0]?.dataset.gid;
      sel.hidden=rs.length<2;
    });
    rs.forEach((r,i)=>{
      $('[data-grp=up]',r).disabled=i===0;
      $('[data-grp=down]',r).disabled=i===rs.length-1;
      $('[data-grp=del]',r).disabled=rs.length===1;
    });
    $('#group-add').disabled=rs.length>=MAX_GROUPS;
  };
  table.addEventListener('click',ev=>{
    const b=ev.target.closest('[data-grp]');
    if(!b||b.disabled)return;
    const row=b.closest('.group-row');
    if(b.dataset.grp==='up')row.previousElementSibling?.before(row);
    else if(b.dataset.grp==='down')row.nextElementSibling?.after(row);
    else row.remove();
    refresh();
  });
  table.addEventListener('input',refresh);
  $('#group-add').addEventListener('click',()=>{
    const n=rows().length;
    if(n>=MAX_GROUPS)return;
    table.insertAdjacentHTML('beforeend',groupRow({name:`第 ${n+1} 组`,strategy:'priority',affinity:true},`n${++seq}`,strategies,true));
    refresh();
  });
  refresh();
}

// 从表单读出各组及组内候选。候选保持表单里的先后顺序（编辑器把已有候选按原顺序排在最前）；
// 暂停状态沿用旧路由里的，编辑器不改它
export function collectGroups(f,oldGroups){
  const groups=$$('#group-table .group-row',f).map(r=>({gid:r.dataset.gid,name:$('.group-name',r).value.trim(),strategy:$('.group-strategy',r).value,affinity:$('.group-affinity',r).checked,candidates:[]}));
  const paused=new Set(flatCandidates(oldGroups).filter(x=>x.disabled).map(x=>x.model_id));
  for(const cb of $$('input[name=candidate]:checked',f)){
    const row=cb.closest('.candidate-row');
    const g=groups.find(x=>x.gid===$('.group-select',row)?.value)||groups[0];
    g.candidates.push({model_id:cb.value,weight:Number($('[data-weight]',row).value),disabled:paused.has(cb.value)});
  }
  return groups.map(({gid,...g})=>g);
}
