const evaluationsView=(()=>{
  'use strict';
  const base=__EXTENSION_BASE__,$=id=>document.getElementById(id),api=(...args)=>managementAuth.request(...args);
  const names={candy:'糖果题',fingerprint:'数字指纹',pelican:'画鹈鹕'};
  const statuses={scheduled:['待执行','muted'],running:['检测中','good'],cancelling:['取消中','warning'],cancelled:['已取消','muted'],interrupted:['已中断','warning'],correct:['答对','good'],wrong:['答错','bad'],completed:['已完成','good'],partial:['部分完成','warning'],failed:['失败','bad'],skipped:['已跳过','muted']};
  let credential=null,generation=0,refreshing=false,submitting=false,detailID='',catalogLoaded=false,historySignature='';
  const element=(tag,text,className)=>{const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node};
  const date=value=>!value||value.startsWith('0001-')?'—':new Date(value).toLocaleString('zh-CN',{hour12:false});
  const selected=name=>[...document.querySelectorAll('input[name="'+name+'"]:checked')].map(input=>input.value);
  const active=status=>['scheduled','running','cancelling'].includes(status);
  const setMessage=(text,error=false)=>{$('evaluationMessage').textContent=text||'';$('evaluationMessage').className='dialog-message'+(error?' bad':'')};
  function title(job){return names[job.test]+(job.test==='pelican'?' · '+(job.variant==='all-in-one'?'多合一':'原版'):'')}
  function verdict(result){
    if(result.attribution?.decision==='not_model')return '数字序列与参考库的模型特征不同，未生成模型排名。';
    if(result.attribution){
      const a=result.attribution,probability=a.probability==null?'':' · '+(a.probability*100).toFixed(1)+'%';
      const match=result.model_match===true?'与所选模型一致':result.model_match===false?'与所选模型不一致':'所选模型不在参考库的已知名称中';
      return '最接近 '+a.prediction_name+probability+' · '+match;
    }
    if(result.status==='correct')return '回答中检测到独立的 21。';
    if(result.status==='wrong')return '回答中未检测到独立的 21。';
    return result.error||'';
  }
  function updatePlan(){
    const tests=selected('evaluationTest'),delays=selected('evaluationDelay'),requests=tests.reduce((sum,test)=>sum+(test==='fingerprint'?3:1),0)*delays.length;
    $('evaluationPlanCount').textContent=tests.length*delays.length+' 项检测 · 预计 '+requests+' 次模型请求';
    $('evaluationVariantLabel').hidden=!tests.includes('pelican');
    $('evaluationCreate').disabled=submitting||!tests.length||!delays.length||!!credential?.disabled;
  }
  function showForm(show){
    $('evaluationForm').hidden=!show;
    $('evaluationNew').setAttribute('aria-expanded',String(show));
    if(show){updatePlan();$('evaluationModel').focus()}
  }
  async function loadCatalog(){
    if(catalogLoaded)return;
    const current=generation,revision=managementAuth.revision;
    try{
      const catalog=await api(base+'/evaluations/catalog');
      if(current!==generation||revision!==managementAuth.revision)return;
      const list=$('evaluationModels');list.replaceChildren();
      for(const model of catalog.models){const option=element('option');option.value=model;list.append(option)}
      catalogLoaded=true;
    }catch(error){if(current===generation)setMessage(error.message,true)}
  }
  function render(jobs){
    const signature=JSON.stringify(jobs);
    if(signature===historySignature)return;
    historySignature=signature;
    const body=$('evaluationHistory'),focus=document.activeElement?.dataset?.evaluationAction;
    body.replaceChildren();
    if(!jobs.length){body.append(element('div','还没有检测记录，点击“新增检测”开始。','empty'));return}
    for(const job of jobs){
      const r=job.result,[label,tone]=statuses[r.status]||[r.status,'muted'],card=element('article',undefined,'evaluation-record');
      const heading=element('div',undefined,'top'),identity=element('div',undefined,'evaluation-record-title');
      identity.append(element('strong',title(job)),element('span',label,'badge '+tone));heading.append(identity);
      const actions=element('div',undefined,'toolbar');
      if(active(r.status)){
        const cancel=element('button',r.status==='cancelling'?'取消中':'取消','secondary');cancel.type='button';cancel.disabled=r.status==='cancelling';cancel.dataset.evaluationAction='cancel:'+job.id;
        cancel.onclick=()=>cancelJob(job,cancel);actions.append(cancel);
      }
      if(r.status!=='scheduled'){
        const detail=element('button',job.test==='pelican'&&r.status==='completed'?'查看画面':'查看详情','secondary');detail.type='button';detail.dataset.evaluationAction='detail:'+job.id;
        detail.onclick=()=>openDetail(job.id);actions.append(detail);
      }
      heading.append(actions);card.append(heading,element('div',job.model+' · '+job.effort,'evaluation-record-model'));
      card.append(element('div','计划 '+date(job.scheduled_at)+(job.started_at&&!job.started_at.startsWith('0001-')?' · 开始 '+date(job.started_at):''),'evaluation-record-time'));
      if(r.status==='running'&&r.total>1){const progress=element('progress');progress.max=r.total;progress.value=r.done;progress.setAttribute('aria-label','已完成挑战 '+r.done+' / '+r.total);card.append(progress)}
      const conclusion=verdict(r);if(conclusion)card.append(element('p',conclusion,'evaluation-verdict'));
      if(r.attempts)card.append(element('small',r.attempts+' 次请求 · 输入 '+r.input_tokens.toLocaleString()+' / 输出 '+r.output_tokens.toLocaleString()+' Token'+(r.duration_ms?' · '+(r.duration_ms/1000).toFixed(1)+' 秒':''),'evaluation-record-time'));
      body.append(card);
    }
    if(focus){for(const button of body.querySelectorAll('button'))if(button.dataset.evaluationAction===focus){button.focus({preventScroll:true});break}}
  }
  async function refresh(){
    if(!credential||!$('evaluationDialog').open||!managementAuth.ready||refreshing)return;
    const current=generation,id=credential.id,revision=managementAuth.revision;refreshing=true;
    try{
      const data=await api(base+'/evaluations?'+new URLSearchParams({credential_id:id}));
      if(current!==generation||revision!==managementAuth.revision)return;
      render(data.records);
      if(data.storage_error)setMessage('检测记录保存异常：'+data.storage_error,true);
    }catch(error){if(current===generation)setMessage(error.message,true)}
    finally{refreshing=false;if(current!==generation&&credential&&$('evaluationDialog').open)refresh()}
  }
  function open(auth,label){
    credential={id:auth.id,disabled:auth.disabled};generation++;detailID='';historySignature='';
    $('evaluationCredential').textContent=label+' · '+auth.provider;
    $('evaluationHistory').replaceChildren(element('div','正在读取检测记录…','empty'));
    setMessage('');showForm(false);$('evaluationNew').disabled=!!auth.disabled;
    if(!$('evaluationDialog').open)$('evaluationDialog').showModal();
    loadCatalog();refresh();
  }
  async function cancelJob(job,button){
    const current=generation;button.disabled=true;
    try{await api(base+'/evaluations/cancel','POST',{credential_id:job.credential_id,id:job.id});if(current===generation){setMessage('已提交取消。');await refresh()}}
    catch(error){if(current===generation){setMessage(error.message,true);button.disabled=false}}
  }
  function section(parent,label,text){const node=element('details'),heading=element('summary',label);node.append(heading,element('pre',text));parent.append(node)}
  function renderDetail(job){
    const r=job.result,body=$('evaluationDetailBody');body.replaceChildren();
    $('evaluationDetailTitle').textContent=title(job)+' · '+(statuses[r.status]?.[0]||r.status);
    $('evaluationDetailMeta').textContent=job.model+' · '+job.effort+' · '+date(job.scheduled_at);
    const conclusion=verdict(r);if(conclusion)body.append(element('p',conclusion,'evaluation-verdict'));
    if(r.html){
      const frame=element('iframe');frame.className='evaluation-canvas';frame.setAttribute('sandbox','allow-scripts');frame.title='鹈鹕画面';frame.referrerPolicy='no-referrer';
      frame.srcdoc='<meta http-equiv="Content-Security-Policy" content="default-src \'none\'; script-src \'unsafe-inline\'; style-src \'unsafe-inline\'; img-src data: blob:; connect-src \'none\'; frame-src \'none\'; form-action \'none\'; base-uri \'none\'">'+r.html;
      body.append(frame);section(body,'查看 HTML / SVG 代码',r.html);
    }
    if(r.answer){body.append(element('pre',r.answer,'evaluation-answer'));body.append(element('p','沿用原版判分：回答中出现独立的 21 即视为答对，不验证推导过程。','notice'))}
    if(r.attribution){
      const a=r.attribution;
      body.append(element('p','有效挑战 '+a.used_outputs+' / 3 · '+(a.family_prediction_name||'未归因'),'notice'));
      if(a.results?.length){
        const table=element('table'),head=element('tr');for(const text of ['候选模型','归因概率','分数'])head.append(element('th',text));const thead=element('thead');thead.append(head);table.append(thead);
        const rows=element('tbody');for(const candidate of a.results){const row=element('tr');row.append(element('td',candidate.display_name),element('td',candidate.probability==null?'有效挑战不足 3 条':(candidate.probability*100).toFixed(2)+'%'),element('td',candidate.score.toFixed(4)));rows.append(row)}table.append(rows);
        const wrap=element('div',undefined,'table-wrap evaluation-ranking');wrap.append(table);body.append(wrap);
      }
      body.append(element('p','概率仅比较参考库中的候选模型，不能证明模型身份。所选 effort 会用于本次请求。','notice'));
    }
    for(const [index,sample] of (r.samples||[]).entries())section(body,'挑战 '+(index+1)+' · '+(sample.accepted?'有效':'无效')+' · '+sample.parsed_numbers+' 个数字',[sample.instructions,sample.prompt,sample.text,sample.error].filter(Boolean).join('\n\n'));
    section(body,'完整检测记录',JSON.stringify(job,null,2));
  }
  async function openDetail(id){
    detailID=id;const current=generation,credentialID=credential.id,revision=managementAuth.revision;
    $('evaluationDetailBody').textContent='正在读取…';$('evaluationDetailTitle').textContent='检测详情';$('evaluationDetailMeta').textContent='';
    if(!$('evaluationDetail').open)$('evaluationDetail').showModal();
    try{const job=await api(base+'/evaluations/record?'+new URLSearchParams({credential_id:credentialID,id}));if(current===generation&&detailID===id&&revision===managementAuth.revision)renderDetail(job)}
    catch(error){if(current===generation&&detailID===id)$('evaluationDetailBody').textContent=error.message}
  }
  $('evaluationForm').onsubmit=async event=>{
    event.preventDefault();if(submitting||!credential||!$('evaluationForm').reportValidity())return;
    const current=generation,revision=managementAuth.revision;
    const plan={credential_id:credential.id,model:$('evaluationModel').value.trim(),effort:$('evaluationEffort').value,tests:selected('evaluationTest'),delays:selected('evaluationDelay').map(Number),variant:$('evaluationVariant').value};
    if(!plan.tests.length||!plan.delays.length)return;
    submitting=true;updatePlan();setMessage('');
    try{
      const created=await api(base+'/evaluations','POST',plan);
      if(current!==generation||revision!==managementAuth.revision)return;
      showForm(false);setMessage('已创建 '+created.count+' 项检测，关闭窗口后仍会执行。');await refresh();
    }catch(error){if(current===generation)setMessage(error.message,true)}
    finally{submitting=false;updatePlan()}
  };
  $('evaluationNew').onclick=()=>showForm($('evaluationForm').hidden);
  $('evaluationAddTime').onclick=()=>{
    const input=$('evaluationDelayMinutes'),minutes=Number(input.value);
    if(!Number.isInteger(minutes)||minutes<1||minutes>1440){setMessage('请输入 1～1440 分钟的整数。',true);input.focus();return}
    const seconds=minutes*60,existing=[...document.querySelectorAll('input[name=evaluationDelay]')].find(node=>Number(node.value)===seconds);
    if(existing){existing.checked=true;setMessage('');updatePlan();return}
    if(document.querySelectorAll('input[name=evaluationDelay]').length>=8){setMessage('最多添加 8 个检测时间。',true);return}
    const label=element('label'),checkbox=element('input');checkbox.type='checkbox';checkbox.name='evaluationDelay';checkbox.value=String(seconds);checkbox.checked=true;
    label.append(checkbox,document.createTextNode(minutes+' 分钟后检测'));$('evaluationTimes').append(label);input.value='';setMessage('');updatePlan();
  };
  $('evaluationRefresh').onclick=refresh;
  $('evaluationClose').onclick=()=>$('evaluationDialog').close();
  $('evaluationDialog').addEventListener('close',()=>{generation++;credential=null;detailID='';$('evaluationDetail').close();$('evaluationDetailBody').replaceChildren()});
  $('evaluationDetailClose').onclick=()=>$('evaluationDetail').close();
  $('evaluationDetail').addEventListener('close',()=>{detailID='';$('evaluationDetailBody').replaceChildren()});
  $('evaluationForm').addEventListener('change',updatePlan);
  setInterval(()=>{if(!document.hidden)refresh()},2000);
  document.addEventListener('visibilitychange',()=>{if(!document.hidden)refresh()});
  window.addEventListener('message',event=>{if(event.origin===location.origin&&event.source===window.parent&&event.data?.type==='cpa:plugin-session-ended')reset()});
  function reset(){generation++;credential=null;detailID='';historySignature='';$('evaluationDialog').close();$('evaluationDetail').close();$('evaluationHistory').replaceChildren();$('evaluationDetailBody').replaceChildren()}
  function showResult(job){detailID='';renderDetail(job);if(!$('evaluationDetail').open)$('evaluationDetail').showModal()}
  return {open,reset,showResult,verdict};
})();
