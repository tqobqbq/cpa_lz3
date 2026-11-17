(()=>{
  'use strict';
  const endpoint='/v8/management/session/plugin',originalFetch=window.fetch.bind(window),requests=new WeakMap();
  let authorization='',syncedAuthorization='',syncedAt=0,pending=null,generation=0,revocation=Promise.resolve();
  let authStore=null;
  /*__CREDENTIAL_QUOTA_SYNC__*/
  function nativeState(){
    const state=authStore?.getState();
    let sameOrigin=false;
    try{sameOrigin=!!state&&new URL(state.apiBase||location.origin,location.href).origin===location.origin}catch{}
    return {bound:!!authStore,connected:!!(state?.isAuthenticated&&state.managementKey),sameOrigin,quota:credentialQuotaSync.state()};
  }
  function pluginAPIPath(value){
    try{
      const url=new URL(value,location.origin);
      if(url.origin!==location.origin||/[\\%]/.test(url.pathname))return false;
      return /^\/v8\/management\/plugins\/[^/]+\/extension(?:\/|$)/.test(url.pathname)||/^\/v8\/management\/config\/plugins\/configs\/[^/]+(?:\/|$)/.test(url.pathname);
    }catch{return false}
  }
  async function pluginRequest(path,method='GET',body){
    const state=nativeState();
    if(!state.bound||!state.connected)return new Response(JSON.stringify({error:'management login required'}),{status:401,headers:{'Content-Type':'application/json'}});
    if(!state.sameOrigin||!pluginAPIPath(path))return new Response(JSON.stringify({error:'plugin request is outside the active management connection'}),{status:403,headers:{'Content-Type':'application/json'}});
    const current=authStore.getState();
    const response=await originalFetch(path,{method,headers:{Authorization:'Bearer '+current.managementKey,'Content-Type':'application/json'},credentials:'same-origin',body:body===undefined?undefined:JSON.stringify(body),cache:'no-store'});
    if(response.status===401)window.dispatchEvent(new Event('unauthorized'));
    return response;
  }
  function bindAuthStore(store){
    if(authStore||typeof store?.getState!=='function'||typeof store?.subscribe!=='function')return;
    authStore=store;let synchronized=false;
    const synchronize=()=>{
      const state=nativeState();
      if(state.connected&&state.sameOrigin){authorization='Bearer '+authStore.getState().managementKey;notify('cpa:plugin-session-ready');ensureSession();credentialQuotaSync.refresh()}
      else if(authorization||!synchronized)revoke();
      synchronized=true;
    };
    store.subscribe(synchronize);synchronize();
  }
  Object.defineProperty(window,'__CPA_PLUGIN_BRIDGE__',{value:Object.freeze({version:3,bindAuthStore,bindQuotaStore:credentialQuotaSync.bind,state:nativeState,request:pluginRequest})});
  function managementURL(value){
    try{const url=new URL(value,location.href);return url.origin===location.origin&&/^\/(v0|v8)\/management\//.test(url.pathname)}catch{return false}
  }
  function pluginFrame(frame){
    try{const url=new URL(frame.src,location.href);return url.origin===location.origin&&url.pathname.startsWith('/v0/resource/plugins/')}catch{return false}
  }
  function notify(type){
    for(const frame of document.querySelectorAll('iframe'))if(pluginFrame(frame))frame.contentWindow?.postMessage({type},location.origin);
  }
  function ensureSession(force=false){
    if(!authorization)return Promise.resolve(false);
    if(pending)return pending;
    if(!force&&authorization===syncedAuthorization&&Date.now()-syncedAt<10*3600000)return Promise.resolve(true);
    const key=authorization,current=generation;
    pending=revocation.then(()=>current===generation?originalFetch(endpoint,{method:'POST',headers:{Authorization:key},credentials:'same-origin',cache:'no-store'}):null)
      .then(async response=>{
        if(!response)return false;
        if(current!==generation){await originalFetch(endpoint,{method:'DELETE',credentials:'same-origin',cache:'no-store'});return false}
        if(!response.ok)return false;
        syncedAuthorization=key;syncedAt=Date.now();notify('cpa:plugin-session-ready');return true;
      }).catch(()=>false).finally(()=>{pending=null;if(authorization&&(authorization!==key||current!==generation))ensureSession()});
    return pending;
  }
  function observe(url,key,status,current){
    if(authStore||current!==generation||!key||!managementURL(url)||status<200||status>=300)return;
    authorization=key;ensureSession();
  }
  function revoke(){
    authorization='';syncedAuthorization='';syncedAt=0;generation++;
    revocation=originalFetch(endpoint,{method:'DELETE',credentials:'same-origin',cache:'no-store'}).catch(()=>{});
    notify('cpa:plugin-session-ended');
  }
  const open=XMLHttpRequest.prototype.open,setHeader=XMLHttpRequest.prototype.setRequestHeader,send=XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open=function(method,url){
    const result=open.apply(this,arguments);requests.set(this,{url,key:''});return result;
  };
  XMLHttpRequest.prototype.setRequestHeader=function(name,value){
    const result=setHeader.apply(this,arguments),request=requests.get(this);
    if(request&&String(name).toLowerCase()==='authorization')request.key=String(value);
    return result;
  };
  XMLHttpRequest.prototype.send=function(){
    const request=requests.get(this),current=generation;
    if(request?.key&&managementURL(request.url))this.addEventListener('loadend',()=>observe(request.url,request.key,this.status,current),{once:true});
    return send.apply(this,arguments);
  };
  window.fetch=function(input,init){
    const url=input instanceof Request?input.url:input,headers=new Headers(init?.headers??(input instanceof Request?input.headers:undefined)),current=generation;
    return originalFetch(input,init).then(response=>{observe(url,headers.get('Authorization'),response.status,current);return response});
  };
  window.addEventListener('message',event=>{
    if(event.origin!==location.origin||event.data?.type!=='cpa:request-plugin-session')return;
    if(!Array.from(document.querySelectorAll('iframe')).some(frame=>frame.contentWindow===event.source&&pluginFrame(frame)))return;
    ensureSession(true);
  });
  window.addEventListener('unauthorized',revoke);
  function checkLoginRoute(){
    if(authStore)return;
    const route=location.hash.replace(/^#/,''),path=location.pathname;
    if(/^\/login(?:[/?]|$)/.test(route)||/\/login\/?$/.test(path))revoke();
  }
  for(const method of ['pushState','replaceState']){
    const original=history[method];history[method]=function(){const result=original.apply(this,arguments);checkLoginRoute();return result};
  }
  window.addEventListener('hashchange',checkLoginRoute);
  window.addEventListener('popstate',checkLoginRoute);
})();
