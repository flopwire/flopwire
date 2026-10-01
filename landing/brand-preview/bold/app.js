(() => {
  'use strict';
  const $ = (selector) => document.querySelector(selector);
  const stage = $('.exchange-stage');
  const path = $('#message-path');
  const label = $('#route-label');
  const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)');

  function drawWires() {
    const box = stage.getBoundingClientRect();
    const a = $('.sender').getBoundingClientRect();
    const b = $('.receiver').getBoundingClientRect();
    const x1 = a.left - box.left + a.width * .57;
    const y1 = a.bottom - box.top;
    const x2 = b.left - box.left + b.width * .67;
    const y2 = b.top - box.top;
    const mid = (y1 + y2) / 2;
    const r = Math.min(17, (x2 - x1) / 3);
    const d = `M${x1},${y1} V${mid-r} Q${x1},${mid} ${x1+r},${mid} H${x2-r} Q${x2},${mid} ${x2},${mid+r} V${y2}`;
    path.setAttribute('d', d);
    label.style.left = `${(x1+x2)/2}px`;
    label.style.top = `${mid}px`;
    label.style.transform = 'translate(-50%,-50%)';
  }

  new ResizeObserver(drawWires).observe(stage);
  document.fonts.ready.then(drawWires);

  // Search highlighting plays once on entry; the hero is static.
  const demoObserver = new IntersectionObserver(entries => {
    entries.forEach(entry => {
      if (!entry.isIntersecting) return;
      demoObserver.unobserve(entry.target);
      if (reduceMotion.matches) return;
      entry.target.classList.add('is-highlighted');
    });
  }, {threshold:0.45});
  demoObserver.observe($('.grep-terminal'));

  document.querySelectorAll('[data-copy]').forEach(button=>button.addEventListener('click',async()=>{
    const value=document.getElementById(button.dataset.copy).textContent;
    try{
      await navigator.clipboard.writeText(value);
      button.querySelector('span').textContent='Copied';$('#copy-status').textContent='Setup commands copied.';
      setTimeout(()=>{button.querySelector('span').textContent='Copy';},2000);
    }catch{
      const selection=window.getSelection();const range=document.createRange();range.selectNodeContents(document.getElementById(button.dataset.copy));selection.removeAllRanges();selection.addRange(range);
      button.querySelector('span').textContent='Select & copy';$('#copy-status').textContent='Automatic copy is unavailable. Commands selected; use your copy shortcut.';
    }
  }));
})();
