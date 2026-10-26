// Isolated synthetic UI only: never contacts a modem, SIP server or carrier.
import React, { useState } from "react";
import ReactDOM from "react-dom/client";
import { LanguageProvider } from "../../src/lib/i18n";
import { IMSLinePanel, OverviewHostIMSCard } from "../../src/components/devices/IMSLinePanel";
import { OverviewVowifiCard } from "../../src/components/devices/OverviewVowifiCard";
import { MessageHost } from "../../src/components/ui/message";
import type { DeviceDetail } from "../../src/components/devices/types";
import "../../src/index.css";
import "../../src/vocat.css";
const q = new URLSearchParams(location.search);
if(q.get("dark")==="1") document.documentElement.classList.add("dark");
let mode = "host_ims", enabled = q.get("state")!=="off";
window.fetch = async (input, init) => {
  const url = new URL(String(input), location.href);
  const reply=(data:unknown,status=200)=>new Response(JSON.stringify(data),{status,headers:{"Content-Type":"application/json"}});
  if(url.pathname==="/api/settings/preferences") return reply({data:{language:"zh"}});
  if(url.pathname==="/api/devices/synthetic-a/ims" && init?.method==="PATCH") {
    if(q.get("fail")==="1") return reply({error:{message:"旧线路仍在清理，请稍后重试"}},409);
    ({mode,enabled}=JSON.parse(String(init.body))); return reply({data:{mode,enabled}});
  }
  return reply({error:{message:"Isolated preview forbids external requests"}},503);
};
function Preview(){
  const [,refresh]=useState(0);
  const ready=q.get("state")!=="loading" && q.get("state")!=="error";
  const rt={simReady:true,accessReady:ready,imsReady:ready,smsReady:ready,tunnelReady:false,phase:q.get("state")==="error"?"failed":ready?"ims_ready":"sim_ready",lastErrorClass:q.get("state")==="error"?"prepare_failed":"",enabled,dataplaneMode:"cellular-host-ims"};
  const device={id:"synthetic-a",name:"Synthetic modem A",deviceType:"pcie_ec20_ec25",imsMode:mode,imsEnabled:enabled,vowifiEnabled:mode==="vowifi"&&enabled,imsRuntime:rt,vowifiRuntime:{...rt,tunnelReady:ready}} as DeviceDetail;
  return <main className="mx-auto max-w-3xl space-y-4 p-4 sm:p-8 text-gray-800 dark:text-gray-100"><p className="text-xs text-gray-500">隔离预览 · 合成数据 · 不访问模组或运营商</p><IMSLinePanel device={device} onUpdated={()=>refresh(v=>v+1)}/><section className="ui-panel-muted p-4"><h2 className="mb-3 text-xs font-bold text-gray-500">运行状态</h2>{mode==="host_ims"?<OverviewHostIMSCard device={device}/>:mode==="vowifi"?<OverviewVowifiCard device={device}/>:<p className="text-sm">模块语音</p>}</section><MessageHost/></main>;
}
ReactDOM.createRoot(document.getElementById("root")!).render(<LanguageProvider><Preview/></LanguageProvider>);
