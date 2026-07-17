const logEl = document.getElementById('log');
const trackpad = document.getElementById('trackpad');
const modeSelect = document.getElementById('mode');
const buttonsEl = document.getElementById('buttons');

// Display the embedded version in the status header.
const versionEl = document.getElementById('version');
if (versionEl && window.APP_VERSION) {
  versionEl.textContent = 'v' + window.APP_VERSION;
}

let mode = modeSelect ? modeSelect.value : 'trackpad';
const activePointers = new Set();
const pressedButtons = new Set();

function updateButtonVisibility() {
  if (!buttonsEl) return;
  if (mode === 'tablet') {
    buttonsEl.classList.add('visible');
  } else {
    buttonsEl.classList.remove('visible');
  }
}

if (modeSelect) {
  modeSelect.addEventListener('change', () => {
    const oldMode = mode;
    activePointers.forEach(id => {
      sendPointerSample('pointerup', { pointerId: id, clientX: 0, clientY: 0 }, oldMode);
      releasePointer({ pointerId: id });
    });
    activePointers.clear();
    mode = modeSelect.value;
    updateButtonVisibility();
    log(`Switched to ${mode} mode`);
  });
}

function log(msg, cls = '') {
  console.log(msg);
  const line = document.createElement('div');
  line.className = 'line ' + cls;
  line.textContent = msg;
  logEl.appendChild(line);
  logEl.scrollTop = logEl.scrollHeight;
}

let currentId = 0;
let ws = null;
let pc = null;
let channel = null;
let reconnectTimer = null;
let reconnectDelay = 1000;
const maxReconnectDelay = 10000;

function scheduleReconnect(reason) {
  if (reconnectTimer) return;
  log(`${reason} — reconnecting in ${reconnectDelay}ms...`, 'err');
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connect();
  }, reconnectDelay);
  reconnectDelay = Math.min(reconnectDelay * 2, maxReconnectDelay);
}

function closeOldConnections() {
  if (ws) {
    try { ws.close(); } catch (e) { /* ignore */ }
    ws = null;
  }
  if (pc) {
    try { pc.close(); } catch (e) { /* ignore */ }
    pc = null;
    channel = null;
  }
  clearTimeout(reconnectTimer);
  reconnectTimer = null;
}

function connect() {
  const myId = ++currentId;
  closeOldConnections();
  reconnectDelay = 1000;

  log('Connecting...');

  ws = new WebSocket(`wss://${location.host}/signal`);

  ws.onopen = () => {
    if (myId !== currentId) return;
    log('WebSocket connected', 'ok');
    startPeerConnection(myId);
  };

  ws.onclose = () => {
    if (myId !== currentId) return;
    scheduleReconnect('WebSocket closed');
  };

  ws.onerror = (e) => {
    if (myId !== currentId) return;
    log('WebSocket error', 'err');
    console.error(e);
  };

  ws.onmessage = async (event) => {
    if (myId !== currentId || !pc) return;

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
        await pc.setRemoteDescription(new RTCSessionDescription({ type: 'answer', sdp: msg.sdp }));
      } else if (msg.type === 'candidate') {
        await pc.addIceCandidate(new RTCIceCandidate(msg));
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
  pc = new RTCPeerConnection(config);

  channel = pc.createDataChannel('touch', { ordered: true });

  channel.onopen = () => {
    if (myId !== currentId) return;
    log('Data channel open', 'ok');
  };

  channel.onclose = () => {
    if (myId !== currentId) return;
    scheduleReconnect('Data channel closed');
  };

  channel.onerror = (e) => {
    if (myId !== currentId) return;
    log('Data channel error', 'err');
    console.error(e);
  };

  pc.onicecandidate = (e) => {
    if (myId !== currentId) return;
    if (e.candidate && ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'candidate', ...e.candidate.toJSON() }));
    }
  };

  pc.onconnectionstatechange = () => {
    if (myId !== currentId) return;
    log('Peer connection state: ' + pc.connectionState);
    if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
      scheduleReconnect('Peer connection ' + pc.connectionState);
    }
  };

  pc.onnegotiationneeded = async () => {
    if (myId !== currentId) return;
    try {
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'offer', sdp: offer.sdp }));
        log('Sent offer', 'ok');
      }
    } catch (err) {
      log('Create offer error: ' + err.message, 'err');
      console.error(err);
    }
  };
}

function sendPointerSample(type, e, sampleMode = mode) {
  if (!channel || channel.readyState !== 'open') return;

  const rect = trackpad.getBoundingClientRect();
  const payload = {
    device: sampleMode,
    type,
    t: [{
      id: e.pointerId,
      x: (e.clientX - rect.left) / rect.width,
      y: (e.clientY - rect.top) / rect.height
    }]
  };
  channel.send(JSON.stringify(payload));
}

function sendButtonEvent(type, button) {
  if (!channel || channel.readyState !== 'open') return;
  const payload = {
    device: 'tablet',
    type,
    button
  };
  channel.send(JSON.stringify(payload));
}

function releasePointer(e) {
  if (trackpad.hasPointerCapture && trackpad.hasPointerCapture(e.pointerId)) {
    trackpad.releasePointerCapture(e.pointerId);
  }
}

trackpad.addEventListener('pointerdown', (e) => {
  e.preventDefault();
  activePointers.add(e.pointerId);
  if (trackpad.setPointerCapture) {
    trackpad.setPointerCapture(e.pointerId);
  }
  sendPointerSample('pointerdown', e);
}, { passive: false });

trackpad.addEventListener('pointermove', (e) => {
  e.preventDefault();
  // Use coalesced events only to get the latest sub-frame sample, but send
  // one message per pointermove frame. This avoids flooding the data channel
  // and keeps pointer motion consistent.
  const events = e.getCoalescedEvents ? e.getCoalescedEvents() : [];
  const last = events.length > 0 ? events[events.length - 1] : e;
  sendPointerSample('pointermove', last);
}, { passive: false });

trackpad.addEventListener('pointerup', (e) => {
  e.preventDefault();
  activePointers.delete(e.pointerId);
  releasePointer(e);
  sendPointerSample('pointerup', e);
});

trackpad.addEventListener('pointercancel', (e) => {
  e.preventDefault();
  activePointers.delete(e.pointerId);
  releasePointer(e);
  log('pointercancel', 'err');
  sendPointerSample('pointercancel', e);
});

trackpad.addEventListener('contextmenu', (e) => {
  e.preventDefault();
});

// Prevent the browser's default touch gestures (long-press menu, text
// selection, vibration, etc.) from cancelling our pointer stream.
trackpad.addEventListener('touchstart', (e) => {
  e.preventDefault();
}, { passive: false });

trackpad.addEventListener('touchmove', (e) => {
  e.preventDefault();
}, { passive: false });

// Wire up the L/M/R click buttons.
if (buttonsEl) {
  buttonsEl.querySelectorAll('.click-btn').forEach(btn => {
    const buttonName = btn.dataset.button;

    btn.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      if (!pressedButtons.has(buttonName)) {
        pressedButtons.add(buttonName);
        sendButtonEvent('buttondown', buttonName);
      }
    }, { passive: false });

    const release = (e) => {
      e.preventDefault();
      if (pressedButtons.has(buttonName)) {
        pressedButtons.delete(buttonName);
        sendButtonEvent('buttonup', buttonName);
      }
    };

    btn.addEventListener('pointerup', release, { passive: false });
    btn.addEventListener('pointerleave', release, { passive: false });
    btn.addEventListener('pointercancel', release, { passive: false });
  });
}

updateButtonVisibility();
connect();
