const {test} = require('node:test');
const assert = require('node:assert/strict');
const report = require('./nightly-validation-issue.cjs');

async function exercise(needs, issues = []) {
  const calls = [];
  const api = Object.fromEntries(['listForRepo', 'create', 'update', 'createComment'].map(method => [method, async args => calls.push({method, args})]));
  await report({
    needs,
    context: {repo: {owner: 'fixture', repo: 'test'}, serverUrl: 'https://github.com', runId: 123, sha: 'abc'},
    github: {rest: {issues: api}, paginate: async () => issues},
    core: {warning: message => calls.push({method: 'warning', message})},
  });
  return calls;
}
const job = (result, configured = 'true') => ({result, outputs: {configured}});
const issue = lane => ({number: 7, user: {login: 'github-actions[bot]'}, body: `<!-- flopwire-nightly-validation:${lane} -->\nOld failure`});

test('creates one failure issue per lane with evidence links', async () => {
  const calls = await exercise({macos: job('failure'), codex: job('failure')});
  assert.equal(calls.length, 2);
  assert.ok(calls.every(c => c.method === 'create' && c.args.body.includes('/actions/runs/123')));
});
test('updates the existing bot issue without duplicates', async () => {
  const calls = await exercise({macos: job('failure'), codex: job('skipped')}, [issue('macos')]);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, 'update');
  assert.equal(calls[0].args.issue_number, 7);
});
test('missing credentials warn and never close a previous Codex failure', async () => {
  const calls = await exercise({macos: job('success'), codex: job('success', 'false')}, [issue('codex')]);
  assert.deepEqual(calls.map(c => c.method), ['warning']);
});
test('setup failure still reports, even before credential output exists', async () => {
  const calls = await exercise({macos: job('success'), codex: job('failure', '')});
  assert.equal(calls[0].method, 'create');
});
test('cancellation or skips never resolve previous failures', async () => {
  assert.deepEqual(await exercise({macos: job('cancelled'), codex: job('skipped')}, [issue('macos'), issue('codex')]), []);
});
test('actual recovery comments then closes the matching issue', async () => {
  const calls = await exercise({macos: job('success'), codex: job('success')}, [issue('codex')]);
  assert.deepEqual(calls.map(c => c.method), ['createComment', 'update']);
  assert.equal(calls[1].args.state, 'closed');
});
test('does not overwrite human issues or pull requests with copied markers', async () => {
  const human = {...issue('macos'), user: {login: 'human'}};
  const pr = {...issue('macos'), pull_request: {}};
  const calls = await exercise({macos: job('failure'), codex: job('success')}, [human, pr]);
  assert.equal(calls[0].method, 'create');
});
