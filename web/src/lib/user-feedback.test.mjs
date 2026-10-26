import test from "node:test";
import assert from "node:assert/strict";
import { chatFailureMessage, copyText } from "./user-feedback.ts";

test("chat failures provide an action without leaking upstream JSON or credentials", () => {
  const output = chatFailureMessage('API error 401: {"message":"Invalid credential sk-secret"}');
  assert.match(output, /API 密钥/);
  assert.doesNotMatch(output, /sk-secret|401|\{/);
  assert.match(chatFailureMessage("context deadline exceeded"), /超时/);
  assert.match(chatFailureMessage("429 quota"), /额度/);
});

test("copy confirms successful async writes", async (t) => {
  let copied = "";
  const original = Object.getOwnPropertyDescriptor(globalThis, "navigator");
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: { clipboard: { writeText: async (text) => { copied = text; } } } });
  t.after(() => { if (original) Object.defineProperty(globalThis, "navigator", original); else delete globalThis.navigator; });
  await copyText("中文🙂");
  assert.equal(copied, "中文🙂");
});

for (const accepted of [true,false]) test(`HTTP clipboard fallback reports success=${accepted}`, async (t) => {
 const saved = new Map(["navigator","document","HTMLElement"].map((key) => [key,Object.getOwnPropertyDescriptor(globalThis,key)]));
 let removed=false, focused=false;
 const field={value:"",style:{},setAttribute(){},select(){},remove(){removed=true;}};
 Object.defineProperty(globalThis,"navigator",{configurable:true,value:{}});
 Object.defineProperty(globalThis,"HTMLElement",{configurable:true,value:class {}});
 Object.defineProperty(globalThis,"document",{configurable:true,value:{activeElement:null,getSelection:()=>null,body:{appendChild(){focused=true;}},createElement:()=>field,execCommand:()=>accepted}});
 t.after(()=>{for(const [key,descriptor] of saved) {if(descriptor) Object.defineProperty(globalThis,key,descriptor);else delete globalThis[key];}});
 if(accepted) await copyText("HTTP text"); else await assert.rejects(copyText("HTTP text"),/复制失败/);
 assert.equal(field.value,"HTTP text"); assert.equal(removed,true); assert.equal(focused,true);
});
