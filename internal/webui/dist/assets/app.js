// 入口：外壳、路由、事件分发与启动流程。
import {rpc,setCSRF} from './api.js';
import {$,$$,E,json,ms,pages,state} from './core.js';
import {icon} from './icons.js';
import {avatar,badge,btn,checked,closeDialog,confirm,copy,empty,field,footer,getModel,nval,pillStatus,report,save,showDialog,tag,toast,val} from './ui.js';
import {aliasEditor,buildQuestions,connectionGuide,defaultQuestions,jobs,keyEditor,keys,modelEditor,models,moveCandidate,overview,playground,preservePlayground,providerEditor,providers,requests,routeEditor,routes,sessions,settings,usage} from './views.js';

export function login(){

 document.title='登录 · Prism Gateway';
  $('#app').innerHTML=`<div class="login-page"><section class="login-art"><div class="brand"><div class="brand-mark">${icon('prism')}</div><div><div class="brand-name">PRISM</div><div class="brand-caption">Agent control plane</div></div></div><div class="login-art-main"><div class="login-orbit">${icon('prism')}<span class="orbit-chip one">OpenAI</span><span class="orbit-chip two">Anthropic</span><span class="orbit-chip three">OpenCode Go</span></div><div class="eyebrow">ONE GATEWAY. YOUR MODELS.</div><h1>连接每一种能力。<br>掌控每一次调用。</h1><p>为 Coding Agent 而设计的轻量控制台。<br>把协议、路由与预算收进一个安静、有序的工作空间。</p></div><div class="login-art-foot"><span>GO BINARY · SQLITE · EMBED.FS</span><span>${state.version?'v'+E(state.version):''}</span></div></section><section class="login-form-wrap"><form class="login-form" id="login-form"><div class="eyebrow">WELCOME TO YOUR WORKSPACE</div><h2>回到你的控制台</h2><p>使用启动时终端显示的管理员令牌登录。<br>它与供应商 API Key、客户端调用 Key 相互独立。</p><div id="login-error" class="form-error hidden"></div>${field('管理员令牌','token','','password','令牌仅用于换取 HttpOnly 会话，不会存入 localStorage。','placeholder="prism_admin_…" autocomplete="current-password" required')}<button class="btn primary full" type="submit">进入工作空间 ${icon('arrow')}</button><div class="login-note">首次运行？复制终端中的 <code>prism_admin_…</code>。<br>遗失令牌：停止进程后，添加 <code>--reset-admin</code> 重启。<br>数据库配置与已保存的上游密钥不会因此被删除。</div><div class="login-security">${icon('shield')}默认本机访问 · 同源管理 API · CSRF 校验</div></form></section></div>`;

 $('#login-form').addEventListener('submit',async ev=>{ev.preventDefault();const b=$('button[type=submit]',ev.currentTarget);b.disabled=true;try{const data=await rpc('auth.login',{token:val(ev.currentTarget,'token')});setCSRF(data.csrf);await enterApp();}catch(e){const box=$('#login-error');if(box){box.textContent=e.message;box.classList.remove('hidden');}else{report(e);}}finally{if(b.isConnected)b.disabled=false;}});

}

export function shell(){

 $('#app').innerHTML=`<div class="app-shell"><aside class="sidebar"><a class="brand" href="#overview" aria-label="Prism 总览"><div class="brand-mark">${icon('prism')}</div><div><div class="brand-name">PRISM</div><div class="brand-caption">Agent control plane</div></div></a><nav>${pages.map(([id,title,ico,group])=>`${group?`<div class="nav-group">${group}</div>`:''}<a class="nav-item ${state.page===id?'active':''}" href="#${id}" data-nav="${id}">${icon(ico)}<span>${title}</span>${id==='playground'?'<span class="nav-tag">LAB</span>':''}</a>`).join('')}</nav><div class="sidebar-footer"><div class="database-pill">${icon('database')}<div><strong>SQLite · 本地持久化</strong><small class="mono">POST /api.json</small></div><span class="dot positive" style="margin-left:auto"></span></div><div class="sidebar-bottom"><span>Prism ${state.version?'v'+E(state.version):''}</span><button class="icon-btn" data-action="guide" aria-label="接入指南">${icon('help')}</button></div></div></aside><div class="loading-line" id="loading-line"></div><header class="topbar"><div class="flex"><button class="icon-btn mobile-menu" data-action="mobile-menu" aria-label="打开导航">${icon('menu')}</button><div class="crumbs"><span class="workspace-pill"><span class="workspace-square">${icon('grid')}</span>${E(state.config?.settings.app_name||'Prism Gateway')}</span><span class="crumb-divider">/</span><span class="crumb-current" id="crumb-current">总览</span></div></div><div class="topbar-right"><button class="command-search" data-action="palette">${icon('search')}<span>跳转页面或执行操作</span><kbd>⌘ K</kbd></button><button class="icon-btn" data-action="theme" aria-label="切换明暗主题">${icon(document.documentElement.dataset.theme==='dark'?'sun':'moon')}</button><button class="icon-btn" data-action="logout" aria-label="退出登录">${icon('logout')}</button><span class="avatar" title="Workspace administrator">AD</span></div></header><main class="main" id="main"></main></div>`;

}

export async function enterApp(){
  state.config=await rpc('config.get');
  const p=location.hash.slice(1).split('?')[0];
  state.page=pages.some(x=>x[0]===p)?p:'overview';
  shell();
  await loadPage();
  clearInterval(state.refreshTimer);
  state.refreshTimer=setInterval(()=>{if(document.visibilityState==='visible'&&!state.paused&&!state.loading&&!$('dialog')&&!['INPUT','TEXTAREA','SELECT'].includes(document.activeElement?.tagName)&&['overview','usage','requests','jobs','sessions'].includes(state.page))loadPage(false).catch(report);},10000);

}

export async function loadPage(animated=true){

 const gen=++state.renderGeneration;
  state.loading=true;
  $('#loading-line')?.classList.add('show');

 try{

  const c=await rpc('config.get');
    if(gen!==state.renderGeneration)return;
    state.config=c;

  let action='',params={

    };
    switch(state.page){
      case 'overview':case 'usage':action='dashboard.get';
      params={
        range:state.range
      };
      break;
      case 'requests':action='request.list';
      params={
        page:state.requestPage,page_size:30,q:state.requestQ,status:state.requestStatus
      };
      break;
      case 'sessions':action='session.list';
      break;
      case 'jobs':action='job.list';
      break;
      case 'keys':action='apikey.list';
      break;
      case 'settings':action='system.info';
      break;
      default:break;

    }

  const data=action?await rpc(action,params):null;
    if(gen!==state.renderGeneration)return;
    state.data=data;
    state.lastSync=Date.now();
    renderPage(animated);

  }
  finally{
    if(gen===state.renderGeneration){
      state.loading=false;
      $('#loading-line')?.classList.remove('show');

    }
  }

}

export function renderPage(animated=false){

 const label=pages.find(p=>p[0]===state.page)?.[1]||'总览';
  document.title=`${label} · Prism Gateway`;
  $('#crumb-current').textContent=label;
  $$('[data-nav]').forEach(e=>e.classList.toggle('active',e.dataset.nav===state.page));

 const views={
    overview:overview,providers:providers,models:models,routes:routes,playground:playground,usage:usage,requests:requests,sessions:sessions,jobs:jobs,keys:keys,settings:settings
  };

 const paint=()=>{
    $('#main').innerHTML=`<div class="${animated?'page-enter':''}">${views[state.page]()}</div>${footer()}`;
    bindPage();

  };

 paint();
  if(animated&&!matchMedia('(prefers-reduced-motion: reduce)').matches)$('#main>div')?.animate([{opacity:0,transform:'translateY(5px)'},{opacity:1,transform:'translateY(0)'}],{duration:180,easing:'ease-out'});

}

export async function navigate(page){
  if(!pages.some(x=>x[0]===page))return;
  closeDialog();
  $('.sidebar')?.classList.remove('open');
  if(state.page===page)return;
  state.page=page;
  state.simulation=null;
  state.routeDraft=null;
  await loadPage();

}

export function bindPage(){

 $('#model-search')?.addEventListener('input',ev=>{const pos=ev.target.selectionStart;state.modelQ=ev.target.value;renderPage(false);const x=$('#model-search');x.focus();x.setSelectionRange(pos,pos);});

 $('#model-protocol')?.addEventListener('change',ev=>{state.modelProtocol=ev.target.value;renderPage(false);});
 // 题型决定要不要显示选项框，换了就得重绘；先把正在编辑的内容读回去
 $$('[name=q-type]').forEach(el=>el.addEventListener('change',()=>{preservePlayground();renderPage(false);}));

 $('#model-provider')?.addEventListener('change',ev=>{state.providerFilter=ev.target.value;renderPage(false);});

 $('#request-filter')?.addEventListener('submit',ev=>{ev.preventDefault();state.requestQ=val(ev.currentTarget,'q');state.requestStatus=val(ev.currentTarget,'status');state.requestPage=1;loadPage(false).catch(report);});

 $('#request-search')?.addEventListener('change',ev=>{state.requestQ=ev.target.value;state.requestPage=1;loadPage(false).catch(report);});

 $('#request-status')?.addEventListener('change',ev=>{state.requestStatus=ev.target.value;state.requestPage=1;loadPage(false).catch(report);});

 $('#settings-form')?.addEventListener('submit',async ev=>{ev.preventDefault();const f=ev.currentTarget,b=$('button[type=submit]',f);b.disabled=true;try{await save('settings.update',{settings:{app_name:val(f,'app_name'),default_route:val(f,'default_route'),retention_days:nval(f,'retention_days'),max_body_mb:nval(f,'max_body_mb'),global_concurrency:nval(f,'global_concurrency'),session_ttl_hours:nval(f,'session_ttl_hours'),allow_estimated_count:checked(f,'allow_estimated_count'),listen:val(f,'listen'),metrics_enabled:checked(f,'metrics_enabled'),log_level:val(f,'log_level'),log_format:val(f,'log_format')}},f);}catch(e){report(e);b.disabled=false;}});

 $('#playground-form')?.addEventListener('submit',async ev=>{ev.preventDefault();preservePlayground();state.playBusy=true;renderPage(false);try{const args={protocol:state.playProtocol,model:state.lastPlayModel,session:state.playSession};
  if(state.playProtocol==='systemone'){args.state=state.playState;args.questions=buildQuestions(state.playQuestions||defaultQuestions());}
  else args.prompt=state.lastPrompt;
  state.playResult=await rpc('playground.run',args);}catch(e){report(e);}finally{state.playBusy=false;if(state.page==='playground')renderPage(false);}});

 let from=-1;
  $$('[data-drag-index]').forEach(el=>{el.addEventListener('dragstart',ev=>{from=Number(el.dataset.dragIndex);ev.dataTransfer.effectAllowed='move';ev.dataTransfer.setData('text/plain',String(from));el.classList.add('dragging');});el.addEventListener('dragend',()=>el.classList.remove('dragging'));el.addEventListener('dragover',ev=>{ev.preventDefault();ev.dataTransfer.dropEffect='move';});el.addEventListener('drop',ev=>{ev.preventDefault();moveCandidate(from,Number(el.dataset.dragIndex));});});

}

export function palette(){
  closeDialog();
  const d=document.createElement('dialog');
  d.className='palette-dialog';
  d.innerHTML=`<div class="palette-search">${icon('search')}<input id="command-search" placeholder="跳转页面，或执行一个操作…" autocomplete="off" aria-label="命令搜索"><kbd>ESC</kbd></div><div id="command-results"></div><div class="palette-hint">↑ ↓ 选择 · Enter 执行 · Esc 关闭</div>`;
  $('#overlay-root').append(d);
  d.showModal();
  let idx=0;
  const commands=[...pages.map(([id,title,ico])=>({label:'打开 '+title,ico,run:()=>{location.hash=id;}})),{label:'添加供应商',ico:'provider',run:()=>providerEditor()},{label:'添加模型',ico:'model',run:()=>modelEditor()},{label:'创建智能路由',ico:'route',run:()=>routeEditor()},{label:'创建 API Key',ico:'key',run:()=>keyEditor()},{label:'切换明亮 / 深色主题',ico:'sun',run:()=>toggleTheme()}];
  let filtered=commands;
  function paint(){
    const q=$('#command-search',d).value.toLowerCase();
    filtered=commands.filter(x=>x.label.toLowerCase().includes(q));
    idx=Math.min(idx,Math.max(0,filtered.length-1));
    $('#command-results',d).innerHTML=filtered.map((x,i)=>`<button class="palette-item ${i===idx?'selected':''}" data-command="${i}">${icon(x.ico)}<span>${E(x.label)}</span>${icon('arrow')}</button>`).join('')||'<div class="empty small">没有找到匹配命令</div>';
    $$('[data-command]',d).forEach(el=>el.onclick=()=>run(Number(el.dataset.command)));

  }
  function run(i){
    const c=filtered[i];
    if(c){
      d.close();
      d.remove();
      c.run();

    }
  }
  $('#command-search',d).oninput=()=>{
    idx=0;
    paint();

  };
  d.addEventListener('keydown',ev=>{if(ev.key==='ArrowDown'||ev.key==='ArrowUp'){ev.preventDefault();idx=(idx+(ev.key==='ArrowDown'?1:-1)+filtered.length)%Math.max(1,filtered.length);paint();$$('.palette-item',d)[idx]?.scrollIntoView({block:'nearest'});}else if(ev.key==='Enter'){ev.preventDefault();run(idx);}});
  d.addEventListener('close',()=>d.remove());
  paint();
  $('#command-search',d).focus();

}

export function toggleTheme(){
  const t=document.documentElement.dataset.theme==='dark'?'light':'dark';
  document.documentElement.dataset.theme=t;
  try{
    localStorage.setItem('prism-theme',t);

  }
  catch{

  }
}

export async function handleAction(el){
  const action=el.dataset.action,id=el.dataset.id||'';
  switch(action){

 case 'close-dialog':closeDialog();
    break;

 case 'mobile-menu':$('.sidebar')?.classList.toggle('open');
    break;

 case 'palette':palette();
    break;

 case 'theme':toggleTheme();
    break;

 case 'logout':await rpc('auth.logout');
    clearInterval(state.refreshTimer);
    closeDialog();
    login();
    break;

 case 'refresh':await loadPage(false);
    toast('已刷新');
    break;

 case 'pause':state.paused=!state.paused;
    renderPage(false);
    break;

 case 'range':state.range=el.dataset.value;
    await loadPage(false);
    break;

 case 'guide':showDialog('客户端接入指南','管理调用走 /api.json；模型调用保持各自协议。',connectionGuide(),null,'',true);
    break;

 case 'add-provider':providerEditor();
    break;

 case 'edit-provider':providerEditor(id);
    break;

 case 'add-model':modelEditor();
    break;

 case 'edit-model':modelEditor(id);
    break;

 case 'add-route':routeEditor();
    break;

 case 'edit-route':routeEditor(id);
    break;

 case 'add-alias':aliasEditor();
    break;

 case 'new-key':keyEditor();
    break;

 case 'copy-secret':await copy($('.secret-box')?.textContent||'');
    break;

 case 'copy-openai':await copy(location.origin+'/openai/v1');
    break;

 case 'copy-anthropic':await copy(location.origin+'/anthropic');
    break;

 case 'copy-base':await copy(el.dataset.value||'');
    break;

 case 'enable-demo':if(await confirm('启用本地 Sandbox？','将添加明确标注的本地演示供应商、三个演示模型与 demo-auto 路由。没有真实 AI 推理，也不会产生云端费用。','启用演示')){
      const c=await rpc('demo.enable',{version:state.config.version});
      state.config=c;
      await loadPage(false);
      toast('本地 Sandbox 已启用');

    }
    break;

 case 'toggle-model':{
      const m=getModel(id);
      await save('model.save',{id,model:{enabled:!m.enabled}});
      break;

    }

 case 'test-provider':{
      el.disabled=true;
      try{
        const r=await rpc('provider.test',{id});
        toast(`HTTP ${r.status} · ${ms(r.latency_ms)} · ${r.message}`,r.status>=400);

      }
      finally{
        el.disabled=false;

      }
      break;

    }

 case 'sync-provider':{
      el.disabled=true;
      try{
        await rpc('provider.sync_models',{id});
        toast('同步任务已创建，可在后台任务中查看进度');

      }
      finally{
        el.disabled=false;

      }
      break;

    }

 case 'delete-provider':case 'delete-model':case 'delete-route':case 'delete-alias':{
      const kind=action.slice(7);
      if(await confirm('删除这个配置？','删除会检查引用关系。仍被模型、路由或别名引用的对象不能删除；历史用量记录会保留。','删除'))await save(kind+'.delete',{id});
      break;

    }

 case 'select-route':state.routeID=id;
    state.routeDraft=null;
    state.simulation=null;
    renderPage(false);
    break;

 case 'candidate-up':moveCandidate(Number(el.dataset.index),Number(el.dataset.index)-1);
    break;

 case 'candidate-down':moveCandidate(Number(el.dataset.index),Number(el.dataset.index)+1);
    break;

 case 'save-route-order':await save('route.save',{id:state.routeID,route:{candidates:state.routeDraft}});
    break;

 case 'simulate-route':state.simulation=await rpc('route.test',{id,protocol:'chat'});
    renderPage(false);
    break;

 case 'play-protocol':preservePlayground();
    state.playProtocol=el.dataset.value;
    state.playResult=null;
    renderPage(false);
    break;

 case 'add-question':preservePlayground();
    state.playQuestions=[...(state.playQuestions||defaultQuestions()),{key:'',type:'choice',instructions:'',criteria:''}];
    renderPage(false);
    break;

 case 'remove-question':preservePlayground();
    state.playQuestions=(state.playQuestions||[]).filter((_,i)=>i!==Number(el.dataset.index));
    renderPage(false);
    break;

 case 'request-detail':{
      const r=await rpc('request.get',{id});
      showDialog('请求诊断',id,`<div class="flex" style="margin-bottom:20px">${pillStatus(r.status)}${badge(r.protocol)}${r.is_demo?tag('本地演示','warning'):''}</div><div class="dialog-note">只记录元数据，不保存 prompt、回答或工具参数。费用未知时不会假装为零。</div><pre>${E(json(r))}</pre>`,null,'',true);
      break;

    }

 case 'prev-requests':case 'request-prev':if(state.requestPage>1){
      state.requestPage--;
      await loadPage(false);

    }
    break;

 case 'next-requests':case 'request-next':state.requestPage++;
    await loadPage(false);
    break;

 case 'unbind-session':case 'delete-session':if(await confirm('解除本地会话亲和？','只删除网关的路由映射，不会清除供应商缓存，也不会重置上游额度。','解除映射')){
      await rpc('session.delete',{id});
      await loadPage(false);

    }
    break;

 case 'revoke-key':if(await confirm('撤销客户端密钥？','新请求会立即失去访问权限，已开始的请求不会被强制中断。此操作不可恢复。','撤销')){
      await rpc('apikey.revoke',{id});
      await loadPage(false);

    }
    break;

 case 'audit':{
      const rows=await rpc('audit.list');
      showDialog('配置与安全审计','最近 200 条操作记录；不包含密钥正文。',`<pre>${E(json(rows))}</pre>`,null,'',true);
      break;

    }

 default:if(action.startsWith('go-')){
      location.hash=action.slice(3);

    }
    break;

  }
}


window.addEventListener('hashchange',()=>navigate(location.hash.slice(1)).catch(report));

document.addEventListener('click',ev=>{const el=ev.target.closest('[data-action]');if(!el||el.disabled)return;ev.preventDefault();handleAction(el).catch(report);});

document.addEventListener('keydown',ev=>{if((ev.metaKey||ev.ctrlKey)&&ev.key.toLowerCase()==='k'&&state.config&&!$('.login-page')){ev.preventDefault();palette();}});

(async()=>{try{const x=await rpc('auth.status');state.version=x.version||'';if(x.authenticated){setCSRF(x.csrf);await enterApp();}else login();}catch(e){login();toast('无法连接管理接口，请确认网关正在运行。',true);}})();
