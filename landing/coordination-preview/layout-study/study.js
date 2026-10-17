const root = document.documentElement;
const parameters = new URLSearchParams(location.search);
for (const [name, allowed] of Object.entries({ layout: ['current', 'wide'], view: ['terminal', 'designed'] })) {
  const value = parameters.get(name);
  if (allowed.includes(value)) root.dataset[name] = value;
  document.querySelector(`input[name="${name}"][value="${root.dataset[name]}"]`).checked = true;
}
document.querySelectorAll('.study-controls input').forEach(input => {
  input.addEventListener('change', () => {
    root.dataset[input.name] = input.value;
    const url = new URL(location.href);
    url.searchParams.set('layout', root.dataset.layout);
    url.searchParams.set('view', root.dataset.view);
    history.replaceState(null, '', url);
    document.getElementById('study-status').textContent = `${root.dataset.layout === 'wide' ? 'Wider demos' : 'Current layout'}, ${root.dataset.view === 'designed' ? 'designed data' : 'terminal output'}.`;
  });
});
