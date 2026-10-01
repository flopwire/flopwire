(() => {
  'use strict';
  document.querySelectorAll('[data-copy]').forEach(button => {
    button.addEventListener('click', async () => {
      const code = document.getElementById(button.dataset.copy);
      const status = document.getElementById('copy-status');
      const label = button.querySelector('span');
      try {
        await navigator.clipboard.writeText(code.textContent);
        label.textContent = 'Copied';
        status.textContent = 'Setup commands copied.';
        setTimeout(() => { label.textContent = 'Copy'; }, 2000);
      } catch {
        const selection = window.getSelection();
        const range = document.createRange();
        range.selectNodeContents(code);
        selection.removeAllRanges();
        selection.addRange(range);
        label.textContent = 'Select & copy';
        status.textContent = 'Automatic copy is unavailable. Commands selected; use your copy shortcut.';
      }
    });
  });
})();
