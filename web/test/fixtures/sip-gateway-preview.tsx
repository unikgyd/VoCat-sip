// Developer-only fixture: Vite dev server, not an entry in the production build.
// All API data are synthetic and confined to this browser page. No SIP socket,
// login, modem, Asterisk or backend service is used by this UI preview.
import React from "react";
import ReactDOM from "react-dom/client";
import { LanguageProvider } from "../../src/lib/i18n";
import { SipGatewayCard } from "../../src/components/settings/SipGatewayCard";
import { MessageHost } from "../../src/components/ui/message";
import "../../src/index.css";
import "../../src/vocat.css";

const query = new URLSearchParams(location.search);
const smsPreview = query.get("state") === "sms";
const peerPreview = ["peer", "peer-only"].includes(query.get("state") || "");
if (query.get("dark") === "1") document.documentElement.classList.add("dark");
let config = {
  enabled: query.get("state") !== "off", listen: "127.0.0.1:15060", realm: "vocat", algorithm: "SHA-256", identity_domain: "voice.example.test", messages_enabled: query.get("state") === "messages",
  calls_enabled: peerPreview || ["calls","carrier"].includes(query.get("state") || ""),
  carrier_enabled: peerPreview || query.get("state") === "carrier",
  sms_enabled: smsPreview || peerPreview,
  carrier_lines: peerPreview || query.get("state") === "carrier" || smsPreview ? [
    { device_id: "fixture-a", label: "Synthetic IMS A", prefix: "", default: true, allow_outgoing: !smsPreview, allow_incoming: !smsPreview, incoming_extensions: smsPreview || query.get("state") === "peer-only" ? [] : ["1001", "1002"], country_code: "44", national_number_length: 10, allow_sms_send: smsPreview, allow_sms_receive: smsPreview, sms_recipients: smsPreview ? ["1001"] : [] },
    { device_id: "fixture-b", label: "Synthetic IMS B", prefix: "82", default: false, allow_outgoing: false, allow_incoming: false, incoming_extensions: [], country_code: "", national_number_length: 0, allow_sms_send: false, allow_sms_receive: false, sms_recipients: [] },
  ] : [],
  allowed_cidrs: ["127.0.0.0/8", "10.0.0.0/8", "192.168.0.0/16"],
  peer: peerPreview ? { enabled: true, endpoint: "127.0.0.1:25060", inbound_auth: { username: "fixture-pbx", password: "", has_password: true }, outbound_auth: { username: "fixture-gateway", realm: "fixture-pbx", password: "", has_password: true, algorithms: ["SHA-256"] }, allow_calls_from_peer: true, allow_sms_from_peer: false, allow_calls_to_peer: true, allow_sms_to_peer: false, outgoing_lines: ["fixture-a"], sms_send_lines: [], incoming_routes: [{device_id:"fixture-a", call_target:"fixture-ring", sms_target:""}] } : null,
  accounts: ["off", "peer-only"].includes(query.get("state") || "") ? [] : [
    { username: "1001", display_name: "Desk", has_password: true, password: "", allow_messages: query.get("state") === "messages", allow_calls: ["calls","carrier"].includes(query.get("state") || ""), outgoing_lines: query.get("state") === "carrier" ? ["fixture-a"] : [], sms_send_lines: smsPreview ? ["fixture-a"] : [] },
    { username: "1002", display_name: "Mobile", has_password: true, password: "", allow_messages: query.get("state") === "messages", allow_calls: ["calls","carrier"].includes(query.get("state") || ""), outgoing_lines: [], sms_send_lines: [] },
  ],
};
const realFetch = window.fetch;
window.fetch = async (input, init) => {
  const url = new URL(String(input), location.href);
  const reply = (data: unknown, status = 200) => new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
  if (url.pathname === "/api/settings/preferences") return reply({ data: { language: query.get("lang") === "en" ? "en" : "zh" } });
  if (url.pathname === "/api/devices") return query.get("devicesFail") === "1" ? reply({error:{message:"Synthetic list failure"}},503) : reply({data:{devices:[{id:"fixture-a",name:"Synthetic modem A"},{id:"fixture-b",name:"Synthetic modem B"}]}});
  if (url.pathname !== "/api/settings/sip") return realFetch(input, init);
  await new Promise(resolve => setTimeout(resolve, 80));
  if (query.get("state") === "load-failure") return reply({ error: { code: "unavailable", message: "Fixture load failure" } }, 503);
  if (init?.method === "PUT") {
    if (query.get("failSave") === "1") return reply({ error: { code: "sip_config_failed", message: "Fixture: port is occupied; previous settings retained" } }, 400);
    const next = JSON.parse(String(init.body));
    next.accounts = next.accounts.map((account: typeof config.accounts[number]) => ({ ...account, has_password: Boolean(account.password) || account.has_password, password: "" }));
    if (next.peer) for (const auth of [next.peer.inbound_auth, next.peer.outbound_auth]) { auth.has_password = Boolean(auth.password) || auth.has_password; auth.password = ""; }
    config = next;
  }
  const failed = query.get("state") === "error";
  return reply({ data: { config, status: { running: config.enabled && !failed, listen: config.listen, error: failed ? "Fixture: address already in use" : "", peer: { enabled: Boolean(config.enabled && !failed && config.peer?.enabled), endpoint: config.peer?.endpoint || "", state: config.enabled && !failed && config.peer?.enabled ? "configured" : "disabled" }, bindings: config.enabled && !failed && config.accounts.some(account => account.username === "1001") ? [{ username: "1001", source: "127.0.0.1:20001", expires_at: new Date(Date.now() + 180000).toISOString() }] : [], calls: config.enabled && config.calls_enabled && query.get("state") === "calls" ? [{id:"fixture-call",caller:"1001",callee:"1002",phase:"active"}] : [], capabilities: { registration: true, calls: config.enabled && config.calls_enabled, messages: config.enabled && config.messages_enabled, carrier_bridge: false, carrier_outgoing: config.carrier_enabled } } } });
};

ReactDOM.createRoot(document.getElementById("root")!).render(<React.StrictMode><LanguageProvider>
  <main className="mx-auto max-w-4xl p-3 sm:p-8 text-gray-800 dark:text-gray-100">
    <p className="mb-4 text-xs text-amber-700 dark:text-amber-300">Isolated UI fixture · synthetic data only · no SIP / modem / carrier access</p>
    <SipGatewayCard />
  </main><MessageHost />
</LanguageProvider></React.StrictMode>);
