// 执行真实页面及通用构件，隔离网络与页面初始化，不启动浏览器。
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
const context=createContext({crypto:globalThis.crypto});
const cache=new Map();
async function load(name){
  if(cache.has(name))return cache.get(name);
  const source=await readFile(new URL(name,assets),'utf8');
  const imports=new Map([...source.matchAll(/import \{([^}]+)\} from '([^']+)'/g)]
    .map(([,names,path])=>[path,names.split(',').map(s=>s.trim())]));
  const module=new SourceTextModule(source,{context});
  cache.set(name,module);
  await module.link(path=>{
    if(['./core.js','./ui.js','./icons.js','./sessions.js'].includes(path))return load(path.slice(2));
    const names=imports.get(path);
    return new SyntheticModule(names,function(){
      for(const key of names)this.setExport(key,()=>assert.fail(`意外调用 ${key}`));
    },{context});
  });
  await module.evaluate();
  return module;
}
const {state}=(await load('core.js')).namespace;
const {sessions}=(await load('views.js')).namespace;
state.config={models:[{id:'a',name:'模型 A'},{id:'b',name:'模型 B'}],
  providers:[{id:'p',name:'供应商 P'}]};
const session={id:'ses_example',model_id:'a',provider_id:'p',requests:5,updated_at:Date.now(),
  expires_at:Date.now()+3600000,models:[
    {model_id:'a',provider_id:'p',requests:3,success_requests:2},
    {model_id:'b',provider_id:'p',requests:4,success_requests:3}],
  cost_nano:125000000,cost_known_requests:5,cost_unknown_requests:2,demo_requests:0};
state.data=[session];
let html=sessions();
assert.match(html,/<th>使用模型 \/ 次数<\/th>/);
assert.match(html,/<th>总费用 · USD<\/th>/);
assert.match(html,/模型 A.*?<span class="mono">3 次<\/span>/);
assert.match(html,/模型 B.*?<span class="mono">4 次<\/span>/);
assert.match(html,/\$0\.125/);
assert.match(html,/已知部分 · 2 次待确认/);
assert.match(html,/成功 3 次/);
assert.match(html,/模型次数含失败记录/);
assert.match(html,/data-action="unbind-session" data-id="ses_example"/);

state.data=[{...session,cost_nano:999000000,cost_known_requests:0}];
html=sessions();
assert.match(html,/<div>待确认<\/div>/);
assert.doesNotMatch(html,/\$0\.999|\$0\.00/,'预留金额和未知费用不能显示成已确认金额');
state.data=[{...session,cost_nano:0,cost_known_requests:5,cost_unknown_requests:0}];
html=sessions();
assert.match(html,/<div>\$0\.00<\/div>/,'已确认免费调用显示真实零费用');
assert.doesNotMatch(html.match(/<tbody>(.*?)<\/tbody>/s)[1],/已知部分|次待确认/);

state.data=[{...session,models:[],cost_nano:0,cost_known_requests:0,cost_unknown_requests:0}];
html=sessions();
assert.match(html,/暂无请求记录/);
assert.match(html,/暂无费用记录/);
assert.doesNotMatch(html,/\$0\.00/);
state.data=[{...state.data[0],demo_requests:7}];
assert.match(sessions(),/演示 7 次不计费/);

state.data=[{...session,models:[{model_id:'<deleted>"',provider_id:'<provider>"',requests:1,success_requests:1}]}];
html=sessions();
assert.match(html,/&lt;deleted&gt;&quot;/);
assert.match(html,/&lt;provider&gt;&quot;/);
assert.doesNotMatch(html,/<deleted>|<provider>/);

state.data=[session,{...session,id:'other',model_id:'b',models:[],cost_nano:500000000}];
state.sessionModel='a';
html=sessions();
assert.match(html,/解除该模型的全部 1 个绑定/);
assert.doesNotMatch(html,/data-id="other"|\$0\.50/);
state.data=[];state.sessionModel='';
assert.match(sessions(),/暂无会话绑定/);
console.log('会话用量：多模型次数、费用已知性、免费、演示、空历史、转义、筛选及解绑入口通过');
