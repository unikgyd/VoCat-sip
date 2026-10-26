import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import ts from "typescript";
const source = await readFile(new URL("../src/components/devices/imsLineModel.ts", import.meta.url), "utf8");
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } });
const { selectedIMSMode, hostIMSStatus } = await import(`data:text/javascript;base64,${Buffer.from(compiled.outputText).toString("base64")}`);
test("legacy mode compatibility does not override explicit Host IMS", () => {
  assert.equal(selectedIMSMode({ vowifiEnabled: true }), "vowifi");
  assert.equal(selectedIMSMode({ vowifiEnabled: false }), "module");
  assert.equal(selectedIMSMode({ imsMode: "host_ims", vowifiEnabled: true }), "host_ims");
});
test("Host IMS readiness never requires an ePDG tunnel or SMS capability", () => {
  assert.equal(hostIMSStatus({ imsEnabled: true, imsRuntime: { simReady: true, accessReady: true, imsReady: true, tunnelReady: false, smsReady: false } }).tone, "success");
});
test("disabled, loading and errors remain distinct", () => {
  assert.equal(hostIMSStatus({ imsEnabled: false }).tone, "neutral");
  assert.equal(hostIMSStatus({ imsEnabled: true }).tone, "warning");
  assert.equal(hostIMSStatus({ imsEnabled: true, imsRuntime: { phase: "failed" } }).tone, "danger");
  assert.equal(hostIMSStatus({ imsEnabled: true, imsRuntime: { lastErrorClass: "cleanup_failed" } }).tone, "danger");
});
