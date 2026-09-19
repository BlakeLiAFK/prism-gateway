/** A single management transport. Never auto-retry mutation requests. */
export class RPCError extends Error {

  constructor(message, code, requestID, status) {
     super(message);
     this.code=code;
     this.requestID=requestID;
     this.status=status;

  }

}

let csrf='';

export function setCSRF(value) {
   csrf=value||'';

}

export async function rpc(action,params={},options={}) {

  const controller=new AbortController();

  const timer=setTimeout(()=>controller.abort(),options.timeout||330000);

  try {

    const res=await fetch('/api.json',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json','X-Prism-CSRF':csrf},body:JSON.stringify({action,params}),signal:options.signal||controller.signal});

    let body;
    try{
      body=await res.json();

    }
    catch{
      throw new RPCError('服务器没有返回有效 JSON','INVALID_RESPONSE','',res.status);

    }

    if(!res.ok||!body.ok)throw new RPCError(body.error?.message||'请求失败',body.error?.code||'ERROR',body.request_id,res.status);

    return body.data;

  }
   catch(e) {
     if(e.name==='AbortError')throw new RPCError('请求已超时或取消；写操作可能已经生效，请刷新确认','TIMEOUT','',0);
    throw e;

  }

  finally{
    clearTimeout(timer);

  }

}
