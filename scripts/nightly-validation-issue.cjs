// One issue per check. Missing credentials or cancelled runs never resolve it.
module.exports = async function report({github, context, core, needs}) {
  const repo = context.repo;
  const run = `${context.serverUrl}/${repo.owner}/${repo.repo}/actions/runs/${context.runId}`;
  const issues = await github.paginate(github.rest.issues.listForRepo, {...repo, state: 'open', per_page: 100});
  for (const [lane, label] of [['macos', 'macOS runtime'], ['codex', 'Codex installed plugin']]) {
    const job = needs[lane];
    if (lane === 'codex' && job.result === 'success' && job.outputs?.configured !== 'true') {
      core.warning('Codex setup required: add the OPENAI_API_KEY repository Actions secret. No live checks ran.');
      continue;
    }
    if (!['success', 'failure', 'timed_out'].includes(job.result)) continue;
    const marker = `<!-- flopwire-nightly-validation:${lane} -->`;
    const existing = issues.find(issue => !issue.pull_request && issue.user?.login === 'github-actions[bot]' && issue.body?.startsWith(marker));
    if (job.result === 'success') {
      if (existing) {
        await github.rest.issues.createComment({...repo, issue_number: existing.number, body: `Recovered in [this run](${run}) at \`${context.sha}\`.`});
        await github.rest.issues.update({...repo, issue_number: existing.number, state: 'closed'});
      }
      continue;
    }
    const body = `${marker}\n${label} failed on main at \`${context.sha}\`.\n\n[Workflow run and job logs](${run})\n\nDownload the nightly-${lane} artifact from that run for test results${lane === 'codex' ? ' and token usage' : ''}. If setup failed before artifact creation, use the job log.\n\nThis nightly check does not block merges or releases. Triage the failure; the next passing check closes this issue.`;
    if (existing) {
      await github.rest.issues.update({...repo, issue_number: existing.number, body});
    } else {
      await github.rest.issues.create({...repo, title: `Nightly validation: ${label} failed`, body});
    }
  }
};
