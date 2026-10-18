// Browser check of the demo pages in a real Chrome: loads / and /playground,
// records every console message and Content-Security-Policy violation, logs
// in through the Keycloak login form (Authorization Code + PKCE), runs a query,
// uses the docs explorer, autocomplete and the Settings and Short-keys dialogs
// in GraphiQL, and screenshots each step. Exits 2 on any CSP violation or
// console error on our pages, except the KNOWN ones below (reported, counted).
// Usage: make browser-check (credentials come from DEMO_USER / DEMO_PASS).
const fs = require('fs');
const os = require('os');
const path = require('path');
const puppeteer = require('puppeteer-core');

const BASE = process.env.BASE || 'http://localhost:8182';
const OUT = process.env.OUT || fs.mkdtempSync(path.join(os.tmpdir(), 'browser-check-'));
const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const USER = process.env.DEMO_USER, PASS = process.env.DEMO_PASS;
if (!USER || !PASS) { console.error('set DEMO_USER and DEMO_PASS'); process.exit(1); }
fs.mkdirSync(OUT, { recursive: true });
// Select-all is Cmd-A on macOS and Ctrl-A elsewhere.
const SELECT_ALL_MOD = process.platform === 'darwin' ? 'Meta' : 'Control';

// KNOWN lists accepted violations: each matches one source, is reported, and
// never fails the check. Anything else does.
const KNOWN = [
  {
    // GraphiQL's Settings and Short-keys dialogs inject a scroll-lock <style>
    // element (react-remove-scroll). Cosmetic: the page behind the dialog may
    // scroll. Allowing it needs 'unsafe-inline' or a hash that changes with the
    // scrollbar width, so it is accepted instead.
    id: 'graphiql-dialog-scroll-lock-style',
    match: (e) => /style-src/.test(e.text) && /graphiql(\.min)?\.js/.test(e.loc || ''),
  },
];
const knownID = (e) => (KNOWN.find((k) => k.match(e)) || {}).id;

(async () => {
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: 'new', args: ['--no-first-run', '--no-default-browser-check'],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 860 });
  const events = [];
  const cdp = await page.createCDPSession();
  await cdp.send('Log.enable');
  await cdp.send('Audits.enable');
  cdp.on('Log.entryAdded', (e) => events.push({ src: 'log:' + e.entry.source, level: e.entry.level, url: e.entry.url || '', loc: e.entry.url || '', text: e.entry.text }));
  cdp.on('Audits.issueAdded', (e) => {
    if (!e.issue.code.includes('ContentSecurityPolicy')) return;
    const d = e.issue.details.contentSecurityPolicyIssueDetails || {};
    events.push({ src: 'audit', level: 'error', url: d.blockedURL || '', loc: (d.sourceCodeLocation || {}).url || '', text: JSON.stringify(d) });
  });
  page.on('console', (m) => events.push({ src: 'console', level: m.type(), url: page.url(), loc: (m.location() || {}).url || '', text: m.text() }));
  page.on('pageerror', (e) => events.push({ src: 'pageerror', level: 'error', url: page.url(), text: e.message }));

  const step = async (name) => { await page.screenshot({ path: `${OUT}/${name}.png` }); console.log('screenshot', name, page.url()); };
  let flowError = null;
  try {

  await page.goto(BASE + '/', { waitUntil: 'networkidle0' });
  console.log('walkthrough status:', await page.$eval('#status', (e) => e.textContent), '| login enabled:', await page.$eval('#btnLogin', (e) => !e.disabled), '| query box filled by JS:', (await page.$eval('#query', (e) => e.value)).startsWith('{ me'));
  await step('1-walkthrough');

  await page.goto(BASE + '/playground', { waitUntil: 'networkidle0' });
  console.log('playground status:', await page.$eval('#who', (e) => e.textContent));
  await step('2-playground-logged-out');

  // PKCE login through the real Keycloak login form.
  await Promise.all([page.waitForNavigation({ waitUntil: 'networkidle0' }), page.click('#btnLogin')]);
  console.log('at identity provider:', page.url().split('?')[0]);
  await page.type('#username', USER);
  await page.type('#password', PASS);
  await Promise.all([page.waitForNavigation({ waitUntil: 'networkidle0' }), page.click('#kc-login')]);
  await page.waitForFunction(() => document.querySelector('#who').classList.contains('ok'), { timeout: 15000 });
  console.log('playground after login:', await page.$eval('#who', (e) => e.textContent));
  await page.waitForSelector('.graphiql-execute-button', { timeout: 20000 });
  await step('3-playground-logged-in');

  await page.click('.graphiql-execute-button');
  await page.waitForFunction(() => { const r = document.querySelector('.graphiql-response'); return r && r.innerText.includes('"me"'); }, { timeout: 20000 });
  const result = await page.$eval('.graphiql-response', (e) => e.innerText.replace(/\s+/g, ' ').slice(0, 220));
  console.log('graphiql result:', result);
  await step('4-playground-query-result');

  // Exercise more of GraphiQL: the docs explorer, typing with autocomplete hints, prettify.
  const docs = await page.$('button[aria-label="Show Documentation Explorer"]');
  if (docs) { await docs.click(); await page.waitForSelector('.graphiql-doc-explorer', { timeout: 10000 }); console.log('docs explorer opened'); }
  await page.click('.graphiql-query-editor .CodeMirror');
  await page.keyboard.down(SELECT_ALL_MOD); await page.keyboard.press('KeyA'); await page.keyboard.up(SELECT_ALL_MOD);
  await page.keyboard.type('{ me { su');
  await page.waitForSelector('.CodeMirror-hints', { timeout: 10000 }).then(() => console.log('autocomplete hints shown')).catch(() => console.log('no hint popup'));
  await page.keyboard.press('Enter');
  await page.keyboard.type(' } }');
  const prettify = await page.$('button[aria-label="Prettify query (Shift-Ctrl-P)"]');
  if (prettify) { await prettify.click(); console.log('prettified'); }
  await step('5-playground-docs-and-autocomplete');

  // GraphiQL's dialogs: Settings, then Short keys (each opened, shot, closed).
  for (const [label, shot] of [[/settings/i, '6-graphiql-settings'], [/short ?keys/i, '7-graphiql-short-keys']]) {
    const buttons = await page.$$('button[aria-label]');
    let opened = false;
    for (const b of buttons) {
      const name = await b.evaluate((el) => el.getAttribute('aria-label'));
      if (!label.test(name)) continue;
      await b.click();
      await page.waitForSelector('[role="dialog"]', { timeout: 10000 });
      console.log('dialog opened:', name);
      await step(shot);
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => !document.querySelector('[role="dialog"]'), { timeout: 10000 });
      opened = true;
      break;
    }
    if (!opened) throw new Error('no GraphiQL button matching ' + label);
  }

  } catch (e) {
    flowError = e;
    console.log('FLOW FAILED:', e.message);
  }

  // Messages from the identity provider's own pages are its concern, not ours.
  const ours = events.filter((e) => e.src === 'audit' || e.url === '' || e.url.startsWith(BASE));
  const isCSP = (e) => /content security policy|Refused to/i.test(e.text) || e.src === 'audit';
  const known = ours.filter((e) => knownID(e));
  const unknown = ours.filter((e) => !knownID(e));
  const csp = unknown.filter(isCSP);
  const errors = unknown.filter((e) => e.level === 'error' && !isCSP(e));
  console.log('events on our pages:', ours.length, '| CSP violations:', csp.length, '| errors:', errors.length, '| known (accepted):', known.length);
  for (const e of [...csp, ...errors].slice(0, 15)) console.log('  ', e.src, e.level, e.url.slice(0, 60), '|', e.text.slice(0, 220));
  for (const id of new Set(known.map(knownID))) console.log('   known:', id, 'x' + known.filter((e) => knownID(e) === id).length);
  console.log('screenshots in', OUT);
  await browser.close();
  process.exit(csp.length || errors.length ? 2 : flowError ? 1 : 0);
})().catch((e) => { console.error('FAILED:', e.message); process.exit(1); });
