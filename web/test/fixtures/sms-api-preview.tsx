// Isolated visual acceptance: no real backend, token, SMS, modem or listener.
import React from "react";
import ReactDOM from "react-dom/client";
import { LanguageProvider } from "../../src/lib/i18n";
import { SMSAPICard } from "../../src/components/settings/SMSAPICard";
import { MessageHost } from "../../src/components/ui/message";
import "../../src/index.css";
import "../../src/vocat.css";
const query = new URLSearchParams(location.search);
if(query.get("dark")==="1")document.documentElement.classList.add("dark");
const enabled=query.get("state")==="running";
let status={enabled,bind_address:"127.0.0.1",port:8787,allow_lan_access:false,token_configured:enabled,running:enabled,error:query.get("state")==="error"?"Synthetic listener failure":"",base_url:"http://127.0.0.1:8787/"};
window.fetch=async(input,init)=>{
  const url=new URL(String(input),location.href);
  const reply=(data:unknown,code=200)=>new Response(JSON.stringify(data),{status:code,headers:{"Content-Type":"application/json"}});
  if(url.pathname==="/api/settings/preferences")return reply({data:{language:query.get("lang")==="en"?"en":"zh"}});
  if(url.pathname!=="/api/settings/sms-api")return reply({error:{code:"fixture_only",message:"No real backend"}},404);
  if(query.get("state")==="load-error")return reply({error:{code:"fixture_failure",message:"Synthetic load failure"}},503);
  if(init?.method==="PUT"){
    if(query.get("failSave")==="1")return reply({error:{code:"fixture_failure",message:"Synthetic save failure"}},500);
    const next=JSON.parse(String(init.body));
    status={...status,...next,token_configured:status.token_configured||!!next.token,running:next.enabled,error:query.get("state")==="error"?"Synthetic listener failure":""};
    delete (status as Record<string,unknown>).token;
  }
  return reply({data:status});
};
ReactDOM.createRoot(document.getElementById("root")!).render(<React.StrictMode><LanguageProvider><main style={{width:736,maxWidth:"calc(100% - 32px)",margin:16}} className="space-y-4 text-gray-800 dark:text-gray-100"><p className="text-xs text-amber-700 dark:text-amber-300">Isolated fixture · synthetic data · no SMS / modem / listener</p><SMSAPICard/></main><MessageHost/></LanguageProvider></React.StrictMode>);
