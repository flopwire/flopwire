(() => {
  'use strict';
  const setup = document.getElementById('agent-setup');
  if (!['flopwire.com', 'www.flopwire.com'].includes(location.hostname)) {
    setup.textContent = `Read ${new URL('../../setup.md', location.href).href} and set up Flopwire for this agent.`;
  }
  document.querySelectorAll('[data-copy]').forEach(button => {
    const initialLabel = button.querySelector('span').textContent;
    let resetTimer;
    button.addEventListener('click', async () => {
      const target = document.getElementById(button.dataset.copy);
      const label = button.querySelector('span');
      const status = document.getElementById('copy-status');
      clearTimeout(resetTimer);
      try {
        await navigator.clipboard.writeText(target.textContent);
        label.textContent = 'Copied';
        status.textContent = 'Agent setup instructions copied.';
        resetTimer = setTimeout(() => { label.textContent = initialLabel; }, 2000);
      } catch {
        target.scrollIntoView({block: 'center'});
        const selection = window.getSelection();
        const range = document.createRange();
        range.selectNodeContents(target);
        selection.removeAllRanges();
        selection.addRange(range);
        label.textContent = 'Instructions selected';
        status.textContent = 'Automatic copy is unavailable. Setup instructions selected; use your copy shortcut.';
      }
    });
  });
})();
