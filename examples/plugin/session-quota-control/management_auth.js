'use strict';
const managementAuth=(()=>{
  let key='',ready=true,connected=false,revision=0,bridgeRequested=false;
  const node=id=>document.getElementById(id);
  function showLogin(){
    connected=false;
    node('authRecovery').hidden=false;
    node('manualAuth').hidden=false;
    const native=parentBridge()?.state();
    node('authStatus').textContent=window.parent!==window&&!native?.bound?'主管理界面的登录衔接尚未就绪，请刷新主管理界面。':native?.connected?'主管理界面已有登录连接，插件请求未通过验证，请查看下方错误。':'请先在主管理界面登录，登录后返回本页即可自动连接。';
  }
  function parentBridge(){
    try{return window.parent!==window?window.parent.__CPA_PLUGIN_BRIDGE__:null}catch{return null}
  }
  function reconnect(value=''){
    key=value.trim();ready=true;connected=false;revision++;
    node('authStatus').textContent='正在连接…';
  }
  async function request(path,method='GET',body){
    const current=revision,headers={'Content-Type':'application/json'};
    if(key)headers.Authorization='Bearer '+key;
    const bridge=parentBridge(),native=bridge?.state();
    const response=!key&&native?.bound?await bridge.request(path,method,body):await fetch(path,{method,headers,credentials:'same-origin',body:body===undefined?undefined:JSON.stringify(body),cache:'no-store'});
    let data;try{data=await response.json()}catch{throw Error('服务器响应无法读取 ('+response.status+')')}
    if(!response.ok){
      if(current===revision&&(response.status===401||response.status===403)){
        ready=false;key='';showLogin();
        if(!bridgeRequested&&window.parent!==window){bridgeRequested=true;window.parent.postMessage({type:'cpa:request-plugin-session'},location.origin)}
      }
      const text=typeof data.error==='string'?data.error:JSON.stringify(data);
      throw Error(text==='management login required'?'尚未登录，请先登录主管理界面。':text);
    }
    if(current===revision){
      connected=true;node('manualAuth').hidden=true;node('key').value='';
      node('authRecovery').hidden=true;node('authStatus').textContent='';
    }
    return data;
  }
  function install(onConnect){
    node('connect').onclick=()=>{reconnect(node('key').value);onConnect()};
    node('key').onkeydown=event=>{if(event.key==='Enter')node('connect').click()};
    node('reconnect').onclick=()=>{reconnect();onConnect()};
    const resume=()=>{if(!ready&&!document.hidden){reconnect();onConnect()}};
    window.addEventListener('focus',resume);
    document.addEventListener('visibilitychange',resume);
    window.addEventListener('message',event=>{
      if(event.origin!==location.origin||window.parent===window||event.source!==window.parent)return;
      if(event.data?.type==='cpa:plugin-session-ready'&&!ready){reconnect();onConnect()}
      if(event.data?.type==='cpa:plugin-session-ended'){key='';ready=false;revision++;showLogin()}
    });
  }
  return {request,install,reconnect,get ready(){return ready},get connected(){return connected},get revision(){return revision}};
})();
