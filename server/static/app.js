(function () {
  'use strict';

  const PANEL_ORDER = ['trackpad', 'tablet', 'log'];
  const SNAP_THRESHOLD = 0.25; // 25% of the area width
  const FLING_VELOCITY = 0.5;  // px/ms in the correct direction
  const RESISTANCE = 0.3;      // over-scroll damping

  const conn = {
    currentId: 0,
    ws: null,
    pc: null,
    channel: null,
    videoTrack: null,
    reconnectTimer: null,
    reconnectDelay: 1000,
    maxReconnectDelay: 10000
  };

  function log(msg, cls = '') {
    console.log(msg);
    document.querySelectorAll('.log-content').forEach(el => {
      const line = document.createElement('div');
      line.className = 'line ' + cls;
      line.textContent = msg;
      el.appendChild(line);
      el.scrollTop = el.scrollHeight;
    });
  }

  document.querySelectorAll('.log-version').forEach(el => {
    if (window.APP_VERSION) {
      el.textContent = 'v' + window.APP_VERSION;
    }
  });

  function sendPointerSample(type, e, device, surface) {
    if (!conn.channel || conn.channel.readyState !== 'open') return;
    // Send raw panel-relative CSS-pixel coordinates plus the panel size and
    // let the server do any normalisation. Coordinates are NOT rounded:
    // clientX/rect are fractional on HiDPI screens, and sub-pixel precision
    // matters for the tablet's absolute positioning. Pointer capture means a
    // drag outside the panel legitimately reports x/y outside [0,w]/[0,h];
    // we send those as-is and the server clamps.
    const rect = surface.getBoundingClientRect();
    const sample = {
      id: e.pointerId,
      x: e.clientX - rect.left,
      y: e.clientY - rect.top
    };
    // Pen/stylus attributes are only relevant for the tablet. The browser
    // exposes pressure (0..1), tiltX/tiltY (degrees, -90..90) on PointerEvent;
    // for a touch pointer pressure is a synthetic 0.5 and tilt is 0. We forward
    // whatever the browser reports so a real pressure-sensitive stylus drives
    // the virtual tablet; the server treats absent values as defaults.
    if (device === 'tablet') {
      if (e.pressure !== undefined) sample.p = e.pressure;
      if (e.tiltX !== undefined) sample.tx = e.tiltX;
      if (e.tiltY !== undefined) sample.ty = e.tiltY;
    }
    const payload = { device, type, w: rect.width, h: rect.height, t: [sample] };
    try {
      conn.channel.send(JSON.stringify(payload));
    } catch (err) {
      console.error('send failed', err);
    }
  }

  // Tell the server whether the tablet panel is the active panel in any area.
  // When it is not, the server releases the virtual pen (proximity-out) so the
  // system mouse works again; when it is, the pen stays in proximity and the
  // keep-alive runs so rapid strokes work. This avoids the perpetual mouse
  // grab that keeping the tool in proximity would otherwise cause when the
  // user is not drawing.
  function notifyTabletActive() {
    if (!conn.channel || conn.channel.readyState !== 'open') return;
    let active = false;
    document.querySelectorAll('.area').forEach(areaEl => {
      const obj = areaEl.__areaObj;
      const idx = obj ? obj.state.currentIndex : null;
      if (idx != null && PANEL_ORDER[idx] === 'tablet') active = true;
    });
    const payload = { device: 'tablet', type: 'activate', active };
    try {
      conn.channel.send(JSON.stringify(payload));
    } catch (err) {
      console.error('send activate failed', err);
    }
  }

  function panelSurface(panel, panelId) {
    if (panelId === 'tablet') {
      return panel.querySelector('.tablet-surface');
    }
    return panel;
  }

  function releaseAllPointers(areaObj) {
    const active = areaObj.state.activePointers;
    if (active.size === 0) return;
    for (const [pid, info] of active) {
      if (info.surface.hasPointerCapture && info.surface.hasPointerCapture(pid)) {
        try { info.surface.releasePointerCapture(pid); } catch (e) { /* ignore */ }
      }
      const rect = info.surface.getBoundingClientRect();
      sendPointerSample('pointerup', {
        clientX: rect.left,
        clientY: rect.top,
        pointerId: pid
      }, info.device, info.surface);
    }
    active.clear();
  }

  function getIndices(idx) {
    return {
      current: idx,
      next: (idx + 1) % PANEL_ORDER.length,
      prev: (idx - 1 + PANEL_ORDER.length) % PANEL_ORDER.length
    };
  }

  function arrangePanels(areaObj, idx, immediate = false) {
    const c = areaObj.carousel;
    const p = getIndices(idx);
    const panels = areaObj.panels;

    c.classList.add('no-transition');
    panels[PANEL_ORDER[p.current]].style.order = 0;
    panels[PANEL_ORDER[p.next]].style.order = 1;
    panels[PANEL_ORDER[p.prev]].style.order = 2;
    c.style.transform = 'translateX(0%)';
    void c.offsetWidth;
    if (!immediate) {
      c.classList.remove('no-transition');
    }
  }

  function preparePan(areaObj, edge) {
    const c = areaObj.carousel;
    const p = getIndices(areaObj.state.currentIndex);
    const panels = areaObj.panels;

    c.classList.add('no-transition');
    if (edge === 'right') {
      panels[PANEL_ORDER[p.current]].style.order = 0;
      panels[PANEL_ORDER[p.next]].style.order = 1;
      panels[PANEL_ORDER[p.prev]].style.order = 2;
      c.style.transform = 'translateX(0%)';
    } else {
      panels[PANEL_ORDER[p.prev]].style.order = 0;
      panels[PANEL_ORDER[p.current]].style.order = 1;
      panels[PANEL_ORDER[p.next]].style.order = 2;
      c.style.transform = 'translateX(-33.333%)';
    }
    void c.offsetWidth;
  }

  function setPanTransform(areaObj, dx) {
    const width = areaObj.el.clientWidth || 1;
    const rawPercent = (dx / width) * 100;
    const base = -33.333;
    let target;

    if (areaObj.state.pan.edge === 'right') {
      target = rawPercent; // negative for right-to-left drags
      if (target > 0) {
        target = target * RESISTANCE;
      } else if (target < base) {
        target = base + (target - base) * RESISTANCE;
      }
    } else {
      target = base + rawPercent; // positive for left-to-right drags
      if (target < base) {
        target = base + (target - base) * RESISTANCE;
      } else if (target > 0) {
        target = target * RESISTANCE;
      }
    }

    areaObj.carousel.classList.add('no-transition');
    areaObj.carousel.style.transform = `translateX(${target.toFixed(3)}%)`;
  }

  function finishSnap(areaObj) {
    const dir = areaObj.state.settling;
    if (!dir) return;
    if (dir === 'snap-next') {
      areaObj.state.currentIndex = (areaObj.state.currentIndex + 1) % PANEL_ORDER.length;
    } else if (dir === 'snap-prev') {
      areaObj.state.currentIndex = (areaObj.state.currentIndex - 1 + PANEL_ORDER.length) % PANEL_ORDER.length;
    }
    releaseAllPointers(areaObj);
    arrangePanels(areaObj, areaObj.state.currentIndex, false);
    areaObj.state.settling = false;
    notifyTabletActive();
  }

  function startPan(areaObj, e, edge, zone) {
    if (areaObj.state.settling) return;
    if (areaObj.state.pan.active) return;

    const pan = areaObj.state.pan;
    pan.active = true;
    pan.pointerId = e.pointerId;
    pan.edge = edge;
    pan.zone = zone;
    pan.startX = e.clientX;
    pan.lastX = e.clientX;
    pan.lastT = performance.now();
    pan.velocity = 0;

    preparePan(areaObj, edge);
    zone.setPointerCapture(e.pointerId);
  }

  function movePan(areaObj, e) {
    const pan = areaObj.state.pan;
    if (!pan.active || pan.pointerId !== e.pointerId) return;

    const now = performance.now();
    const dt = now - pan.lastT;
    if (dt > 0) {
      pan.velocity = (e.clientX - pan.lastX) / dt;
    }
    pan.lastX = e.clientX;
    pan.lastT = now;

    setPanTransform(areaObj, e.clientX - pan.startX);
  }

  function endPan(areaObj, e) {
    const pan = areaObj.state.pan;
    if (!pan.active || pan.pointerId !== e.pointerId) return;

    pan.active = false;
    if (pan.zone && pan.zone.hasPointerCapture && pan.zone.hasPointerCapture(pan.pointerId)) {
      try { pan.zone.releasePointerCapture(pan.pointerId); } catch (err) { /* ignore */ }
    }

    const width = areaObj.el.clientWidth || 1;
    const dx = e.clientX - pan.startX;
    const distanceRatio = Math.abs(dx) / width;
    const correctDir = (pan.edge === 'right' && dx < 0) || (pan.edge === 'left' && dx > 0);
    const velocityCorrect = (pan.edge === 'right' && pan.velocity < -FLING_VELOCITY) ||
                            (pan.edge === 'left' && pan.velocity > FLING_VELOCITY);
    const commit = correctDir && (distanceRatio > SNAP_THRESHOLD || velocityCorrect);

    const c = areaObj.carousel;
    c.classList.remove('no-transition');

    if (pan.edge === 'right') {
      if (commit) {
        areaObj.state.settling = 'snap-next';
        c.style.transform = 'translateX(-33.333%)';
      } else {
        areaObj.state.settling = 'snap-back-next';
        c.style.transform = 'translateX(0%)';
      }
    } else {
      if (commit) {
        areaObj.state.settling = 'snap-prev';
        c.style.transform = 'translateX(0%)';
      } else {
        areaObj.state.settling = 'snap-back-prev';
        c.style.transform = 'translateX(-33.333%)';
      }
    }
  }

  function attachSurfaceListeners(areaObj, panelId) {
    const surface = panelSurface(areaObj.panels[panelId], panelId);
    if (!surface) return;
    const device = panelId;

    surface.addEventListener('pointerdown', e => {
      e.preventDefault();
      if (areaObj.state.settling) return;
      areaObj.state.activePointers.set(e.pointerId, { surface, device });
      surface.setPointerCapture(e.pointerId);
      sendPointerSample('pointerdown', e, device, surface);
    }, { passive: false });

    surface.addEventListener('pointermove', e => {
      e.preventDefault();
      if (areaObj.state.settling) return;
      const info = areaObj.state.activePointers.get(e.pointerId);
      if (!info) return;
      // For the trackpad we send the latest coalesced sample for smoother
      // high-frequency cursor motion. For the tablet we send the main event
      // instead: coalesced PointerEvents from getCoalescedEvents() report
      // pressure/tilt as 0 on some platforms (the pen attributes are only
      // reliably present on the dispatched event e), which made the pen's
      // pressure collapse to 0 immediately after pointerdown and lifted the
      // tip mid-stroke. Position fidelity from the main event is fine for the
      // tablet's absolute coordinates.
      if (info.device === 'tablet') {
        sendPointerSample('pointermove', e, info.device, info.surface);
      } else {
        const events = e.getCoalescedEvents ? e.getCoalescedEvents() : [];
        const last = events.length > 0 ? events[events.length - 1] : e;
        sendPointerSample('pointermove', last, info.device, info.surface);
      }
    }, { passive: false });

    const endPointer = e => {
      e.preventDefault();
      const info = areaObj.state.activePointers.get(e.pointerId);
      if (!info) return;
      areaObj.state.activePointers.delete(e.pointerId);
      if (info.surface.hasPointerCapture && info.surface.hasPointerCapture(e.pointerId)) {
        try { info.surface.releasePointerCapture(e.pointerId); } catch (err) { /* ignore */ }
      }
      const type = e.type === 'pointercancel' ? 'pointercancel' : 'pointerup';
      sendPointerSample(type, e, info.device, info.surface);
    };

    surface.addEventListener('pointerup', endPointer, { passive: false });
    surface.addEventListener('pointercancel', endPointer, { passive: false });
    surface.addEventListener('contextmenu', e => e.preventDefault());

    // Prevent the browser from interpreting rapid touches as touch gestures
    // (double-tap zoom, long-press, etc.). On some browsers a fast second
    // touch is swallowed as a double-tap and never fires a pointerdown, so a
    // rapid double-touch (e.g. two quick pen taps) would lose the second
    // stroke entirely. Cancelling the underlying touch events stops that
    // gesture detection and lets every touch through as a pointer event.
    // (passive:false so preventDefault is honoured.)
    surface.addEventListener('touchstart', e => e.preventDefault(), { passive: false });
    surface.addEventListener('touchmove', e => e.preventDefault(), { passive: false });
    surface.addEventListener('touchend', e => e.preventDefault(), { passive: false });
    surface.addEventListener('touchcancel', e => e.preventDefault(), { passive: false });
  }

  function initArea(areaEl) {
    const initialIndex = areaEl.id === 'area-bottom' ? 2 : 0;
    const state = {
      currentIndex: initialIndex,
      settling: false,
      activePointers: new Map(),
      pan: {
        active: false,
        pointerId: null,
        edge: null,
        zone: null,
        startX: 0,
        lastX: 0,
        lastT: 0,
        velocity: 0
      }
    };

    const carousel = areaEl.querySelector('.carousel');
    const panels = {};
    areaEl.querySelectorAll('.panel[data-panel]').forEach(p => {
      panels[p.dataset.panel] = p;
    });

    const areaObj = { el: areaEl, state, carousel, panels };
    areaEl.__areaObj = areaObj;

    arrangePanels(areaObj, state.currentIndex, false);

    carousel.addEventListener('transitionend', () => finishSnap(areaObj));

    const leftEdge = areaEl.querySelector('.edge-zone.left');
    const rightEdge = areaEl.querySelector('.edge-zone.right');

    [leftEdge, rightEdge].forEach(zone => {
      const isLeft = zone.classList.contains('left');

      zone.addEventListener('pointerdown', e => {
        e.preventDefault();
        startPan(areaObj, e, isLeft ? 'left' : 'right', zone);
      }, { passive: false });

      zone.addEventListener('pointermove', e => {
        e.preventDefault();
        movePan(areaObj, e);
      }, { passive: false });

      const endSwipe = e => {
        e.preventDefault();
        endPan(areaObj, e);
      };

      zone.addEventListener('pointerup', endSwipe, { passive: false });
      zone.addEventListener('pointercancel', endSwipe, { passive: false });
      zone.addEventListener('contextmenu', e => e.preventDefault());
    });

    attachSurfaceListeners(areaObj, 'trackpad');
    attachSurfaceListeners(areaObj, 'tablet');

    areaEl.addEventListener('contextmenu', e => e.preventDefault());
  }

  function attachVideoToSurfaces() {
    const track = conn.videoTrack;
    // The desktop video lives directly in #area-top, behind the carousel, so
    // every panel renders on top of it (panels are transparent). Attach the
    // track as soon as it arrives, on page load — do not wait for the tablet
    // panel to be swiped in.
    const video = document.querySelector('#area-top > .tablet-video');
    if (!video) return;
    if (track) {
      if (video.srcObject === null || video.srcObject === undefined) {
        video.srcObject = new MediaStream([track]);
      }
    } else if (video.srcObject) {
      video.srcObject = null;
    }
  }

  function scheduleReconnect(reason) {
    if (conn.reconnectTimer) return;
    log(`${reason} — reconnecting in ${conn.reconnectDelay}ms...`, 'err');
    conn.reconnectTimer = setTimeout(() => {
      conn.reconnectTimer = null;
      connect();
    }, conn.reconnectDelay);
    conn.reconnectDelay = Math.min(conn.reconnectDelay * 2, conn.maxReconnectDelay);
  }

  function closeOldConnections() {
    if (conn.ws) {
      try { conn.ws.close(); } catch (e) { /* ignore */ }
      conn.ws = null;
    }
    if (conn.pc) {
      try { conn.pc.close(); } catch (e) { /* ignore */ }
      conn.pc = null;
      conn.channel = null;
    }
    conn.videoTrack = null;
    clearTimeout(conn.reconnectTimer);
    conn.reconnectTimer = null;
  }

  function connect() {
    const myId = ++conn.currentId;
    closeOldConnections();
    conn.reconnectDelay = 1000;

    log('Connecting...');

    conn.ws = new WebSocket(`wss://${location.host}/signal`);

    conn.ws.onopen = () => {
      if (myId !== conn.currentId) return;
      log('WebSocket connected', 'ok');
      startPeerConnection(myId);
    };

    conn.ws.onclose = () => {
      if (myId !== conn.currentId) return;
      scheduleReconnect('WebSocket closed');
    };

    conn.ws.onerror = e => {
      if (myId !== conn.currentId) return;
      log('WebSocket error', 'err');
      console.error(e);
    };

    conn.ws.onmessage = async event => {
      if (myId !== conn.currentId || !conn.pc) return;

      let msg;
      try {
        msg = JSON.parse(event.data);
      } catch (err) {
        log('Bad signal message: ' + event.data, 'err');
        return;
      }

      try {
        if (msg.type === 'answer') {
          log('Received answer', 'ok');
          await conn.pc.setRemoteDescription(new RTCSessionDescription({ type: 'answer', sdp: msg.sdp }));
        } else if (msg.type === 'candidate') {
          await conn.pc.addIceCandidate(new RTCIceCandidate(msg));
        }
      } catch (err) {
        log('Signal handling error: ' + err.message, 'err');
        console.error(err);
      }
    };
  }

  function startPeerConnection(myId) {
    const config = {
      iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
    };
    conn.pc = new RTCPeerConnection(config);

    conn.channel = conn.pc.createDataChannel('touch', { ordered: true });

    // Request a receive-only video track so the offer includes a video
    // m-line. The server adds its send-only H264 track when it answers.
    conn.pc.addTransceiver('video', { direction: 'recvonly' });

    conn.pc.ontrack = e => {
      if (myId !== conn.currentId) return;
      const track = e.track || (e.streams[0] && e.streams[0].getVideoTracks()[0]);
      if (!track || track.kind !== 'video') return;
      conn.videoTrack = track;
      log('Video track received', 'ok');
      // Hide the tablet placeholder once the first frame is rendered.
      const video = document.querySelector('#area-top > .tablet-video');
      if (video) {
        video.addEventListener('playing', function onPlaying() {
          video.removeEventListener('playing', onPlaying);
          const area = video.closest('.area');
          if (area) area.classList.add('has-video');
        });
      }
      attachVideoToSurfaces();
      track.onended = () => {
        if (myId !== conn.currentId) return;
        conn.videoTrack = null;
        document.querySelectorAll('.area.has-video').forEach(a => a.classList.remove('has-video'));
        const v = document.querySelector('#area-top > .tablet-video');
        if (v) v.srcObject = null;
      };
    };

    conn.channel.onopen = () => {
      if (myId !== conn.currentId) return;
      log('Data channel open', 'ok');
      notifyTabletActive();
    };

    conn.channel.onclose = () => {
      if (myId !== conn.currentId) return;
      scheduleReconnect('Data channel closed');
    };

    conn.channel.onerror = e => {
      if (myId !== conn.currentId) return;
      log('Data channel error', 'err');
      console.error(e);
    };

    conn.pc.onicecandidate = e => {
      if (myId !== conn.currentId) return;
      if (e.candidate && conn.ws && conn.ws.readyState === WebSocket.OPEN) {
        conn.ws.send(JSON.stringify({ type: 'candidate', ...e.candidate.toJSON() }));
      }
    };

    conn.pc.onconnectionstatechange = () => {
      if (myId !== conn.currentId) return;
      log('Peer connection state: ' + conn.pc.connectionState);
      if (conn.pc.connectionState === 'failed' || conn.pc.connectionState === 'closed') {
        scheduleReconnect('Peer connection ' + conn.pc.connectionState);
      }
    };

    conn.pc.onnegotiationneeded = async () => {
      if (myId !== conn.currentId) return;
      try {
        const offer = await conn.pc.createOffer();
        await conn.pc.setLocalDescription(offer);
        if (conn.ws && conn.ws.readyState === WebSocket.OPEN) {
          conn.ws.send(JSON.stringify({ type: 'offer', sdp: offer.sdp }));
          log('Sent offer', 'ok');
        }
      } catch (err) {
        log('Create offer error: ' + err.message, 'err');
        console.error(err);
      }
    };
  }

  document.querySelectorAll('.area').forEach(initArea);
  connect();

  // We no longer register a service worker. A SW intercepted top-level
  // navigation, which prevented iOS Safari from showing the "accept
  // self-signed certificate" interstitial when the server certificate
  // changed — leaving the phone unable to load the page or re-trust the
  // new cert. See static/sw.js for the full rationale.
  //
  // Remove any SW + caches left by older versions so they can never
  // intercept navigation again.
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.getRegistrations()
      .then(regs => regs.forEach(r => {
        r.unregister().then(ok => console.log('Unregistered stale service worker', r.scope, ok));
      }))
      .catch(err => console.error('Service worker cleanup failed', err));
    if (window.caches && typeof caches.keys === 'function') {
      caches.keys().then(keys => keys.forEach(k => caches.delete(k)));
    }
    navigator.serviceWorker.addEventListener('message', ev => {
      if (ev.data === 'reload') location.reload();
    });
  }

  const splitter = document.querySelector('.splitter');
  const topArea = document.getElementById('area-top');
  const bottomArea = document.getElementById('area-bottom');
  const MIN_AREA_HEIGHT_PCT = 0.15;
  let splitPointerId = null;
  let splitStartY = 0;
  let splitStartTopHeight = 0;
  let portraitTopHeight = null;
  let lastOrientation = null;

  function isLandscape() {
    if (typeof window.orientation === 'number') {
      return Math.abs(window.orientation) === 90;
    }
    if (window.screen && window.screen.orientation && window.screen.orientation.angle != null) {
      return Math.abs(window.screen.orientation.angle) === 90;
    }
    // Fallback when no orientation API is available (or reports a stale
    // value during the rotation transition): compare the actual viewport
    // dimensions. At resize-event time these are settled and reliable.
    return window.innerWidth > window.innerHeight;
  }

  function applyPortraitHeights(topPx) {
    const vh = window.innerHeight;
    const splitterHeight = splitter.getBoundingClientRect().height || 12;
    const minPx = vh * MIN_AREA_HEIGHT_PCT;
    const maxPx = vh - splitterHeight - minPx;
    const clampedTopPx = Math.max(minPx, Math.min(topPx, maxPx));
    const bottomPx = vh - splitterHeight - clampedTopPx;
    topArea.style.height = clampedTopPx + 'px';
    bottomArea.style.height = bottomPx + 'px';
    portraitTopHeight = clampedTopPx;
  }

  function setLandscapeLayout() {
    // Use innerHeight, not 100vh: on mobile browsers 100vh is the height
    // with the URL bar hidden (the "large viewport"), and rotation forces
    // the URL bar to reappear, so 100vh makes the area taller than the
    // visible screen and the video (object-fit: contain) is centered in
    // that oversize area, leaving a bigger black bar at the top.
    topArea.style.height = window.innerHeight + 'px';
    bottomArea.style.display = 'none';
    splitter.style.display = 'none';
  }

  function setPortraitLayout() {
    bottomArea.style.display = '';
    splitter.style.display = '';
    if (portraitTopHeight === null) {
      topArea.style.height = 'calc(50vh - 6px)';
      bottomArea.style.height = 'calc(50vh - 6px)';
    } else {
      applyPortraitHeights(portraitTopHeight);
    }
  }

  function updateLayout() {
    const landscape = isLandscape();
    const orientation = landscape ? 'landscape' : 'portrait';
    const changed = lastOrientation !== orientation;
    lastOrientation = orientation;
    if (landscape) {
      // Re-apply on every resize, not just on the orientation flip: the
      // browser URL bar can show/hide after rotation, changing innerHeight
      // without another orientation change.
      setLandscapeLayout();
    } else if (changed || portraitTopHeight !== null) {
      // Re-apply the portrait heights even when the orientation is
      // unchanged: a resize (rotation settling, URL bar show/hide) can
      // change innerHeight, and the inline pixel heights are computed
      // from it, so they must be recomputed against the new viewport.
      setPortraitLayout();
    }
  }

  function resizeAreas(newTopHeight) {
    if (isLandscape()) return;
    applyPortraitHeights(newTopHeight);
  }

  splitter.addEventListener('pointerdown', e => {
    e.preventDefault();
    if (isLandscape()) return;
    if (splitPointerId !== null) return;
    splitPointerId = e.pointerId;
    splitStartY = e.clientY;
    splitStartTopHeight = topArea.getBoundingClientRect().height;
    splitter.setPointerCapture(e.pointerId);
  }, { passive: false });

  splitter.addEventListener('pointermove', e => {
    e.preventDefault();
    if (isLandscape()) return;
    if (splitPointerId !== e.pointerId) return;
    applyPortraitHeights(splitStartTopHeight + (e.clientY - splitStartY));
  }, { passive: false });

  const endSplit = e => {
    e.preventDefault();
    if (splitPointerId !== e.pointerId) return;
    if (splitter.hasPointerCapture && splitter.hasPointerCapture(e.pointerId)) {
      try { splitter.releasePointerCapture(e.pointerId); } catch (err) { /* ignore */ }
    }
    splitPointerId = null;
  };

  splitter.addEventListener('pointerup', endSplit, { passive: false });
  splitter.addEventListener('pointercancel', endSplit, { passive: false });
  splitter.addEventListener('contextmenu', e => e.preventDefault());

  window.addEventListener('orientationchange', updateLayout);
  if (window.screen && window.screen.orientation && window.screen.orientation.addEventListener) {
    window.screen.orientation.addEventListener('change', updateLayout);
  }
  // resize always fires after a rotation (even when orientationchange does
  // not, or fires before the viewport dimensions have settled), so it is
  // the reliable trigger for re-laying out the areas.
  window.addEventListener('resize', updateLayout);

  updateLayout();
})();
