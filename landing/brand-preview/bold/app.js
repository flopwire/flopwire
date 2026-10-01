(() => {
  'use strict';
  document.querySelectorAll('[data-copy]').forEach((button) => {
    button.addEventListener('click', async () => {
      const target = document.getElementById(button.dataset.copy);
      const label = button.querySelector('span');
      const status = document.getElementById('copy-status');
      try {
        await navigator.clipboard.writeText(target.textContent);
        label.textContent = 'Copied';
        status.textContent = 'Setup commands copied.';
        window.setTimeout(() => { label.textContent = 'Copy'; }, 2000);
      } catch {
        const selection = window.getSelection();
        const range = document.createRange();
        range.selectNodeContents(target);
        selection.removeAllRanges();
        selection.addRange(range);
        label.textContent = 'Select & copy';
        status.textContent = 'Automatic copy is unavailable. Commands selected; use your copy shortcut.';
      }
    });
  });
})();
