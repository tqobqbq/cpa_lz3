const credentialQuotaSync=(()=>{
  let store=null,providers=null,busy=false,lastSync=0,lastError='';
  const numeric=value=>value===undefined||value===null||String(value).trim()===''?null:Number.isFinite(Number(value))?Number(value):null;
  const boolean=value=>/^(true|1)$/i.test(String(value))?true:/^(false|0)$/i.test(String(value))?false:null;
  function timestamp(value){
    const number=numeric(value),at=number===null?Date.parse(value):number*1000;
    return Number.isFinite(at)&&at>0&&at<=8.64e15?at:null;
  }
  function rateWindow(headers,prefix,at){
    const used=numeric(headers[prefix+'used-percent']),minutes=numeric(headers[prefix+'window-minutes']);
    let reset=timestamp(headers[prefix+'reset-at']);
    const after=numeric(headers[prefix+'reset-after-seconds']);
    if(reset===null&&after!==null&&after>=0)reset=at+after*1000;
    if(used===null&&reset===null)return null;
    return {...used===null?{}:{used_percent:Math.max(0,Math.min(100,used))},...minutes===null||minutes<=0?{}:{limit_window_seconds:minutes*60},...reset===null?{}:{reset_at:reset/1000}};
  }
  function rateLimit(headers,prefix,at){
    const primary=rateWindow(headers,prefix+'primary-',at),secondary=rateWindow(headers,prefix+'secondary-',at);
    if(!primary&&!secondary)return null;
    const allowed=boolean(headers[prefix+'allowed']),reached=boolean(headers[prefix+'limit-reached']);
    return {...primary?{primary_window:primary}:{},...secondary?{secondary_window:secondary}:{},...allowed===null?{}:{allowed},...reached===null?{}:{limit_reached:reached}};
  }
  function codexPayload(headers,at){
    const main=rateLimit(headers,'x-codex-',at),review=rateLimit(headers,'x-codex-code-review-',at),additional=[];
    const namespaces=new Set();
    for(const name of Object.keys(headers)){
      const match=name.match(/^x-codex-(.+)-(?:primary|secondary)-(?:used-percent|window-minutes|reset-at|reset-after-seconds)$/);
      if(match&&match[1]!=='code-review')namespaces.add(match[1]);
    }
    for(const namespace of namespaces){
      const prefix='x-codex-'+namespace+'-',limit=rateLimit(headers,prefix,at),name=namespace.replace(/^additional-/,'');
      if(limit)additional.push({limit_name:headers[prefix+'limit-name']||name,metered_feature:name,rate_limit:limit});
    }
    return {...headers['x-codex-plan-type']?{plan_type:headers['x-codex-plan-type']}:{},...main?{rate_limit:main}:{},...review?{code_review_rate_limit:review}:{},...additional.length?{additional_rate_limits:additional}:{}};
  }
  function claudePayload(headers){
    const payload={};
    for(const [name,key] of Object.entries({'5h':'five_hour','7d':'seven_day','7d-opus':'seven_day_opus','7d-sonnet':'seven_day_sonnet','7d-oauth-apps':'seven_day_oauth_apps','7d-cowork':'seven_day_cowork'})){
      const prefix='anthropic-ratelimit-unified-'+name+'-',reset=timestamp(headers[prefix+'reset']);
      let used=numeric(headers[prefix+'utilization']);
      if(headers[prefix+'status']?.toLowerCase()==='rejected')used=1;
      if(used!==null||reset!==null)payload[key]={...used===null?{}:{utilization:Math.max(0,Math.min(100,used*100))},...reset===null?{}:{resets_at:new Date(reset).toISOString()}};
    }
    return payload;
  }
  function fieldsFromHeaders(provider,headers){
    const fields={};
    if(provider!=='codex')return fields;
    for(const [name,key] of Object.entries({'plan-type':'planType','active-limit':'activeLimit','credits-balance':'creditsBalance'})){
      if(headers['x-codex-'+name]!==undefined)fields[key]=headers['x-codex-'+name];
    }
    for(const [name,key] of Object.entries({'credits-has-credits':'creditsHasCredits','credits-unlimited':'creditsUnlimited'})){
      const value=boolean(headers['x-codex-'+name]);if(value!==null)fields[key]=value;
    }
    const over=numeric(headers['x-codex-primary-over-secondary-limit-percent']);
    if(over!==null)fields.primaryOverSecondaryLimitPercent=over;
    const reached=boolean(headers['x-codex-limit-reached']);
    if(reached!==null)fields.rateLimitReachedType=reached?'unknown':null;
    return fields;
  }
  function observedState(file,provider){
    const at=Date.parse(file.quota?.observed_at);
    if(!Number.isFinite(at)||at<=0)return null;
    const headers=Object.fromEntries(Object.entries(file.quota?.signals||{}).map(([name,value])=>[name.toLowerCase(),String(value)]));
    const payload=provider==='codex'?codexPayload(headers,at):claudePayload(headers);
    const windows=providers[provider].parse(payload,at).map(window=>({...window,observationSource:'response_header',observedAtMs:at,quotaProgressObservedAtMs:typeof window.usedPercent==='number'?at:null}));
    const fields=fieldsFromHeaders(provider,headers);
    if(provider==='codex'&&windows.length&&fields.rateLimitReachedType===undefined)fields.rateLimitReachedType=null;
    return windows.length||Object.keys(fields).length?{at,windows,fields}:null;
  }
  function mergeSnapshot(entries,file,adapter,observed){
    const config=adapter.config,key=config.getStoreKey(file);
    if(!key)return entries;
    const previous=config.scopeState(file,entries[key])??config.scopeState(file,entries[file.name]);
    if(previous?.status==='loading')return entries;
    const latest=Math.max(previous?.fetchedAtMs||0,previous?.observedAtMs||0,previous?.failedAtMs||0);
    if(latest>=observed.at)return entries;
    const windows=[...(previous?.windows||[])];
    for(const current of observed.windows){
      const index=windows.findIndex(window=>window.id===current.id||window.providerWindowAliases?.includes(current.id)||current.providerWindowAliases?.includes(window.id));
      if(index<0)windows.push(current);
      else if((windows[index].observedAtMs??previous?.fetchedAtMs??0)<observed.at)windows[index]={...windows[index],...current};
    }
    const next={...previous,...config.buildLoadingState(file),...observed.fields,status:'success',windows,observedAtMs:observed.at,observedFromUsageHeaders:true,quotaInventoryObserved:previous?.quotaInventoryObserved??false};
    delete next.error;delete next.errorStatus;delete next.failedAtMs;
    delete next.observedErrorKind;delete next.observedErrorCode;delete next.observedTraceId;
    return {...entries,[key]:next};
  }
  async function refresh(){
    const state=nativeState();
    if(!store||!authStore||!state.connected||!state.sameOrigin||document.hidden||busy)return;
    busy=true;
    const current=authStore.getState(),key=current.managementKey,apiBase=current.apiBase,revision=generation,cacheGeneration=store.getState().cacheGeneration;
    const isCurrent=()=>{const active=authStore.getState();return active.isAuthenticated&&active.managementKey===key&&active.apiBase===apiBase&&generation===revision&&store.getState().cacheGeneration===cacheGeneration};
    try{
      const response=await originalFetch('/v8/management/credentials/quota-observations',{headers:{Authorization:'Bearer '+key},credentials:'same-origin',cache:'no-store'});
      if(!isCurrent())return;
      if(!response.ok){if(response.status===401)window.dispatchEvent(new Event('unauthorized'));throw Error('HTTP '+response.status)}
      const data=await response.json();
      if(!isCurrent())return;
      if(!Array.isArray(data.files))throw Error('Invalid credential snapshot');
      for(const provider of ['codex','claude']){
        const observations=[];
        for(const file of data.files){
          if(String(file.provider||file.type).toLowerCase()!==provider)continue;
          const observed=observedState(file,provider);
          if(observed)observations.push({file,observed});
        }
        const setter=store.getState()[provider==='codex'?'setCodexQuota':'setClaudeQuota'];
        if(observations.length)setter(entries=>observations.reduce((result,{file,observed})=>mergeSnapshot(result,file,providers[provider],observed),entries));
      }
      lastSync=Date.now();lastError='';
    }catch(error){if(isCurrent())lastError=error.message||String(error)}finally{busy=false}
  }
  function bind(value,adapters){
    if(store||typeof value?.getState!=='function'||!['codex','claude'].every(provider=>typeof adapters?.[provider]?.parse==='function'&&typeof adapters[provider].config?.getStoreKey==='function'&&typeof adapters[provider].config?.scopeState==='function'&&typeof adapters[provider].config?.buildLoadingState==='function'))return;
    const state=value.getState();
    if(typeof state.setCodexQuota!=='function'||typeof state.setClaudeQuota!=='function')return;
    store=value;providers=adapters;
    setInterval(refresh,2000);
    window.addEventListener('focus',refresh);
    document.addEventListener('visibilitychange',refresh);
    refresh();
  }
  return {bind,refresh,state:()=>({bound:!!store,lastSync,error:lastError})};
})();
