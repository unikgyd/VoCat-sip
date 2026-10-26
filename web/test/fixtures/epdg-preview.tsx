// Synthetic API only. No device, SIM, DNS, IKE, IMS or production backend.
import React from "react";
import ReactDOM from "react-dom/client";
import { LanguageProvider } from "../../src/lib/i18n";
import { EpdgDiscoveryPanel } from "../../src/components/devices/EpdgDiscoveryPanel";
import { MessageHost } from "../../src/components/ui/message";
import "../../src/index.css";
import "../../src/vocat.css";
const query=new URLSearchParams(location.search);if(query.get("dark")==="1")document.documentElement.classList.add("dark");
let config={preferred_addresses:["epdg.example.test"],additional_plmns:["51566"],manual_only:false,geographic_dns_fallback:false,sim_token:"synthetic-card-a"};
const realFetch=window.fetch;window.fetch=async(input,init)=>{const url=new URL(String(input),location.href);const reply=(data:unknown,status=200)=>new Response(JSON.stringify(data),{status,headers:{"Content-Type":"application/json"}});if(url.pathname==="/api/settings/preferences")return reply({data:{language:query.get("lang")==="en"?"en":"zh"}});if(url.pathname!=="/api/devices/fixture/vowifi/discovery")return realFetch(input,init);if(init?.method==="PUT"){if(query.get("failSave")==="1")return reply({error:{code:"sim_changed",message:"Fixture: SIM changed; reload settings"}},409);config=JSON.parse(String(init.body))}return reply({data:config})};
ReactDOM.createRoot(document.getElementById("root")!).render(<React.StrictMode><LanguageProvider><main className="mx-auto max-w-3xl space-y-4 p-4 sm:p-8 text-gray-800 dark:text-gray-100"><p className="text-xs text-amber-700 dark:text-amber-300">Isolated UI fixture · synthetic data · no SIM / DNS / IMS access</p><EpdgDiscoveryPanel deviceId="fixture" simKey="synthetic-card-a"/></main><MessageHost/></LanguageProvider></React.StrictMode>);
