// Synthetic UI only; fetch and media are confined to this fixture. No login,
// modem, microphone, SIP/IMS endpoint, speaker output, call or SMS is used.
import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter, Link, Route, Routes, useLocation } from "react-router-dom";
import { LanguageProvider } from "../../src/lib/i18n";
import { PhoneProvider } from "../../src/phone/PhoneProvider";
import PhonePage, { GlobalPhone } from "../../src/phone/PhoneView";
import { PhoneAudio } from "../../src/phone/audio";
import "../../src/index.css";
import "../../src/vocat.css";

const query = new URLSearchParams(location.search);
if (query.get("dark") === "1") document.documentElement.classList.add("dark");
const state = query.get("state") || "incoming";
let calls = state === "idle" ? [] : [{ id: "fixture-call", number: "+15550000001", direction: "incoming", state: state === "active" ? "active" : "ringing", started_at: new Date().toISOString(), media_ready: state === "active", audio_owner: state === "other" ? "other_browser" : "" }];
let permissionPrepares = 0, submitted = 0;
PhoneAudio.prototype.prepare = async function () {
  permissionPrepares++;
  if (query.get("denyMic") === "1") throw new DOMException("Fixture permission denied", "NotAllowedError");
};
PhoneAudio.prototype.connect = function () { this.onLevel(0.15); this.onState("connected"); };
PhoneAudio.prototype.close = function () {};
window.fetch = async (input, init) => {
  const url = new URL(String(input), location.href);
  const reply = (data: unknown, status = 200) => new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
  if (url.pathname === "/api/settings/preferences") return reply({ data: { language: query.get("lang") || "zh" } });
  if (url.pathname.startsWith("/api/phone")) {
    if (init?.method === "POST") {
      submitted++;
      if (query.get("fail") === "1") return reply({ error: { message: "Synthetic action failure" } }, 409);
      if (url.pathname.endsWith("/hangup")) calls = [];
      else if (url.pathname.endsWith("/answer")) calls = calls.map(call => ({ ...call, state: "active", media_ready: true, audio_owner: "this_browser", answered_at: new Date().toISOString() }));
      else {
        const body = JSON.parse(String(init.body));
        calls = [{ id: "fixture-outgoing", number: body.number, direction: "outgoing", state: "ringing", media_ready: false, audio_owner: "this_browser", started_at: new Date().toISOString() }];
      }
      return reply({ data: { call: calls[0], audio_token: "fixture-only" } }, 202);
    }
    if (query.get("loadFail") === "1") return reply({ error: { message: "Synthetic connection failure" } }, 503);
    return reply({ data: { lines: state === "empty" ? [] : [
      { device_id: "fixture-a", name: "EC20 · Synthetic IMS A", ready: true, calls },
      { device_id: "fixture-b", name: "Module B · Independent", ready: false, calls: [] },
    ] } });
  }
  throw new Error(`Unexpected fixture request ${url.pathname}`);
};
function FixtureCounters() {
  const [counts, setCounts] = React.useState({ permissionPrepares: 0, submitted: 0 });
  React.useEffect(() => { const timer = setInterval(() => setCounts({ permissionPrepares, submitted }), 300); return () => clearInterval(timer); }, []);
  return <details className="mt-8 text-xs text-gray-400"><summary>Fixture counters</summary><output>{JSON.stringify(counts)}</output></details>;
}
function PreviewGlobalPhone() { return useLocation().pathname === "/other" ? <GlobalPhone /> : null; }
ReactDOM.createRoot(document.getElementById("root")!).render(<React.StrictMode><LanguageProvider><BrowserRouter><PhoneProvider>
  <main className="mx-auto max-w-6xl p-4 text-gray-800 sm:p-8 dark:text-gray-100">
    <nav className="mb-6 flex flex-wrap gap-4 text-xs text-gray-500"><span>Synthetic UI · no carrier / microphone access</span><Link to={`/phone${location.search}`}>Phone</Link><Link to={`/other${location.search}`}>Other page</Link></nav>
    <Routes><Route path="/other" element={<div className="rounded-2xl border border-gray-200 p-8">Another page — incoming notification stays available here.</div>} /><Route path="*" element={<PhonePage />} /></Routes>
    <FixtureCounters />
  </main><PreviewGlobalPhone />
</PhoneProvider></BrowserRouter></LanguageProvider></React.StrictMode>);
