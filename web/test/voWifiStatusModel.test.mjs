import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import test from "node:test";
import ts from "typescript";
const load = async (path, edit = value => value) => {
  const source = edit(await readFile(new URL(path,import.meta.url),"utf8"));
  const compiled = ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.ES2022,target:ts.ScriptTarget.ES2022}});
  return import(`data:text/javascript;base64,${Buffer.from(compiled.outputText).toString("base64")}`);
};
const {voWiFiStatus} = await load("../src/components/devices/voWifiStatusModel.ts");
// Only translation is stubbed; exercise the actual API camelizer, not a copy.
const {camelize} = await load("../src/api.ts",s=>s.replace('import { tl } from "./lib/i18n";','const tl = (value: string) => value;'));
const {EN_DICT} = await load("../src/lib/i18n-en.ts");
const retry = (source="operator",changes={}) => ({source,scope:"epdg",notifyType:10500,afterSeconds:600,untilCardEvent:false,cached:false,cleanupPending:false,...changes});
const device = (changes={}) => ({id:"synthetic-a",imsMode:"vowifi",imsEnabled:true,vowifiEnabled:true,modem:{iccid:"test-card"},
  vowifiRuntime:{deviceId:"synthetic-a",iccid:"test-card",enabled:true,phase:"failed",lastErrorClass:"ike_retry_blocked",ikeRetry:retry()},...changes});
const withRuntime = changes => device({vowifiRuntime:{...device().vowifiRuntime,...changes}});

test("operator wait overrides generic failure without animating as active loading",()=>{
  const result=voWiFiStatus(device());assert.equal(result.tone,"warning");assert.equal(result.label,"等待重试");assert.equal(result.animated,false);
});
for(const source of ["implementation-missing","implementation-invalid"])
  test(`${source} remains a conservative wait`,()=>{const r=voWiFiStatus(withRuntime({ikeRetry:retry(source)}));assert.equal(r.tone,"warning");assert.match(r.reason,/本地保守等待/);});
for(const source of ["uncertain-attempt","store-unavailable","binding-unavailable","future-status"])
  test(`${source} never becomes a retry countdown`,()=>{const r=voWiFiStatus(withRuntime({ikeRetry:retry(source,{untilCardEvent:true})}));assert.equal(r.tone,"danger");assert.equal(r.label,"已暂停");});
test("operator lifecycle restriction is red even if an old duration is present",()=>{assert.equal(voWiFiStatus(withRuntime({ikeRetry:retry("operator",{untilCardEvent:true})})).tone,"danger");});
test("cleanup pause cannot be hidden behind an operator timer",()=>{const r=voWiFiStatus(withRuntime({ikeRetry:retry("operator",{cleanupPending:true})}));assert.equal(r.tone,"danger");assert.match(r.reason,/清理尚未完成/);});
test("saved and boundary observations require rechecking instead of declaring permission",()=>{
  assert.equal(voWiFiStatus(withRuntime({ikeRetry:retry("operator",{cached:true})})).label,"状态待复核");
  assert.equal(voWiFiStatus(withRuntime({ikeRetry:retry("operator",{afterSeconds:0})})).label,"状态待复核");
});
for(const duration of [-1,1.5,1116001,NaN,Infinity,undefined])
  test(`invalid duration ${String(duration)} stays paused`,()=>{assert.equal(voWiFiStatus(withRuntime({ikeRetry:retry("operator",{afterSeconds:duration})})).tone,"danger");});
test("wall-clock deadlines never change presentation or grant retry permission",()=>{
  const a=withRuntime({ikeRetry:retry("operator",{notBefore:"1970-01-01T00:00:00Z"})});
  const b=withRuntime({ikeRetry:retry("operator",{notBefore:"2099-01-01T00:00:00Z"})});
  assert.equal(voWiFiStatus(a).label,voWiFiStatus(b).label);assert.equal(voWiFiStatus(a).tone,"warning");
});
test("a healthy voice registration does not require SMS capability or get broken by a stale new-handshake observation",()=>{
  const r=voWiFiStatus(withRuntime({phase:"ims_ready",simReady:true,accessReady:true,tunnelReady:true,imsReady:true,smsReady:false,lastErrorClass:"",lastError:""}));assert.equal(r.tone,"success");
});
test("other SIM and device observations are removed before exposing detail data",()=>{
  for(const change of [{iccid:"other-card"},{deviceId:"other-device"}]){const r=voWiFiStatus(withRuntime(change));assert.equal(r.tone,"danger");assert.equal(r.runtime,undefined);}
});
test("disabled remains gray and cannot expose another card's saved details",()=>{
  const r=voWiFiStatus(device({imsEnabled:false,modem:{iccid:"different"}}));assert.equal(r.tone,"neutral");assert.equal(r.runtime,undefined);
});
test("absent SIM cannot advertise an old healthy registration",()=>{const r=voWiFiStatus(device({modem:{simInserted:false}}));assert.equal(r.tone,"danger");assert.equal(r.runtime,undefined);});
test("ordinary startup, failures and missing legacy observations remain distinct",()=>{
  assert.equal(voWiFiStatus(withRuntime({ikeRetry:undefined,lastErrorClass:"",phase:"sim_ready"})).tone,"warning");
  assert.equal(voWiFiStatus(withRuntime({ikeRetry:undefined})).tone,"danger");
});
test("actual API camelization carries safe observations into the production model",()=>{
  const data=camelize({id:"synthetic-a",ims_mode:"vowifi",ims_enabled:true,modem:{iccid:"test-card"},vowifi_runtime:{device_id:"synthetic-a",iccid:"test-card",enabled:true,phase:"failed",ike_retry:{source:"operator",after_seconds:600,until_card_event:false,cached:false,cleanup_pending:false}}});
  assert.equal(data.vowifiRuntime.ikeRetry.afterSeconds,600);assert.equal(voWiFiStatus(data).tone,"warning");
});
test("all new status and reason keys have English translations",()=>{
  const samples=[device(),...['implementation-missing','implementation-invalid','uncertain-attempt','store-unavailable','binding-unavailable','future-status'].map(source=>withRuntime({ikeRetry:retry(source,{untilCardEvent:!source.startsWith('implementation-')})})),withRuntime({ikeRetry:retry('operator',{cleanupPending:true})}),withRuntime({ikeRetry:retry('operator',{untilCardEvent:true})})];
  for(const item of samples){const r=voWiFiStatus(item);assert.ok(EN_DICT[r.label],r.label);if(r.reason)assert.ok(EN_DICT[r.reason],r.reason);}
});
