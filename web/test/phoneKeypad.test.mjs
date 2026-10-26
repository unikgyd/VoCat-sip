import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ts from "typescript";

let source = await readFile(new URL("../src/phone/PhoneView.tsx", import.meta.url), "utf8");
source = source.replace(/import \{ Bell[^\n]+\n/, "const Bell=()=>null,BellOff=Bell,Mic=Bell,MicOff=Bell,Phone=Bell,PhoneIncoming=Bell,PhoneOff=Bell;\n")
  .replace(/import \{ Button, PageHeader \}[^\n]+\n/, 'const Button=({children,disabled}:any)=><button disabled={disabled}>{children}</button>, PageHeader=()=>null;\n')
  .replace(/import \{ useI18n \}[^\n]+\n/, 'const useI18n=()=>({t:(text:string)=>text});\n')
  .replace(/import \{ usePhone[^\n]+\n/, 'const usePhone=()=>globalThis.__phoneKeypadFixture; type PhoneLine=any; type PhoneCall=any;\n')
  .replace(/import \{ CallRecordingsPanel \}[^\n]+\n/, 'const CallRecordingsPanel=()=>null;\n');
let compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
for (const dependency of ["react", "react/jsx-runtime", "react-router-dom"]) compiled = compiled.replaceAll(`from "${dependency}"`, `from "${import.meta.resolve(dependency)}"`);
const { CallCard } = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`);
function render({ mine = true, digits = "0123456789*#", state = "active", connected = true, pending = false } = {}) {
  globalThis.__phoneKeypadFixture = { owned: mine ? { deviceId: "a", callId: "call-a" } : null, audioState: connected ? "connected" : "closed", dtmfBusy: pending, level: 0 };
  return renderToStaticMarkup(createElement(CallCard, { line: { deviceId: "a", name: "Line A" }, call: { id: "call-a", state, number: "+15550000001", dtmfDigits: digits } }));
}
test("the owning browser renders the negotiated 12-key call keypad", () => {
  const html = render();
  for (const key of "123456789*0#") assert.ok(html.includes(`aria-label="发送按键 ${key}"`));
  assert.equal((html.match(/aria-label="发送按键/g) || []).length, 12);
});
test("another browser or a ringing call cannot expose the keypad", () => {
  for (const options of [{ mine: false }, { state: "ringing" }]) assert.ok(!render(options).includes('aria-label="发送按键'));
});
test("missing negotiation explains unavailability and never invents keys", () => {
  const html = render({ digits: "" });
  assert.ok(html.includes("对方未协商通话按键"));
  assert.ok(!html.includes('aria-label="发送按键'));
});
test("disconnected audio, a pending key, and unsupported digits disable submission", () => {
  for (const options of [{ connected: false }, { pending: true }]) assert.equal((render(options).match(/aria-label="发送按键 [^"]+" disabled=""/g) || []).length, 12);
  const partial = render({ digits: "0123456789" });
  assert.ok(partial.includes('aria-label="发送按键 #" disabled=""'));
  assert.ok(partial.includes('aria-label="发送按键 *" disabled=""'));
  assert.ok(!partial.includes('aria-label="发送按键 5" disabled=""'));
});
