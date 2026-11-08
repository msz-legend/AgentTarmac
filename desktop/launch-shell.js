const {spawn}=require('child_process'); const fs=require('fs');
const env={...process.env}; delete env.ELECTRON_RUN_AS_NODE; delete env.ELECTRON_ENABLE_LOGGING;
const root='D:\\DevTools\\AgentHub\\install';
const out=fs.openSync(root+'\\shell.out','w'), err=fs.openSync(root+'\\shell.err','w');
const p=spawn(root+'\\AgentTarmac.exe',[],{env,cwd:root,detached:true,stdio:['ignore',out,err],windowsHide:false});
p.unref();
console.log('SPAWNED pid='+p.pid);