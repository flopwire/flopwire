const {test} = require('node:test');
const assert = require('node:assert/strict');
const cleanup = require('./cancel-obsolete-merge-groups.cjs');
const branch = 'gh-readonly-queue/main/pr-123-abc';
const run = {id: 1, event: 'merge_group', status: 'in_progress', head_branch: branch};

function fixture(runs = [run], refs = []) {
  const calls = [];
  const github = {
    rest: {actions: {listWorkflowRunsForRepo: 'runs', cancelWorkflowRun: async args => calls.push(args.run_id)},
      git: {listMatchingRefs: 'refs'}},
    paginate: async (method, args) => {
      if (method === 'runs') {
        assert.equal(args.event, 'merge_group');
        assert.equal(args.per_page, 100);
        return runs.filter(r => r.status === args.status);
      }
      assert.equal(args.ref, `heads/${branch}`);
      if (refs instanceof Error) throw refs;
      return refs;
    },
  };
  return {github, context: {repo: {owner: 'owner', repo: 'repo'}}, core: {info() {}}, calls};
}

test('cancels missing queue branches across all unfinished statuses', async () => {
  const f = fixture(['queued', 'in_progress', 'waiting', 'pending', 'requested'].map((status, id) => ({...run, id, status})));
  await cleanup(f);
  assert.deepEqual(f.calls, [0, 1, 2, 3, 4]);
});
test('preserves a live queue branch regardless of SHA', async () => {
  const f = fixture([run], [{ref: `refs/heads/${branch}`, object: {sha: 'new-sha'}}]);
  await cleanup(f);
  assert.deepEqual(f.calls, []);
});
test('only exact branch matches preserve a run', async () => {
  const f = fixture([run], [{ref: `refs/heads/${branch}-other`}]);
  await cleanup(f);
  assert.deepEqual(f.calls, [1]);
});
test('ignores PR, push, completed, unrelated branch and missing branch runs', async () => {
  const f = fixture([{...run, event: 'pull_request'}, {...run, event: 'push'},
    {...run, status: 'completed'}, {...run, head_branch: 'main'},
    {...run, head_branch: 'gh-readonly-queue/other/pr-123'}, {...run, head_branch: null}]);
  await cleanup(f);
  assert.deepEqual(f.calls, []);
});
test('API failure fails closed, including a 404', async () => {
  const error = Object.assign(new Error('not found'), {status: 404});
  const f = fixture([run], error);
  await assert.rejects(cleanup(f), error);
  assert.deepEqual(f.calls, []);
});
test('tolerates completed-run races but propagates cancellation failures', async () => {
  const f = fixture();
  f.github.rest.actions.cancelWorkflowRun = async () => {throw Object.assign(new Error('done'), {status: 409});};
  await cleanup(f);
  f.github.rest.actions.cancelWorkflowRun = async () => {throw Object.assign(new Error('forbidden'), {status: 403});};
  await assert.rejects(cleanup(f), /forbidden/);
});
