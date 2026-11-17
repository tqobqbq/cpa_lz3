const schedulingView=(()=>{
'use strict';
const base=__EXTENSION_BASE__,configPath=__CONFIG_PATH__,defaults=__DEFAULT_CONFIG__;
let state=null,busy=false,initialized=false,serverOffset=0;
const sessionRows=new Map(),affinityHistoryRows=new Map();
const $=id=>document.getElementById(id);
function element(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node}
function message(text){$('message').textContent=text||''}
function renderSettings(config){for(const field of $('settings').elements){if(!(field.name in defaults))continue;const value=config[field.name]??defaults[field.name];if(field.type==='checkbox')field.checked=value;else field.value=value}}
const api=(...args)=>managementAuth.request(...args);
function now(){return Date.now()+serverOffset}
function seconds(value){return Math.max(0,Math.ceil((Date.parse(value)-now())/1000))}
function duration(value){if(!Number.isFinite(value))return '等待观测';return value>=3600?(value/3600).toFixed(1)+' 小时':value>=60?Math.ceil(value/60)+' 分钟':value+' 秒'}
function timeLeft(parent,value,prefix){const label=element('span',prefix||'');const span=element('span');span.dataset.deadline=value;label.append(span);parent.append(label);return label}
function countdown(){document.querySelectorAll('[data-deadline]').forEach(node=>node.textContent=duration(seconds(node.dataset.deadline)))}
function credentialName(auth){if(!auth)return '未知 credential';return auth.label&&auth.name&&auth.label!==auth.name?auth.label+' · '+auth.name:auth.label||auth.name||auth.id}
function empty(body,cols,text){const row=element('tr'),cell=element('td',text,'empty');cell.colSpan=cols;row.append(cell);body.append(row)}
function affinityDetails(){const details=element('details',undefined,'affinity-details'),summary=element('summary','查看全部判断字段'),pre=element('pre');details.append(summary,pre);return {details,summary,pre}}
function updateAffinityDetails(view,data){const text=JSON.stringify(data,null,2);if(view.pre.textContent!==text)view.pre.textContent=text}
function renderSessionCandidates(view,pending,credentials){
  if(document.activeElement===view.select)return;
  const candidates=pending.candidates.map(candidate=>{
    const auth=credentials.get(candidate.id);
    return {id:candidate.id,label:credentialName(auth)+' · '+candidate.provider+' · 优先级 '+candidate.priority,disabled:!auth||auth.disabled};
  });
  const signature=JSON.stringify(candidates);if(signature===view.candidates)return;
  const selected=view.select.value,initial=view.candidates===undefined;
  view.select.replaceChildren();
  for(const candidate of candidates){const option=element('option',candidate.label);option.value=candidate.id;option.disabled=candidate.disabled;view.select.append(option);}
  view.select.value=initial?(candidates.find(candidate=>!candidate.disabled)?.id||''):selected;
  view.assign.disabled=!view.select.selectedOptions.length||view.select.selectedOptions[0].disabled;
  view.candidates=signature;
}
function renderSessions(credentials){
  const body=$('sessions'),pendingIDs=new Set(state.pending.map(pending=>pending.id));
  for(const [id,view] of sessionRows){if(!pendingIDs.has(id)){sessionRows.delete(id);view.row.remove();}}
  if(!state.pending.length){const placeholder=body.querySelector('.empty');if(placeholder)placeholder.textContent='没有等待手动分配的 session';else empty(body,5,'没有等待手动分配的 session');return;}
  body.querySelector('.empty')?.parentElement.remove();
  for(const pending of state.pending){
    let view=sessionRows.get(pending.id);
    if(!view){
      const row=element('tr'),identity=element('td'),session=element('code'),apiKey=element('code'),waiters=element('small'),reason=element('small');identity.append(session,apiKey,waiters,reason);
      const route=element('td'),provider=element('span'),model=element('small');route.append(provider,model);
      const wait=element('td'),deadline=element('span');wait.append(deadline);
      const action=element('td'),group=element('div',undefined,'actions'),select=element('select');select.dataset.id=pending.id;select.setAttribute('aria-label','选择 credential');
      const assign=element('button','分配'),release=element('button','自动调度','secondary');
      assign.onclick=()=>assignSession(pending.id,select.value,false);release.onclick=()=>assignSession(pending.id,'',true);
      select.onchange=()=>{assign.disabled=!select.selectedOptions.length||select.selectedOptions[0].disabled;};
      select.onblur=()=>queueMicrotask(()=>{
        const current=state.pending.find(session=>session.id===pending.id);
        if(current&&sessionRows.get(pending.id)===view)renderSessionCandidates(view,current,new Map(state.credentials.map(auth=>[auth.id,auth])));
      });
      const diagnostic=element('td'),fields=affinityDetails();diagnostic.append(fields.details);
      group.append(select,assign,release);action.append(group);row.append(identity,route,wait,action,diagnostic);
      view={row,session,apiKey,waiters,reason,provider,model,deadline,select,assign,fields};sessionRows.set(pending.id,view);body.append(row);
    }
    view.session.textContent=pending.session_id||'未提供 session 标识';view.waiters.textContent=pending.waiters+' 个等待请求';
    view.apiKey.textContent=pending.api_key?'API key：'+pending.api_key:'无客户端 API key';
    const previous=pending.affinity?.previous_auth_id;
    view.reason.textContent=previous?'原手动选择已不可用，请重选：'+credentialName(credentials.get(previous)):'';
    view.provider.textContent=pending.provider;view.model.textContent=pending.model;view.deadline.dataset.deadline=pending.deadline;
    renderSessionCandidates(view,pending,credentials);
    updateAffinityDetails(view.fields,pending);
  }
}
function renderAffinityHistory(){
  const body=$('affinityHistory'),history=state.affinity_history||[],ids=new Set(history.map(record=>record.session.id));
  for(const [id,view] of affinityHistoryRows){if(!ids.has(id)){view.details.remove();affinityHistoryRows.delete(id);}}
  if(!history.length){body.textContent='暂时没有已结束的待分配记录';return;}
  if(!affinityHistoryRows.size)body.replaceChildren();
  const results={manual_assignment:'已手动指定',automatic_release:'已放行原生调度',timeout:'已超时，回退原生调度',disabled:'已关闭手动分配',canceled:'请求已中断',access_denied:'访问权限已变更，结束等待'};
  for(const record of history){
    let view=affinityHistoryRows.get(record.session.id);
    if(!view){view=affinityDetails();view.summary.textContent=record.session.session_id||'未提供 session 标识';view.summary.append(element('span',record.session.model+' · '+(results[record.result]||record.result),'result'));affinityHistoryRows.set(record.session.id,view);body.prepend(view.details);}
    updateAffinityDetails(view,record);
  }
}
function render(){const credentials=new Map(state.credentials.map(auth=>[auth.id,auth]));$('pendingCount').textContent=state.pending.length;$('waitingCount').textContent=state.waiting;$('activeCount').textContent=state.in_flight;
renderSessions(credentials);
renderAffinityHistory();
const models=$('modelAverages');models.replaceChildren();
if(!state.models.length)empty(models,3,'收到可归因的额度变化后显示');
for(const model of state.models){const row=element('tr');row.append(element('td',model.provider+' / '+model.model),element('td',model.cost_percent.toFixed(4)+'%'),element('td',String(model.samples)));models.append(row);}
countdown();}
async function refresh(){if(!managementAuth.ready||busy)return;busy=true;const revision=managementAuth.revision;try{const next=await api(base+'/state');if(revision!==managementAuth.revision)return;state=next;serverOffset=Date.parse(state.now)-Date.now();$('connection').textContent='已连接 · 每 2 秒更新';if(!initialized){renderSettings(state.config);initialized=true}render()}catch(error){message(error.message);$('connection').textContent=managementAuth.ready?'连接异常':'请登录主管理界面'}finally{busy=false}}
async function assignSession(id,authID,release){try{await api(base+'/assign','POST',{id,auth_id:authID,release});message('');await refresh()}catch(error){message(error.message)}}
$('settings').onsubmit=async event=>{event.preventDefault();if(!managementAuth.connected){message('请先登录主管理界面并连接');return}const patch={};for(const field of event.target.elements){if(!field.name)continue;patch[field.name]=field.type==='checkbox'?field.checked:Number(field.value)}try{await api(configPath,'PATCH',patch);message('设置已保存，正在等待配置热更新。');setTimeout(()=>{initialized=false;refresh()},1500)}catch(error){message(error.message)}};
renderSettings(defaults);
setInterval(()=>{if(!document.hidden&&!$('dashboardPanel').hidden)refresh()},2000);
setInterval(()=>{if(!$('dashboardPanel').hidden)countdown()},1000);
return {refresh,reset(){initialized=false}};
})();
