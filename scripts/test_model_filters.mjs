// 直接执行模型库模块；仅替换页面重绘和外部依赖，不启动浏览器。
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
const context=createContext({crypto:globalThis.crypto});
const core=new SourceTextModule(await readFile(new URL('core.js',assets),'utf8'),{context});
await core.link(()=>assert.fail('core 应保持独立'));
await core.evaluate();
const {state}=core.namespace;
let renders=0;
const mocks={
  renderPage:()=>renders++,
  modelUsageOf:id=>id==='a'?{requests:1}:undefined,
  rangeSwitch:()=>'',
  btn:(label,action)=>`<button data-action="${action}">${label}</button>`,
};
const source=await readFile(new URL('models.js',assets),'utf8');
const imports=new Map([...source.matchAll(/import \{([^}]+)\} from '([^']+)'/g)]
  .map(([,names,path])=>[path,names.split(',').map(name=>name.trim())]));
const models=new SourceTextModule(source,{context});
await models.link(path=>{
  if(path==='./core.js')return core;
  const names=imports.get(path);
  return new SyntheticModule(names,function(){
    for(const name of names)this.setExport(name,mocks[name]||(()=>assert.fail(`意外调用 ${name}`)));
  },{context});
});
await models.evaluate();
const {modelMatches,modelFilterBar,modelActions}=models.namespace;
const base={id:'a',name:'普通模型',upstream:'vendor/model',provider_id:'p',protocol:'chat',
  enabled:true,pricing_set:true,input_price:0,output_price:0,cache_price:0,write_price:0,
  context_window:262144,tools:true,vision:true};
const matches=(filter,over={})=>{state.modelFilters=filter;return modelMatches({...base,...over});};

for(const field of ['id','name','upstream'])assert.equal(matches({stealth:true},{[field]:'Vendor/STEALTH-model'}),true,field);
assert.equal(matches({stealth:true}),false);
assert.equal(matches({stealth:true},{id:undefined,name:null,upstream:''}),false);
assert.equal(matches({free:true}),true);
assert.equal(matches({free:true},{pricing_set:false}),false);
for(const field of ['input_price','output_price','cache_price','write_price']){
  assert.equal(matches({free:true},{[field]:0.000001}),false,field);
}
assert.equal(matches({free:true},{price_tiers:[{min_input_tokens:200000,input_price:0,output_price:1}]}),false);
assert.equal(matches({free:true},{price_tiers:[{min_input_tokens:200000,input_price:0,output_price:0}]}),true);
for(const minContext of [128000,200000,256000,512000,1000000,2000000]){
  for(const [size,want] of [[minContext-1,false],[minContext,true],[minContext+1,true],[0,false],[undefined,false]]){
    assert.equal(matches({minContext},{context_window:size}),want,`${minContext}: ${size}`);
  }
}
assert.equal(matches({minContext:200000},{context_window:200000}),true);
assert.equal(matches({minContext:256000}),true);
assert.equal(matches({minContext:1000000},{context_window:1048576}),true);
assert.equal(matches({minContext:0},{context_window:undefined}),true);

state.config={models:[base,
  {...base,id:'b',upstream:'vendor/stealth',context_window:1048576},
  {...base,id:'c',upstream:'vendor/stealth',input_price:1},
  {...base,id:'d',upstream:'vendor/stealth',provider_id:'other'},
  {...base,id:'e',upstream:'vendor/stealth',protocol:'messages'},
],routes:[{groups:[{candidates:[{model_id:'a'}]}]}]};
assert.equal(matches({inRoute:true,tools:true,vision:true,used:true,free:true,minContext:256000}),true);
assert.equal(matches({free:true,unpriced:true}),false);
assert.equal(matches({free:true,status:'disabled'}),false);
assert.equal(matches({free:true,tools:true},{tools_unknown:true}),false);
state.modelQ='stealth';state.providerFilter='p';state.modelProtocol='chat';
state.modelFilters={free:true};
const html=modelFilterBar();
assert.match(html,/data-key="stealth">Stealth<span class="filter-count">1<\/span>/);
assert.match(html,/data-value="1000000"[^>]*>≥1M<span class="filter-count">1<\/span>/);
assert.match(html,/data-value="2000000"[^>]*>≥2M<span class="filter-count">0<\/span>/);
assert.match(html,/aria-pressed="true" data-action="model-filter-toggle" data-key="free"/);

state.modelFilters={stealth:true,free:true,showAll:true};
modelActions['model-filter-context']({dataset:{value:'1000000'}});
assert.equal(state.modelFilters.minContext,1000000);
assert.equal(state.modelFilters.showAll,false);
assert.equal(state.modelFilters.free,true);
assert.deepEqual(state.config.models.filter(modelMatches).map(m=>m.id),['b']);
assert.match(modelFilterBar(),/aria-pressed="true" data-action="model-filter-context" data-value="1000000"/);
modelActions['model-filter-context']({dataset:{value:'256000'}});
assert.equal(state.modelFilters.minContext,256000);
modelActions['model-filter-context']({dataset:{value:'256000'}});
assert.equal(state.modelFilters.minContext,0);
modelActions['model-filter-toggle']({dataset:{key:'stealth'}});
assert.equal(state.modelFilters.stealth,false);
modelActions['model-filter-clear']();
assert.equal(Object.keys(state.modelFilters).length,0);
assert.equal(renders,5);
assert.doesNotMatch(modelFilterBar(),/data-action="model-filter-clear"/);
console.log('模型库筛选：Stealth、免费、容量边界、组合计数、切换与清除全部通过');
