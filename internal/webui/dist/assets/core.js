// 状态与格式化。本模块不依赖任何其它业务模块，先于它们求值。

export const $=(q,root=document)=>root.querySelector(q);

export const $$=(q,root=document)=>[...root.querySelectorAll(q)];

export const E=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

export const number=n=>new Intl.NumberFormat('en-US',{maximumFractionDigits:0}).format(Number(n)||0);

export const compact=n=>Number(n)>=1e6?(n/1e6).toFixed(2)+'M':Number(n)>=1000?(n/1000).toFixed(1)+'K':number(n);

export const money=n=>'$'+Number(n||0).toLocaleString('en-US',{minimumFractionDigits:2,maximumFractionDigits:Number(n)>0&&Number(n)<.01?6:4});

export const dateTimeSec=n=>n?new Date(n).toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hour12:false}):'—';

export const dateTime=n=>n?new Date(n).toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false}):'—';

export const ms=n=>Number(n)>=1000?(Number(n)/1000).toFixed(2)+' s':Math.round(Number(n)||0)+' ms';

export const json=v=>JSON.stringify(v,null,2);

export const uid=()=>crypto.randomUUID?.()||Math.random().toString(36).slice(2);

export const pages=[['overview','总览','grid','WORKSPACE'],['providers','供应商','provider'],['models','模型库','model'],['routes','智能路由','route'],['playground','调试台','play'],['usage','额度与用量','usage','OBSERVABILITY'],['requests','请求记录','request'],['sessions','会话亲和','session'],['jobs','后台任务','job'],['keys','访问密钥','key','SYSTEM'],['settings','系统设置','settings']];

export const state={refreshTimer:null,dialogSubmit:null,formError:null,secretValue:'',renderGeneration:0,
  page:'overview',version:'',config:null,data:null,range:'24h',paused:false,loading:false,routeID:'',routeDraft:null,simulation:null,requestPage:1,requestQ:'',requestStatus:'',requestProvider:'',modelFilters:{},simulateMode:'claude-code',usageShowAll:false,sessionModel:'',modelQ:'',modelProtocol:'',providerFilter:'',providerUsage:null,playProtocol:'chat',playResult:null,playBusy:false,playSession:uid(),lastSync:0
};



try{
  document.documentElement.dataset.theme=localStorage.getItem('prism-theme')||'light';

}
catch{

}
