// 系统设置里的「定时任务」卡片：每项任务的开关、参数、最近一次运行结果与「立即运行」。
import {rpc} from './api.js';
import {$,$$,E,dateTime} from './core.js';
import {btn,tag,toast} from './ui.js';

// [任务 ID, 名称, 说明, [[字段, 标签, 最小值, 最大值]]]
const TASKS=[
  ['backup','自动备份','每天生成一份数据库快照（data/backups/gateway-auto-*.db），只轮转自动备份，手动备份不动。快照不含 .key 主密钥，恢复时需配套原主密钥。',[['backup_hour','执行时刻 · 点',0,23],['backup_keep','保留份数',1,365]]],
  ['quota','额度预警','定时查询 Command Code、Z.AI 的窗口额度，用量达到阈值时推送；同一窗口每个重置周期只推一次。只读额度接口，不消耗额度。',[['quota_percent','阈值 · %',1,100],['quota_minutes','间隔 · 分钟',5,1440]]],
  ['report','每日日报','推送昨日的请求数、成功率、估算花费、常用模型、花费最多的 Key 与失败最多的模型。',[['report_hour','推送时刻 · 点',0,23]]],
  ['expiry','Key 到期提醒','每小时检查一次，设了到期时间的网关 Key 在到期前推送一次提醒。',[['expiry_days','提前天数',1,90]]],
  ['maintain','数据库维护','更新 SQLite 查询统计（PRAGMA optimize），并把 WAL 合并回主库后截断，防止 WAL 文件持续增大。',[['maintain_hour','执行时刻 · 点',0,23]]],
  ['upstream','上游检查','对照上游模型列表，找出可能已下架的模型与价格变动；只提示，不自动修改。有新发现时推送。',[['upstream_hours','间隔 · 小时',1,168]]]
];

export const scheduleCard=()=>'<section class="card setting-card" id="schedule-card" style="margin-top:22px"><h2>定时任务</h2><div class="skeleton skeleton-line" style="margin-top:14px"></div></section>';

function lastRun(r){
  if(!r?.at)return '<span class="muted">尚未运行</span>';
  const ok=r.status==='succeeded',skip=r.status==='skipped';
  return `${tag(ok?'成功':skip?'未推送':'失败',ok?'':skip?'warning':'error')} <span class="mono">${dateTime(r.at)}</span> · <span class="${ok||skip?'':'negative'}">${E(r.result||'正常，无需处理')}</span>`;
}

function render(d){
  const c=d.config,runs=d.runs||{};
  return `<div class="between"><h2>定时任务</h2><div class="sched-zone"><label for="sched-offset">时区 UTC</label><input id="sched-offset" type="number" min="-12" max="14" value="${c.utc_offset}"></div></div><p class="small muted" style="margin-top:6px">执行时刻按所设时区计算；每分钟检查一次，保存后下一分钟生效。推送走下方「告警推送」配置的通道。</p>${d.channel?'':'<div class="enable-note" style="margin-top:12px">推送通道未启用：额度预警、日报、到期提醒与上游变动都无法送达，请先在下方「告警推送」中配置并启用。</div>'}<div class="sched-list">${TASKS.map(([id,name,help,fields])=>`<div class="sched-row"><label class="checkline"><input type="checkbox" name="${id}_enabled" ${c[id+'_enabled']?'checked':''}><span><strong>${E(name)}</strong><small>${E(help)}</small></span></label><div class="sched-fields">${fields.map(([f,l,min,max])=>`<label><span>${E(l)}</span><input type="number" name="${f}" min="${min}" max="${max}" value="${E(c[f])}"></label>`).join('')}</div><div class="sched-last small">${lastRun(runs[id])}</div><div class="sched-run">${btn('立即运行','schedule-run','bolt',`type="button" data-task="${id}"`,'small')}</div></div>`).join('')}</div><div class="flex" style="margin-top:16px">${btn('保存定时任务','schedule-save','check','type="button"','primary small')}</div>`;
}

function fill(card,d){card.innerHTML=render(d);}

// 设置页渲染后调用：取配置并填充卡片
export function bindSchedule(){
  const card=$('#schedule-card');
  if(!card)return;
  rpc('schedule.get').then(d=>{if(card.isConnected)fill(card,d);})
    .catch(e=>{card.innerHTML=`<h2>定时任务</h2><p class="small negative">${E(e.message||String(e))}</p>`;});
}

export const scheduleActions={
  'schedule-save':async()=>{
    const card=$('#schedule-card'),p={utc_offset:Number($('#sched-offset',card).value)};
    $$('.sched-row input',card).forEach(x=>{p[x.name]=x.type==='checkbox'?x.checked:Number(x.value);});
    fill(card,await rpc('schedule.save',p));toast('定时任务已保存');
  },
  'schedule-run':async b=>{
    const task=b.dataset.task;
    b.disabled=true;
    try{
      const r=await rpc('schedule.run',{task});
      toast(r.status==='succeeded'?(r.result?'已完成：'+r.result.split('\n')[0]:'已完成，无需处理'):(r.status==='skipped'?'':'运行失败：')+r.result,r.status==='failed');
      bindSchedule();
    }finally{b.disabled=false;}
  }
};
