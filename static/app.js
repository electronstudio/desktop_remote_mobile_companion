const statusEl = document.getElementById('status');
const trackpad = document.getElementById('trackpad');

function log(msg, cls = '') {
  console.log(msg);
  const line = document.createElement('div');
  line.className = 'line ' + cls;
  line.textContent = msg;
  statusEl.appendChild(line);
  statusEl.scrollTop = statusEl.scrollHeight;
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

  channel = pc.createDataChannel('touch', { ordered: false, maxRetransmits: 0 });

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

function sendEvent(type, touchList) {
  if (!channel || channel.readyState !== 'open') return;

  const rect = trackpad.getBoundingClientRect();
  const touches = [];
  for (let i = 0; i < touchList.length; i++) {
    const t = touchList[i];
    touches.push({
      id: t.identifier,
      x: (t.clientX - rect.left) / rect.width,
      y: (t.clientY - rect.top) / rect.height
    });
  }
  const payload = { type, t: touches };
  channel.send(JSON.stringify(payload));
  console.log('sent', payload);
}

['touchstart', 'touchmove', 'touchend', 'touchcancel'].forEach((eventName) => {
  trackpad.addEventListener(eventName, (e) => {
    e.preventDefault();
    sendEvent(eventName, e.changedTouches);
  }, { passive: false });
});

connect();
