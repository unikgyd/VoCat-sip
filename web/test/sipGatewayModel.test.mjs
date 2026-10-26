import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import ts from "typescript";
const source = await readFile(new URL("../src/components/settings/sipGatewayModel.ts", import.meta.url), "utf8");
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } });
const { emptySIPPeer, cloneSIPPeer, adoptSIPConfig, serializeSIPForm, updateSIPAccount, updateSIPRealm, updatePeerInbound, updatePeerOutbound, restrictPeerLines, sipPeerFormError, sipPeerStatus } = await import(`data:text/javascript;base64,${Buffer.from(compiled.outputText).toString("base64")}`);
const base = () => ({ enabled: false, listen: "127.0.0.1:15060", realm: "vocat", identityDomain: "", algorithm: "SHA-256", allowedCidrs: ["127.0.0.0/8"], accounts: [], carrierLines: [], messagesEnabled: false, callsEnabled: false, carrierEnabled: false, smsEnabled: false });
const peer = () => ({ ...emptySIPPeer(), enabled: true, endpoint: "127.0.0.1:25060", inboundAuth: { username: "pbx-in", hasPassword: true, password: "never-copy-from-response" }, outboundAuth: { username: "gateway-out", realm: "pbx-realm", hasPassword: true, password: "never-copy-outbound-secret", algorithms: ["SHA-256", "MD5"] }, allowCallsFromPeer: true, allowSmsFromPeer: true, allowCallsToPeer: true, allowSmsToPeer: true, outgoingLines: ["modem-a", "modem-b"], smsSendLines: ["modem-b"], incomingRoutes: [{ deviceId: "modem-a", callTarget: "600", smsTarget: "messages" }, { deviceId: "modem-b", callTarget: "601", smsTarget: "" }] });
test("legacy settings preserve absent peer without inventing endpoint, account or permissions", () => {
  const form = adoptSIPConfig(base(), () => "key");
  assert.equal(form.peer, null);
  assert.equal(serializeSIPForm(form).peer, null);
  assert.equal(sipPeerFormError(null), null);
  assert.equal(sipPeerFormError(emptySIPPeer()), null);
  assert.deepEqual(emptySIPPeer().outboundAuth.algorithms, ["SHA-256"]);
  assert.equal(emptySIPPeer().allowCallsFromPeer, false);
});
test("load clears both passwords but retains server password markers and complete peer configuration", () => {
  const config = { ...base(), peer: peer() };
  const form = adoptSIPConfig(config, () => "key");
  assert.equal(form.peer.inboundAuth.password, ""); assert.equal(form.peer.outboundAuth.password, "");
  assert.equal(form.peer.inboundAuth.hasPassword, true); assert.equal(form.peer.outboundAuth.hasPassword, true);
  for (const key of ["enabled", "endpoint", "allowCallsFromPeer", "allowSmsFromPeer", "allowCallsToPeer", "allowSmsToPeer", "outgoingLines", "smsSendLines", "incomingRoutes"]) assert.deepEqual(form.peer[key], config.peer[key]);
  assert.equal(sipPeerFormError(form.peer), null);
});
test("form and payload never alias saved peer ACLs, target routes or algorithm arrays", () => {
  const config = { ...base(), peer: peer() }; const form = adoptSIPConfig(config, () => "key");
  form.peer.outgoingLines.push("modem-c"); form.peer.incomingRoutes[0].callTarget = "changed"; form.peer.outboundAuth.algorithms.pop();
  assert.deepEqual(config.peer.outgoingLines, ["modem-a", "modem-b"]); assert.equal(config.peer.incomingRoutes[0].callTarget, "600"); assert.equal(config.peer.outboundAuth.algorithms.length, 2);
  const payload = serializeSIPForm(form); payload.peer.incomingRoutes[0].smsTarget = "payload-only"; payload.peer.smsSendLines.length = 0;
  assert.equal(form.peer.incomingRoutes[0].smsTarget, "messages"); assert.deepEqual(form.peer.smsSendLines, ["modem-b"]);
});
test("complete save retains peer fields and explicit passwords without UI-only keys", () => {
  const form = adoptSIPConfig({ ...base(), peer: peer() }, () => "key");
  form.peer.inboundAuth.password = "fresh-inbound-secret"; form.peer.outboundAuth.password = "fresh-outbound-secret";
  form.networks = "127.0.0.0/8, 10.0.0.0/8\n";
  const payload = serializeSIPForm(form);
  assert.equal(payload.peer.inboundAuth.password, "fresh-inbound-secret"); assert.equal(payload.peer.outboundAuth.password, "fresh-outbound-secret");
  assert.deepEqual(payload.allowedCidrs, ["127.0.0.0/8", "10.0.0.0/8"]); assert.deepEqual(payload.peer.incomingRoutes, form.peer.incomingRoutes);
  assert.equal(Object.hasOwn(payload, "networks"), false);
  const saved = adoptSIPConfig({ ...payload, peer: { ...payload.peer, inboundAuth: { ...payload.peer.inboundAuth, hasPassword: true }, outboundAuth: { ...payload.peer.outboundAuth, hasPassword: true } } }, () => "key");
  assert.equal(saved.peer.inboundAuth.password, ""); assert.equal(saved.peer.outboundAuth.password, "");
});
test("editing authentication identity invalidates only the corresponding saved password marker", () => {
  const value = cloneSIPPeer(peer(), true);
  assert.equal(updatePeerInbound(value, { username: "different" }).inboundAuth.hasPassword, false);
  assert.equal(updatePeerInbound(value, { password: "replacement-secret" }).inboundAuth.hasPassword, true);
  for (const change of [{ username: "other" }, { realm: "other-realm" }]) {
    const changed = updatePeerOutbound(value, change);
    assert.equal(changed.outboundAuth.hasPassword, false); assert.equal(changed.inboundAuth.hasPassword, true);
    assert.ok(sipPeerFormError(changed));
  }
});
test("removed or rebound line permissions are dropped rather than silently granted to a new SIM", () => {
  const value = restrictPeerLines(peer(), new Set(["modem-a"]));
  assert.deepEqual(value.outgoingLines, ["modem-a"]); assert.deepEqual(value.smsSendLines, []); assert.equal(value.incomingRoutes.length, 1);
  assert.equal(restrictPeerLines(null, new Set()), null);
});
test("fixed endpoint accepts private literal IPv4 and IPv6, not public, DNS, URI, mapped or self-unspecified endpoints", () => {
  for (const endpoint of ["127.0.0.1:5060", "10.2.3.4:5060", "172.20.1.2:5060", "192.168.1.2:5060", "[::1]:5060", "[fd00::1]:5060"]) assert.equal(sipPeerFormError({ ...emptySIPPeer(), enabled: true, endpoint }), null, endpoint);
  for (const endpoint of ["", "0.0.0.0:5060", "[::]:5060", "[::ffff:127.0.0.1]:5060", "8.8.8.8:5060", "example.test:5060", "sip:100@127.0.0.1:5060", "127.0.0.1:0", "127.0.0.1:65536", "010.0.0.1:5060", "[fd00::1%eth0]:5060"]) assert.ok(sipPeerFormError({ ...emptySIPPeer(), enabled: true, endpoint }), endpoint);
});
test("direction credentials, explicit MD5 and safe target users are validated without granting permissions", () => {
  const value = { ...emptySIPPeer(), enabled: true, endpoint: "127.0.0.1:25060" };
  assert.equal(sipPeerFormError(value), null);
  assert.ok(sipPeerFormError({ ...value, allowCallsFromPeer: true }));
  assert.ok(sipPeerFormError({ ...value, allowCallsFromPeer: true, inboundAuth: { username: "in", password: "long-enough-secret", hasPassword: false } }), "outbound credentials are also required for carrier-originated BYE");
  assert.ok(sipPeerFormError({ ...value, allowSmsToPeer: true }));
  assert.ok(sipPeerFormError({ ...value, outboundAuth: { username: "out", realm: "", password: "long-enough-secret", hasPassword: false, algorithms: ["SHA-256"] } }));
  assert.ok(sipPeerFormError({ ...value, incomingRoutes: [{ deviceId: "a", callTarget: "sip:600@elsewhere", smsTarget: "" }] }));
  assert.ok(sipPeerFormError({ ...value, outboundAuth: { ...value.outboundAuth, algorithms: ["unknown"] } }));
  assert.deepEqual(cloneSIPPeer({ ...value, outboundAuth: { ...value.outboundAuth, algorithms: [] } }).outboundAuth.algorithms, ["SHA-256"]);
});
test("runtime peer refresh reflects external disable without changing unsaved credentials, ACLs or targets", () => {
  const form = adoptSIPConfig({ ...base(), peer: peer() }, () => "key");
  form.peer.inboundAuth.password = "unsaved-private-password";
  form.peer.outgoingLines.push("draft-line");
  form.peer.incomingRoutes[0].callTarget = "draft-target";
  const draft = structuredClone(form);
  const configured = { running: true, peer: { enabled: true, endpoint: "127.0.0.1:25060", state: "configured" } };
  assert.equal(sipPeerStatus(configured).label, "配置有效（未验证连通）");
  const externalDisable = { running: true, peer: { enabled: false, endpoint: "", state: "disabled" } };
  assert.deepEqual(sipPeerStatus(externalDisable), { label: "中继已关闭", tone: "neutral" });
  assert.deepEqual(form, draft);
  assert.equal(form.peer.enabled, true, "a status refresh must not reset the draft's switch");
  assert.equal(form.peer.inboundAuth.password, "unsaved-private-password");
});
test("peer status never upgrades stale, unavailable, stopped or error states into readiness", () => {
  const configured = { running: true, peer: { enabled: true, endpoint: "127.0.0.1:25060", state: "configured" } };
  assert.deepEqual(sipPeerStatus(configured, true), { label: "中继状态暂不可用", tone: "warning" });
  assert.deepEqual(sipPeerStatus({ running: true }), { label: "中继状态暂不可用", tone: "warning" });
  assert.equal(sipPeerStatus({ ...configured, peer: { ...configured.peer, state: "unavailable" } }).tone, "warning");
  assert.equal(sipPeerStatus({ ...configured, running: false }).label, "中继已停用（SIP 服务关闭）");
  assert.equal(sipPeerStatus({ ...configured, error: "listener failure" }).tone, "danger");
  assert.equal(sipPeerStatus(configured).tone, "warning");
});

test("local recipient reset is explicit, one-shot and clear after successful adopt", () => {
  const account = { username: "1001", displayName: "Desk", password: "must-not-copy", hasPassword: true, allowMessages: false, allowCalls: true, outgoingLines: ["a"], smsSendLines: ["a"], resetCredentialIdentity: true };
  const form = adoptSIPConfig({ ...base(), accounts: [account] }, () => "local-key");
  assert.equal(form.accounts[0].password, "");
  assert.equal(form.accounts[0].resetCredentialIdentity, false, "a loaded response cannot replay a reset request");
  form.accounts[0] = updateSIPAccount(form.accounts[0], { resetCredentialIdentity: true });
  const payload = serializeSIPForm(form);
  assert.equal(payload.accounts[0].resetCredentialIdentity, true);
  assert.equal(payload.accounts[0].password, "");
  assert.equal(Object.hasOwn(payload.accounts[0], "key"), false);
  assert.equal(Object.hasOwn(payload.accounts[0], "credentialEpoch"), false);
  assert.equal(adoptSIPConfig(payload, () => "saved-key").accounts[0].resetCredentialIdentity, false);
  assert.equal(updateSIPAccount(form.accounts[0], { resetCredentialIdentity: false }).resetCredentialIdentity, false);
});

test("local credential edits preserve explicit same password while renamed users and realm require credentials", () => {
  const account = { username: "1001", displayName: "Desk", hasPassword: true, allowMessages: true, allowCalls: true, outgoingLines: [], smsSendLines: [], resetCredentialIdentity: true };
  const form = adoptSIPConfig({ ...base(), accounts: [account], peer: peer() }, () => "local-key");
  form.accounts[0] = updateSIPAccount(form.accounts[0], { password: "same-password-reset" });
  assert.equal(serializeSIPForm(form).accounts[0].password, "same-password-reset");
  const renamed = updateSIPAccount({ ...form.accounts[0], resetCredentialIdentity: true }, { username: "1002" });
  assert.equal(renamed.hasPassword, false);
  assert.equal(renamed.resetCredentialIdentity, false);
  assert.equal(renamed.key, "local-key");
  assert.equal(updateSIPRealm(form, form.realm), form);
  const changed = updateSIPRealm(form, "new-realm");
  assert.equal(changed.accounts[0].hasPassword, false);
  assert.equal(changed.accounts[0].resetCredentialIdentity, false);
  assert.equal(changed.peer.inboundAuth.hasPassword, false);
  assert.equal(changed.peer.outboundAuth.hasPassword, true);
  assert.equal(form.accounts[0].hasPassword, true, "editing must not mutate the last-saved form");
});
