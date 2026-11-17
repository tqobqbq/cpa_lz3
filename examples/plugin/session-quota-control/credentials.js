const credentialsView=(()=>{
'use strict';
const base=__EXTENSION_BASE__,configPath=__CONFIG_PATH__,$=id=>document.getElementById(id);
const rows=new Map(),drafts=new Map(),quotaDetailsOpen=new Set(),numbers=new Intl.NumberFormat('zh-CN');
let state=null,busy=false,refreshQueued=false,serverOffset=0,provider='',masked=false,dialogID='',dialogTab='overview',saving=false;
const api=(...args)=>managementAuth.request(...args);
function element(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node}
function now(){return Date.now()+serverOffset}
function message(text){$('message').textContent=text||''}
function count(value){return numbers.format(value||0)}
function compact(value){return new Intl.NumberFormat('zh-CN',{notation:'compact',maximumFractionDigits:1}).format(value||0)}
function duration(value){if(!Number.isFinite(value))return '未知';return value>=3600?(value/3600).toFixed(1)+' 小时':value>=60?(value/60).toFixed(1)+' 分钟':Number(value.toFixed(1))+' 秒'}
function time(value){const at=quotaTimestamp(value);return at===null?'—':new Date(at).toLocaleString('zh-CN',{hour12:false})}
function timeLeft(parent,value,prefix=''){const label=element('span',prefix),span=element('span');span.dataset.credentialDeadline=value;label.append(span);parent.append(label)}
function countdown(){document.querySelectorAll('[data-credential-deadline]').forEach(node=>node.textContent=duration(Math.max(0,Math.ceil((Date.parse(node.dataset.credentialDeadline)-now())/1000))))}
function display(value){const text=String(value||'');return masked?text.replace(/([A-Za-z0-9._%+-]{2})[A-Za-z0-9._%+-]*(@[A-Za-z0-9.-]+\.[A-Za-z]{2,})/g,'$1***$2'):text}
function credentialName(auth){return auth.email||auth.account||auth.label||auth.name||auth.id}
function providerName(value){return ({codex:'Codex',claude:'Claude',antigravity:'Antigravity',gemini:'Gemini',openai:'OpenAI'})[value]||value||'未知'}
function logo(node,value){node.className='provider-logo '+(value==='codex'||value==='claude'?value:'');node.textContent=value==='codex'?'>_':value==='claude'?'✳':providerName(value).slice(0,1)}
function authByID(id){return state?.credentials.find(auth=>auth.id===id)}
function poolFor(auth){return state.pools.find(pool=>pool.auth_index===auth.auth_index||pool.auth_id===auth.id)}
function storedRule(auth){return state.config.credential_waits?.[auth.auth_index]||state.config.credential_waits?.[auth.id]||{}}
function ruleFor(auth){const draft=drafts.get(auth.auth_index||auth.id);return draft?.submitted?draft.rule:storedRule(auth)}
function quotaHeaders(auth){return Object.fromEntries(Object.entries(auth.quota_observation?.signals||{}).map(([name,value])=>[name.toLowerCase(),String(value)]))}
function quotaNumber(value){if(value===undefined||value===null||String(value).trim()==='')return null;const number=Number(value);return Number.isFinite(number)?number:null}
function quotaTimestamp(value){if(!value||String(value).startsWith('0001-'))return null;const number=quotaNumber(value),at=number===null?Date.parse(value):number*1000;return Number.isFinite(at)&&at>0&&at<=8.64e15?at:null}
function quotaWindowLabel(prefix,headers){
  if(prefix.startsWith('anthropic-ratelimit-unified-')){
    const name=prefix.slice('anthropic-ratelimit-unified-'.length),match=name.match(/^(\d+)(h|d)(?:[-_](.+))?$/);
    if(!match)return name==='overage'?'额外用量':name;
    const period=match[1]==='7'&&match[2]==='d'?'周（7 天）':match[1]+(match[2]==='h'?' 小时':' 天');
    return (match[3]?match[3]+' · ':'')+period;
  }
  const name=prefix.slice('x-codex-'.length),scope=name.replace(/(?:^|-)(primary|secondary)$/,''),minutes=quotaNumber(headers[prefix+'-window-minutes']);
  const period=minutes>0?(minutes===10080?'周（7 天）':minutes%1440===0?minutes/1440+' 天':minutes%60===0?minutes/60+' 小时':minutes+' 分钟'):name.endsWith('secondary')?'次窗口':'主窗口';
  const label=scope==='code-review'?'代码审查':headers['x-codex-'+scope+'-limit-name']||scope.replace(/^additional-/,'');
  return (label?label+' · ':'')+period;
}
function responseQuotaWindows(headers,observedAt){
  const prefixes=new Set();
  for(const name of Object.keys(headers)){
    const match=name.match(/^(x-codex-.+)-(?:used-percent|window-minutes|reset-at|reset-after-seconds)$/)||name.match(/^(anthropic-ratelimit-unified-.+)-(?:utilization|reset|status)$/);
    if(match)prefixes.add(match[1]);
  }
  return [...prefixes].sort().map(prefix=>{
    const claude=prefix.startsWith('anthropic-'),used=quotaNumber(headers[prefix+(claude?'-utilization':'-used-percent')]);
    const remaining=used===null||used<0?null:Math.max(0,Math.min(100,100-used*(claude?100:1)));
    let reset=quotaTimestamp(headers[prefix+(claude?'-reset':'-reset-at')]);
    const after=quotaNumber(headers[prefix+'-reset-after-seconds']);
    if(reset===null&&observedAt!==null&&after!==null&&after>=0&&observedAt+after*1000<=8.64e15)reset=observedAt+after*1000;
    return {name:quotaWindowLabel(prefix,headers),remaining,reset,status:headers[prefix+'-status']};
  });
}
function translateReason(reason){let text=reason||'';const capped=text.endsWith('; capped');if(capped)text=text.slice(0,-8);return (({'fixed estimate':'单次自动估算','manual fixed delay':'使用固定时间','no current five-hour observation':'缺少当前 5 小时观测，直接放行'})[text]||text||'尚无记录')+(capped?' · 已限制上限':'');}

function quotaSummary(auth){const headers=quotaHeaders(auth),windows=responseQuotaWindows(headers,quotaTimestamp(auth.quota_observation?.observed_at));const values=windows.filter(window=>window.remaining!==null).map(window=>window.remaining);const minimum=values.length?Math.min(...values):null;return {headers,windows,minimum,kind:minimum===null?'unknown':minimum<=0?'exhausted':minimum<20?'low':'healthy'}}
function plan(auth){const headers=quotaHeaders(auth),value=headers['x-codex-plan-type']||'';return value?({free:'Free',plus:'Plus',pro:'Pro',team:'Team',business:'Business',enterprise:'Enterprise',edu:'Edu'})[value.toLowerCase()]||value:'未知'}
function status(auth){if(auth.disabled)return {kind:'disabled',label:'已禁用',tone:'neutral'};if(auth.unavailable||quotaTimestamp(auth.next_retry_after)>now())return {kind:'cooldown',label:'冷却中',tone:'warning'};if(['error','failed','invalid'].includes(auth.status))return {kind:'problem',label:'异常',tone:'bad'};if(['active','ready','ok'].includes(auth.status))return {kind:'available',label:'可用',tone:'good'};return {kind:'unknown',label:'待确认',tone:'neutral'}}
function renderResponseQuota(cell,auth,details=false){
  if(auth.quota_observation_error){cell.append(element('span','额度快照读取失败','muted'));return}
  const snapshot=auth.quota_observation,summary=quotaSummary(auth),headers=summary.headers;
  if(!Object.keys(headers).length){cell.append(element('span','尚未收到响应额度','muted'));return}
  const observedAt=quotaTimestamp(snapshot.observed_at),windows=element('div',undefined,'quota-windows');
  for(const window of summary.windows){
    const group=element('div',undefined,'quota-window'),label=element('div',undefined,'quota-window-label');
    label.append(element('span',window.name,'quota-period'),element('strong',window.remaining===null?'剩余未知':'剩余 '+Number(window.remaining.toFixed(2))+'%'));group.append(label);
    if(window.remaining!==null){const progress=element('progress',undefined,window.remaining<=0?'exhausted':window.remaining<20?'low':'');progress.max=100;progress.value=window.remaining;progress.setAttribute('aria-label',window.name+' 剩余 '+window.remaining+'%');group.append(progress)}
    if(window.status)group.append(element('small',({allowed:'可用',allowed_warning:'接近限额',rejected:'已限制'})[window.status]||window.status));
    if(window.reset!==null){const reset=element('small');if(window.reset>now())timeLeft(reset,new Date(window.reset).toISOString(),'重置于 ');else reset.textContent='待新响应确认重置';group.append(reset)}
    windows.append(group);
  }
  if(windows.childElementCount)cell.append(windows);
  if(/^(true|1)$/i.test(headers['x-codex-credits-unlimited']||''))cell.append(element('small','点数：无限'));
  else if(headers['x-codex-credits-balance']!==undefined)cell.append(element('small','剩余点数：'+headers['x-codex-credits-balance']));
  else if(/^(false|0)$/i.test(headers['x-codex-credits-has-credits']||''))cell.append(element('small','无可用点数'));
  for(const name of Object.keys(headers).sort()){const match=name.match(/^x-ratelimit-remaining-(.+)$/);if(!match)continue;const label={requests:'请求数',tokens:'Token'}[match[1]]||match[1],limit=headers['x-ratelimit-limit-'+match[1]];cell.append(element('small',label+'：剩余 '+headers[name]+(limit===undefined?'':' / '+limit)))}
  if(observedAt!==null)cell.append(element('small','响应观测 '+time(snapshot.observed_at)));
  if(details){const detail=element('details',undefined,'quota-details'),summary=element('summary','全部响应额度字段'),pre=element('pre',JSON.stringify(snapshot.signals,null,2));const key=auth.auth_index||auth.id;detail.open=quotaDetailsOpen.has(key);detail.addEventListener('toggle',()=>{if(!detail.isConnected)return;if(detail.open)quotaDetailsOpen.add(key);else quotaDetailsOpen.delete(key)});detail.append(summary,pre);cell.append(detail)}
}
function setCell(cell,signature,render){const key=JSON.stringify(signature);if(cell.dataset.signature===key||cell.contains(document.activeElement))return;cell.replaceChildren();render(cell);cell.dataset.signature=key}
function renderProviders(){
  const body=$('credentialProviders'),counts=new Map();for(const auth of state.credentials)counts.set(auth.provider,(counts.get(auth.provider)||0)+1);
  const entries=[['','全部',state.credentials.length],...[...counts].sort(([a],[b])=>a.localeCompare(b)).map(([key,value])=>[key,providerName(key),value])],signature=JSON.stringify([entries,provider]);
  if(body.dataset.signature===signature)return;body.replaceChildren();
  for(const [key,label,total] of entries){const button=element('button',label,key===provider?'active-tab':'secondary');button.type='button';button.setAttribute('aria-pressed',String(key===provider));button.append(element('span',count(total),'tab-count'));button.onclick=()=>{provider=key;render()};body.append(button)}body.dataset.signature=signature;
}
function renderPlans(){const node=$('credentialPlan');if(document.activeElement===node)return;const plans=[...new Set(state.credentials.map(plan))].sort(),key=JSON.stringify(plans);if(node.dataset.signature===key)return;const selected=node.value;node.replaceChildren();for(const value of ['',...plans]){const option=element('option',value||'全部套餐');option.value=value;node.append(option)}node.value=plans.includes(selected)?selected:'';node.dataset.signature=key}
function renderMetrics(){
  const body=$('credentialMetrics'),auths=state.credentials,available=auths.filter(auth=>status(auth).kind==='available').length,problems=auths.filter(auth=>status(auth).kind==='problem').length,risk=auths.filter(auth=>['low','exhausted'].includes(quotaSummary(auth).kind)).length,disabled=auths.filter(auth=>auth.disabled).length,unknown=auths.filter(auth=>status(auth).kind==='unknown').length;
  const values=[['总凭证',auths.length,'当前全部 Credential',''],['正常可用',available,'原生路由状态可用','good'],['需要处理',problems,'状态异常的 Credential','bad'],['额度风险',risk,'存在低额度或耗尽窗口','warning'],['已禁用',disabled,'原生管理中已禁用','muted'],['状态待确认',unknown,'未提供明确运行状态','muted']];
  body.replaceChildren();for(const [name,total,note,tone] of values){const metric=element('div',undefined,'metric '+tone),title=element('span',undefined,'muted');title.append(element('i',undefined,'metric-dot'),document.createTextNode(name));metric.append(title,element('strong',count(total)),element('small',note));body.append(metric)}
}
function selectedCredentials(){
  const query=$('credentialSearch').value.trim().toLowerCase(),statusFilter=$('credentialStatus').value,planFilter=$('credentialPlan').value,quotaFilter=$('credentialQuota').value;
  const list=state.credentials.filter(auth=>(!provider||auth.provider===provider)&&(!query||[auth.name,auth.email,auth.account,auth.label,auth.note,auth.auth_index].join(' ').toLowerCase().includes(query))&&(!statusFilter||(statusFilter==='enabled'?!auth.disabled:status(auth).kind===statusFilter))&&(!planFilter||plan(auth)===planFilter)&&(!quotaFilter||quotaSummary(auth).kind===quotaFilter));
  const sort=$('credentialSort').value;return list.sort((a,b)=>{let difference=0;if(sort==='recent')difference=(quotaTimestamp(b.history?.last_request_at)||0)-(quotaTimestamp(a.history?.last_request_at)||0);if(sort==='priority')difference=(b.priority||0)-(a.priority||0);if(sort==='remaining')difference=(quotaSummary(a).minimum??101)-(quotaSummary(b).minimum??101);if(sort==='waiting')difference=(poolFor(b)?.sleeping||0)-(poolFor(a)?.sleeping||0);return difference||(a.name||a.id).localeCompare(b.name||b.id)})
}
function makeRow(auth){
  const row=element('tr'),cells=Array.from({length:8},()=>element('td'));row.append(...cells);cells[2].className='credential-status';cells[3].className='credential-recent';cells[4].className='credential-history';cells[5].className='credential-quota';cells[6].className='credential-pacing';
  const actions=element('div',undefined,'credential-actions'),settings=element('button','等待设置','secondary'),detail=element('button','详情','secondary'),evaluation=element('button','降智检测','secondary'),access=element('button','访问控制','secondary');settings.type=detail.type=evaluation.type=access.type='button';settings.onclick=()=>openCredential(auth.id,'wait');detail.onclick=()=>openCredential(auth.id,'overview');evaluation.onclick=()=>{const current=authByID(auth.id);if(current)evaluationsView.open(current,display(credentialName(current)))};access.onclick=()=>{const current=authByID(auth.id);if(current)accessView.openCredential(current.auth_index)};access.disabled=!auth.auth_index;settings.setAttribute('aria-label',credentialName(auth)+' 的等待设置');detail.setAttribute('aria-label',credentialName(auth)+' 的详情');access.setAttribute('aria-label',credentialName(auth)+' 的访问控制');actions.append(settings,detail,evaluation,access);cells[7].append(actions);return {row,cells,settings,detail,evaluation,access};
}
function renderRecent(cell,auth){
  const recent=auth.recent_requests||[],active=recent.filter(item=>item.success||item.failed),latest=auth.history?.last_request_at;
  cell.append(element('span',quotaTimestamp(latest)!==null?time(latest):active.length?active.at(-1).time:'—'));
  const bars=element('div',undefined,'request-bars');bars.setAttribute('aria-label','最近请求成功与失败');
  for(const item of recent.slice(-20)){const bar=element('span',undefined,item.success&&item.failed?'mixed':item.failed?'failed':item.success?'success':'');bar.title=item.time+' · '+count(item.success)+' 成功 / '+count(item.failed)+' 失败';bars.append(bar)}
  if(!bars.childElementCount)cell.append(element('small','暂无最近请求'));else cell.append(bars);
}
function renderHistory(cell,auth){
  const history=auth.history,total=history?.executions??(auth.source?(auth.success||0)+(auth.failed||0):null),failed=history?.failed??auth.failed??0;
  if(total===null){cell.append(element('span','暂无用量记录','muted'));return}
  for(const [symbol,text,title] of [['↗',compact(total)+' 次',count(total)+' 次上游调用'],['◫',history?compact(history.total_tokens)+' Token':'Token 未知',history?count(history.total_tokens)+' Token':'未启用插件统计'],['✓',total?((total-failed)*100/total).toFixed(1)+'%':'—','成功率；失败 '+count(failed)+' 次']]){const line=element('div',undefined,'history-line');line.title=title;line.append(element('span',symbol,'history-symbol'),element('strong',text));cell.append(line)}
  if(!history)cell.append(element('small','原生运行时累计'));
}
function renderPacing(cell,auth){
  const pool=poolFor(auth),rule=ruleFor(auth);cell.append(element('strong',rule.fixed_wait_seconds==null?'自动估算':'固定 '+duration(rule.fixed_wait_seconds)));
  cell.append(element('small','上限 '+(rule.max_wait_seconds==null?'不限制':duration(rule.max_wait_seconds))));
  if(!state.config.quota_pacing_enabled)cell.append(element('small','全局节流已关闭','warning'));
  if(pool){cell.append(element('small','执行 '+pool.in_flight+' / 等待 '+pool.sleeping));cell.append(element('small','最近计划 '+duration(pool.wait_seconds)));if(pool.sleeping){const label=element('small');timeLeft(label,pool.next_admission,'最近恢复 ');cell.append(label)}}
  else cell.append(element('small','尚无节流记录'));
}
function renderRows(){
  const body=$('credentialRows'),list=selectedCredentials(),allIDs=new Set(state.credentials.map(auth=>auth.id)),visible=new Set(list.map(auth=>auth.id));
  for(const [id,view] of rows){if(!visible.has(id))view.row.remove();if(!allIDs.has(id))rows.delete(id)}
  body.querySelector('.empty')?.parentElement.remove();
  if(!list.length){const row=element('tr'),cell=element('td',state.credentials.length?'没有符合筛选条件的 Credential':'没有 Credential','empty');cell.colSpan=8;row.append(cell);body.append(row)}
  for(const [index,auth] of list.entries()){
    let view=rows.get(auth.id);if(!view){view=makeRow(auth);rows.set(auth.id,view)}const cells=view.cells,pool=poolFor(auth),health=status(auth);
    setCell(cells[0],[masked,auth.name,auth.email,auth.account,auth.label,auth.provider,auth.note],cell=>{const identity=element('div',undefined,'credential-name'),mark=element('span'),text=element('div',undefined,'credential-name-text');logo(mark,auth.provider);text.append(element('strong',display(credentialName(auth))),element('small',display(auth.name)),element('small',providerName(auth.provider)));identity.append(mark,text);cell.append(identity);if(auth.note)cell.append(element('div',display(auth.note),'credential-note'))});
    setCell(cells[1],plan(auth),cell=>cell.append(element('strong',plan(auth))));
    setCell(cells[2],[health,auth.priority,auth.status_message,auth.next_retry_after],cell=>{cell.append(element('span',health.label,'badge '+health.tone),element('small','优先级 '+(auth.priority||0)));if(auth.status_message)cell.title=auth.status_message;else cell.removeAttribute('title');if(health.kind==='cooldown'&&quotaTimestamp(auth.next_retry_after)>now()){const label=element('small');timeLeft(label,auth.next_retry_after,'重试 ');cell.append(label)}});
    setCell(cells[3],[auth.history?.last_request_at,auth.recent_requests],cell=>renderRecent(cell,auth));
    setCell(cells[4],[auth.history,auth.success,auth.failed,auth.source],cell=>renderHistory(cell,auth));
    setCell(cells[5],[auth.quota_observation,auth.quota_observation_error],cell=>renderResponseQuota(cell,auth));
    setCell(cells[6],[pool,ruleFor(auth),state.config.quota_pacing_enabled],cell=>renderPacing(cell,auth));view.settings.disabled=!auth.auth_index;view.settings.setAttribute('aria-label',display(credentialName(auth))+' 的等待设置');view.detail.setAttribute('aria-label',display(credentialName(auth))+' 的详情');view.evaluation.setAttribute('aria-label',display(credentialName(auth))+' 的降智检测');
    if(body.children[index]!==view.row)body.insertBefore(view.row,body.children[index]||null);
  }
  $('credentialMatches').textContent='显示 '+count(list.length)+' / '+count(state.credentials.length)+' 个 Credential · 额度、节流状态和等待设置集中管理';
  $('credentialHistoryStatus').textContent=state.credential_history_error?'历史用量读取失败：'+state.credential_history_error:!state.config.usage_report_enabled?'统计采集已关闭；显示可用的原生运行时请求计数，Token 用量未知。':'历史用量从插件开始记录时累计，按上游调用统计，包含重试；不包含启用插件之前的用量。每 5 秒汇总一次。';
}
function fields(body,entries){body.replaceChildren();for(const [name,value] of entries){const field=element('div',undefined,'info-field');field.append(element('span',name),element('strong',String(value??'未知')));body.append(field)}}
function showDialogTab(value,focus=false){dialogTab=value;for(const [name,tab,panel] of [['overview','credentialOverviewTab','credentialOverviewPanel'],['wait','credentialWaitTab','credentialWaitPanel']]){const selected=name===value;$(tab).setAttribute('aria-selected',String(selected));$(tab).tabIndex=selected?0:-1;$(panel).hidden=!selected;if(selected&&focus)$(tab).focus()}}
function renderDialog(){
  if(!$('credentialDialog').open)return;const auth=authByID(dialogID);if(!auth){$('credentialDialog').close();message('此 Credential 已被移除，请在列表中重新选择。');return}
  const pool=poolFor(auth),health=status(auth);$('credentialDialogTitle').textContent=display(credentialName(auth));$('credentialDialogSubtitle').textContent=display(auth.name)+' · '+providerName(auth.provider);logo($('credentialDialogLogo'),auth.provider);
  fields($('credentialInfo'),[['Provider',providerName(auth.provider)],['套餐',plan(auth)],['状态',health.label],['优先级',auth.priority||0],['Credential 索引',auth.auth_index||'未知'],['最近更新',time(auth.updated_at)],['上游执行中',pool?.in_flight||0],['正在等待',pool?.sleeping||0],['最近响应额度观测',time(auth.quota_observation?.observed_at)]]);
  setCell($('credentialDialogQuota'),[auth.quota_observation,auth.quota_observation_error],cell=>renderResponseQuota(cell,auth,true));
  const raw={...auth};delete raw.quota_observation;const rawText=JSON.stringify(raw,null,2);$('credentialRaw').textContent=masked?display(rawText):rawText;
  fields($('credentialPacingInfo'),[['最近模型',pool?.model||'尚无记录'],['模型平均消耗',pool?Number(pool.estimated_cost_percent.toFixed(4))+'%':'未知'],['历史平均并发',pool?pool.estimated_concurrency.toFixed(2):'未知'],['最近已定等待',pool?duration(pool.wait_seconds):'尚无记录'],['执行 / 等待',pool?pool.in_flight+' / '+pool.sleeping:'0 / 0'],['原因',pool?translateReason(pool.reason):'尚无节流记录']]);
}
function updateSaveControls(){const disabled=saving||!authByID(dialogID)?.auth_index;for(const id of ['credentialWaitSave','credentialWaitReset','credentialMaxWait','credentialFixedWait'])$(id).disabled=disabled}
function openCredential(id,tab){
  const auth=authByID(id);if(!auth)return;dialogID=id;const draft=drafts.get(auth.auth_index||auth.id),rule=draft?.rule||storedRule(auth);
  $('credentialMaxWait').value=rule.max_wait_seconds??'';$('credentialFixedWait').value=rule.fixed_wait_seconds??'';$('credentialDraftStatus').textContent=draft&&!draft.submitted?'有未保存的修改':'';$('credentialSaveStatus').textContent='';
  updateSaveControls();
  if(!$('credentialDialog').open)$('credentialDialog').showModal();showDialogTab(tab);renderDialog();countdown();
}
function rememberDraft(){const auth=authByID(dialogID);if(!auth||saving)return;const rule={max_wait_seconds:$('credentialMaxWait').value===''?null:Number($('credentialMaxWait').value),fixed_wait_seconds:$('credentialFixedWait').value===''?null:Number($('credentialFixedWait').value)};drafts.set(auth.auth_index||auth.id,{rule,submitted:false});$('credentialDraftStatus').textContent='有未保存的修改';$('credentialSaveStatus').textContent=''}
function render(){renderProviders();renderPlans();renderMetrics();renderRows();renderDialog();countdown()}
async function refresh(){
  if(!managementAuth.ready)return;if(busy){refreshQueued=true;return}busy=true;const revision=managementAuth.revision;
  try{const next=await api(base+'/state');if(revision!==managementAuth.revision)return;state=next;serverOffset=Date.parse(next.now)-Date.now();
    for(const auth of state.credentials){const key=auth.auth_index||auth.id,draft=drafts.get(key),stored=storedRule(auth);if(draft?.submitted&&(stored.max_wait_seconds??null)===draft.rule.max_wait_seconds&&(stored.fixed_wait_seconds??null)===draft.rule.fixed_wait_seconds)drafts.delete(key)}
    $('connection').textContent='已连接 · 每 2 秒更新';render();
  }catch(error){message(error.message);$('connection').textContent=managementAuth.ready?'连接异常':'请登录主管理界面'}finally{busy=false;if(refreshQueued){refreshQueued=false;queueMicrotask(refresh)}}
}
for(const id of ['credentialSearch','credentialStatus','credentialPlan','credentialQuota','credentialSort'])$(id).addEventListener(id==='credentialSearch'?'input':'change',()=>{if(state)render()});
$('credentialRefresh').onclick=()=>{if(!managementAuth.ready)managementAuth.reconnect();refresh()};$('credentialMask').onclick=()=>{masked=!masked;$('credentialMask').setAttribute('aria-pressed',String(masked));$('credentialMask').textContent=masked?'显示完整信息':'脱敏显示';if(state)render()};
$('credentialDialogClose').onclick=()=>$('credentialDialog').close();$('credentialDialog').addEventListener('close',()=>{dialogID=''});
for(const [id,value] of [['credentialOverviewTab','overview'],['credentialWaitTab','wait']]){$(id).onclick=()=>showDialogTab(value);$(id).onkeydown=event=>{if(['ArrowLeft','ArrowRight','Home','End'].includes(event.key)){event.preventDefault();showDialogTab(event.key==='Home'?'overview':event.key==='End'?'wait':dialogTab==='overview'?'wait':'overview',true)}}}
for(const id of ['credentialMaxWait','credentialFixedWait'])$(id).oninput=rememberDraft;
$('credentialWaitReset').onclick=()=>{$('credentialMaxWait').value='';$('credentialFixedWait').value='';rememberDraft()};
$('credentialWaitForm').onsubmit=async event=>{
  event.preventDefault();if(saving||!event.target.reportValidity())return;const auth=authByID(dialogID);if(!auth?.auth_index)return;rememberDraft();const draft=drafts.get(auth.auth_index),id=dialogID,revision=managementAuth.revision;
  saving=true;updateSaveControls();$('credentialSaveStatus').className='dialog-message';
  try{await api(configPath+'/credential_waits/'+encodeURIComponent(auth.auth_index),'PATCH',draft.rule);if(revision!==managementAuth.revision)return;draft.submitted=true;if(dialogID===id){$('credentialDraftStatus').textContent='';$('credentialSaveStatus').textContent='已保存，对后续请求生效。'}message('已保存 '+display(credentialName(auth))+' 的等待设置。');renderRows();setTimeout(refresh,1500)}catch(error){if(dialogID===id){$('credentialSaveStatus').className='dialog-message bad';$('credentialSaveStatus').textContent=error.message}else message(error.message)}finally{saving=false;updateSaveControls()}
};
setInterval(()=>{if(!document.hidden&&!$('credentialsPanel').hidden&&$('credentialAutoRefresh').checked)refresh()},2000);
setInterval(()=>{if(!$('credentialsPanel').hidden)countdown()},1000);
return {refresh,reset(){state=null;drafts.clear();$('credentialDialog').close();evaluationsView.reset()}};
})();
