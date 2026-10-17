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

const copyBox = document.querySelector('.copy-box');
let cancelSetupArrival = () => {};
document.querySelectorAll('a[href="#get-started"]').forEach(link => {
  link.addEventListener('click', event => {
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    cancelSetupArrival();
    copyBox.classList.remove('setup-highlight');
    const reducedMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;
    let fallback;
    let clearHighlight;
    const arrive = () => {
      clearTimeout(fallback);
      document.removeEventListener('scrollend', arrive);
      copySetup.focus({ preventScroll: true });
      copyBox.classList.add('setup-highlight');
      clearHighlight = setTimeout(() => copyBox.classList.remove('setup-highlight'), 1400);
    };
    cancelSetupArrival = () => {
      clearTimeout(fallback);
      clearTimeout(clearHighlight);
      document.removeEventListener('scrollend', arrive);
    };
    history.replaceState(null, '', '#get-started');
    if (reducedMotion) {
      copyBox.scrollIntoView({ behavior: 'instant', block: 'center' });
      arrive();
    } else {
      document.addEventListener('scrollend', arrive, { once: true });
      // Also handles an already-visible target and browsers without scrollend.
      fallback = setTimeout(arrive, 1600);
      copyBox.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
  });
});
