const setupPrompt = document.getElementById('setup-prompt');
const copySetup = document.getElementById('copy-setup');
const copyFeedback = document.getElementById('copy-feedback');
copySetup.addEventListener('click', async () => {
  try {
    await navigator.clipboard.writeText(setupPrompt.value);
    copyFeedback.textContent = 'Copied. Paste it into your agent.';
  } catch {
    setupPrompt.focus();
    setupPrompt.select();
    copyFeedback.textContent = 'Instruction selected. Use your copy shortcut.';
  }
});
