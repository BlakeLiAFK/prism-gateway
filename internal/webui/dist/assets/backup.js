// 系统设置里的「备份与还原」卡片：R2 存储配置、本机与 R2 备份列表、在线还原。
import {rpc} from './api.js';
import {$,E,dateTime} from './core.js';
import {btn,confirm,toast} from './ui.js';

const MB=n=>(Number(n||0)/1048576).toFixed(1)+' MB';

export const backupCard=()=>'<section class="card setting-card" id="backup-card" style="margin-top:22px"><h2>备份与还原</h2><div class="skeleton skeleton-line" style="margin-top:14px"></div></section>';

function r2Form(c){
  const f=(id,label,value,extra='')=>`<div class="field"><label for="r2-${id}">${label}</label><input id="r2-${id}" value="${E(value||'')}" autocomplete="off" ${extra}></div>`;
  return `<div class="form-grid" style="margin-top:14px">${f('endpoint','Endpoint',c.endpoint,'placeholder="https://<账户ID>.r2.cloudflarestorage.com"')}${f('bucket','存储桶',c.bucket)}${f('access','Access Key ID',c.access_key_id)}<div class="field"><label for="r2-secret">Secret Access Key</label><input id="r2-secret" type="password" autocomplete="off" placeholder="${c.has_secret?'已设置，留空表示不修改':''}"></div>${f('prefix','对象前缀',c.prefix,'placeholder="prism/"')}${f('region','Region',c.region,'placeholder="auto"')}</div><div class="flex" style="margin-top:14px">${btn('保存 R2 配置','r2-save','check','type="button"','primary small')}${btn('测试连接','r2-test','bolt','type="button"','small')}</div>`;
}

function list(title,rows,source,tools){
  const body=rows===null?'<p class="small muted" style="padding:10px 0">R2 未配置，保存配置后显示远程备份。</p>'
    :typeof rows==='string'?`<p class="small negative" style="padding:10px 0">${E(rows)}</p>`
    :!rows.length?'<p class="small muted" style="padding:10px 0">暂无备份</p>'
    :`<div class="backup-list">${rows.map(r=>`<div class="backup-row"><div><div class="mono small">${E(r.label)}</div><div class="tiny muted">${dateTime(r.at)} · ${MB(r.bytes)}</div></div>${btn('还原','backup-restore','',`type="button" data-source="${source}" data-name="${E(r.name)}"`,'small')}</div>`).join('')}</div>`;
  return `<div class="backup-section"><div class="between"><h3>${title}</h3>${tools}</div>${body}</div>`;
}

function render(c,local,remote){
  return `<h2>备份与还原</h2><p class="small muted" style="margin-top:6px">R2 备份在内存中生成快照并压缩后直接上传，本机不落盘；定时上传在上方「定时任务 → 远程备份」开启。还原在线完成、无需重启：先自动生成一份本机安全快照，管理员令牌与登录状态保持不变。备份不含 <span class="mono">.key</span> 主密钥，换机器还原时必须带上原主密钥。</p>${r2Form(c)}<div class="backup-lists">${list('本机备份',local,'local',btn('立即备份','backup-now','plus','type="button"','small'))}${list('R2 备份',remote,'remote',c.configured?btn('立即上传','backup-remote-now','plus','type="button"','small'):'')}</div>`;
}

// 设置页渲染后调用；R2 列表要访问外网，失败只影响这一栏
export async function bindBackup(){
  const card=$('#backup-card');
  if(!card)return;
  try{
    const [c,local]=await Promise.all([rpc('r2.get'),rpc('backup.list')]);
    const localRows=local.map(b=>({name:b.name,label:b.name,at:b.modified_at,bytes:b.bytes}));
    let remote=null;
    if(c.configured){
      try{remote=(await rpc('backup.remote_list')).map(o=>({name:o.key,label:o.key.split('/').pop(),at:o.at,bytes:o.size}));}
      catch(e){remote=e.message||String(e);}
    }
    if(card.isConnected)card.innerHTML=render(c,localRows,remote);
  }catch(e){card.innerHTML=`<h2>备份与还原</h2><p class="small negative">${E(e.message||String(e))}</p>`;}
}

export const backupActions={
  'r2-save':async()=>{
    const v=id=>$('#r2-'+id).value;
    await rpc('r2.save',{endpoint:v('endpoint'),bucket:v('bucket'),access_key_id:v('access'),secret:v('secret'),prefix:v('prefix'),region:v('region')});
    toast('R2 配置已保存');bindBackup();
  },
  'r2-test':async()=>{await rpc('r2.test');toast('R2 连接正常：写入、列出、删除均成功');},
  'backup-now':async()=>{const r=await rpc('backup.create');toast('本机备份已生成：'+r.path.split('/').pop());bindBackup();},
  'backup-remote-now':async b=>{
    b.disabled=true;
    try{
      const r=await rpc('schedule.run',{task:'remote'});
      toast(r.status==='succeeded'?r.result:'上传失败：'+r.result,r.status!=='succeeded');bindBackup();
    }finally{b.disabled=false;}
  },
  'backup-restore':async b=>{
    const {source,name}=b.dataset;
    if(!await confirm('用这份备份覆盖当前数据？',`${name.split('/').pop()} 将覆盖当前全部配置、密钥与请求记录，备份之后的改动都会丢失。还原前会先在本机生成一份安全快照，可以用它再还原回来。`,'还原'))return;
    b.disabled=true;
    try{
      const r=await rpc('backup.restore',{source,name,confirm:true});
      toast(`已还原，安全快照：${r.safety_backup}。页面即将刷新`);
      setTimeout(()=>location.reload(),1500);
    }finally{b.disabled=false;}
  }
};
