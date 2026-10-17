const setupPrompt = document.getElementById('setup-prompt');
const copySetup = document.getElementById('copy-setup');
const copyFeedback = document.getElementById('copy-feedback');
let resetCopy;
copySetup.addEventListener('click', async () => {
  clearTimeout(resetCopy);
  try {
    await navigator.clipboard.writeText(setupPrompt.textContent.trim());
    copySetup.querySelector('use').setAttribute('href', '#i-check');
    copySetup.querySelector('.copy-label').textContent = 'Copied!';
    copySetup.setAttribute('aria-label', 'Copied setup instruction');
    copySetup.title = 'Copied!';
    copyFeedback.classList.add('sr-only');
    copyFeedback.textContent = 'Copied!';
    resetCopy = setTimeout(() => {
      copySetup.querySelector('use').setAttribute('href', '#i-copy');
      copySetup.querySelector('.copy-label').textContent = '';
      copySetup.setAttribute('aria-label', 'Copy setup instruction');
      copySetup.title = 'Copy instruction';
      copyFeedback.textContent = '';
    }, 3000);
  } catch {
    copyFeedback.classList.remove('sr-only');
    setupPrompt.focus();
    const range = document.createRange();
    range.selectNodeContents(setupPrompt);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    copyFeedback.textContent = 'Instruction selected. Use your copy shortcut.';
  }
});
