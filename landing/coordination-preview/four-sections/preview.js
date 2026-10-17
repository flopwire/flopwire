// A source link should reveal the passage it points to, including on deep links.
function revealSource() {
  const id = window.location.hash.slice(1);
  const target = id && document.getElementById(id);
  if (target instanceof HTMLDetailsElement) target.open = true;
}
window.addEventListener('hashchange', revealSource);
document.querySelector('a[href="#search-source"]').addEventListener('click', () => {
  document.getElementById('search-source').open = true;
});
revealSource();
