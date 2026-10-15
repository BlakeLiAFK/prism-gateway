// 验证供应商额度字段的绝对重置时间渲染，不启动浏览器。
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createContext,runInContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
const context=createContext({crypto:globalThis.crypto});
const core=new SourceTextModule(await readFile(new URL('core.js',assets),'utf8'),{context});
await core.link(()=>assert.fail('core 应保持独立'));
await core.evaluate();
const {dateTime,state}=core.namespace;
const mocks={
  avatar:()=>'',btn:()=>'',head:()=>'',icon:()=>'',providerStatsBlock:()=>'',rangeSwitch:()=>'',tag:()=>'',
};
const source=await readFile(new URL('views.js',assets),'utf8');
const imports=new Map([...source.matchAll(/(?:import|export) \{([^}]+)\} from '([^']+)'/g)]
  .map(([,names,path])=>[path,names.split(',').map(name=>name.trim())]));
const views=new SourceTextModule(source,{context});
await views.link(path=>path==='./core.js'?core:new SyntheticModule(imports.get(path),function(){
  for(const name of imports.get(path))this.setExport(name,mocks[name]||(()=>assert.fail(`意外调用 ${name}`)));
},{context}));
await views.evaluate();

const reset=Date.UTC(2030,0,2,3,4);
state.config={providers:[{id:'go',name:'OpenCode Go',kind:'opencode',enabled:true,auth:'auto',has_key:true}],models:[]};
state.providerUsage={go:{supported:true,headline:'5 小时已用 42%',fetched_at:reset-60000,note:'来自 <官方> & 精确金额',limits:[],fields:[
  {label:'5 小时',value:'已用 42%',reset_at:reset},
  {label:'每周',value:'已用 0%',reset_at:reset+86400000},
  {label:'每月',value:'已用 0% · 原有说明'},
  {label:'滚动',value:'已用 0% · 滚动重置时间待确认'},
  {label:'非法字符串',value:'待确认',reset_at:'2030-01-02T03:04:00Z'},
  {label:'非法数字',value:'待确认',reset_at:Number.NaN},
  {label:'越界数字',value:'待确认',reset_at:Number.MAX_VALUE},
  {label:'零时间',value:'待确认',reset_at:0},
  {label:'负时间',value:'待确认',reset_at:-1},
]}};

const first=views.namespace.providers();
assert.match(first,new RegExp(`${dateTime(reset)}重置`),'有效毫秒时间应显示绝对本地时间');
assert.match(first,new RegExp(`${dateTime(reset+86400000)}重置`),'0% 周期也应显示有效重置时间');
assert.match(first,/已用 0%<small>原有说明<\/small>/,'无 metadata 的原有字段应原样渲染');
assert.match(first,/滚动重置时间待确认/,'零 rolling 应保留待确认文案');
assert.ok(first.includes(dateTime(reset-60000)),'应显示后端返回的查询时间');
assert.match(first,/来自 &lt;官方&gt; &amp; 精确金额/,'后端说明必须转义后显示');
assert.doesNotMatch(first,/来自 <官方>/,'后端说明不得注入 HTML');
assert.doesNotMatch(first,/4 小时 59 分|后重置|Invalid Date/,'未知或非法 metadata 不得伪造倒计时或日期');
assert.doesNotMatch(first,/2030-01-02T03:04:00Z/,'字符串 metadata 不得被当作时间戳');

runInContext('Date.now=()=>4102444800000',context);
const second=views.namespace.providers();
assert.equal(second,first,'时钟变化不得改变缓存字段的绝对重置时间');
console.log('供应商用量：绝对重置时间、零值、非法 metadata 与稳定渲染通过');
