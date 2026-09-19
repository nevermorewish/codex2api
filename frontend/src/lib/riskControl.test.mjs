import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { parseRiskWords, parseRiskIDs } from './riskControl.ts'
test('risk words normalize case duplicates and whitespace',()=>assert.deepEqual(parseRiskWords('  hello\nHELLO\n\n中文\r\nworld'),['HELLO','中文','world']))
test('risk scope validates positive integer identities',()=>{assert.deepEqual(parseRiskIDs('1, 2，2\n3'),[1,2,3]);assert.deepEqual(parseRiskIDs(''),[]);for(const value of ['abc','-1','1.2','0','9007199254740993'])assert.throws(()=>parseRiskIDs(value))})
test('risk center follows shared controls, routes and localization contracts',()=>{
 const page=readFileSync(new URL('../pages/RiskControl.tsx',import.meta.url),'utf8')
 assert.doesNotMatch(page,/<select[\s>]|<Input\s+type="number"|window\.confirm/)
 for(const token of ['<Select','<DraftNumberInput','useConfirmDialog','api.updateRiskConfig','api.getRiskLogs','api.unbanRiskKey','api.deleteRiskHash','api.testRiskKey'])assert.ok(page.includes(token),token)
 for(const token of ['logsLoading','logsError','bansLoading','bansError','role="alert"','config?.config.mode === "off"'])assert.ok(page.includes(token),token)
 const route=readFileSync(new URL('../App.tsx',import.meta.url),'utf8')
 assert.ok(route.includes('/risk-control/:view'));assert.ok(route.includes('/risk-control/prompt/:view'))
 const locales=['zh','zh-TW','en'].map(name=>JSON.parse(readFileSync(new URL('../locales/'+name+'.json',import.meta.url),'utf8')))
 assert.deepEqual(Object.keys(locales[0].riskControl),Object.keys(locales[1].riskControl))
 assert.deepEqual(Object.keys(locales[0].riskControl),Object.keys(locales[2].riskControl))
 for(const locale of locales){assert.ok(locale.nav.riskControl);for(const [,key] of page.matchAll(/t\("riskControl\.([^"]+)"/g))assert.ok(key.split('.').reduce((value,part)=>value?.[part],locale.riskControl),key)}
})

test('custom model audit exposes pool, policy, prompt and dry-run with shared controls',()=>{
 const page=readFileSync(new URL('../pages/RiskModelAudit.tsx',import.meta.url),'utf8')
 assert.doesNotMatch(page,/<select[\s>]|<Input\s+type="number"|window\.confirm/)
 for(const token of ['api.testModelAudit','system_prompt','block_threshold','flag_threshold','fail_open','max_input_chars','clear_api_key','ArrowUp','ArrowDown','useConfirmDialog','<DraftNumberInput','<Switch','<Select'])assert.ok(page.includes(token),token)
 const locales=['zh','zh-TW','en'].map(name=>JSON.parse(readFileSync(new URL('../locales/'+name+'.json',import.meta.url),'utf8')).riskControl.modelAudit)
 for(const locale of locales) {
   assert.deepEqual(Object.keys(locale),Object.keys(locales[0]))
   assert.equal(Object.keys(locale.categories).length,9)
   for(const [,key] of page.matchAll(/tr\("([^"]+)"\)/g)) assert.ok(locale[key],key)
 }
})
