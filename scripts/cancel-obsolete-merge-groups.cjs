// Run only from trusted main: this helper holds actions:write.
const prefix = 'gh-readonly-queue/main/';
const active = new Set(['queued', 'in_progress', 'waiting', 'pending', 'requested']);

module.exports = async function cleanup({github, context, core}) {
  const repo = context.repo;
  for (const status of active) {
    const runs = await github.paginate(github.rest.actions.listWorkflowRunsForRepo,
      {...repo, event: 'merge_group', status, per_page: 100});
    for (const run of runs) {
      if (run.event !== 'merge_group' || !active.has(run.status) ||
          !run.head_branch?.startsWith(prefix)) continue;
      // A successful refs response is required. Never interpret API errors as
      // a missing branch. Check each candidate immediately before cancellation.
      const refs = await github.paginate(github.rest.git.listMatchingRefs,
        {...repo, ref: `heads/${run.head_branch}`, per_page: 100});
      if (refs.some(ref => ref.ref === `refs/heads/${run.head_branch}`)) continue;
      try {
        await github.rest.actions.cancelWorkflowRun({...repo, run_id: run.id});
        core.info(`Cancelled obsolete merge-group run ${run.id} (${run.head_branch})`);
      } catch (error) {
        // A run can finish between listing and cancellation.
        if (error.status !== 409) throw error;
        core.info(`Run ${run.id} already finished or is being cancelled`);
      }
    }
  }
};
