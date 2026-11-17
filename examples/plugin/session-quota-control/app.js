(()=>{
  'use strict';
  const defaults=__INITIAL_VIEW__,views={scheduling:schedulingView,credentials:credentialsView,access:accessView,usage:usageView,hub:hubView};
  const tabs={scheduling:document.getElementById('dashboardTab'),credentials:document.getElementById('credentialsTab'),access:document.getElementById('accessTab'),usage:document.getElementById('usageTab'),hub:document.getElementById('hubTab')};
  const panels={scheduling:document.getElementById('dashboardPanel'),credentials:document.getElementById('credentialsPanel'),access:document.getElementById('accessPanel'),usage:document.getElementById('usagePanel'),hub:document.getElementById('hubPanel')};
  const names=Object.keys(views);
  let active=defaults;
  function readView(){const hash=location.hash.slice(1);return hash==='access'?'access':hash==='hub'?'hub':hash==='usage'||hash==='report'?'usage':hash==='credentials'||hash==='credential'?'credentials':hash==='scheduling'||hash==='dashboard'?'scheduling':defaults}
  function show(view,updateURL=false){
    active=view;
    for(const name of Object.keys(views)){
      const selected=name===active;panels[name].hidden=!selected;
      tabs[name].setAttribute('aria-selected',String(selected));tabs[name].tabIndex=selected?0:-1;
    }
    if(updateURL)history.replaceState(null,'','#'+view);
    document.title='Session 与额度调度 · '+({scheduling:'会话调度',credentials:'Credential 管理',access:'访问控制',usage:'用量统计',hub:'Hub'})[active];
    views[active].refresh();
  }
  for(const [name,tab] of Object.entries(tabs)){
    tab.onclick=()=>show(name,true);
    tab.onkeydown=event=>{
      if(!['ArrowLeft','ArrowRight','Home','End'].includes(event.key))return;
      event.preventDefault();const index=names.indexOf(name),next=event.key==='Home'?names[0]:event.key==='End'?names.at(-1):names[(index+(event.key==='ArrowRight'?1:-1)+names.length)%names.length];show(next,true);tabs[next].focus();
    };
  }
  managementAuth.install(()=>{document.getElementById('message').textContent='';for(const view of Object.values(views))view.reset();views[active].refresh()});
  window.addEventListener('hashchange',()=>show(readView()));
  show(readView());
})();
