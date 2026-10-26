// Synthetic fixture only. All API requests are intercepted; no modem or carrier access.
import React, {useState} from "react";
import ReactDOM from "react-dom/client";
import {LanguageProvider} from "../../src/lib/i18n";
import {IMSLinePanel} from "../../src/components/devices/IMSLinePanel";
import {OverviewVowifiCard} from "../../src/components/devices/OverviewVowifiCard";
import {DeviceCard} from "../../src/components/DeviceCard";
import {MessageHost} from "../../src/components/ui/message";
import type {DeviceDetail} from "../../src/components/devices/types";
import type {DashboardDevice, IKERetryObservation} from "../../src/types";
import "../../src/index.css";
import "../../src/vocat.css";
const query=new URLSearchParams(location.search),scenario=query.get("state")||"timer";
if(query.get("dark")==="1") document.documentElement.classList.add("dark");
let checks=0,notify=()=>{};
window.fetch=async(input,init)=>{
  const url=new URL(String(input),location.href),reply=(data:unknown,status=200)=>new Response(JSON.stringify(data),{status,headers:{"Content-Type":"application/json"}});
  if(url.pathname==="/api/settings/preferences") return reply({data:{language:query.get("lang")==="en"?"en":"zh"}});
  if(url.pathname==="/api/devices/synthetic-a/vowifi/actions/reconnect"&&init?.method==="POST"){
    checks++;notify();return reply({data:{accepted:true,status:"reconnecting"}},202);
  }
  return reply({error:{message:"Isolated fixture forbids external requests"}},503);
};
function Preview(){
  const [,refresh]=useState(0);notify=()=>refresh(value=>value+1);
  let retry:IKERetryObservation|undefined={source:"operator",scope:"epdg",notifyType:10500,afterSeconds:600,untilCardEvent:false,cached:false,cleanupPending:false,observedAt:"2026-10-26T01:00:00Z"};
  if(scenario==="missing") retry.source="implementation-missing";
  if(scenario==="invalid") retry.source="implementation-invalid";
  if(scenario==="cached") retry.cached=true;
  if(["unknown","store","binding","card"].includes(scenario)){retry.source=({unknown:"uncertain-attempt",store:"store-unavailable",binding:"binding-unavailable",card:"operator"} as Record<string,string>)[scenario];retry.untilCardEvent=true;retry.afterSeconds=0;}
  if(scenario==="cleanup") retry.cleanupPending=true;
  if(scenario==="ready"||scenario==="off") retry=undefined;
  const ready=scenario==="ready",enabled=scenario!=="off";
  const rt={deviceId:"synthetic-a",iccid:"synthetic-card",enabled,simReady:true,accessReady:ready,tunnelReady:ready,imsReady:ready,smsReady:false,phase:ready?"ims_ready":enabled?"failed":"idle",
    lastErrorClass:enabled&&!ready?"ike_retry_blocked":"",lastError:enabled&&!ready?"Synthetic diagnostic detail":"",lastReason:"fixture-only",dataplaneMode:"userspace",ikeRetry:retry,tunnel:{epdg:"epdg.example",epdgAddress:"192.0.2.9"}};
  const device={id:"synthetic-a",name:"Synthetic modem A",deviceType:"pcie_ec20_ec25",imsMode:"vowifi",imsEnabled:enabled,vowifiEnabled:enabled,imsRuntime:rt,vowifiRuntime:rt,modem:{iccid:"synthetic-card"},healthy:true,signalDbm:-75,networkMode:"LTE",operator:"Synthetic",vowifiActive:ready} as DeviceDetail;
  const healthy={...device,id:"synthetic-b",name:"Synthetic modem B",modem:{iccid:"synthetic-card-b"},vowifiRuntime:{...rt,deviceId:"synthetic-b",iccid:"synthetic-card-b",phase:"ims_ready",imsReady:true,accessReady:true,tunnelReady:true,lastErrorClass:"",lastError:"",ikeRetry:undefined}};
  return <main className="mx-auto max-w-4xl space-y-4 p-4 sm:p-8 text-gray-800 dark:text-gray-100">
    <p className="text-xs text-gray-500">隔离预览 · 合成数据 · 不访问模组或运营商</p>
    <div className="grid gap-4 sm:grid-cols-2"><DeviceCard device={device as unknown as DashboardDevice} onOpen={()=>{}}/><DeviceCard device={healthy as unknown as DashboardDevice} onOpen={()=>{}}/></div>
    <IMSLinePanel device={device} onUpdated={()=>refresh(value=>value+1)}/>
    <section className="ui-panel-muted p-4"><OverviewVowifiCard device={device}/></section>
    <p data-testid="check-count" className="text-xs text-gray-500">Synthetic rechecks: {checks} · restriction retained</p><MessageHost/>
  </main>;
}
ReactDOM.createRoot(document.getElementById("root")!).render(<LanguageProvider><Preview/></LanguageProvider>);
