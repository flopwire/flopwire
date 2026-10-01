(() => {
  'use strict';
  const $ = (selector) => document.querySelector(selector);
  const exchange = $('#exchange');
  const stage = $('.exchange-stage');
  const path = $('#message-path');
  const label = $('#route-label');
  const packet = $('#packet');
  const replay = $('#replay');
  const pause = $('#pause');
  const status = $('#demo-status');
  const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)');
  let frame = 0;
  let elapsed = 0;
  let lastTick = 0;
  let running = false;
  let phase = 'ready';

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

  function setPhase(next) {
    if (phase === next) return;
    phase = next;
    exchange.dataset.phase = next;
    status.textContent = {ready:'Context retained',sending:'Sending update',delivered:'Update incorporated',saved:'Context retained'}[next];
  }

  function tick(now) {
    if (!running) return;
    if (lastTick) elapsed += now-lastTick;
    lastTick = now;
    if (elapsed < 1500) {
      setPhase('sending');
      const point = path.getPointAtLength(path.getTotalLength()*Math.min(1,elapsed/1500));
      packet.setAttribute('cx', point.x); packet.setAttribute('cy', point.y); packet.style.opacity='1';
    } else if (elapsed < 2650) {
      packet.style.opacity='0'; setPhase('delivered');
    } else {
      finish(); return;
    }
    frame = requestAnimationFrame(tick);
  }

  function finish() {
    running=false;cancelAnimationFrame(frame);packet.style.opacity='0';setPhase('saved');
    replay.disabled=false;replay.querySelector('span').textContent='Replay exchange';pause.hidden=true;
  }

  replay.addEventListener('click', () => {
    drawWires();cancelAnimationFrame(frame);elapsed=0;lastTick=0;phase='ready';
    if(reduceMotion.matches){finish();return;}
    running=true;replay.disabled=true;replay.querySelector('span').textContent='Playing exchange';pause.hidden=false;pause.textContent='Pause';frame=requestAnimationFrame(tick);
  });
  pause.addEventListener('click', () => {
    running=!running;
    pause.textContent=running?'Pause':'Resume';
    if(running){lastTick=0;frame=requestAnimationFrame(tick);}else cancelAnimationFrame(frame);
  });
  reduceMotion.addEventListener('change', () => { if(reduceMotion.matches)finish(); });
  document.addEventListener('visibilitychange', () => {if(document.hidden && running){running=false;cancelAnimationFrame(frame);pause.textContent='Resume';}});
  new ResizeObserver(drawWires).observe(stage);
  document.fonts.ready.then(drawWires);

  const reason = $('#commit-reason');
  const reasonButton = $('#show-reason');
  function showReason(open) {
    reason.hidden = !open;
    reasonButton.setAttribute('aria-expanded', String(open));
    reasonButton.innerHTML = `${open ? 'Hide the discussion' : 'Read the discussion behind this commit'} <span aria-hidden="true">${open ? '−' : '↗'}</span>`;
  }
  reasonButton.addEventListener('click', () => showReason(reason.hidden));
  $('.source-link').addEventListener('click', (event) => {
    event.preventDefault();showReason(true);
    reason.setAttribute('tabindex','-1');reason.focus({preventScroll:true});
    reason.scrollIntoView({behavior:reduceMotion.matches?'instant':'smooth',block:'center'});
  });
  const matchButton = $('.read-match');
  matchButton.addEventListener('click', () => {
    const context = $('#grep-context');context.hidden = !context.hidden;
    matchButton.setAttribute('aria-expanded', String(!context.hidden));
  });
  // Once per demo, on entry. All content stays readable with motion disabled.
  const demoObserver = new IntersectionObserver(entries => {
    entries.forEach(entry => {
      if (!entry.isIntersecting) return;
      demoObserver.unobserve(entry.target);
      if (reduceMotion.matches) return;
      if (entry.target === exchange && !running) replay.click();
      else entry.target.classList.add('is-highlighted');
    });
  }, {threshold:0.45});
  demoObserver.observe(exchange);demoObserver.observe($('.grep-terminal'));

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
