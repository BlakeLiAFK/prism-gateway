// 验证供应商额度刷新状态，不启动浏览器。
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
const context=createContext({crypto:globalThis.crypto});
const state={page:'settings',config:{providers:[{id:'go',kind:'opencode'},{id:'demo',kind:'mock'}]},providerUsage:{go:{headline:'旧额度'}}};
const calls=[];
const rpc=action=>{
  assert.equal(action,'provider.usage');
  return new Promise((resolve,reject)=>calls.push({resolve,reject}));
};
const source=(await readFile(new URL('app.js',assets),'utf8')).split('\nwindow.addEventListener')[0];
const imports=new Map();
for(const[,names,path]of source.matchAll(/import \{([^}]+)\} from '([^']+)'/g)){
  imports.set(path,[...new Set([...(imports.get(path)||[]),...names.split(',').map(name=>name.trim())])]);
}
const app=new SourceTextModule(source,{context});
await app.link(path=>new SyntheticModule(imports.get(path),function(){
  for(const name of imports.get(path))this.setExport(name,name==='state'?state:name==='rpc'?rpc:()=>{});
},{context}));
await app.evaluate();
const {loadProviderUsage,refreshPages,shouldAutoRefreshPage}=app.namespace;

assert.ok(refreshPages.includes('providers'),'供应商页必须加入自动刷新名单');

state.page='providers';
const failed=loadProviderUsage();
assert.equal(app.namespace.providerUsagePending,true,'请求期间应标记额度查询仍在进行');
assert.equal(shouldAutoRefreshPage(),false,'慢额度请求期间自动轮询不得启动下一轮');
state.page='settings';
calls.shift().reject(new Error('网络失败'));
await failed;
assert.equal(app.namespace.providerUsagePending,false,'请求结束后应清除在途标记');
state.page='providers';
assert.equal(shouldAutoRefreshPage(),true,'请求结束后自动轮询应恢复');
assert.equal(JSON.stringify(state.providerUsage),JSON.stringify({go:{id:'go',supported:true,error:'额度查询失败，请稍后重试'}}),'查询失败必须清除旧额度并显示明确错误');

state.page='settings';
const slow=loadProviderUsage(),slowCall=calls.shift();
const latest=loadProviderUsage(),latestCall=calls.shift();
latestCall.resolve([{id:'go',supported:true,headline:'新额度'}]);
await latest;
slowCall.resolve([{id:'go',supported:true,headline:'旧响应'}]);
await slow;
assert.equal(state.providerUsage.go.headline,'新额度','较慢的旧请求不得覆盖最新结果');

console.log('供应商额度刷新：自动刷新、失败清旧与并发代次保护通过');
