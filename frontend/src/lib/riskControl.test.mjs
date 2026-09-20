import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { parseRiskWords, parseRiskIDs, createAuditNodeID } from './riskControl.ts'
test('risk words normalize case duplicates and whitespace',()=>assert.deepEqual(parseRiskWords('  hello\nHELLO\n\n中文\r\nworld'),['HELLO','中文','world']))
test('risk scope validates positive integer identities',()=>{assert.deepEqual(parseRiskIDs('1, 2，2\n3'),[1,2,3]);assert.deepEqual(parseRiskIDs(''),[]);for(const value of ['abc','-1','1.2','0','9007199254740993'])assert.throws(()=>parseRiskIDs(value))})
test('risk center follows shared controls, routes and localization contracts',()=>{
 const page=readFileSync(new URL('../pages/RiskControl.tsx',import.meta.url),'utf8')
 assert.doesNotMatch(page,/api\.unbanRiskKey|auto_ban_enabled|bansLoading/)
 assert.doesNotMatch(page,/<select[\s>]|<Input\s+type="number"|window\.confirm/)
 for(const token of ['<Select','<DraftNumberInput','useConfirmDialog','api.updateRiskConfig','api.getRiskLogs','api.deleteRiskHash'])assert.ok(page.includes(token),token)
 for(const token of ['logsLoading','logsError','role="alert"','config?.config.mode === "off"'])assert.ok(page.includes(token),token)
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
 for(const token of ['api.testModelAudit','system_prompt','block_threshold','flag_threshold','tr("failOpen")','max_input_chars','clear_api_key','ArrowUp','ArrowDown','useConfirmDialog','<DraftNumberInput','<Switch','onSave()','tr("savePrompt")'])assert.ok(page.includes(token),token)
 assert.doesNotMatch(page, /<Select|mode: v|enabled: v|keyword_blocking_mode:|patch\(\{ fail_open/)
 assert.ok(page.includes('to="/risk-control/policy"'))
 assert.doesNotMatch(page, /config\.mode\b|config\.enabled\b|config\.keyword_blocking_mode\b|riskControl\.text038/)
 const locales=['zh','zh-TW','en'].map(name=>JSON.parse(readFileSync(new URL('../locales/'+name+'.json',import.meta.url),'utf8')).riskControl.modelAudit)
 for(const locale of locales) {
   assert.deepEqual(Object.keys(locale),Object.keys(locales[0]))
   assert.equal(Object.keys(locale.categories).length,9)
   for(const [,key] of page.matchAll(/tr\("([^"]+)"\)/g)) assert.ok(locale[key],key)
 }
})

test('audit node IDs support insecure HTTP without randomUUID', () => {
 const insecureCrypto = { getRandomValues: bytes => { bytes.fill(42); return bytes } }
 assert.equal(createAuditNodeID(insecureCrypto), 'audit-' + '2a'.repeat(16))
 const ids = Array.from({length: 1000}, () => createAuditNodeID({}))
 assert.equal(new Set(ids).size, ids.length)
 for (const id of ids) { assert.ok(id.length <= 80); assert.doesNotMatch(id, /[\\/\s]/) }
 assert.equal(createAuditNodeID({randomUUID: () => 'secure-uuid'}), 'secure-uuid')
 const page = readFileSync(new URL('../pages/RiskModelAudit.tsx', import.meta.url), 'utf8')
 assert.ok(page.includes('createAuditNodeID()'))
 assert.ok(!page.includes('crypto.randomUUID()'))
})

test('risk center has one audit pool and no retired settings',()=>{
 const page=readFileSync(new URL('../pages/RiskControl.tsx',import.meta.url),'utf8')
 const audit=readFileSync(new URL('../pages/RiskModelAudit.tsx',import.meta.url),'utf8')
 assert.doesNotMatch(page,/form\.email_on_hit|form\.model_filter|api\.testRiskKey|SMTP|scopeKeys/)
 assert.doesNotMatch(audit,/value: "moderations"|config\.audit_engine/)
 assert.ok(page.includes('fallback_on_block_enabled'))
})
