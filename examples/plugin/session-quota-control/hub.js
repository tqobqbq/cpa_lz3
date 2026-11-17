const hubView=(()=>{
  'use strict';
  const base=__EXTENSION_BASE__+'/hub', $=id=>document.getElementById(id), form=$('hubSettings');
  const fields=['key_id','model','effort','protocol','graphql_url','api_base_url','project_id','proxy_url'];
  let state=null, initialized=false, dirty=false, refreshing=false, submitting=false, generation=0, channelSignature='', historySignature='';
  const element=(tag,text,cls)=>{const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(cls)node.className=cls;return node};
  const date=value=>!value||value.startsWith('0001-')?'—':new Date(value).toLocaleString('zh-CN',{hour12:false});
  const message=(text,error=false)=>{$('hubMessage').textContent=text;$('hubMessage').className='dialog-message'+(error?' bad':'')};
  const passed=r=>r.status==='completed'&&r.model_match===true&&r.attribution?.used_outputs===3&&r.attribution?.decision!=='not_model';
  function ids(strict=false){
    const parts=$('hubChannelIDs').value.trim().split(/[\s,，;；]+/).filter(Boolean), values=parts.map(Number);
    if(values.some(n=>!Number.isSafeInteger(n)||n<=0)){if(strict)throw Error('渠道 ID 必须为正整数');return []}
    return [...new Set(values)];
  }
  function draft(){dirty=true;$('hubDraft').textContent='有未保存的修改';renderChannels();updateControls()}
  function settings(){
    const value={};for(const name of fields)value[name]=form.elements[name].value.trim();
    value.key_id=Number(value.key_id)||0;value.project_id=Number(value.project_id);
    for(const name of ['authorization','cookie'])value[name]=form.elements[name].value.trim();
    for(const name of ['clear_authorization','clear_cookie'])value[name]=form.elements[name].checked;
    value.filter=$('hubFilter').value.trim();value.channel_ids=ids(true);return value;
  }
  function fill(data){
    for(const name of fields)form.elements[name].value=data.settings[name]??'';
    if(!data.settings.key_id)form.elements.key_id.value='';
    for(const name of ['authorization','cookie'])form.elements[name].value='';
    for(const name of ['clear_authorization','clear_cookie'])form.elements[name].checked=false;
    $('hubFilter').value=data.settings.filter||'';
    $('hubChannelIDs').value=(data.settings.channel_ids||[]).join(', ');
    initialized=true;dirty=false;$('hubDraft').textContent='已保存到服务端';
  }
  function render(data){
    state=data;if(!initialized)fill(data);
    $('hubAuthorizationStatus').textContent=data.authorization_saved?'已保存；留空保留，填写新值后保存可替换':'尚未保存';
    form.elements.authorization.placeholder=data.authorization_saved?'已保存，留空保留':'粘贴 Authorization / Bearer Token';
    $('hubCookieStatus').textContent=data.cookie_saved?'已保存；留空保留':'尚未保存（可选）';
    const models=$('hubModels');if(!models.children.length)for(const model of data.models||[]){const option=element('option');option.value=model;models.append(option)}
    $('hubRecovery').hidden=!data.recovery_required;
    $('hubRecoveryDetails').textContent=data.recovery?JSON.stringify(data.recovery,null,2):'';
    const names={discover:'正在查找免费渠道', 'key-channels':'正在读取 Key 渠道',detect:'正在检测渠道',restore:'正在恢复原绑定',recover:'正在核对绑定',apply:'正在应用通过渠道'};
    $('hubOperation').textContent=data.busy?(names[data.action]||'正在执行'):data.message||'等待操作';
    if(data.storage_error)message(data.storage_error,true);
    $('connection').textContent='已连接';renderChannels();renderHistory();updateControls();
  }
  function updateControls(){
    const busy=submitting||!!state?.busy, blocked=busy||!state||!!state.recovery_required;
    $('hubFields').disabled=busy;$('hubSave').disabled=busy||!state;
    for(const id of ['hubDiscover','hubLoadKey','hubDetect'])$(id).disabled=blocked;
    $('hubDetect').disabled=blocked||!ids().length;
    for(const id of ['hubFilter','hubSelect','hubClear','hubChannelIDs'])$(id).disabled=busy;
    $('hubRecover').disabled=busy;$('hubCancel').hidden=!state?.busy;$('hubCancel').disabled=submitting;
    const count=ids().length;$('hubPlan').textContent=count?count+' 个渠道 · 预计 '+(count*3)+' 次模型请求，失败重试可能增加请求数。':'选择渠道后显示预计请求数。';
  }
  function filtered(){
    const keyword=$('hubFilter').value.trim().toLowerCase();
    return (state?.channels||[]).filter(c=>!keyword||!c.models?.length||c.models.some(m=>m.toLowerCase().includes(keyword)));
  }
  function renderChannels(){
    if(!state)return;
    const channels=filtered(), chosen=new Set(ids()), signature=JSON.stringify([channels,[...chosen],state.busy,submitting]);
    $('hubChannelsMeta').textContent=({free:'免费渠道',key:'Key 当前渠道'}[state.source]||'渠道列表')+' · 显示 '+channels.length+' / '+(state.channels||[]).length+' · 更新 '+date(state.found_at);
    if(signature===channelSignature)return;channelSignature=signature;
    const body=$('hubChannels');body.replaceChildren();
    if(!channels.length){const row=element('tr'),cell=element('td','暂无匹配渠道。可查找、读取 Key 渠道，或直接填写渠道 ID。','empty');cell.colSpan=4;row.append(cell);body.append(row)}
    for(const channel of channels){
      const row=element('tr'),cell=element('td'),input=element('input');input.type='checkbox';input.checked=chosen.has(channel.id);input.disabled=state.busy||submitting;input.setAttribute('aria-label','选择渠道 '+channel.id);
      input.onchange=()=>{const selected=new Set(ids());if(input.checked)selected.add(channel.id);else selected.delete(channel.id);$('hubChannelIDs').value=[...selected].join(', ');draft()};cell.append(input);
      const identity=element('td');identity.append(element('strong',channel.name),element('small','#'+channel.id));
      row.append(cell,identity,element('td',(channel.models||[]).join(' · ')||'未获取模型列表'),element('td',(channel.tags||[]).join(' · ')||'—'));body.append(row);
    }
  }
  function renderHistory(){
    const signature=JSON.stringify([state.jobs,state.busy,state.recovery_required,submitting]);if(signature===historySignature)return;historySignature=signature;
    const history=$('hubHistory');history.replaceChildren();
    if(!state.jobs?.length){history.append(element('div','尚无检测记录。查找渠道后，点击“检测所选渠道”开始。','empty'));return}
    const statuses={queued:'待检测',running:'检测中',completed:'已完成',partial:'样本不足',failed:'失败',cancelled:'已取消',interrupted:'已中断'};
    for(const job of state.jobs){
      const card=element('article',undefined,'evaluation-record'),top=element('div',undefined,'top'),title=element('div');
      title.append(element('strong','KEY #'+job.settings.key_id+' · '+job.settings.model),element('small',date(job.created_at)+' · '+job.settings.effort+' · '+job.settings.protocol+' · '+(statuses[job.status]||job.status)));
      const count=job.results.filter(c=>passed(c.result)).length,apply=element('button','应用通过渠道（'+count+'）');apply.type='button';
      apply.disabled=submitting||state.busy||state.recovery_required||job.status!=='completed'||!count;
      apply.onclick=()=>action('apply',{job_id:job.id});top.append(title,apply);card.append(top);
      if(job.error)card.append(element('p',job.error,'bad'));
      if(job.applied_at&&!job.applied_at.startsWith('0001-'))card.append(element('small','已应用：'+date(job.applied_at)));
      const table=element('table'),thead=element('thead'),head=element('tr');for(const text of ['渠道','检测状态','模型识别','耗时 / 用量','操作'])head.append(element('th',text));thead.append(head);table.append(thead);
      const body=element('tbody');
      for(const entry of job.results){
        const r=entry.result,row=element('tr'),status=passed(r)?'通过':r.model_match===false?'模型不一致':statuses[r.status]||r.status;
        const cell=element('td');cell.append(element('span',status,'badge '+(passed(r)?'good':r.status==='failed'||r.model_match===false?'bad':'neutral')));
        if(r.status==='running')cell.append(element('small',(r.done||0)+' / '+(r.total||3)+' 道挑战'));
        const detail=element('button','详情','secondary');detail.type='button';detail.onclick=()=>openDetail(job,entry.channel);
        const actions=element('td');actions.append(detail);
        row.append(element('td',entry.channel.name+' #'+entry.channel.id),cell,element('td',evaluationsView.verdict(r)||'—'),element('td',(r.duration_ms/1000).toFixed(1)+' 秒 · '+((r.input_tokens||0)+(r.output_tokens||0)).toLocaleString()+' Token'),actions);body.append(row);
      }
      table.append(body);const wrap=element('div',undefined,'table-wrap');wrap.append(table);card.append(wrap);history.append(card);
    }
  }
  async function save(){
    if(!form.reportValidity())throw Error('请检查配置中的地址和数字格式');
    const current=generation,revision=managementAuth.revision;
    const data=await managementAuth.request(base+'/settings','POST',settings());
    if(current!==generation||revision!==managementAuth.revision)throw Error('管理连接已变化，请重新连接');
    fill(data);render(data);
  }
  async function action(name,body={}){
    if(submitting)return;submitting=true;updateControls();message('');
    const current=generation,revision=managementAuth.revision;
    try{
      if(name!=='cancel'&&dirty)await save();
      if(name==='detect')body={channel_ids:ids(true)};
      const data=await managementAuth.request(base+'/'+name,'POST',body);
      if(current===generation&&revision===managementAuth.revision)render(data);
    }catch(error){if(current===generation)message(error.message,true)}
    finally{submitting=false;updateControls();if(state){renderChannels();renderHistory()}}
  }
  async function refresh(){
    if(refreshing||submitting||!managementAuth.ready)return;refreshing=true;
    const current=generation,revision=managementAuth.revision;
    try{const data=await managementAuth.request(base+'/state');if(current===generation&&revision===managementAuth.revision)render(data)}
    catch(error){if(current===generation)message(error.message,true)}finally{refreshing=false}
  }
  async function openDetail(job,channel){
    const current=generation,revision=managementAuth.revision;
    try{const data=await managementAuth.request(base+'/record?'+new URLSearchParams({id:job.id,channel_id:channel.id}));if(current===generation&&revision===managementAuth.revision)evaluationsView.showResult(data)}
    catch(error){if(current===generation)message(error.message,true)}
  }
  form.addEventListener('input',draft);
  form.onsubmit=async event=>{event.preventDefault();if(submitting)return;submitting=true;updateControls();try{await save();message('配置已保存，下次打开可直接复用。')}catch(error){message(error.message,true)}finally{submitting=false;updateControls()}};
  $('hubFilter').oninput=draft;$('hubChannelIDs').oninput=draft;
  $('hubSelect').onclick=()=>{$('hubChannelIDs').value=[...new Set([...ids(),...filtered().map(c=>c.id)])].join(', ');draft()};
  $('hubClear').onclick=()=>{$('hubChannelIDs').value='';draft()};
  $('hubDiscover').onclick=()=>action('discover');$('hubLoadKey').onclick=()=>action('key-channels');$('hubDetect').onclick=()=>action('detect');$('hubRecover').onclick=()=>action('recover');$('hubCancel').onclick=()=>action('cancel');$('hubRefresh').onclick=refresh;
  setInterval(()=>{if(!document.hidden&&!$('hubPanel').hidden)refresh()},2000);
  document.addEventListener('visibilitychange',()=>{if(!document.hidden&&!$('hubPanel').hidden)refresh()});
  function reset(){generation++;state=null;initialized=false;dirty=false;channelSignature='';historySignature='';for(const name of ['authorization','cookie'])form.elements[name].value='';$('hubChannels').replaceChildren();$('hubHistory').replaceChildren();message('')}
  window.addEventListener('message',event=>{if(event.origin===location.origin&&event.source===window.parent&&event.data?.type==='cpa:plugin-session-ended')reset()});
  return {refresh,reset};
})();
