const usageView=(()=>{
'use strict';
const base=__EXTENSION_BASE__,configPath=__CONFIG_PATH__,defaults=__DEFAULT_CONFIG__;
const $=id=>document.getElementById(id),number=new Intl.NumberFormat('zh-CN');
let trendMode='tokens',trendSelection='';
let data=null,busy=false,refreshQueued=false,page=1,group='models',detailID='',detailRevision=0,detailOpener='overview',storageInitialized=false;
const resultNames={succeeded:'成功',failed:'失败',canceled:'已取消',rejected:'执行前终止',running:'进行中',interrupted:'服务中断'};
const admissionNames={manual_assignment:'已手动指定',automatic_release:'已放行自动调度',timeout:'超时回退',disabled:'已关闭',canceled:'已取消'};
const filterValues=()=>Object.fromEntries(new FormData($('filters')));
let appliedFilters={},appliedRangeDays=7,serverDate='';
function element(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node}
function message(text){$('message').textContent=text||''}
function count(value){return number.format(value||0)}
function duration(ms){if(!Number.isFinite(ms))return '未知';if(ms<1000)return Math.round(ms)+' ms';if(ms<60000)return (ms/1000).toFixed(2)+' 秒';if(ms<3600000)return (ms/60000).toFixed(2)+' 分钟';return (ms/3600000).toFixed(2)+' 小时'}
function ttft(item){return item.ttft_count?duration(item.ttft_ms/item.ttft_count):'未知'}
function tps(item){return item.tps_count&&item.generation_ms>0?(item.generation_tokens*1000/item.generation_ms).toFixed(1)+' tok/s':'未知'}
function recordedWait(ms,unknown){return duration(ms)+(unknown?' + 未知':'')}
function credential(id){return data?.credential_labels?.[id]||id||'未选择'}
function validTime(value){return typeof value==='string'&&!value.startsWith('0001-')&&Number.isFinite(Date.parse(value))}
function time(value){return validTime(value)?new Date(value).toLocaleString('zh-CN',{hour12:false}):'—'}
function dateValue(value){const p=n=>String(n).padStart(2,'0');return value.getFullYear()+'-'+p(value.getMonth()+1)+'-'+p(value.getDate())}
function rangeDates(days,date=serverDate||dateValue(new Date())){
  const end=new Date(date+'T12:00:00Z'),start=new Date(end);start.setUTCDate(start.getUTCDate()-days+1);
  return {start:start.toISOString().slice(0,10),end:end.toISOString().slice(0,10)};
}
function setRange(days){const dates=rangeDates(days);$('start').value=dates.start;$('end').value=dates.end}
function advancePresetRange(report){
  const date=report.server_date||(report.collected_at||'').slice(0,10);
  if(!/^\d{4}-\d{2}-\d{2}$/.test(date)||!Number.isFinite(Date.parse(date+'T12:00:00Z')))return false;
  serverDate=date;
  if(!appliedRangeDays)return false;
  const dates=rangeDates(appliedRangeDays);
  if(dates.start===appliedFilters.start&&dates.end===appliedFilters.end)return false;
  const updateForm=$('range').value===String(appliedRangeDays)&&$('start').value===appliedFilters.start&&$('end').value===appliedFilters.end;
  appliedFilters={...appliedFilters,...dates};page=1;trendSelection='';
  if(updateForm){$('start').value=dates.start;$('end').value=dates.end}
  return true;
}
function empty(body,columns,text){const row=element('tr'),cell=element('td',text,'empty');cell.colSpan=columns;row.append(cell);body.append(row)}
const api=(...args)=>managementAuth.request(...args);
function updateOptions(select,options,label){if(document.activeElement===select)return;const values=['',...options],signature=JSON.stringify(values);if(select.dataset.options===signature)return;const value=select.value;select.replaceChildren();for(const item of values){const option=element('option',item||label);option.value=item;select.append(option)}if(!values.includes(value)&&value){const option=element('option',value);option.value=value;select.append(option)}select.value=value;select.dataset.options=signature}
function endToEndTPS(item){return item?.end_to_end_count&&item.end_to_end_ms>0?(item.end_to_end_tokens*1000/item.end_to_end_ms).toFixed(1)+' tok/s':'未知'}
function cacheRate(item){return item?.input>0&&!item.accounting_unknown?(100*item.cache/item.input).toFixed(1)+'%'+(item.missing||item.usage_missing?'（已知部分）':''):'未知'}
function money(value){return Number.isFinite(value)?'$'+new Intl.NumberFormat('en-US',{minimumFractionDigits:2,maximumFractionDigits:6}).format(value):'未定价'}
function costLabel(cost){const pending=cost?.missing?'待定（'+count(cost.missing)+' 次）':'';return cost?.priced?money(cost.usd)+(pending?' + '+pending:''):pending||'—'}
function priceReason(reason){return {model_not_priced:'模型未定价',rate_missing:'缺少适用单价',service_tier_not_priced:'速度档位未定价',usage_missing:'缺少可计费用量',token_accounting_unknown:'Token 分项不完整',unknown:'原因未记录'}[reason]||'未定价'}
function costNote(cost){
  const unconfirmed=cost?.tier_unconfirmed?'；其中 '+count(cost.tier_unconfirmed)+' 次速度档位未确认，按出站档位或 Standard 基准估价':'';
  if(!cost?.missing)return count(cost?.priced)+' 次调用已估价'+unconfirmed;
  const reasons=Object.entries(cost.missing_reasons||{}).filter(([,n])=>n>0).sort(([a],[b])=>a.localeCompare(b)).map(([reason,n])=>priceReason(reason)+' × '+count(n));
  return (reasons.length?reasons.join('；'):count(cost.missing)+' 次调用缺少单价或完整用量')+unconfirmed;
}
function speedName(value){const key=(value||'').trim().toLowerCase();return {default:'Standard',standard:'Standard',priority:'Fast',fast:'Fast',ultrafast:'Ultra-fast','ultra-fast':'Ultra-fast',ultra_fast:'Ultra-fast',flex:'Flex',auto:'Auto'}[key]||value||'未记录'}
function codexTierUnconfirmed(item){return (item.provider||'').trim().toLowerCase()==='codex'&&!['apikey','api_key','api-key'].includes((item.auth_type||'').trim().toLowerCase())&&['','auto','default','standard'].includes((item.response_service_tier||'').trim().toLowerCase())}
function serviceSpeed(item){
  const outbound=(item.outbound_service_tier||'').trim();
  if(outbound)return speedName(outbound)+'（出站）';
  const requested=(item.service_tier||'').trim(),reported=(item.response_service_tier||'').trim();
  if(codexTierUnconfirmed(item))return requested&&requested.toLowerCase()!=='auto'?speedName(requested)+'（客户端，出站未采集）':'出站未采集';
  if(reported&&reported.toLowerCase()!=='auto'){
    return speedName(reported)+'（响应，出站未采集）';
  }
  return requested?speedName(requested)+'（客户端，出站未采集）':'出站未采集';
}
function speedDescription(item){return '最终出站 service_tier：'+(item.outbound_service_tier||'未采集')+'\n客户端 service_tier：'+(item.service_tier||'未记录')+'\n上游响应 service_tier：'+(item.response_service_tier||'未记录')+(item.outbound_service_tier==='auto'?'\n最终出站未指定档位或指定 auto，由上游自动选择。':'')+(!item.outbound_service_tier?'\n此记录未采集最终出站档位，客户端原始值无法确认配置规则处理后的结果。':'')}
function tierBasis(quote){return {response:'上游响应确认',outbound:'按最终出站档位估价，上游未确认',request:'按历史客户端档位估价，出站未采集',codex_request:'按历史客户端档位估价，出站未采集；Codex 响应无法确认实际档位',standard_reference:'档位未确认，按 Standard 基准参考价'}[quote?.tier_source]||''}
function priceCaption(quote){return (quote?.service_tier?speedName(quote.service_tier)+' · ':'')+(quote?.rates?'输入 '+money(quote.rates.input)+' / 输出 '+money(quote.rates.output):priceReason(quote?.reason))}
function priceDescription(quote){if(!quote)return '未定价';return [quote.provider+' / '+quote.model,priceCaption(quote),tierBasis(quote),'缓存读 '+money(quote.rates?.cache_read)+' / 写 '+money(quote.rates?.cache_write),quote.context_above?'上下文 > '+count(quote.context_above)+' Token':'标准上下文','USD / 百万 Token',quote.source||''].filter(Boolean).join('\n')}
function priceSources(report){return 'OpenAI 档位单价：官方价格表 · 快照 '+time(report?.tier_price_date||data?.tier_price_date)+'；其他模型：models.dev · 快照 '+time(report?.price_date||data?.price_date)+' · USD / 百万 Token。'}
function clientKey(item){return item.api_key||(item.key&&item.key!=='未知 Key'?'历史匿名标识（无法还原）：'+item.key:'未采集 API Key')}
function renderMetrics(){
  const s=data.summary,body=$('metrics');body.replaceChildren();
  const completed=s.succeeded+s.failed,success=completed?(100*s.succeeded/completed).toFixed(1)+'%':'—';
  const values=[['下游请求数',count(s.requests),count(s.executions)+' 次上游调用 · '+count(s.running)+' 进行中'],['完成成功率',success,count(s.succeeded)+' 成功 · '+count(s.failed)+' 未成功'],['平均总耗时',s.duration_count?duration(s.total_ms/s.duration_count):'—','包含调度、等待与重试'],['手动分配等待',recordedWait(s.manual_wait_ms,s.manual_unknown),'当前筛选范围累计'],['5h 实际等待',recordedWait(s.quota_wait_ms,s.quota_unknown),'计划累计 '+duration(s.quota_planned_ms)]];
  for(const [title,value,note] of values){const card=element('div',undefined,'metric');card.append(element('span',title,'muted'),element('strong',value),element('small',note));body.append(card)}
  const signals=$('usageMetrics');signals.replaceChildren();
  for(const [title,value,note] of [
    ['总 Token',count(s.total),'输入 '+count(s.input)+' · 输出 '+count(s.output)],
    ['缓存读取率',cacheRate(s),'缓存读取 ÷ 总输入'],
    ['API 参考费用',costLabel(s.cost),costNote(s.cost)],
    ['平均首字',ttft(s),'从上游调用开始计时'],
    ['TPS（不含首字）',tps(s),'输出 ÷（执行耗时 − 首字耗时）'],
    ['TPS（含首字）',endToEndTPS(s),'输出 ÷ 执行耗时']
  ])signals.append(detailMetric(title,value,note));
  $('priceSource').textContent=priceSources(data)+'参考费用优先采用可确认的响应档位；Codex 的 default/auto 不确认实际档位，按下游请求档位估价。不代表套餐实扣账单。';
}
function svgNode(tag,attributes,text){const node=document.createElementNS('http://www.w3.org/2000/svg',tag);for(const [key,value] of Object.entries(attributes||{}))node.setAttribute(key,String(value));if(text!==undefined)node.textContent=text;return node}
function trendConfig(){
  const series=(name,color,value)=>({name,color,value});
  if(trendMode==='requests')return {title:'请求量',unit:'次',format:count,series:[series('下游请求','var(--app-accent)',b=>b.requests||0),series('上游调用','var(--chart-quota)',b=>b.executions||0),series('未成功请求','var(--app-danger)',b=>b.failed||0)]};
  if(trendMode==='cost')return {title:'API 参考费用',unit:'USD',format:money,series:[series('已估价费用','var(--app-accent)',b=>b.cost?.priced||!b.executions?b.cost?.usd||0:null)]};
  if(trendMode==='wait')return {title:'等待时间',unit:'累计时长',format:duration,series:[series('手动分配等待','var(--chart-manual)',b=>b.manual_wait_ms||0),series('5h 实际等待','var(--chart-quota)',b=>b.quota_wait_ms||0)]};
  return {title:'Token 用量',unit:'Token',format:count,series:[series('总 Token','var(--app-accent)',b=>b.total||0),series('输入（含缓存）','var(--chart-input)',b=>b.input||0),series('输出（含推理）','var(--chart-output)',b=>b.output||0)]};
}
function trendValue(config,bin){
  if(trendMode==='cost')return costLabel(bin.cost);
  if(trendMode==='wait')return recordedWait((bin.manual_wait_ms||0)+(bin.quota_wait_ms||0),(bin.manual_unknown||0)+(bin.quota_unknown||0));
  return config.format(config.series[0].value(bin));
}
function renderTrend(){
  const body=$('trendChart'),detail=$('trendDetail'),legend=$('trendLegend'),summary=$('trendSummary'),config=trendConfig(),bins=data.buckets||[];
  body.replaceChildren();detail.replaceChildren();legend.replaceChildren();summary.replaceChildren();body.onpointermove=body.onpointerdown=body.onkeydown=null;
  body.setAttribute('aria-label',config.title+'趋势图');
  const granularity=data.bucket||appliedFilters.bucket||'day',grain={hour:'小时',day:'天',month:'月'}[granularity];
  $('trendScope').textContent=[appliedFilters.start+' → '+appliedFilters.end,'按'+grain+' · '+data.timezone,appliedFilters.model||'全部模型',appliedFilters.provider||'全部 Provider',resultNames[appliedFilters.outcome],appliedFilters.user_agent?'User-Agent · '+appliedFilters.user_agent:'',appliedFilters.q?'搜索 · '+appliedFilters.q:''].filter(Boolean).join(' · ');
  for(const button of $('trendTabs').querySelectorAll('button')){const active=button.dataset.trend===trendMode;button.classList.toggle('active-tab',active);button.setAttribute('aria-pressed',String(active))}
  const summaryItems=[['当前范围合计',trendValue(config,data.summary)],['统计时段',count(bins.length)+' '+({hour:'小时',day:'天',month:'个月'}[granularity])]];
  if(trendMode==='cost')summaryItems.push(['估价情况',costNote(data.summary.cost)]);
  else if(bins.length){const peak=bins.reduce((best,bin)=>(config.series[0].value(bin)||0)>(config.series[0].value(best)||0)?bin:best,bins[0]);summaryItems.push([config.series[0].name+'峰值',config.format(config.series[0].value(peak))+' · '+peak.label])}
  for(const [label,value] of summaryItems){const item=element('div');item.append(element('span',label),element('strong',value));summary.append(item)}
  for(const item of config.series){const label=element('span'),dot=element('i');dot.style.background=item.color;label.append(dot,document.createTextNode(item.name));legend.append(label)}
  if(trendMode==='cost'&&data.summary.cost?.missing){const label=element('span','○ 含待定调用','warning');legend.append(label)}
  if(!data.total||!bins.length){body.append(element('p',data.enabled?'当前筛选范围没有请求记录':'统计采集已关闭','empty'));return}
  const width=Math.max(240,body.clientWidth||900),height=260,left=70,right=18,top=22,bottom=38,plotWidth=width-left-right,plotHeight=height-top-bottom;
  let maximum=0;for(const bin of bins)for(const item of config.series)maximum=Math.max(maximum,item.value(bin)||0);
  const rawStep=(maximum||1)/4,magnitude=Math.pow(10,Math.floor(Math.log10(rawStep))),factor=[1,2,5,10].find(v=>v>=rawStep/magnitude)||10;
  const tick=Math.max(trendMode==='cost'?0.000001:1,factor*magnitude),ceiling=tick*4;
  const x=index=>left+(bins.length===1?plotWidth/2:index*plotWidth/(bins.length-1)),y=value=>top+plotHeight*(1-value/ceiling);
  const svg=svgNode('svg',{viewBox:`0 0 ${width} ${height}`,role:'img','aria-label':config.title+'，'+bins.length+' 个时段'});
  svg.append(svgNode('title',{},config.title+'；使用图表下方的时段明细查看精确数值'));
  const axisValue=value=>trendMode==='cost'?money(value):trendMode==='wait'?duration(value):new Intl.NumberFormat('en',{notation:'compact',maximumFractionDigits:1}).format(value);
  for(let i=0;i<=4;i++){const value=tick*i;svg.append(svgNode('line',{x1:left,x2:width-right,y1:y(value),y2:y(value),class:'chart-grid'}),svgNode('text',{x:left-10,y:y(value)+4,'text-anchor':'end',class:'chart-label'},axisValue(value)))}
  svg.append(svgNode('text',{x:left,y:12,class:'chart-label'},config.unit));
  const labels=Math.min(bins.length,Math.max(2,Math.floor(plotWidth/125)));
  for(let i=0;i<labels;i++){const index=labels===1?0:Math.round(i*(bins.length-1)/(labels-1)),label=bins[index].label;
    svg.append(svgNode('text',{x:x(index),y:height-12,'text-anchor':i===0?'start':i===labels-1?'end':'middle',class:'chart-label'},granularity==='hour'?label.slice(5,16):granularity==='day'?label.slice(5,10):label));
  }
  config.series.forEach((item,seriesIndex)=>{
    let segment=[];const flush=()=>{
      if(!segment.length)return;
      const line=segment.map(([px,py],i)=>(i?'L':'M')+px.toFixed(2)+','+py.toFixed(2)).join(' ');
      if(seriesIndex===0&&segment.length>1)svg.append(svgNode('path',{d:line+` L${segment.at(-1)[0]},${y(0)} L${segment[0][0]},${y(0)} Z`,fill:item.color,opacity:'.07',class:'trend-area'}));
      svg.append(svgNode('path',{d:line,fill:'none',stroke:item.color,'stroke-width':seriesIndex===0?2.5:1.8,'stroke-linejoin':'round','stroke-linecap':'round','stroke-dasharray':seriesIndex===1?'5 4':'',class:'trend-line'}));
      if(bins.length<=32||segment.length===1)for(const [px,py] of segment)svg.append(svgNode('circle',{cx:px,cy:py,r:3,fill:item.color,class:'trend-point'}));
      segment=[];
    };
    bins.forEach((bin,index)=>{const value=item.value(bin);if(Number.isFinite(value))segment.push([x(index),y(value)]);else flush()});flush();
  });
  if(trendMode==='cost')bins.forEach((bin,index)=>{if(bin.cost?.missing){const marker=svgNode('circle',{cx:x(index),cy:y(config.series[0].value(bin)||0),r:5,fill:'var(--app-surface-strong)',stroke:'var(--app-warning)','stroke-width':2,class:'trend-pending'});marker.append(svgNode('title',{},bin.label+'：'+costLabel(bin.cost)+'；'+costNote(bin.cost)));svg.append(marker)}});
  const cursor=svgNode('g',{'aria-hidden':'true',class:'trend-cursor'});svg.append(cursor);body.append(svg);
  let selected=-1;
  function select(index){
    index=Math.max(0,Math.min(bins.length-1,index));if(index===selected)return;selected=index;
    const bin=bins[index];trendSelection=bin.label;cursor.replaceChildren(svgNode('line',{x1:x(index),x2:x(index),y1:top,y2:y(0),stroke:'var(--app-border-strong)','stroke-dasharray':'4 4'}));
    for(const item of config.series){const value=item.value(bin);if(Number.isFinite(value))cursor.append(svgNode('circle',{cx:x(index),cy:y(value),r:4,fill:'var(--app-surface-strong)',stroke:item.color,'stroke-width':2}))}
    detail.replaceChildren(element('strong',bin.label+' · '+data.timezone,'trend-detail-time'));
    const values=element('div',undefined,'trend-detail-values');
    for(const item of config.series)values.append(labeledValue(item.name,Number.isFinite(item.value(bin))?config.format(item.value(bin)):'待定'));
    if(trendMode==='tokens'){values.append(labeledValue('缓存读取',count(bin.cache)),labeledValue('缓存写入',count(bin.cache_creation)),labeledValue('推理',count(bin.reasoning)),labeledValue('缓存率',cacheRate(bin)));if(bin.unclassified)values.append(labeledValue('未分类',count(bin.unclassified)))}
    if(trendMode==='requests')values.append(labeledValue('成功',count(bin.succeeded)),labeledValue('进行中',count(bin.running)));
    detail.append(values);
    if(trendMode==='cost'){detail.append(element('small',bin.executions?costNote(bin.cost):'此时段无上游调用',bin.cost?.missing?'warning':''));if(bin.cost?.missing)detail.append(element('small','曲线仅计已估价金额；空心点表示含待定调用，待定部分不按零费用计算。'))}
    if(trendMode==='wait'&&(bin.manual_unknown||bin.quota_unknown))detail.append(element('small','有 '+count((bin.manual_unknown||0)+(bin.quota_unknown||0))+' 项等待未完整计时，仅展示已记录时长。','warning'));
  }
  const remembered=bins.findIndex(bin=>bin.label===trendSelection),lastUsed=bins.findLastIndex(bin=>bin.requests>0);select(remembered>=0?remembered:Math.max(0,lastUsed));
  const point=event=>{const rect=svg.getBoundingClientRect(),px=(event.clientX-rect.left)*width/rect.width;select(bins.length===1?0:Math.round((px-left)/plotWidth*(bins.length-1)))};
  body.onpointermove=event=>{if(event.pointerType!=='touch')point(event)};body.onpointerdown=point;
  body.onkeydown=event=>{if(!['ArrowLeft','ArrowRight','Home','End'].includes(event.key))return;event.preventDefault();select(event.key==='Home'?0:event.key==='End'?bins.length-1:selected+(event.key==='ArrowRight'?1:-1))};
}

function renderGroups(){
  const body=$('groups');body.replaceChildren();$('groupTitle').textContent={models:'模型',providers:'上游 Provider',credentials:'上游 Credential',keys:'下游 API Key'}[group];
  $('groupDescription').textContent=group==='keys'?'按来源明文 API Key 归集请求及全部调用用量；同一请求重试只计一次，Token 与参考费用累计所有调用。':'按实际模型、上游 Provider 或 Credential 归集调用用量；同一请求可能使用多个上游，各组请求数不能直接相加。';
  const groups=data[group]||[];if(!groups.length){empty(body,12,'当前范围没有分组数据');return}
  for(const item of groups){
    const row=element('tr'),name=element('td',undefined,'breakdown-name');
    name.append(element(group==='keys'?'code':'span',group==='credentials'?credential(item.name):item.name));
    if(group==='credentials')name.append(element('code',item.name));
    const tokens=element('td',undefined,'group-tokens');tokens.append(tokenBreakdown(item));
    const cost=element('td',costLabel(item.cost));cost.title=costNote(item.cost);if(item.cost?.missing||item.cost?.tier_unconfirmed)cost.append(element('small',costNote(item.cost),'request-cost-note'));
    if(item.prices?.length){const prices=element('small',item.prices.length===1?priceCaption(item.prices[0]):item.prices.length+' 组 API 单价','traffic-price-caption');prices.title=item.prices.map(priceDescription).join('\n\n');cost.append(prices)}
    row.append(name,element('td',count(item.requests)+' / '+count(item.executions)),tokens,element('td',count(item.total)),element('td',cacheRate(item)),cost,element('td',count(item.failed_executions)),element('td',item.executions?duration(item.upstream_ms/item.executions):'—'),element('td',ttft(item)),element('td',tps(item)),element('td',endToEndTPS(item)),element('td',recordedWait(item.manual_wait_ms+item.quota_wait_ms,item.manual_unknown+item.quota_unknown)));body.append(row);
  }
}
function outcomeBadge(outcome){
  const tone={succeeded:'good',failed:'bad',running:'running',canceled:'neutral',rejected:'warning',interrupted:'warning'}[outcome]||'neutral';
  return element('span',resultNames[outcome]||outcome||'未知','badge request-outcome '+tone);
}
function clipped(tag,text,className=''){
  const node=element(tag,text,'request-truncate '+className);node.title=text||'';return node;
}
function labeledValue(label,value,className=''){
  const line=element('div',undefined,'request-value '+className);
  line.append(element('span',label),element('strong',value));return line;
}
function totalDuration(record,metrics,now){
  if(validTime(record.completed_at))return metrics.total_ms;
  return record.outcome==='running'&&validTime(record.started_at)?Math.max(0,now-Date.parse(record.started_at)):NaN;
}
function tokenParts(metrics){
  return [['输入',metrics.input,'输入包含缓存读取与缓存写入'],['净输入',metrics.uncached,'未命中缓存的输入'],['缓存读取',metrics.cache],['缓存写入',metrics.cache_creation],['输出',metrics.output,'输出包含推理 Token'],['推理',metrics.reasoning],...(metrics.unclassified?[['未分类',metrics.unclassified]]:[])];
}
function tokenBreakdown(metrics){
  const body=element('div',undefined,'token-breakdown');
  for(const [label,value,note] of tokenParts(metrics)){
    const line=labeledValue(label,Number.isFinite(value)?count(value):'未知');if(note)line.title=note;body.append(line);
  }
  return body;
}
function renderRequestScope(){
  const scope=$('requestScope');scope.replaceChildren();
  const labels=[['日期',appliedFilters.start+' → '+appliedFilters.end],['模型',appliedFilters.model],['Provider',appliedFilters.provider],['结果',resultNames[appliedFilters.outcome]||appliedFilters.outcome],['User-Agent',appliedFilters.user_agent],['搜索',appliedFilters.q]];
  for(const [label,value] of labels)if(value)scope.append(clipped('span',label+' · '+value,'request-filter-chip'));
  scope.append(element('span','按开始时间倒序 · '+data.timezone,'request-scope-note'));
}
function renderRequests(){
  const body=$('requests');
  const focusedID=body.contains(document.activeElement)?document.activeElement.dataset.requestId:null,focusedPrice=document.activeElement?.classList.contains('request-price-open');
  body.replaceChildren();renderRequestScope();
  $('requestCount').textContent=count(data.total);
  $('requestListStatus').textContent=data.enabled?($('autoRefresh').checked?'每 5 秒更新':'手动刷新')+' · '+new Date(data.collected_at).toLocaleTimeString('zh-CN',{hour12:false}):'统计采集已关闭';
  if(!data.events.length){
    const row=element('tr'),cell=element('td',undefined,'empty request-empty');cell.colSpan=10;
    cell.append(element('span','≡','request-empty-icon'),element('strong',data.enabled?'没有匹配的请求':'统计记录已关闭'),element('small',data.enabled?'试试扩大日期范围，或调整上方的模型、结果与搜索条件。':'可在下方「统计存储设置」中重新启用。'));
    row.append(cell);body.append(row);
  }
  for(const item of data.events){
    const row=element('tr'),identity=element('td',undefined,'request-identity');
    identity.append(element('strong',validTime(item.started_at)?new Date(item.started_at).toLocaleTimeString('zh-CN',{hour12:false}):'—'),element('small',validTime(item.started_at)?new Date(item.started_at).toLocaleDateString('zh-CN'):'—'),clipped('code',item.id));
    identity.title=time(item.started_at)+(item.trace_id?'\nTrace '+item.trace_id:'');
    const session=element('td',undefined,'request-model');session.append(clipped('strong',item.requested_model||item.model||'未知模型'));
    if(item.requested_model&&item.requested_model!==item.model)session.append(clipped('small','执行 · '+item.model));
    session.append(clipped('small','响应 · '+(item.response_model||'未记录'),'request-response-model'),clipped('small','Effort · '+(item.reasoning_effort||'未记录'),'request-effort'),element('small',item.stream?'流式':'非流式'),clipped('code',item.session_id||'未提供 Session'));
    const source=element('td',undefined,'request-source');source.append(element('code',clientKey(item),'request-source-key'),element('small','IP · '+(item.client_ip||'未采集')),clipped('small','UA · '+(item.user_agent||'未采集'),'request-user-agent'));
    const speed=clipped('small','速度 · '+serviceSpeed(item),'request-service-tier');speed.title=speedDescription(item);session.insertBefore(speed,session.querySelector('.request-effort').nextSibling);
    const auth=element('td',undefined,'request-credential');auth.append(clipped('strong',item.provider||'未记录 Provider'),clipped('small',credential(item.auth_index||item.auth_id)));
    if(item.auth_index||item.auth_id)auth.append(clipped('code',item.auth_index||item.auth_id));
    const outcome=element('td');outcome.append(outcomeBadge(item.outcome),element('small',count(item.executions)+' 次调用 · '+count(item.selection_count)+' 次选择'));
    if(item.status_code)outcome.append(element('small','HTTP '+item.status_code));
    if(item.error)outcome.append(clipped('small',item.error,'request-row-error'));
    const totals=element('td',undefined,'request-tokens');totals.append(labeledValue('总计',count(item.total),'token-total'),tokenBreakdown(item),labeledValue('缓存率',cacheRate(item)));
    const cost=element('td',undefined,'request-cost');cost.append(element('strong',costLabel(item.cost)));cost.title=costNote(item.cost);if(item.cost?.missing||item.cost?.tier_unconfirmed)cost.append(element('small',costNote(item.cost),'request-cost-note'));
    const priceButton=element('button','单价与费用 ↗','secondary request-price-open');priceButton.onclick=()=>openDetail(item.id,'traffic');priceButton.setAttribute('aria-label','查看请求 '+item.id+' 的 API 单价与费用');priceButton.dataset.requestId=item.id;cost.append(priceButton);
    if(item.prices?.length)cost.title+='\n\n'+item.prices.map(priceDescription).join('\n\n');
    const waits=element('td',undefined,'request-waits');waits.append(labeledValue('5h 实际',recordedWait(item.quota_wait_ms,item.quota_unknown)),labeledValue('计划',duration(item.quota_planned_ms)));
    if(item.manual_wait_ms||item.manual_unknown)waits.append(labeledValue('手动分配',recordedWait(item.manual_wait_ms,item.manual_unknown)));
    const timing=element('td',undefined,'request-performance');timing.append(labeledValue(item.outcome==='running'?'总耗时 · 计时中':'总耗时',duration(totalDuration(item,item,Date.parse(data.collected_at)))),labeledValue('首字',ttft(item)),labeledValue('TPS（不含首字）',tps(item)),labeledValue('TPS（含首字）',endToEndTPS(item)));
    const action=element('td'),button=element('button','查看 →','secondary request-open');button.dataset.requestId=item.id;button.setAttribute('aria-label','查看请求 '+item.id+' 的详情');button.onclick=()=>openDetail(item.id);action.append(button);
    row.append(identity,source,session,auth,outcome,totals,cost,waits,timing,action);body.append(row);
    if(focusedID===item.id)(focusedPrice?priceButton:button).focus({preventScroll:true});
  }
  const first=data.total?(page-1)*data.page_size+1:0,last=Math.min(page*data.page_size,data.total);
  $('pageInfo').textContent=count(first)+'–'+count(last)+' 条 / 共 '+count(data.total)+' 条 · 第 '+page+' / '+Math.max(1,Math.ceil(data.total/data.page_size))+' 页';
  $('previous').disabled=page<=1;$('next').disabled=page*data.page_size>=data.total;
}
function render(){renderMetrics();updateOptions($('modelFilter'),data.model_options,'全部模型');updateOptions($('providerFilter'),data.provider_options,'全部 Provider');renderTrend();renderGroups();renderRequests();$('storageStatus').textContent=(data.enabled?'正在采集':'采集已关闭')+' · '+data.db_path+' · 时区 '+data.timezone+' · 最近写入 '+time(data.last_write)+(data.skipped?' · 无法读取 '+data.skipped+' 条记录':'');if(!storageInitialized){$('usageEnabled').checked=data.enabled;$('dbPath').value=data.db_path||defaults.usage_db_path;storageInitialized=true}message(data.collect_err||(!data.enabled?'统计采集已关闭，可在下方存储设置中启用。':data.skipped?'存在无法读取的记录，请查看数据库。':''))}
function reportQuery(){return new URLSearchParams({...appliedFilters,page:String(page),page_size:$('requestPageSize').value}).toString()}
async function refresh(){if(!managementAuth.ready)return;if(busy){refreshQueued=true;return}busy=true;const query=reportQuery(),revision=managementAuth.revision;try{const next=await api(base+'/usage?'+query);if(query!==reportQuery()||revision!==managementAuth.revision){refreshQueued=true;return}if(advancePresetRange(next)){refreshQueued=true;return}const lastPage=Math.max(1,Math.ceil(next.total/next.page_size));if(page>lastPage){page=lastPage;refreshQueued=true;return}data=next;$('connection').textContent='已连接 · '+time(data.collected_at);render()}catch(error){message(error.message);$('connection').textContent=managementAuth.ready?'连接异常':'请登录主管理界面'}finally{busy=false;if(refreshQueued){refreshQueued=false;queueMicrotask(refresh)}}}
async function copyValue(value,button){
  const label=button.textContent;button.disabled=true;
  try{await navigator.clipboard.writeText(value);button.textContent='已复制'}
  catch{button.textContent='复制失败，请手动选择';button.title='浏览器未允许剪贴板访问，可直接选择旁边的文本复制。'}
  setTimeout(()=>{button.textContent=label;button.disabled=false},1800);
}
function detailField(body,label,value,copy=false){
  const row=element('div',undefined,'request-field'),content=element('dd');
  content.append(element(copy?'code':'span',value||'—'));
  if(copy&&value){const button=element('button','复制','secondary request-copy');button.setAttribute('aria-label','复制'+label);button.onclick=()=>copyValue(value,button);content.append(button)}
  row.append(element('dt',label),content);body.append(row);
}
function detailMetric(title,value,note,tone=''){
  const card=element('div',undefined,'request-detail-metric '+tone);card.append(element('span',title),element('strong',value),element('small',note));return card;
}
function showDetailTab(name){
  for(const button of $('detailTabs').querySelectorAll('button')){
    const selected=button.dataset.detailTab===name;button.setAttribute('aria-selected',String(selected));button.tabIndex=selected?0:-1;$(button.getAttribute('aria-controls')).hidden=!selected;
  }
}
function renderDetailOverview(record,metrics,now){
  const identity=$('detailIdentity');identity.replaceChildren();
  for(const [label,value,copy] of [
    ['来源 API Key',clientKey(record),Boolean(record.api_key)],['来源 IP',record.client_ip||'未采集',Boolean(record.client_ip)],['来源 User-Agent',record.user_agent||'未采集',Boolean(record.user_agent)],
    ['请求 ID',record.id,true],['Trace ID',record.trace_id,true],['Session',record.session_id,true],['父 Session',record.parent_session_id,true],
    ['请求模型',record.requested_model||record.model],['执行模型',record.model],['上游响应模型',record.response_model||'未记录'],['Effort',record.reasoning_effort||'未记录'],['出站档位',serviceSpeed(record)],['最终出站 service_tier',record.outbound_service_tier||'未采集'],['客户端 service_tier',record.service_tier||'未记录'],['响应 service_tier',record.response_service_tier||'未记录'],['上游 Provider',record.provider],
    ['最终 Credential',credential(record.auth_index||record.auth_id)],['Credential ID',record.auth_id,true],['Credential 索引',record.auth_index,true],
    ['响应方式',record.stream?'流式响应':'非流式响应'],['开始时间',time(record.started_at)],['完成时间',record.outcome==='running'?'进行中':time(record.completed_at)],
    ['HTTP 状态',record.status_code?String(record.status_code):'未记录']
  ])detailField(identity,label,value,copy);
  const tokens=$('detailTokens');tokens.replaceChildren();
  tokens.append(labeledValue('总 Token',count(metrics.total),'request-total'));
  tokens.append(tokenBreakdown(metrics));
  tokens.append(labeledValue('缓存读取率',cacheRate(metrics)),labeledValue('API 参考费用',costLabel(metrics.cost)));if(metrics.cost?.missing||metrics.cost?.tier_unconfirmed)tokens.append(element('p',costNote(metrics.cost),'request-footnote request-cost-note'));tokens.append(element('p','输入包含缓存读取与写入，输出包含推理；子项不重复相加。缓存率按总输入加权。','request-footnote'));
  const timing=$('detailTiming');timing.replaceChildren();
  for(const [label,value] of [
    [record.outcome==='running'?'总耗时（计时中）':'总耗时',duration(totalDuration(record,metrics,now))],
    ['上游执行累计',duration(metrics.upstream_ms)],['手动分配等待',recordedWait(metrics.manual_wait_ms,metrics.manual_unknown)],
    ['5h 节流实际等待',recordedWait(metrics.quota_wait_ms,metrics.quota_unknown)],['5h 节流计划等待',duration(metrics.quota_planned_ms)],
    ['平均首字延迟',ttft(metrics)],['TPS（不含首字）',tps(metrics)],['TPS（含首字）',endToEndTPS(metrics)]
  ])timing.append(labeledValue(label,value));
  timing.append(element('p','总耗时包含等待、调度和重试。TPS（不含首字）= 输出 Token ÷（执行耗时 − 首字耗时）；TPS（含首字）= 输出 Token ÷ 执行耗时。两者不含调用前的调度和等待，仅汇总有效成功调用；缺少 TTFT 时不推算不含首字 TPS。','request-footnote'));
}
function executionTokens(item){
  const t=item.tokens||{},b=t.TokenBreakdown;
  if(b?.schema_version===2)return {input:b.input.total_tokens,output:b.output.total_tokens,cache:b.input.cache_read_tokens,cache_creation:b.input.cache_write_tokens,uncached:b.input.uncached_tokens,reasoning:b.output.reasoning_tokens,unclassified:b.unclassified_tokens,total:b.total_tokens,accounting_unknown:b.quality==='complete'?0:1};
  return {input:Math.max(0,t.InputTokens||0),output:Math.max(0,t.OutputTokens||0),cache:Math.max(0,t.CacheReadTokens||t.CachedTokens||0),cache_creation:Math.max(0,t.CacheCreationTokens||0),reasoning:Math.max(0,t.ReasoningTokens||0),total:Math.max(0,t.TotalTokens||((t.InputTokens||0)+(t.OutputTokens||0))),accounting_unknown:1};
}
function executionSpeed(item,includeFirstToken=false){
  const tokens=executionTokens(item),elapsed=includeFirstToken?item.latency_ms:item.latency_ms-item.ttft_ms;
  return !item.failed&&!tokens.accounting_unknown&&elapsed>0&&tokens.output>0&&(includeFirstToken||item.ttft_ms>0)?(tokens.output*1000/elapsed).toFixed(1)+' tok/s':'未知';
}
function renderDetailTraffic(record,metrics,detail){
  $('detailCostStatus').textContent='参考费用：'+costLabel(metrics.cost)+'。'+costNote(metrics.cost)+(metrics.cost?.missing?'。待定表示数据不足，未计入金额，不代表免费；可在执行时间线查看每次调用的原因。':'。');
  const quotes=[],seen=new Set();for(const quote of (record.executions||[]).map(item=>item.price)){if(!quote)continue;const key=JSON.stringify([quote.provider,quote.model,quote.service_tier,quote.tier_source,quote.context_above,quote.rates,quote.reason]);if(!seen.has(key)){quotes.push(quote);seen.add(key)}}
  const body=$('detailPrices');body.replaceChildren();
  const wrap=element('div',undefined,'table-wrap'),table=element('table',undefined,'request-price-table'),thead=element('thead'),tr=element('tr');
  for(const label of ['计价模型 / 速度','输入','缓存读取','缓存写入','输出','推理']){const th=element('th',label);th.scope='col';tr.append(th)}thead.append(tr);table.append(thead);
  const tbody=element('tbody');if(!quotes.length)empty(tbody,6,'暂无可用于计价的模型和用量');
  for(const quote of quotes){const row=element('tr'),model=element('td',quote.model);model.append(element('strong',speedName(quote.service_tier),'request-price-tier'),element('small',tierBasis(quote)),element('small',quote.provider+(quote.context_above?' · 上下文 > '+count(quote.context_above):' · 标准上下文')));row.append(model);for(const rate of [quote.rates?.input,quote.rates?.cache_read,quote.rates?.cache_write,quote.rates?.output,quote.rates?.reasoning??quote.rates?.output])row.append(element('td',money(rate)));tbody.append(row)}table.append(tbody);wrap.append(table);body.append(wrap);
  $('detailPriceSource').textContent=priceSources(detail)+'OpenAI 缓存写入按 30 分钟参考价，当前用量记录未区分 12 小时写入。';
}
function timelineWait(record,start,finish,actual){
  if(validTime(finish))return duration(actual);
  if(validTime(start))return record.outcome==='running'?duration(actual)+' · 等待中':duration(actual)+' + 未知';
  return '未记录';
}
function renderDetailTimeline(record){
  const events=[{at:record.started_at,title:'接收请求',tone:'received',description:[record.requested_model||record.model,record.stream?'流式响应':'非流式响应'].filter(Boolean).join(' · ')}];
  (record.admissions||[]).forEach((item,index)=>events.push({
    at:item.started_at,title:'手动分配 · '+(index+1),tone:validTime(item.finished_at)?'manual':'warning',
    description:(admissionNames[item.result]||item.result||(record.outcome==='running'?'等待分配':'结果未知'))+' · '+timelineWait(record,item.started_at,item.finished_at,item.actual_ms),
    fields:[['开始',time(item.started_at)],['完成',time(item.finished_at)],['实际等待',timelineWait(record,item.started_at,item.finished_at,item.actual_ms)],['指定 Credential',credential(item.auth_id)],['此前 Credential',credential(item.previous_auth_id)]]
  }));
  (record.selections||[]).forEach((item,index)=>{
    const wait=item.planned_ms||validTime(item.wait_started_at),waiting=wait&&!validTime(item.wait_finished_at);
    events.push({at:item.selected_at,title:'Credential 选择 · '+(index+1),tone:waiting?'warning':'selection',
      description:credential(item.auth_index||item.auth_id)+' · '+(wait?'5h 等待 '+timelineWait(record,item.wait_started_at,item.wait_finished_at,item.actual_ms):'无需节流等待'),
      fields:[['Credential ID',item.auth_id],['Credential 索引',item.auth_index],['Provider / 模型',[item.provider,item.model].filter(Boolean).join(' / ')],['5h 计划',duration(item.planned_ms)],['5h 实际',wait?timelineWait(record,item.wait_started_at,item.wait_finished_at,item.actual_ms):duration(0)],['等待开始',time(item.wait_started_at)],['等待结束',time(item.wait_finished_at)],['调度依据',item.reason],['额度观测时间',time(item.quota_observed_at)]],quota:item.quota_windows
    });
  });
  (record.executions||[]).forEach((item,index)=>{
    const tokens=executionTokens(item),validTTFT=!item.failed&&item.ttft_ms>0&&item.latency_ms>=item.ttft_ms;
    events.push({at:item.timestamp,title:'上游调用 · '+(index+1),tone:item.failed?'bad':'good',open:item.failed,
      description:[item.model,'响应 '+(item.response_model||'未记录'),'Effort '+(item.reasoning_effort||'未记录'),'速度 '+serviceSpeed(item),credential(item.auth_index||item.auth_id)].filter(Boolean).join(' · '),
      badge:outcomeBadge(item.failed?'failed':'succeeded'),
      metrics:[['执行',duration(item.latency_ms)],['首字',validTTFT?duration(item.ttft_ms):'未知'],['TPS（不含首字）',executionSpeed(item)],['TPS（含首字）',executionSpeed(item,true)],['Token',count(tokens.total)]],
      fields:[['调用 ID',item.id],['Provider',item.provider],['执行模型',item.model],['上游响应模型',item.response_model||'未记录'],['Effort',item.reasoning_effort||'未记录'],['出站档位',serviceSpeed(item)],['最终出站 service_tier',item.outbound_service_tier||'未采集'],['客户端 service_tier',item.service_tier||'未记录'],['响应 service_tier',item.response_service_tier||'未记录'],['模型别名',item.alias],['Credential ID',item.auth_id],['Credential 索引',item.auth_index],['HTTP 状态',item.status_code?String(item.status_code):'未记录'],...tokenParts(tokens).map(([label,value])=>[label,Number.isFinite(value)?count(value):'未知']),['缓存读取率',cacheRate(tokens)],['API 参考费用',item.price?.cost_usd!=null?money(item.price.cost_usd):priceReason(item.price?.reason)],['API 单价（USD / 百万 Token）',priceDescription(item.price)]]
    });
  });
  // Invalid timestamps remain visible after the ordered events, before the terminal state.
  events.sort((a,b)=>(validTime(a.at)?Date.parse(a.at):Infinity)-(validTime(b.at)?Date.parse(b.at):Infinity));
  events.push({at:record.completed_at,title:record.outcome==='running'?'请求进行中':'请求结束',tone:{succeeded:'good',failed:'bad',running:'running'}[record.outcome]||'warning',description:record.outcome==='running'?'可点击「刷新详情」查看最新进展。':resultNames[record.outcome]||record.outcome,badge:outcomeBadge(record.outcome)});
  const body=$('detailTimeline');body.replaceChildren();$('detailEventCount').textContent=count(events.length);
  for(const event of events){
    const row=element('li',undefined,'request-event '+event.tone),stamp=element('div',undefined,'request-event-time');
    const offset=validTime(event.at)&&validTime(record.started_at)?Math.max(0,Date.parse(event.at)-Date.parse(record.started_at)):NaN;
    stamp.append(element('strong',Number.isFinite(offset)?'+'+duration(offset):'—'),element('small',validTime(event.at)?new Date(event.at).toLocaleTimeString('zh-CN',{hour12:false}):'未记录时间'));stamp.title=time(event.at);
    const content=element(event.fields?'details':'div',undefined,'request-event-card'),heading=element(event.fields?'summary':'div',undefined,'request-event-summary'),title=element('div',undefined,'request-event-title');
    title.append(element('strong',event.title));if(event.badge)title.append(event.badge);
    heading.append(title,element('p',event.description));content.append(heading);
    if(event.metrics){const metrics=element('div',undefined,'request-event-metrics');for(const [label,value] of event.metrics)metrics.append(labeledValue(label,value));heading.append(metrics)}
    if(event.fields){
      content.open=Boolean(event.open);const fields=element('dl',undefined,'request-fields request-event-fields');
      for(const [label,value] of event.fields)if(value)detailField(fields,label,value);
      content.append(fields);
      if(event.quota?.length){const quota=element('details',undefined,'request-quota-snapshot');quota.append(element('summary','选择时的额度快照'),element('pre',JSON.stringify(event.quota,null,2)));content.append(quota)}
    }
    row.append(stamp,content);body.append(row);
  }
}
function renderDetail(detail){
  const record=detail.request,metrics=detail.summary,now=Date.now();
  $('detailSummary').textContent=time(record.started_at)+' · '+(record.provider||'未知 Provider');
  const hero=$('detailHero');hero.replaceChildren();
  const model=element('div',undefined,'request-hero-model');model.append(element('strong',record.requested_model||record.model||'未知模型'));
  if(record.requested_model&&record.requested_model!==record.model)model.append(element('small','执行模型 → '+record.model));
  const badges=element('div',undefined,'toolbar');badges.append(outcomeBadge(record.outcome),element('span',record.stream?'流式':'非流式','badge neutral'));if(record.status_code)badges.append(element('span','HTTP '+record.status_code,'badge neutral'));
  hero.append(model,badges);
  $('detailMetrics').replaceChildren(
    detailMetric('总 Token',count(metrics.total),count(metrics.executions)+' 次上游调用，包含重试','tokens'),
    detailMetric('总耗时',duration(totalDuration(record,metrics,now)),record.outcome==='running'?'计时中 · 包含等待和调度':'执行累计 '+duration(metrics.upstream_ms)),
    detailMetric('API 参考费用',costLabel(metrics.cost),costNote(metrics.cost)),
    detailMetric('缓存读取率',cacheRate(metrics),'缓存读取 ÷ 总输入')
  );
  $('detailError').hidden=!record.error;$('detailError').replaceChildren();
  if(record.error)$('detailError').append(element('strong','请求错误'),element('p',record.error));
  renderDetailOverview(record,metrics,now);renderDetailTraffic(record,metrics,detail);renderDetailTimeline(record);
  const {run_id,...request}=record;
  $('detailBody').textContent=JSON.stringify({request,summary:metrics},null,2);
  $('detailContent').hidden=false;
}
async function openDetail(id,tab='overview'){
  const isNew=detailID!==id||!$('detail').open,revision=++detailRevision,authRevision=managementAuth.revision;
  detailID=id;
  if(isNew){detailOpener=tab;$('detailContent').hidden=true;$('detailSummary').textContent='正在读取请求记录';showDetailTab(tab)}
  if(!$('detail').open)$('detail').showModal();
  $('detailFeedback').textContent='正在读取…';$('detailFeedback').classList.remove('bad');$('detailRefresh').disabled=true;$('detailContent').setAttribute('aria-busy','true');
  try{
    const detail=await api(base+'/usage/request?'+new URLSearchParams({id}));
    if(detailRevision!==revision||detailID!==id||authRevision!==managementAuth.revision)return;
    renderDetail(detail);$('detailFeedback').textContent='';
  }catch(error){
    if(detailRevision===revision&&detailID===id&&authRevision===managementAuth.revision){$('detailFeedback').textContent=($('detailContent').hidden?'读取失败：':'刷新失败，当前显示上次数据：')+error.message+'。可点击「刷新详情」重试。';$('detailFeedback').classList.add('bad')}
  }finally{
    if(detailRevision===revision){$('detailRefresh').disabled=false;$('detailContent').setAttribute('aria-busy','false')}
  }
}
$('usageRefresh').onclick=()=>{if(!managementAuth.ready)managementAuth.reconnect();refresh()};
$('filters').onsubmit=event=>{event.preventDefault();appliedRangeDays=$('range').value==='custom'?0:Number($('range').value);appliedFilters=filterValues();page=1;refresh()};$('range').onchange=()=>{if($('range').value!=='custom'){setRange(Number($('range').value));$('filters').elements.bucket.value=$('range').value==='1'?'hour':'day'}};for(const id of ['start','end'])$(id).onchange=()=>{$('range').value='custom'};
$('previous').onclick=()=>{page=Math.max(1,page-1);refresh()};$('next').onclick=()=>{page++;refresh()};
for(const button of $('groupTabs').querySelectorAll('button'))button.onclick=()=>{group=button.dataset.group;for(const tab of $('groupTabs').querySelectorAll('button'))tab.classList.toggle('active-tab',tab===button);if(data)renderGroups()};
for(const button of $('trendTabs').querySelectorAll('button'))button.onclick=()=>{trendMode=button.dataset.trend;if(data)renderTrend()};
let trendWidth=0;new ResizeObserver(entries=>{const width=entries[0].contentRect.width;if(width>0&&Math.abs(width-trendWidth)>1){trendWidth=width;if(data)renderTrend()}}).observe($('trendChart'));
$('requestPageSize').onchange=()=>{page=1;refresh()};
$('autoRefresh').onchange=()=>{if(data)renderRequests()};
$('detailClose').onclick=()=>$('detail').close();
$('detail').addEventListener('close',()=>{const id=detailID;detailID='';detailRevision++;$('detailRefresh').disabled=false;$('detailContent').setAttribute('aria-busy','false');if(!$('usagePanel').hidden){const opener=Array.from($('requests').querySelectorAll('button')).find(button=>button.dataset.requestId===id&&button.classList.contains(detailOpener==='traffic'?'request-price-open':'request-open'));opener?.focus({preventScroll:true})}});
$('detailRefresh').onclick=()=>{if(detailID)openDetail(detailID)};
$('detailCopyJSON').onclick=()=>copyValue($('detailBody').textContent,$('detailCopyJSON'));
const detailTabs=Array.from($('detailTabs').querySelectorAll('button'));
for(const [index,button] of detailTabs.entries()){
  button.onclick=()=>showDetailTab(button.dataset.detailTab);
  button.onkeydown=event=>{
    if(!['ArrowLeft','ArrowRight','Home','End'].includes(event.key))return;
    event.preventDefault();const next=event.key==='Home'?0:event.key==='End'?detailTabs.length-1:(index+(event.key==='ArrowRight'?1:-1)+detailTabs.length)%detailTabs.length;
    showDetailTab(detailTabs[next].dataset.detailTab);detailTabs[next].focus();
  };
}
$('storage').onsubmit=async event=>{event.preventDefault();if(!managementAuth.connected){message('请先登录主管理界面并连接');return}try{await api(configPath,'PATCH',{usage_report_enabled:$('usageEnabled').checked,usage_db_path:$('dbPath').value.trim()});message('设置已保存，等待配置热更新。');setTimeout(()=>{storageInitialized=false;refresh()},1500)}catch(error){message(error.message)}};
setRange(7);appliedFilters=filterValues();
setInterval(()=>{if($('autoRefresh').checked&&!document.hidden&&!$('usagePanel').hidden)refresh()},5000);
return {refresh,reset(){storageInitialized=false;if($('detail').open)$('detail').close();detailID='';detailRevision++;$('detailContent').hidden=true;$('detailBody').textContent=''}};
})();
