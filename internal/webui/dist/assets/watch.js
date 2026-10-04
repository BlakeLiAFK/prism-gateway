// 系统设置「终端监控」：生成带令牌的 curl 命令，列出并撤销有效令牌。
// 令牌只能打开监控流，不能修改任何配置；完整令牌只在生成时显示一次。
import {rpc} from './api.js';
import {$,E,dateTime} from './core.js';
import {icon} from './icons.js';
import {btn,confirm,showDialog,toast} from './ui.js';

const MODES=[['dashboard','仪表盘：原地刷新，像 top'],['log','日志：逐行追加，可接 grep、tee']];
const DAYS=[[1,'1 天'],[7,'7 天'],[30,'30 天']];

export const watchCard=()=>'<section class="card setting-card" id="watch-card"><h2>终端监控</h2><div class="skeleton skeleton-line" style="margin-top:14px"></div></section>';

// 生成命令。自带断线重连：服务重启（比如部署）时流会结束，约 2 秒后重连；
// 令牌失效时服务返回 401，curl 以退出码 22 结束，循环随之停止。
// 仪表盘模式用 tput 取终端宽高，取不到时退回默认值。
export function watchCommand(token,mode,color){
  const params=mode==='dashboard'
    ?`{"mode":"dashboard","width":$(tput cols 2>/dev/null||echo 100),"height":$(tput lines 2>/dev/null||echo 40),"color":${color}}`
    :`{"mode":"log","color":${color}}`;
  const body=`{"action":"watch.stream","params":${params}}`.replace(/"/g,'\\"');
  return `while :; do curl -sN --fail-with-body ${location.origin}/api.json -H "Authorization: Bearer ${token}" -H "Content-Type: application/json" -d "${body}"; [ $? -eq 22 ] && break; sleep 2; done`;
}

function render(items){
  const rows=items.map(t=>`<tr><td>${E(t.name)}</td><td class="mono">${E(dateTime(t.created_at))}</td><td class="mono">${E(dateTime(t.expires_at))}</td><td class="mono">${t.last_used_at?E(dateTime(t.last_used_at)):'从未使用'}</td><td><div class="table-actions"><button class="icon-btn" data-action="watch-revoke" data-id="${E(t.id)}" aria-label="撤销令牌 ${E(t.name)}">${icon('trash')}</button></div></td></tr>`).join('');
  const list=items.length
    ?`<div class="table-scroll" style="margin-top:18px"><table><thead><tr><th>名称</th><th>创建</th><th>到期</th><th>最近使用</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`
    :'<p class="small muted" style="margin-top:18px">还没有有效的监控令牌。</p>';
  return `<div class="between" style="margin-bottom:9px"><h2>终端监控</h2>${items.length?`<span class="badge">${items.length} 个有效令牌</span>`:''}</div>
<p class="small muted">在终端里实时查看网关的状态与事件：生成一条带令牌的 curl 命令，粘贴到终端即可。事件只含元数据（模型、耗时、错误码），不含对话内容。令牌只读、可随时撤销，不能修改任何配置。</p>
<div class="form-grid" style="margin-top:14px">
<div class="field"><label for="watch-mode">显示方式</label><select id="watch-mode">${MODES.map(([v,l])=>`<option value="${v}">${E(l)}</option>`).join('')}</select></div>
<div class="field"><label for="watch-days">令牌有效期</label><select id="watch-days">${DAYS.map(([v,l])=>`<option value="${v}" ${v===7?'selected':''}>${E(l)}</option>`).join('')}</select></div>
<div class="field"><label for="watch-name">名称</label><input id="watch-name" maxlength="80" placeholder="例如：笔记本"></div>
</div>
<label class="checkline"><input type="checkbox" id="watch-color" checked><span>彩色输出<small>输出重定向到文件或交给其他程序处理时请关闭</small></span></label>
<div class="flex" style="margin-top:14px">${btn('生成命令','watch-create','code','type="button"','primary small')}</div>${list}`;
}

async function load(){
  const card=$('#watch-card');
  if(!card)return;
  try{
    const items=await rpc('watch.token_list');
    if(card.isConnected)card.innerHTML=render(items);
  }catch(e){
    if(card.isConnected)card.innerHTML=`<h2>终端监控</h2><p class="small negative">${E(e.message||String(e))}</p>`;
  }
}

// 设置页渲染后调用：取令牌列表并填充卡片
export const bindWatch=load;

export const watchActions={
  'watch-create':async()=>{
    const card=$('#watch-card');
    const mode=$('#watch-mode',card).value,color=$('#watch-color',card).checked;
    const made=await rpc('watch.token_create',{name:$('#watch-name',card).value.trim(),days:Number($('#watch-days',card).value)});
    showDialog('终端监控命令','令牌只显示这一次，关闭前请复制。',`<div class="banner">令牌只能打开监控流。命令会留在终端历史里，请不要分享；不再使用时回到这里撤销。</div><pre class="secret-box">${E(watchCommand(made.token,mode,color))}</pre>${btn('复制命令','copy-secret','copy','','primary')}<ul class="small muted" style="margin:16px 0 0 18px;line-height:1.8"><li>粘贴到终端运行，按 Ctrl+C 退出。</li><li>服务重启（例如部署）后约 2 秒自动重连；令牌被撤销或过期时停止。</li><li>仪表盘模式需要支持 ANSI 转义序列的终端，macOS 与 Linux 默认支持。</li></ul>`);
    load();
  },
  'watch-revoke':async el=>{
    if(!await confirm('撤销监控令牌？','使用这个令牌的终端会在几秒内断开，无法恢复。','撤销'))return;
    await rpc('watch.token_revoke',{id:el.dataset.id});
    toast('监控令牌已撤销');
    await load();
  }
};
