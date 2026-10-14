// 路由编辑工作区：分组独立编辑，所有修改在一次版本校验中保存。
import {$,$$,E,state} from './core.js';
import {check,checked,closeDialog,field,getModel,save,selectField,showDialog,toast,val} from './ui.js';
import {flatCandidates,groupsOf,mountGroupEditor} from './route-groups.js';
import {setCandidateWeight} from './candidate-picker.js';

export const routeEditorDirty=()=>!!$('dialog[data-route-editor]')?.routeHasChanges?.();

export function openRouteEditor(id,strategies,initialGroup=0){
  const old=state.config.routes.find(r=>r.id===id);
  const r=old||{id:'',name:'',description:'',enabled:true,sort:0,affinity_default:true,groups:[{name:'主',strategy:'priority',affinity:null,candidates:[]}]};
  const groups=structuredClone(old&&id===state.routeID?groupsOf(r):r.groups);
  const before=JSON.stringify(groups);
  const opener=document.activeElement;
  let busy=false;
  const affinityDefault=r.affinity_default!==false;
  const body=`<details class="route-basics" ${old?'':'open'}><summary>路由基本信息 · ${E(r.name||'新路由')}</summary><div class="form-grid">${field('路由 ID','id',r.id,'text','客户端可直接将其作为 model。',`${old?'readonly':''} required placeholder="auto-coding"`)}${field('显示名称','name',r.name,'text','','required')}${selectField('会话亲和','route-affinity',affinityDefault?'on':'off',[['on','开启：同一会话优先沿用上次的模型'],['off','关闭：每次请求都按策略重新选择']],'根源开关：对所有使用本路由的请求生效；没单独设置的分组都跟随它。')}</div>${field('路由描述','description',r.description)}${check('启用路由','enabled',r.enabled)}</details><div id="route-group-editor"></div><div class="dialog-note route-close-confirm" role="alert" hidden><p>有尚未保存的修改。放弃后无法恢复本次编辑。</p><div class="flex"><button type="button" class="btn small" data-route-continue>继续编辑</button><button type="button" class="btn danger small" data-route-discard>放弃修改并关闭</button></div></div>`;
  const dialog=showDialog(old?'编辑路由':'创建智能路由','按组编辑候选。切换分组保留草稿；点击保存后，整条路由的修改一次生效。',body,async form=>{
    if(groups.some(g=>!g.name.trim()))throw new Error('每个分组都需要名称，请检查分组设置。');
    flatCandidates(groups).forEach(c=>setCandidateWeight(c,c.weight));
    const enable_models=flatCandidates(groups).some(c=>!getModel(c.model_id)?.enabled);
    const route={id:val(form,'id').trim(),name:val(form,'name').trim(),description:val(form,'description'),enabled:checked(form,'enabled'),sort:r.sort||0,affinity_default:val(form,'route-affinity')==='on',groups:groups.map(g=>({...g,name:g.name.trim()}))};
    busy=true;$('.drawer-body',dialog).inert=true;
    try{await save('route.save',{id,enable_models,route},form);}
    finally{busy=false;$('.drawer-body',dialog).inert=false;}
  },'保存路由');
  dialog.classList.add('route-editor-dialog');
  $('.drawer-head h2',dialog).id='route-editor-title';
  dialog.setAttribute('aria-labelledby','route-editor-title');
  dialog.dataset.routeEditor='';
  if(old&&id===state.routeID&&state.routeDraft)$('#drawer-form',dialog).dataset.version=state.routeDraftVersion??state.config.version;
  const form=$('#drawer-form',dialog);
  const dirty=()=>before!==JSON.stringify(groups)||['id','name','description'].some(k=>val(form,k)!==String(r[k]||''))||checked(form,'enabled')!==r.enabled||(val(form,'route-affinity')==='on')!==affinityDefault;
  dialog.routeHasChanges=dirty;
  mountGroupEditor($('#route-group-editor',dialog),groups,strategies,initialGroup,{affinity_default:affinityDefault});
  guardClose(dialog,dirty,()=>busy);
  dialog.addEventListener('close',()=>{if(opener?.isConnected)opener.focus();});
}

function guardClose(dialog,dirty,isBusy){
  const note=$('.route-close-confirm',dialog);
  $('.drawer-footer',dialog).prepend(note);
  let previousFocus=null;
  const close=()=>{
    if(isBusy()){toast('正在保存，请稍候');return;}
    if(!dirty()){closeDialog();return;}
    previousFocus=document.activeElement;
    note.hidden=false;note.scrollIntoView({block:'nearest'});
    $('[data-route-continue]',note).focus();
  };
  dialog.addEventListener('cancel',ev=>{ev.preventDefault();close();});
  dialog.addEventListener('click',ev=>{
    // 捕获阶段拦住遮罩点击，避免通用抽屉监听先关闭并丢弃草稿。
    if(ev.target===dialog){ev.preventDefault();ev.stopImmediatePropagation();return;}
    if(ev.target.closest('[data-action="close-dialog"]')){
      ev.preventDefault();ev.stopImmediatePropagation();close();
    }else if(ev.target.closest('[data-route-discard]')){
      if(!isBusy())closeDialog();
    }else if(ev.target.closest('[data-route-continue]')){
      note.hidden=true;if(previousFocus?.isConnected)previousFocus.focus();
    }
  },true);
}
