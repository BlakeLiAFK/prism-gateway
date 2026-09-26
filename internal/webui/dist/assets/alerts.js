// 系统设置里的「告警推送」卡片：Telegram / Webhook。凭证只写不读，接口只告诉界面是否已设置。
import {rpc} from './api.js';
import {$,E,state} from './core.js';
import {btn,toast} from './ui.js';

const EVENTS=[['quota','上游额度用完','Command Code、Z.AI 的额度窗口用满，旗下模型冷却到重置时刻'],['failures','模型连续失败','同一模型连续 5 次请求失败（客户端取消不算）'],['route','路由无可用候选','某条路由的全部候选都因容量或上游问题失败']];

export const alertCard=()=>'<section class="card setting-card" id="alert-card" style="margin-top:22px"><h2>告警推送</h2><div class="skeleton skeleton-line" style="margin-top:14px"></div></section>';

function render(c){
  const telegram=c.kind!=='webhook'&&c.kind!=='webhook_text';
  return `<div class="between" style="margin-bottom:9px"><h2>告警推送</h2><label class="checkline"><input type="checkbox" id="alert-enabled" ${c.enabled?'checked':''}><span>启用</span></label></div><p class="small muted">同一事件 30 分钟内只推送一次；推送失败不影响请求处理。JSON 方式发送 <span class="mono">{"event","text","at"}</span>；纯文本方式原样发送消息正文（text/plain，保留换行）。</p><div class="form-grid" style="margin-top:14px"><div class="field"><label for="alert-kind">推送方式</label><select id="alert-kind"><option value="telegram" ${telegram?'selected':''}>Telegram Bot</option><option value="webhook" ${c.kind==='webhook'?'selected':''}>Webhook（POST JSON）</option><option value="webhook_text" ${c.kind==='webhook_text'?'selected':''}>Webhook（POST 纯文本）</option></select></div><div class="field"><label for="alert-secret" id="alert-secret-label">${telegram?'Bot Token':'Webhook 地址'}</label><input id="alert-secret" type="password" autocomplete="off" placeholder="${c.has_secret?'已设置，留空表示不修改':''}"></div><div class="field" id="alert-chat-field" ${telegram?'':'hidden'}><label for="alert-chat">Chat ID</label><input id="alert-chat" value="${E(c.chat_id||'')}" placeholder="例如 123456789"></div></div><div class="alert-events">${EVENTS.map(([k,l,h])=>`<label class="checkline"><input type="checkbox" name="alert-event" value="${k}" ${c.events?.[k]?'checked':''}><span>${E(l)}<small>${E(h)}</small></span></label>`).join('')}</div><div class="flex" style="margin-top:14px">${btn('保存告警设置','alert-save','check','type="button"','primary small')}${btn('发送测试消息','alert-test','bolt','type="button"','small')}</div>`;
}

// 设置页渲染后调用：取配置并填充卡片
export function bindAlerts(){
  const card=$('#alert-card');
  if(!card)return;
  rpc('alert.get').then(c=>{
    if(!card.isConnected)return;
    card.innerHTML=render(c);
    $('#alert-kind',card).addEventListener('change',ev=>{const tg=ev.target.value==='telegram';$('#alert-chat-field',card).hidden=!tg;$('#alert-secret-label',card).textContent=tg?'Bot Token':'Webhook 地址';});
  }).catch(e=>{card.innerHTML=`<h2>告警推送</h2><p class="small negative">${E(e.message||String(e))}</p>`;});
}

export const alertActions={
  'alert-save':async()=>{
    const card=$('#alert-card');
    const events={};card.querySelectorAll('[name=alert-event]').forEach(x=>{events[x.value]=x.checked;});
    const c=await rpc('alert.save',{enabled:$('#alert-enabled',card).checked,kind:$('#alert-kind',card).value,secret:$('#alert-secret',card).value,chat_id:$('#alert-chat',card).value,events});
    card.innerHTML=render(c);toast('告警设置已保存');bindAlerts();
  },
  'alert-test':async()=>{await rpc('alert.test');toast('测试消息已发送，请查看推送目标');}
};
