import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import ts from 'typescript';
const raw=await readFile(new URL('../src/components/settings/smsAPIModel.ts',import.meta.url),'utf8');
const code=ts.transpileModule(raw,{compilerOptions:{module:ts.ModuleKind.ES2022,target:ts.ScriptTarget.ES2022}}).outputText;
const {adoptSmsAPI,smsAPIFormError,generateSmsAPIToken}=await import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`);
const status={enabled:false,bindAddress:'127.0.0.1',port:8787,allowLanAccess:false,tokenConfigured:false,running:false,baseUrl:'http://127.0.0.1:8787/'};
test('API form never adopts plaintext or credential digest from a status',()=>{
  const form=adoptSmsAPI({...status,token:'must-not-copy',tokenHash:'must-not-copy'});
  assert.deepEqual(form,{enabled:false,bindAddress:'127.0.0.1',port:'8787',allowLanAccess:false,token:''});
});
test('enabled API requires a token; blank keeps an existing token',()=>{
  const form={...adoptSmsAPI(status),enabled:true};
  assert.ok(smsAPIFormError(form,false));assert.equal(smsAPIFormError(form,true),null);
  assert.equal(smsAPIFormError({...form,token:'a'.repeat(32)},false),null);
  for(const token of ['a'.repeat(31),'a'.repeat(129),'a'.repeat(31)+' ','a'.repeat(31)+'汉'])assert.ok(smsAPIFormError({...form,token},true));
});
test('port boundaries and wildcard listener are rejected before save',()=>{
  const form=adoptSmsAPI(status);
  for(const port of ['0','80','65536','+8787','8e3','8787x',''])assert.ok(smsAPIFormError({...form,port},false));
  for(const port of ['1024','65535'])assert.equal(smsAPIFormError({...form,port},false),null);
  for(const bindAddress of ['0.0.0.0','::','*',' 127.0.0.1'])assert.ok(smsAPIFormError({...form,bindAddress},false));
});
test('token generator consumes exactly 32 crypto random bytes, encoded URL safe',()=>{
  let calls=0;
  const token=generateSmsAPIToken({getRandomValues(bytes){calls++;assert.equal(bytes.length,32);for(let i=0;i<bytes.length;i++)bytes[i]=240+i;return bytes;}});
  assert.equal(calls,1);assert.match(token,/^[A-Za-z0-9_-]{43}$/);
});
