// 无浏览器回归：实际执行分组草稿、画布拖放绑定与编辑器切组逻辑。
import './test_route_editor_dialog.mjs';
import assert from 'node:assert/strict';
import {readFile,readdir} from 'node:fs/promises';
import {createContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
const elements=new Map();
const document={querySelectorAll:q=>elements.get(q)||[],querySelector:()=>null};
const context=createContext({crypto:globalThis.crypto,structuredClone,document});
const cache=new Map();
let renders=0,pickerArgs,modelLookups=0,saveRequest;
const mocks={
  renderPage:()=>renders++,routeEditorDirty:()=>false,
  tag:name=>`<span>${name}</span>`,field:()=>'',check:()=>'',selectField:()=>'',icon:()=>'',
  mountCandidatePicker:(root,args)=>{pickerArgs=args;},
  head:()=>'',btn:()=>'',badge:()=>'',sourceTag:()=>'',empty:()=>'',
  getModel:id=>{modelLookups++;return state.config.models.find(m=>m.id===id);},
  getProvider:id=>state.config.providers.find(p=>p.id===id),
  save:(...args)=>new Promise((resolve,reject)=>{saveRequest={args,resolve,reject};}),
};
async function load(name){
  if(cache.has(name))return cache.get(name);
  const source=await readFile(new URL(name,assets),'utf8');
  const imports=new Map([...source.matchAll(/import \{([^}]+)\} from '([^']+)'/g)].map(([,names,path])=>[path,names.split(',').map(s=>s.trim())]));
  const module=new SourceTextModule(source,{context});
  cache.set(name,module);
  await module.link(async path=>{
    if(['./core.js','./route-groups.js'].includes(path))return load(path.slice(2));
    const names=imports.get(path);
    return new SyntheticModule(names,function(){for(const key of names)this.setExport(key,mocks[key]||(()=>assert.fail(`意外调用 ${key}`)));},{context});
  });
  await module.evaluate();
  return module;
}
const {state}=(await load('core.js')).namespace;
const {moveGroupCandidate,groupSections,bindCandidateDrag,mountGroupEditor,resolvedAffinity,routeAffinityDefault}=(await load('route-groups.js')).namespace;
const {moveCandidate,routeActions,routeHasChanges,routes}=(await load('routes.js')).namespace;
const candidate=id=>({model_id:id,weight:37,disabled:true,extra:'保留'});
const fixture=()=>[{name:'主',strategy:'priority',affinity:true,candidates:[candidate('a'),candidate('b')]},{name:'备用',strategy:'cost',affinity:false,candidates:[candidate('c')]},{name:'空',strategy:'priority',affinity:true,candidates:[]}];
const ids=groups=>groups.map(g=>Array.from(g.candidates,c=>c.model_id));
let groups=fixture(),moved=groups[0].candidates[0];
assert.equal(moveGroupCandidate(groups,0,0,2,0),true);
assert.deepEqual(ids(groups),[['b'],['c'],['a']]);
assert.equal(groups[2].candidates[0],moved);
assert.equal(moved.weight,37);assert.equal(moved.disabled,true);assert.equal(moved.extra,'保留');
assert.equal(moveGroupCandidate(groups,2,0,1,1),true);
assert.deepEqual(ids(groups),[['b'],['c','a'],[]]);
assert.equal(moveGroupCandidate(groups,1,1,1,0),true);
assert.deepEqual(ids(groups),[['b'],['a','c'],[]]);
for(const args of [[1,0,1,0],[9,0,1,0],[1,-1,0,0],[1,0,1,2],[1,0,2,1],[1,0.5,2,0]]){
  const before=JSON.stringify(groups);
  assert.equal(moveGroupCandidate(groups,...args),false,String(args));
  assert.equal(JSON.stringify(groups),before);
}

state.config={version:7,routes:[{id:'r',groups:fixture()}]};state.routeID='r';
moveCandidate(0,0,0,2);
assert.equal(state.routeDraftVersion,7);
assert.deepEqual(ids(state.config.routes[0].groups),[['a','b'],['c'],[]]);
assert.deepEqual(ids(state.routeDraft),[['b'],['c'],['a']]);
state.config.version=9;
moveCandidate(2,0,1,1);
assert.equal(state.routeDraftVersion,7,'后续配置刷新不得升级草稿的保存版本');
assert.equal(routeHasChanges(),true);
routeActions['candidate-up']({dataset:{group:'1',index:'0'}});
assert.deepEqual(ids(state.routeDraft),[['b','c'],['a'],[]]);
routeActions['candidate-down']({dataset:{group:'1',index:'0'}});
assert.deepEqual(ids(state.routeDraft),[['b','c'],[],['a']]);
routeActions['discard-route-draft']();
assert.equal(state.routeDraft,null);assert.equal(state.routeDraftVersion,null);assert.equal(routeHasChanges(),false);
assert.equal(renders,5);

const html=groupSections([{name:'<unsafe>',strategy:'priority',affinity:true,candidates:[]}],()=>'',x=>x);
assert.match(html,/&lt;unsafe&gt;/);assert.match(html,/data-drop-group="0"/);assert.match(html,/编辑本组/);assert.match(html,/空组/);

// 亲和的三态解析必须与后端 route.affinity 一致：组显式覆盖，nil 继承路由默认值，都没有按 true 兜底
assert.equal(routeAffinityDefault({}),true,'路由未设置默认值时按 true');
assert.equal(routeAffinityDefault({affinity_default:false}),false);
assert.equal(resolvedAffinity({affinity_default:false},{affinity:true}),true,'组显式打开应覆盖路由的关闭');
assert.equal(resolvedAffinity({affinity_default:true},{affinity:false}),false,'组显式关闭应覆盖路由的开启');
assert.equal(resolvedAffinity({affinity_default:false},{}),false,'组未设置应继承路由的关闭');
assert.equal(resolvedAffinity({affinity_default:false},{affinity:null}),false,'组显式 null 同样按继承');
assert.equal(resolvedAffinity({},{}),true,'路由与组都未设置按 true');
// 画布标签用解析后的值，不是组的原始字段
const label=affinityOf=>groupSections([{name:'主',strategy:'priority',affinity:true,candidates:[]}],()=>'',x=>x,affinityOf);
assert.match(label(()=>'会话亲和'),/会话亲和/);
assert.match(label(()=>'无亲和'),/无亲和/);

// 最小事件替身验证真实拖拽监听：空组、末尾和同组向后插入。
const node=(dataset={})=>({dataset,listeners:{},classList:{add(){},remove(){}},
  addEventListener(name,fn){this.listeners[name]=fn;},contains(){return false;},querySelectorAll(){return this.cards||[];},
  closest(selector){return selector==='.candidate'?this:null;},getBoundingClientRect(){return {top:0,height:100};}});
const a=node({group:'0',dragIndex:'0'}),b=node({group:'0',dragIndex:'1'});
const list=node({dropGroup:'0'});list.cards=[a,b];
const empty=node({dropGroup:'1'});empty.cards=[];
elements.set('.candidate[data-drag-kind="candidate"]',[a,b]);elements.set('[data-drop-group]',[list,empty]);
const calls=[];
bindCandidateDrag((...args)=>calls.push(args));
const event=target=>({target,clientY:80,dataTransfer:{setData(){}},preventDefault(){},stopPropagation(){}});
a.listeners.dragstart(event(a));assert.equal(state.routeDragging,true);
empty.listeners.drop(event({closest:()=>null}));
assert.deepEqual(calls.pop(),[0,0,0,1]);assert.equal(state.routeDragging,false);
a.listeners.dragstart(event(a));list.listeners.drop(event(b));
assert.deepEqual(calls.pop(),[0,0,1,0]);
b.listeners.dragstart(event(b));list.listeners.drop(event({closest:()=>null}));
assert.deepEqual(calls.pop(),[0,1,1,0]);
a.listeners.dragstart(event(a));a.listeners.dragend();assert.equal(state.routeDragging,false);

// 组编辑不依赖数组索引作为身份：改名、组排序和切换均保留同一个组及候选。
const nav={innerHTML:'',querySelector:()=>({focus(){}})};
const deletion={disabled:false};
const panel={innerHTML:'',querySelector:q=>q.includes('delete')?deletion:{}};
const root={innerHTML:'',listeners:{},querySelector:q=>q.includes('nav')?nav:panel,addEventListener(name,fn){this.listeners[name]=fn;}};
groups=fixture();
mountGroupEditor(root,groups,[],1);
assert.equal(pickerArgs.groupIndex,1);
const click=dataset=>root.listeners.click({target:{closest:()=>({dataset,disabled:false})}});
click({groupOp:'up'});
assert.equal(pickerArgs.groupIndex,0);assert.equal(groups[0].name,'备用');
root.listeners.input({target:{name:'group-name',value:'新名称'}});
assert.equal(groups[0].name,'新名称');assert.match(nav.innerHTML,/新名称/);
click({groupSelect:'1'});assert.equal(pickerArgs.groupIndex,1);
assert.deepEqual(ids(groups),[['c'],['a','b'],[]]);
click({groupOp:'delete'});assert.equal(groups.length,3,'非空组不可删除');
click({groupSelect:'2'});click({groupOp:'delete'});assert.equal(groups.length,2);
click({groupOp:'add'});assert.equal(groups.length,3);assert.equal(pickerArgs.groupIndex,2);
for(let i=0;i<4;i++)click({groupOp:'add'});
assert.equal(groups.length,5);
state.config={version:10,models:Array.from({length:1000},(_,i)=>({id:`m${i}`,provider_id:'p',enabled:true,protocol:i%2?'messages':'chat',context_window:262144})),providers:[{id:'p',enabled:true}],aliases:[],routes:[{id:'r',groups:[{name:'主',strategy:'priority',affinity:true,candidates:Array.from({length:1000},(_,i)=>candidate(`m${i}`))}]}]};
let shareScans=0;
state.data={share:{r:new Proxy(Object.fromEntries(state.config.models.map(m=>[m.id,{attempts:1,success:1}])),{ownKeys:target=>{shareScans++;return Reflect.ownKeys(target);}})}};
const rendered=routes();
assert.equal(shareScans,1,'整条路由流量只应求和一次');
assert.equal((rendered.match(/data-drag-kind="candidate"/g)||[]).length,1000);
assert.match(rendered,/会话切入受限/);
assert.equal(modelLookups,0,'画布渲染应复用模型索引，不应逐卡线性查找模型');
moveCandidate(0,0,1);
const draft=JSON.stringify(state.routeDraft);
const pending=routeActions['save-route-order']();
assert.equal(state.routeDraftSaving,true);
moveCandidate(0,0,2);
assert.equal(JSON.stringify(state.routeDraft),draft,'保存处理中不能生成随后被覆盖的新草稿');
assert.equal(saveRequest.args[1].version,10);
saveRequest.reject(new Error('VERSION_CONFLICT'));
await assert.rejects(pending,/VERSION_CONFLICT/);
assert.equal(state.routeDraftSaving,false);
assert.equal(JSON.stringify(state.routeDraft),draft,'保存失败必须保留草稿');
// 只链接完整生产模块图，校验新增模块的真实导入/导出，不执行浏览器启动流程。
const graph=new Map(),linkContext=createContext({});
for(const file of (await readdir(assets)).filter(name=>name.endsWith('.js'))){
  const url=new URL(file,assets);
  graph.set(url.href,new SourceTextModule(await readFile(url,'utf8'),{context:linkContext,identifier:url.href}));
}
await graph.get(new URL('app.js',assets).href).link((path,ref)=>graph.get(new URL(path,ref.identifier).href));
console.log('路由编排：跨组/空组/末尾、权重与暂停、草稿版本、拖放事件、切组与排序保留全部通过');
