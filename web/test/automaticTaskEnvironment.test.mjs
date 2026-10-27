import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ts from "typescript";

const text = await readFile(new URL("../src/pages/AutomaticTasksPage.tsx", import.meta.url), "utf8");
const tree = ts.createSourceFile("tasks.tsx", text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const component = tree.statements.find(node => ts.isFunctionDeclaration(node) && node.name?.text === "AutomaticTaskEnvironmentSelect");
assert.ok(component);
const source = `const Select=({value,disabled,options}:any)=><select defaultValue={value} disabled={disabled}>{options.map((o:any)=><option key={o.value} value={o.value}>{o.label}</option>)}</select>;\n${component.getText(tree)}`;
let compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
compiled = compiled.replaceAll('from "react/jsx-runtime"', `from "${import.meta.resolve("react/jsx-runtime")}"`);
const { AutomaticTaskEnvironmentSelect } = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`);
const render = (reader = false, disabled = false) => renderToStaticMarkup(createElement(AutomaticTaskEnvironmentSelect, { reader, disabled, value: reader ? "vowifi" : "host_ims", onChange: () => {}, t: x => x }));

test("modem task editor renders an explicitly selected Host IMS environment", () => {
  assert.match(render(), /value="host_ims" selected="">Host IMS/);
});
test("USB SIM task editor cannot offer cellular or Host IMS environments", () => {
  const html = render(true);
  assert.match(html, /VoWiFi/);
  assert.doesNotMatch(html, /host_ims|value="cellular"/);
});
test("registration/data task environment can remain locked", () => {
  assert.match(render(false, true), /<select disabled=""/);
});
