(()=>{
  'use strict';
  const root=document.documentElement,media=matchMedia('(prefers-color-scheme: dark)');
  let nativeRoot=null;
  try{if(window.parent!==window)nativeRoot=window.parent.document.documentElement}catch{}
  function sync(){
    const theme=nativeRoot?.getAttribute('data-theme');
    root.dataset.theme=theme==='dark'?'dark':theme==='white'||theme==='light'?'light':media.matches?'dark':'light';
    if(nativeRoot){
      const font=window.parent.getComputedStyle(window.parent.document.body||nativeRoot).fontFamily;
      if(font)root.style.setProperty('--app-font',font);
    }
  }
  sync();
  media.addEventListener('change',sync);
  if(nativeRoot)new MutationObserver(sync).observe(nativeRoot,{attributes:true,attributeFilter:['data-theme','class']});
})();
