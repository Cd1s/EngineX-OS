// Exercise the console's actual tracklist controls without a browser dependency.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const page = fs.readFileSync('pkg/webui/index.html', 'utf8');
new vm.Script(page.split('<script>')[1].split('</script>')[0]);
const elements = {
  tracklistSet: {
    options: [], selected: '',
    get value() { return this.options.some(o => o.value === this.selected) ? this.selected : this.options[0]?.value || ''; },
    set value(v) { this.selected = v; },
    replaceChildren() { this.options = []; this.selected = ''; },
    append(o) { this.options.push(o); },
  },
  tracklistTxt: {}, tracklistCsv: {}, tracklistState: {},
};
const context = vm.createContext({
  $: id => elements[id], URLSearchParams, location: {},
  document: { createElement: () => ({ dataset: {}, set innerHTML(v) { throw Error('Unsafe label rendering: ' + v); } }) },
  t: (key, arg) => key + (arg ? ': ' + arg : ''),
});
const start = page.indexOf('let lastTracklists=');
const end = page.indexOf('function renderSessions()', start);
assert.ok(start >= 0 && end > start, 'tracklist controls exist');
vm.runInContext(page.slice(start, end), context);
const older = { drive: 'USB & test', id: 1, title: '<img src=x onerror=alert(1)>', startTime: '2023-11-14T22:13:20Z' };
const newer = { ...older, id: 2, title: 'New set' };
context.api = async () => ({ sets: [older], unavailable: [] });
(async () => {
  await vm.runInContext('showTracklists()', context);
  const selected = elements.tracklistSet.value;
  assert.equal(elements.tracklistSet.options[0].textContent, older.drive + ' · ' + older.title + ' · ' + older.startTime);
  context.api = async () => ({ sets: [newer, older], unavailable: ['Damaged USB'] });
  await vm.runInContext('showTracklists()', context);
  assert.equal(elements.tracklistSet.value, selected, 'refresh must keep the same set when a new set arrives');
  vm.runInContext('downloadTracklist("csv")', context);
  const download = new URL(context.location.href, 'http://console.invalid');
  assert.equal(download.searchParams.get('drive'), older.drive);
  assert.equal(download.searchParams.get('id'), '1');
  assert.equal(download.searchParams.get('format'), 'csv');
  assert.equal(elements.tracklistState.textContent, 'tlReadError: Damaged USB');
  context.api = async () => ({ sets: [], unavailable: [] });
  await vm.runInContext('showTracklists()', context);
  assert.equal(elements.tracklistCsv.disabled, true);
  assert.equal(elements.tracklistTxt.disabled, true);
  assert.equal(elements.tracklistState.textContent, '');
  console.log('tracklist controls: PASS');
})().catch(error => { console.error(error); process.exitCode = 1; });
