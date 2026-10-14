// 执行真实弹窗和路由编辑模块，使用事件替身验证关闭与保存保护，不启动浏览器。
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
let dialog,request;
const dialogs=new Set();
const document={activeElement:null,
  querySelector:q=>q==='dialog[data-route-editor]'?dialog:q==='#overlay-root'?{append:d=>dialogs.add(d)}:{append(){}},
  querySelectorAll:()=>[...dialogs],
  createElement:tag=>tag==='dialog'?makeDialog():{...node(),querySelector:()=>node(),remove(){}},
};
function node(){
  const classes=new Set();
  return {hidden:true,isConnected:true,classList:{add:name=>classes.add(name),remove:name=>classes.delete(name),contains:name=>classes.has(name)},
    focus(){document.activeElement=this;},scrollIntoView(){},prepend(child){this.firstChild=child;},
    closest(){return null;},setAttribute(){},addEventListener(){}};
}
function makeDialog(){
  const listeners=[];
  const fields=Object.fromEntries(['id','name','description','enabled','route-affinity'].map(k=>[k,{value:k==='id'?'r':k==='name'?'原名称':k==='route-affinity'?'on':'',checked:true}]));
  const form={...node(),dataset:{},elements:{namedItem:k=>fields[k]}};
  const nodes=new Map([['#drawer-form',form],['.drawer-body',node()],['.drawer-footer',node()],
    ['.drawer-head h2',node()],['.drawer-head p',node()],['#route-group-editor',node()],
    ['.route-close-confirm',node()],['[data-route-continue]',node()],['button[type=submit]',node()],['#form-error',node()]]);
  const d={...node(),dataset:{},style:{},open:false,
    querySelector:q=>nodes.get(q),
    addEventListener:(type,fn,capture=false)=>listeners.push({type,fn,capture}),
    getBoundingClientRect:()=>({left:40,right:1400,top:24,bottom:900}),
    showModal(){this.open=true;},
    close(){this.open=false;this.fire('close');},remove(){dialogs.delete(this);},
    fire(type,target=d,position={}){
      const ev={target,clientX:0,clientY:0,...position,defaultPrevented:false,stopped:false,
        preventDefault(){this.defaultPrevented=true;},stopImmediatePropagation(){this.stopped=true;}};
      for(const entry of listeners.filter(x=>x.type===type).sort((a,b)=>Number(b.capture)-Number(a.capture))){
        if(ev.stopped)break;
        entry.fn(ev);
      }
      return ev;
    },
  };
  for(const n of nodes.values())n.querySelector=selector=>nodes.get(selector);
  form.addEventListener=(type,fn)=>{if(type==='submit')form.submit=()=>fn({preventDefault(){},currentTarget:form});};
  d.fields=fields;return d;
}
const context=createContext({document,crypto:globalThis.crypto,structuredClone,setTimeout:()=>0});
const mocks={rpc:(action,params)=>new Promise((resolve,reject)=>{request={action,params,resolve,reject};}),
  loadPage:async()=>{},login(){},shell(){},icon:()=>'',brand:()=>'',brandOf:()=>'',
  groupsOf:r=>r.groups,flatCandidates:groups=>groups.flatMap(g=>g.candidates),mountGroupEditor(){},setCandidateWeight(){}};
const modules=new Map(),imports=new Map();
for(const file of ['core.js','ui.js','route-editor.js']){
  const source=await readFile(new URL(file,assets),'utf8');
  const module=new SourceTextModule(source,{context});
  modules.set('./'+file,module);
  imports.set(module,new Map([...source.matchAll(/import \{([^}]+)\} from '([^']+)'/g)].map(([,names,path])=>[path,names.split(',').map(x=>x.trim())])));
}
await modules.get('./route-editor.js').link((path,ref)=>{
  if(modules.has(path))return modules.get(path);
  const names=imports.get(ref).get(path);
  return new SyntheticModule(names,function(){for(const name of names)this.setExport(name,mocks[name]||(()=>assert.fail(`意外调用 ${name}`)));},{context});
});
await modules.get('./route-editor.js').evaluate();
const {state}=modules.get('./core.js').namespace;
const {openRouteEditor}=modules.get('./route-editor.js').namespace;
const opener=node();
function open(){
  state.config={version:7,routes:[{id:'r',name:'原名称',description:'',enabled:true,groups:[{name:'主',candidates:[]}]}]};
  document.activeElement=opener;
  openRouteEditor('r',[]);
  dialog=[...dialogs][0];
  assert.equal(dialog.classList.contains('route-editor-dialog'),true);
  assert.equal(dialog.querySelector('.drawer-footer').firstChild,dialog.querySelector('.route-close-confirm'));
  return dialog;
}
const button=selector=>({closest:q=>q===selector?{}:null});
for(const dirty of [false,true]){
  const d=open();if(dirty)d.fields.name.value='未保存名称';
  const note=d.querySelector('.route-close-confirm');
  for(const point of [{clientX:0,clientY:100},{clientX:1500,clientY:100},{clientX:100,clientY:0},{clientX:100,clientY:950}]){
    d.fire('click',d,point);
    assert.equal(d.open,true,'点击任意一侧遮罩不关闭');
    assert.equal(note.hidden,true,'遮罩不打断编辑或弹确认');
  }
  d.fire('click',node());assert.equal(d.open,true,'内部空白不关闭');
  if(!dirty){assert.equal(d.fire('click',button('[data-action="close-dialog"]')).stopped,true);assert.equal(d.open,false);continue;}
  const input=node();input.focus();
  assert.equal(d.fire('click',button('[data-action="close-dialog"]')).stopped,true,'阻止通用关闭事件继续执行');
  assert.equal(d.open,true);assert.equal(note.hidden,false);
  assert.equal(document.activeElement,d.querySelector('[data-route-continue]'));
  d.fire('click',button('[data-route-continue]'));
  assert.equal(note.hidden,true);assert.equal(document.activeElement,input);
  assert.equal(d.fields.name.value,'未保存名称');
  assert.equal(d.fire('cancel').defaultPrevented,true);
  assert.equal(note.hidden,false);
  d.fire('click',button('[data-route-discard]'));
  assert.equal(d.open,false);assert.equal(document.activeElement,opener);
}
const clean=open();clean.fire('cancel');assert.equal(clean.open,false,'无修改 Esc 允许退出');
const saving=open();saving.fields.name.value='保存中的名称';
const pending=saving.querySelector('#drawer-form').submit();
assert.equal(request.action,'route.save');assert.equal(request.params.version,7);
assert.equal(saving.querySelector('.drawer-body').inert,true);
saving.fire('click',button('[data-action="close-dialog"]'));saving.fire('cancel');
assert.equal(saving.open,true);assert.equal(saving.querySelector('.route-close-confirm').hidden,true);
request.reject(new Error('VERSION_CONFLICT'));await pending;
assert.equal(saving.open,true);assert.equal(saving.fields.name.value,'保存中的名称');
assert.equal(saving.querySelector('.drawer-body').inert,false);
assert.equal(saving.querySelector('#form-error').textContent,'VERSION_CONFLICT');
const retry=saving.querySelector('#drawer-form').submit();request.resolve({...state.config,version:8});await retry;
assert.equal(saving.open,false,'保存成功关闭');
const generic=modules.get('./ui.js').namespace.showDialog('其他配置','','');
generic.fire('click',generic,{clientX:0});assert.equal(generic.open,false,'其他抽屉保留原有关闭行为');
console.log('路由弹窗：遮罩、显式退出、Esc、继续编辑、焦点恢复、保存中和失败重试通过');
