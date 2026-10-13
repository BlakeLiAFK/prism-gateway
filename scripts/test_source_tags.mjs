// 执行真实标签模块，隔离页面初始化，不启动浏览器。
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
async function load(){
  const context=createContext({crypto:globalThis.crypto});
  const core=new SourceTextModule(await readFile(new URL('core.js',assets),'utf8'),{context});
  await core.link(()=>assert.fail('core 应保持独立'));
  await core.evaluate();
  const source=await readFile(new URL('ui.js',assets),'utf8');
  const imports=new Map([...source.matchAll(/import \{([^}]+)\} from '([^']+)'/g)]
    .map(([,names,path])=>[path,names.split(',').map(name=>name.trim())]));
  const ui=new SourceTextModule(source,{context});
  await ui.link(path=>path==='./core.js'?core:new SyntheticModule(imports.get(path),function(){
    for(const name of imports.get(path))this.setExport(name,()=>assert.fail(`意外调用 ${name}`));
  },{context}));
  await ui.evaluate();
  return {state:core.namespace.state,sourceTag:ui.namespace.sourceTag};
}

const {state,sourceTag}=await load();
const provider={id:'command-main',name:'Command Code主'};
state.config={providers:[provider,{id:'other',name:'备用供应商'}]};
const original=sourceTag(provider.id);
const hue=html=>Number(html.match(/--h:(\d+)/)[1]);
assert.equal(sourceTag('missing'),'');
state.config.providers.reverse();
assert.equal(sourceTag(provider.id),original,'排序不能改变颜色');
state.config.providers.unshift({id:'new',name:'新增供应商'});
assert.equal(sourceTag(provider.id),original,'新增不能改变已有颜色');
state.config.providers=[provider];
assert.equal(sourceTag(provider.id),original,'删除不能改变已有颜色');
const reloaded=await load();
reloaded.state.config=JSON.parse(JSON.stringify(state.config));
assert.equal(reloaded.sourceTag(provider.id),original,'重新加载后颜色不变');
state.config.providers.push({id:'same-name',name:provider.name});
assert.equal(hue(sourceTag('same-name')),hue(original),'同名不同 ID 使用同色');
state.config.providers.push({id:'fallback',name:''},{id:'named',name:'fallback'});
assert.equal(hue(sourceTag('fallback')),hue(sourceTag('named')),'空名称回退 ID');
state.config.providers.push({id:'<id>"',name:'<script>&"'});
assert.match(sourceTag('<id>"'),/title="供应商 &lt;id&gt;&quot;"/);
assert.match(sourceTag('<id>"'),/>&lt;script&gt;&amp;&quot;<\/span>$/);
const colors=new Set();
for(let i=0;i<2000;i++){
  provider.name=`供应商 ${i}`;
  const color=hue(sourceTag(provider.id));
  assert.ok(color>=100&&color<=280,'排除红色、粉红和橙黄色域');
  colors.add(color);
}
assert.ok(colors.size>=170,`颜色分布不足：${colors.size}`);
console.log(`source tags: passed (${colors.size} hues; stable names, ordering, reload, escaping)`);
