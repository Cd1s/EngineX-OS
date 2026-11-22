// Drives the console's Buttons page in a real browser against the real Go handlers
// (mapping_browser_test.go starts them and feeds the fake MIDI events).
// usage: node mapping_page_check.js <base url> <access code> <chrome> <screenshot dir or ->
const assert = require('node:assert/strict');
const { chromium } = require('playwright-core');
const [base, code, chrome, shots] = process.argv.slice(2);

(async () => {
  const browser = await chromium.launch({ executablePath: chrome, args: ['--no-sandbox'] });
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const login = await context.request.post(base + '/login', { form: { token: code }, maxRedirects: 0 });
  assert.ok([200, 302, 303].includes(login.status()), 'login ' + login.status());
  const page = await context.newPage();
  const problems = [];
  page.on('pageerror', e => problems.push('pageerror: ' + e.message));
  page.on('dialog', d => d.accept());
  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  const text = sel => page.locator(sel).innerText();
  const options = sel => page.$$eval(sel + ' option', os => os.map(o => o.value));
  const rows = () => page.$$eval('#mapRows .row', rs => rs.map(r => r.innerText));
  const waitText = (sel, re) => page.waitForFunction(([s, r]) => new RegExp(r).test(document.querySelector(s).innerText), [sel, re.source], { timeout: 8000 });

  await page.evaluate(() => showMap());
  assert.equal(await page.locator('#mapBody').isVisible(), true);
  assert.match(await text('#mapState'), /No mappings are applied/);
  assert.equal(await page.locator('#mapRestart').isVisible(), false);

  // the control list offers only controls with a gesture to take; Shift and navigation are not among them
  const controls = await options('#mapControl');
  assert.ok(controls.includes('PadModeSelect:30') && controls.includes('Sync:20'), controls.join());
  for (const gone of ['Shift:26', 'Back:16', 'Shortcuts:12', 'BrowseEncoder:18', 'Censor:7']) assert.ok(!controls.includes(gone), gone);
  await page.selectOption('#mapControl', 'PadModeSelect:30');
  assert.deepEqual(await options('#mapGesture'), ['shift', 'double', 'hold'], 'a press on a pad-mode button is stock');
  await page.selectOption('#mapControl', 'Sync:20');
  assert.deepEqual(await options('#mapGesture'), ['hold'], 'only the Sync hold can be taken');

  // add: Shift + Loop -> cycle waveform
  await page.selectOption('#mapControl', 'PadModeSelect:30');
  await page.selectOption('#mapGesture', 'shift');
  const shiftActions = await options('#mapAction');
  assert.ok(shiftActions.includes('waveform_cycle') && shiftActions.includes('censor') && shiftActions.includes('loop_halve'));
  await page.selectOption('#mapGesture', 'hold');
  const holdActions = await options('#mapAction');
  assert.ok(holdActions.includes('waveform_cycle') && !holdActions.includes('censor') && !holdActions.includes('loop_halve'), 'a hold drives actions and toggles only');
  await page.selectOption('#mapGesture', 'shift');
  await page.selectOption('#mapAction', 'waveform_cycle');
  await page.click('#mapAdd');
  await waitText('#mapRows', /Shift \+ press/);
  assert.match((await rows())[0], /Pad Mode Button 1 \(Loop\).*Shift \+ press.*Cycle waveform type/);
  assert.equal(await page.locator('#mapRestart').isVisible(), true);
  assert.match(await text('#mapNeed'), /apply when Engine restarts/);
  assert.equal((await page.evaluate(() => lastMap.mappings)).length, 1);

  // the same gesture again replaces the entry rather than failing as a duplicate
  await page.selectOption('#mapAction', 'beat_jump_forward');
  await page.click('#mapAdd');
  await waitText('#mapRows', /Beat jump forward/);
  assert.equal((await rows()).length, 1);
  await page.selectOption('#mapAction', 'waveform_cycle');
  await page.click('#mapAdd');
  await waitText('#mapRows', /Cycle waveform/);

  // MIDI learn: the Go side plays Shift + Loop
  await page.click('#mapLearn');
  await waitText('#mapLearnState', /Press the control on the deck/);
  await page.selectOption('#mapControl', 'PadModeSelect:27'); // the page keeps working while it listens
  await waitText('#mapLearnState', /Caught: Pad Mode Button 1 \(Loop\), Shift \+ press/);
  assert.equal(await page.inputValue('#mapControl'), 'PadModeSelect:30');
  assert.equal(await page.inputValue('#mapGesture'), 'shift');
  assert.equal(await text('#mapLearn'), 'Learn');
  // a learn of a control that cannot be mapped says why and changes nothing
  await page.click('#mapLearn');
  await waitText('#mapLearnState', /cannot be mapped: Shift and the navigation controls are never mapped/);
  assert.equal(await page.inputValue('#mapControl'), 'PadModeSelect:30');
  // stopping a learn
  await page.click('#mapLearn');
  await waitText('#mapLearn', /Stop learning/);
  await page.click('#mapLearn');
  await waitText('#mapLearn', /^Learn$/);

  // a second mapping, then remove the first
  await page.selectOption('#mapControl', 'PadModeSelect:27');
  await page.selectOption('#mapGesture', 'hold');
  await page.selectOption('#mapAction', 'sync');
  await page.click('#mapAdd');
  await waitText('#mapRows', /Hold/);
  assert.equal((await rows()).length, 2);
  await page.locator('#mapRows .row').first().locator('button').click();
  await page.waitForFunction(() => document.querySelectorAll('#mapRows .row').length === 1);
  assert.match((await rows())[0], /Hold/);

  // the reference table: all controls, mappable gestures marked
  const ref = await page.$$eval('#mapRef tr', trs => trs.map(tr => [...tr.children].map(c => c.className + '|' + c.textContent)));
  assert.ok(ref.length > 30, 'rows ' + ref.length);
  const loop = ref.find(r => r[0].includes('Pad Mode Button 1 (Loop)'));
  assert.ok(loop[2].startsWith('free|') && loop[3].startsWith('free|') && loop[4].startsWith('free|') && !loop[1].startsWith('free|'));
  const back = ref.find(r => r[0].includes('Back'));
  assert.ok(back.slice(1).every(c => !c.startsWith('free|')));

  // the three other languages
  for (const [lang, state, gesture] of [['zh-Hans', /没有已应用的映射/, /长按/], ['zh-Hant', /沒有已套用的對應/, /長按/], ['es', /No hay asignaciones aplicadas/, /Mantener/]]) {
    await page.evaluate(l => setLang(l, false), lang);
    assert.match(await text('#mapState'), state, lang);
    assert.match((await rows())[0], gesture, lang);
    assert.ok((await text('#mapRestart')).length > 3 && (await text('#mapAdd')).length > 3, lang);
    assert.doesNotMatch(await text('#s-buttons'), /\bmap[A-Z]\w*\b/, 'an untranslated key shows in ' + lang);
  }
  await page.evaluate(() => setLang('en', false));

  // import and export
  const exported = await context.request.get(base + '/api/mapping/export');
  assert.equal(exported.status(), 200);
  assert.equal(JSON.parse(await exported.text()).length, 1);
  const upload = (name, body) => page.setInputFiles('#mapImportFile', { name, mimeType: 'application/json', buffer: Buffer.from(body) });
  await upload('mappings.json', JSON.stringify([
    { board: 'JP07', control: 'PadModeSelect:28', gesture: 'double', action: 'key_lock' },
    { board: 'JP07', control: 'PadModeSelect:29', gesture: 'shift', action: 'loop_double' }]));
  await waitText('#mapRows', /Roll/);
  assert.equal((await rows()).length, 2);
  await upload('bad.json', JSON.stringify([{ board: 'JP07', control: 'Shift:26', gesture: 'press', action: 'sync' }]));
  await waitText('#notice', /never mapped/);
  assert.equal((await rows()).length, 2, 'a rejected import changes nothing');
  await upload('bad.json', 'not json');
  await waitText('#notice', /not a valid list/);

  // restart
  await page.click('#mapRestart');
  await waitText('#notice', /Engine is restarting/);

  if (shots !== '-') {
    await page.evaluate(() => document.querySelector('#s-buttons').scrollIntoView());
    await page.locator('#mapRefCard summary').click();
    await page.screenshot({ path: shots + '/buttons-en.png', fullPage: false });
    await page.evaluate(() => setLang('zh-Hans', false));
    await page.screenshot({ path: shots + '/buttons-zh-Hans.png', fullPage: false });
  }
  assert.deepEqual(problems, []);
  await browser.close();
  console.log('page ok');
})().catch(e => { console.error(e.stack || e); process.exit(1); });
