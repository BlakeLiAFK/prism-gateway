// 执行真实供应商删除动作，隔离网络与页面初始化，不启动浏览器。
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
const context=createContext({crypto:globalThis.crypto});
const core=new SourceTextModule(await readFile(new URL('core.js',assets),'utf8'),{context});
await core.link(()=>assert.fail('core 应保持独立'));
await core.evaluate();
const {state}=core.namespace;
let approved=false,prompt,saved,notice,saveError;
const mocks={
  getProvider:id=>state.config.providers.find(p=>p.id===id),
  confirm:async(...args)=>{prompt=args;return approved;},
  save:async(...args)=>{saved=args;if(saveError)throw saveError;},
  toast:(...args)=>{notice=args;},
};
const source=await readFile(new URL('views.js',assets),'utf8');
const imports=new Map([...source.matchAll(/(?:import|export) \{([^}]+)\} from '([^']+)'/g)]
  .map(([,names,path])=>[path,names.split(',').map(name=>name.trim())]));
const views=new SourceTextModule(source,{context});
await views.link(path=>path==='./core.js'?core:new SyntheticModule(imports.get(path),function(){
  for(const name of imports.get(path))this.setExport(name,mocks[name]||(()=>assert.fail(`意外调用 ${name}`)));
},{context}));
await views.evaluate();
const remove=views.namespace.pageActions['delete-provider'];
assert.equal(typeof remove,'function','供应商删除应使用专用确认动作');

state.config={version:7,
  providers:[{id:'p',name:'待删除供应商'},{id:'keep',name:'保留供应商'}],
  models:[{id:'a',provider_id:'p',enabled:true},{id:'b',provider_id:'p',enabled:false},{id:'c',provider_id:'keep'}],
  routes:[
    {id:'shared',groups:[{candidates:[{model_id:'a'},{model_id:'c'}]},{candidates:[{model_id:'b'}]}]},
    {id:'exclusive',groups:[{candidates:[{model_id:'a'}]}]},
    {id:'unrelated',groups:[{candidates:[{model_id:'c'}]}]},
  ],
  aliases:[{id:'a-alias',target:'a'},{id:'b-alias',target:'b',enabled:false},
    {id:'route-alias',target:'shared'},{id:'keep-alias',target:'c'}],
};
const before=JSON.stringify(state.config);
await remove(null,'p');
assert.match(prompt[0],/待删除供应商/);
assert.match(prompt[1],/2 个模型/,'停用模型也在关联删除范围内');
assert.match(prompt[1],/2 个模型别名/,'只统计直接指向所属模型的别名');
assert.match(prompt[1],/2 条路由/,'同一路由多个组只计一次');
assert.match(prompt[1],/空路由.*停用/);
assert.match(prompt[1],/历史.*保留/);
assert.equal(prompt[2],'关联删除');
assert.equal(saved,undefined,'取消不得发出删除请求');
assert.equal(JSON.stringify(state.config),before,'确认前不得修改本地配置');

approved=true;
await remove(null,'p');
assert.equal(saved[0],'provider.delete');
assert.equal(JSON.stringify(saved[1]),JSON.stringify({id:'p'}));
assert.equal(JSON.stringify(state.config),before,'实际配置由统一 save 在服务端成功后更新');

saveError=new Error('配置版本冲突');
await assert.rejects(remove(null,'p'),/配置版本冲突/,'保存失败应交给现有错误处理显示');
saveError=undefined;
state.config.providers.push({id:'empty',name:'空供应商'});
await remove(null,'empty');
assert.match(prompt[1],/0 个模型/);
assert.match(prompt[1],/0 个模型别名/);
assert.match(prompt[1],/0 条路由/);
assert.equal(saved[1].id,'empty');

prompt=saved=notice=undefined;
await remove(null,'missing');
assert.equal(prompt,undefined);
assert.equal(saved,undefined);
assert.match(notice[0],/不存在.*刷新/);
assert.equal(notice[1],true);
console.log('供应商删除：关联数量、停用模型、确认/取消、保存错误、空供应商及过期对象通过');
