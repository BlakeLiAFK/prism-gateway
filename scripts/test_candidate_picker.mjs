// 在 Node VM 中验证候选选择器的数据逻辑，不启动浏览器。
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {createContext,SourceTextModule,SyntheticModule} from 'node:vm';

const assets=new URL('../internal/webui/dist/assets/',import.meta.url);
const context=createContext({crypto:globalThis.crypto});
const core=new SourceTextModule(await readFile(new URL('core.js',assets),'utf8'),{context});
await core.link(()=>assert.fail('core 应保持独立'));
await core.evaluate();
const source=await readFile(new URL('candidate-picker.js',assets),'utf8');
const imports=new Map([...source.matchAll(/import \{([^}]+)\} from '([^']+)'/g)].map(([,names,path])=>[path,names.split(',').map(x=>x.trim())]));
const picker=new SourceTextModule(source,{context});
await picker.link(path=>{
  if(path==='./core.js')return core;
  return new SyntheticModule(imports.get(path),function(){for(const name of imports.get(path))this.setExport(name,()=>assert.fail(`意外调用 ${name}`));},{context});
});
await picker.evaluate();
const {CANDIDATE_PAGE_SIZE,appendCandidates,applyCandidateWeightInput,availableCandidates,availableCandidateSelection,candidateMatches,candidatePage,moveCandidate,moveCandidateWithin,setCandidateWeight}=picker.namespace;

const providers=[{id:'p0',name:'主渠道'},{id:'p1',name:'备用渠道'}];
const base={name:'普通模型',upstream:'vendor/model',provider_id:'p0',protocol:'chat',supported_protocols:['chat'],enabled:true,tools:true,tools_unknown:false,vision:true,vision_unknown:false,max_output_tokens:65536,pricing_set:true,input_price:0,output_price:0,cache_price:0,write_price:0,context_window:262144};
const models=Array.from({length:1500},(_,i)=>({...base,id:`model-${i}`,name:`模型 ${i}`,provider_id:i%2?'p1':'p0'}));
models[120]={...models[120],id:'vendor-stealth-free',name:'隐身模型',upstream:'special/stealth',context_window:1048576,supported_protocols:['messages','responses']};
models[121]={...models[121],id:'unknown-capability',tools_unknown:true,vision_unknown:true,supported_protocols:[]};
models[122]={...models[122],id:'paid',input_price:0.1};
const groups=[{name:'主组',candidates:[{model_id:'model-0',weight:7,disabled:true}]},{name:'备用组',candidates:[{model_id:'model-1',weight:3,disabled:false}]}];

const weightTarget={dataset:{weight:'model-0'},value:''};
assert.equal(applyCandidateWeightInput(groups,0,weightTarget),true);
assert.equal(groups[0].candidates[0].weight,'');
weightTarget.value='27';
assert.equal(applyCandidateWeightInput(groups,0,weightTarget),true);
assert.equal(groups[0].candidates[0].weight,'27');
assert.equal(setCandidateWeight(groups[0].candidates[0],weightTarget.value),27);

assert.equal(candidateMatches(models[120],'主渠道',{q:'STEALTH',protocol:'responses',free:true,stealth:true,minContext:1000000,tools:true,vision:true,out64k:true,priced:true,enabled:true}),true);
assert.equal(candidateMatches(models[120],'主渠道',{q:'vendor-stealth-free'}),true);
assert.equal(candidateMatches({...models[120],name:'独特名称'},'主渠道',{q:'独特'}),true);
assert.equal(candidateMatches(models[121],'备用渠道',{tools:true}),false);
assert.equal(candidateMatches(models[121],'备用渠道',{vision:true}),false);
assert.equal(candidateMatches(models[121],'备用渠道',{protocol:'chat'}),false);
assert.equal(candidateMatches(models[122],'主渠道',{free:true}),false);
assert.equal(candidateMatches({...models[120],pricing_set:false},'主渠道',{free:true}),false);
assert.equal(candidateMatches({...models[120],cache_price:0.01},'主渠道',{free:true}),false);
assert.equal(candidateMatches({...models[120],context_window:undefined},'主渠道',{minContext:128000}),false);
assert.equal(candidateMatches(models[120],'主渠道',{provider:'p0',q:'主渠道'}),true);

const all=availableCandidates(models,providers,groups,{});
assert.equal(all.length,1498);
assert.equal(all.some(m=>m.id==='model-0'||m.id==='model-1'),false);
assert.deepEqual([...availableCandidateSelection(models,providers,groups,{},['model-0','model-2'])].map(m=>m.id),['model-2']);
const page=candidatePage(all,2);
assert.equal(CANDIDATE_PAGE_SIZE,50);
assert.equal(page.items.length,50);
assert.equal(page.page,2);
assert.equal(candidatePage(all,999).items.length,48);
assert.equal(candidatePage(models,1).items.length,50);

const target={name:'新组',candidates:[]};
assert.equal(appendCandidates(target,models.slice(10,70)).length,60);
assert.equal(appendCandidates(target,models.slice(10,20)).length,0);
assert.deepEqual({...target.candidates[0]},{model_id:'model-10',weight:10,disabled:false});
setCandidateWeight(target.candidates[0],22.4);
assert.equal(target.candidates[0].weight,22);
assert.equal(moveCandidate(groups,0,'model-0',1),true);
assert.deepEqual({...groups[1].candidates.at(-1)},{model_id:'model-0',weight:27,disabled:true});
assert.equal(groups[0].candidates.length,0);
assert.equal(moveCandidate(groups,1,'model-0',0),true);
assert.equal(moveCandidateWithin(groups[0],'model-0',-1),false);
groups[0].candidates.push({model_id:'model-2',weight:4,disabled:false});
assert.equal(moveCandidateWithin(groups[0],'model-2',-1),true);
assert.deepEqual(groups[0].candidates.map(c=>[c.model_id,c.disabled]),[['model-2',false],['model-0',true]]);

console.log('候选选择器：1500 模型筛选、未知能力、免费、Stealth、上下文、排重、分页、批量添加与跨组保留全部通过');
