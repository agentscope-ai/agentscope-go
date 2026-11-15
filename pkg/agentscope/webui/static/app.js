// AgentScope Studio - Web UI for agentscope-go
// Vanilla JS SPA - no build step required.

'use strict';

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------
const state = {
  sessions: [],           // [{session_id, agent_name, created_at}]
  activeSession: null,    // session_id
  streaming: false,       // whether SSE is active
  eventSource: null,      // current EventSource
  pendingConfirm: null,   // {session_id, tool_calls:[{id, name}]}
};

// ---------------------------------------------------------------------------
// API helpers
// ---------------------------------------------------------------------------
const API = {
  async post(path, body) {
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({ error: res.statusText }));
      throw new Error(err.error || res.statusText);
    }
    return res.json();
  },

  async get(path) {
    const res = await fetch(path);
    if (!res.ok) {
      const err = await res.json().catch(() => ({ error: res.statusText }));
      throw new Error(err.error || res.statusText);
    }
    return res.json();
  },

  async del(path) {
    const res = await fetch(path, { method: 'DELETE' });
    if (!res.ok && res.status !== 204) {
      const err = await res.json().catch(() => ({ error: res.statusText }));
      throw new Error(err.error || res.statusText);
    }
  },
};

// ---------------------------------------------------------------------------
// Initialization
// ---------------------------------------------------------------------------
document.addEventListener('DOMContentLoaded', async () => {
  await loadSessions();
  checkHealth();
  setInterval(checkHealth, 30000);

  document.getElementById('btn-new-session').addEventListener('click', showNewSessionDialog);
  document.getElementById('btn-models').addEventListener('click', showModelsDialog);
});

async function checkHealth() {
  const dot = document.getElementById('connection-status');
  try {
    await fetch('/healthz');
    dot.className = 'status-dot status-ok';
  } catch {
    dot.className = 'status-dot status-err';
  }
}

// ---------------------------------------------------------------------------
// Session management
// ---------------------------------------------------------------------------
async function loadSessions() {
  try {
    state.sessions = await API.get('/api/sessions');
  } catch {
    state.sessions = [];
  }
  renderSessionList();
}

function renderSessionList() {
  const list = document.getElementById('session-list');
  if (!state.sessions.length) {
    list.innerHTML = '<div class="empty-state">No sessions yet</div>';
    return;
  }

  // Sort by created_at descending
  const sorted = [...state.sessions].sort((a, b) =>
    new Date(b.created_at) - new Date(a.created_at)
  );

  list.innerHTML = sorted.map(s => {
    const isActive = s.session_id === state.activeSession;
    const name = s.agent_name || s.session_id;
    return `
      <div class="session-item ${isActive ? 'active' : ''}"
           onclick="switchSession('${s.session_id}')"
           data-sid="${s.session_id}">
        <span class="session-item-name">${escapeHtml(name)}</span>
        <span class="session-item-delete"
              onclick="event.stopPropagation(); deleteSession('${s.session_id}')"
              title="Delete session">&times;</span>
      </div>
    `;
  }).join('');
}

function showNewSessionDialog() {
  document.getElementById('dialog-overlay').classList.remove('hidden');
  document.getElementById('input-agent-name').focus();
}

function hideDialog() {
  document.getElementById('dialog-overlay').classList.add('hidden');
}

function closeDialog(e) {
  if (e.target === e.currentTarget) hideDialog();
}

async function createSession() {
  const name = document.getElementById('input-agent-name').value.trim() || 'assistant';
  const prompt = document.getElementById('input-system-prompt').value.trim() || 'You are a helpful assistant.';

  try {
    const session = await API.post('/api/session', {
      agent_name: name,
      system_prompt: prompt,
    });
    hideDialog();
    state.sessions.push(session);
    switchSession(session.session_id);
    renderSessionList();
  } catch (err) {
    alert('Failed to create session: ' + err.message);
  }
}

async function switchSession(sessionId) {
  // Close any active stream
  closeStream();

  state.activeSession = sessionId;
  renderSessionList();

  // Show chat view
  document.getElementById('welcome-screen').classList.add('hidden');
  const chatView = document.getElementById('chat-view');
  chatView.classList.remove('hidden');

  const session = state.sessions.find(s => s.session_id === sessionId);
  document.getElementById('chat-session-name').textContent = session?.agent_name || 'Agent';
  document.getElementById('chat-session-id').textContent = sessionId;

  // Clear messages (sessions are server-side, we don't persist chat history in UI yet)
  const messagesEl = document.getElementById('chat-messages');
  if (!messagesEl.dataset[sessionId]) {
    messagesEl.innerHTML = '';
    messagesEl.dataset[sessionId] = '1';
  }

  document.getElementById('chat-input').focus();
}

async function deleteSession(sessionId) {
  try {
    await API.del('/api/session/' + sessionId);
    state.sessions = state.sessions.filter(s => s.session_id !== sessionId);

    if (state.activeSession === sessionId) {
      closeStream();
      state.activeSession = null;
      document.getElementById('chat-view').classList.add('hidden');
      document.getElementById('welcome-screen').classList.remove('hidden');
    }

    renderSessionList();
  } catch (err) {
    alert('Failed to delete session: ' + err.message);
  }
}

function deleteCurrentSession() {
  if (state.activeSession) {
    deleteSession(state.activeSession);
  }
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------
function handleInputKeydown(e) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    sendMessage();
  }
}

function autoResize(el) {
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 200) + 'px';
}

async function sendMessage() {
  const input = document.getElementById('chat-input');
  const msg = input.value.trim();
  if (!msg || !state.activeSession || state.streaming) return;

  input.value = '';
  input.style.height = 'auto';

  // Append user message
  appendMessage('user', 'You', msg);

  // Start streaming
  startStream(state.activeSession, msg);
}

function appendMessage(role, name, text) {
  const container = document.getElementById('chat-messages');
  const div = document.createElement('div');
  div.className = 'message';

  const avatarClass = role === 'user' ? 'avatar-user' : 'avatar-assistant';
  const avatarText = role === 'user' ? 'U' : 'A';
  const time = new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

  div.innerHTML = `
    <div class="message-header">
      <div class="message-avatar ${avatarClass}">${avatarText}</div>
      <span class="message-name">${escapeHtml(name)}</span>
      <span class="message-time">${time}</span>
    </div>
    <div class="message-body">${renderMarkdown(text)}</div>
  `;

  container.appendChild(div);
  scrollToBottom();
}

// ---------------------------------------------------------------------------
// SSE Streaming
// ---------------------------------------------------------------------------
function startStream(sessionId, message) {
  state.streaming = true;
  document.getElementById('btn-stop').classList.remove('hidden');
  document.getElementById('btn-send').disabled = true;

  const container = document.getElementById('chat-messages');

  // Create assistant message container
  const msgDiv = document.createElement('div');
  msgDiv.className = 'message';
  msgDiv.id = 'streaming-msg';

  const time = new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  msgDiv.innerHTML = `
    <div class="message-header">
      <div class="message-avatar avatar-assistant">A</div>
      <span class="message-name">Assistant</span>
      <span class="message-time">${time}</span>
    </div>
    <div class="message-body" id="streaming-body">
      <div class="typing-indicator"><span></span><span></span><span></span></div>
    </div>
  `;
  container.appendChild(msgDiv);
  scrollToBottom();

  // Tracking state for the current stream
  const streamState = {
    textContent: '',
    thinkingContent: '',
    currentBlock: null,       // 'text' | 'thinking' | null
    toolCalls: {},            // id -> {name, args, resultText, status}
    currentToolCallId: null,
    hasContent: false,
  };

  const url = `/api/chat/stream?session_id=${encodeURIComponent(sessionId)}&message=${encodeURIComponent(message)}`;
  const es = new EventSource(url);
  state.eventSource = es;

  // Generic message handler — the service sends events as event: <type>\ndata: <json>
  es.onmessage = (e) => {
    handleStreamEvent('message', e.data, streamState);
  };

  // Named event handlers for each event type
  const eventTypes = [
    'reply_start', 'reply_end',
    'model_call_start', 'model_call_end',
    'text_block_start', 'text_block_delta', 'text_block_end',
    'thinking_block_start', 'thinking_block_delta', 'thinking_block_end',
    'tool_call_start', 'tool_call_delta', 'tool_call_end',
    'tool_result_start', 'tool_result_text_delta', 'tool_result_data_delta', 'tool_result_end',
    'hint_block',
    'require_user_confirm',
    'exceed_max_iters',
    'custom',
  ];

  for (const type of eventTypes) {
    es.addEventListener(type, (e) => {
      handleStreamEvent(type, e.data, streamState);
    });
  }

  es.onerror = () => {
    endStream(streamState);
  };
}

function handleStreamEvent(type, rawData, ss) {
  let data;
  try {
    data = JSON.parse(rawData);
  } catch {
    return;
  }

  const body = document.getElementById('streaming-body');
  if (!body) return;

  switch (type) {
    case 'reply_start':
      // Clear typing indicator
      if (!ss.hasContent) {
        body.innerHTML = '';
        ss.hasContent = true;
      }
      break;

    case 'text_block_start':
      ss.currentBlock = 'text';
      if (!ss.hasContent) {
        body.innerHTML = '';
        ss.hasContent = true;
      }
      break;

    case 'text_block_delta':
      ss.textContent += data.delta || '';
      updateTextContent(body, ss);
      scrollToBottom();
      break;

    case 'text_block_end':
      ss.currentBlock = null;
      // Remove streaming cursor
      const textEl = body.querySelector('.text-stream');
      if (textEl) textEl.classList.remove('streaming-cursor');
      break;

    case 'thinking_block_start':
      ss.currentBlock = 'thinking';
      ss.thinkingContent = '';
      if (!ss.hasContent) {
        body.innerHTML = '';
        ss.hasContent = true;
      }
      // Add thinking block
      const thinkDiv = document.createElement('div');
      thinkDiv.className = 'thinking-block';
      thinkDiv.id = 'thinking-' + (data.block_id || 'current');
      thinkDiv.innerHTML = `
        <div class="thinking-header" onclick="toggleCollapse(this)">
          <span class="toggle-icon">&#9660;</span>
          Thinking...
        </div>
        <div class="thinking-content streaming-cursor"></div>
      `;
      body.appendChild(thinkDiv);
      scrollToBottom();
      break;

    case 'thinking_block_delta':
      ss.thinkingContent += data.delta || '';
      const thinkBlock = body.querySelector('.thinking-block:last-of-type .thinking-content');
      if (thinkBlock) {
        thinkBlock.textContent = ss.thinkingContent;
        scrollToBottom();
      }
      break;

    case 'thinking_block_end':
      ss.currentBlock = null;
      const thinkEnd = body.querySelector('.thinking-block:last-of-type');
      if (thinkEnd) {
        const header = thinkEnd.querySelector('.thinking-header');
        if (header) header.innerHTML = '<span class="toggle-icon">&#9660;</span> Thought';
        const content = thinkEnd.querySelector('.thinking-content');
        if (content) content.classList.remove('streaming-cursor');
      }
      break;

    case 'tool_call_start':
      if (!ss.hasContent) {
        body.innerHTML = '';
        ss.hasContent = true;
      }
      const tcId = data.tool_call_id || data.id;
      ss.currentToolCallId = tcId;
      ss.toolCalls[tcId] = {
        name: data.tool_call_name || 'tool',
        args: '',
        resultText: '',
        status: 'running',
      };
      const toolDiv = document.createElement('div');
      toolDiv.className = 'tool-call-block';
      toolDiv.id = 'tool-' + tcId;
      toolDiv.innerHTML = `
        <div class="tool-call-header" onclick="toggleCollapse(this)">
          <span class="toggle-icon">&#9660;</span>
          <span class="tool-call-name">${escapeHtml(data.tool_call_name || 'tool')}</span>
          <span class="tool-call-status running">running</span>
        </div>
        <div class="tool-call-args streaming-cursor"></div>
      `;
      body.appendChild(toolDiv);
      scrollToBottom();
      break;

    case 'tool_call_delta':
      const deltaId = data.tool_call_id || ss.currentToolCallId;
      if (deltaId && ss.toolCalls[deltaId]) {
        ss.toolCalls[deltaId].args += data.delta || '';
        const argsEl = document.querySelector('#tool-' + CSS.escape(deltaId) + ' .tool-call-args');
        if (argsEl) {
          argsEl.textContent = tryFormatJSON(ss.toolCalls[deltaId].args);
          scrollToBottom();
        }
      }
      break;

    case 'tool_call_end': {
      const endId = data.tool_call_id || ss.currentToolCallId;
      const toolEl = document.querySelector('#tool-' + CSS.escape(endId) + ' .tool-call-args');
      if (toolEl) {
        toolEl.classList.remove('streaming-cursor');
        toolEl.textContent = tryFormatJSON(toolEl.textContent);
      }
      break;
    }

    case 'tool_result_start': {
      const trId = data.tool_call_id;
      const toolName = data.tool_call_name || '';
      const resultDiv = document.createElement('div');
      resultDiv.className = 'tool-result-block';
      resultDiv.id = 'result-' + trId;
      resultDiv.innerHTML = `
        <div class="tool-result-header" onclick="toggleCollapse(this)">
          <span class="toggle-icon">&#9660;</span>
          Result${toolName ? ' (' + escapeHtml(toolName) + ')' : ''}
        </div>
        <div class="tool-result-content streaming-cursor"></div>
      `;
      body.appendChild(resultDiv);
      scrollToBottom();
      break;
    }

    case 'tool_result_text_delta': {
      const rId = data.tool_call_id;
      if (rId && ss.toolCalls[rId]) {
        ss.toolCalls[rId].resultText += data.delta || '';
      }
      const rEl = document.querySelector('#result-' + CSS.escape(rId) + ' .tool-result-content');
      if (rEl) {
        rEl.textContent = ss.toolCalls[rId]?.resultText || data.delta || '';
        scrollToBottom();
      }
      break;
    }

    case 'tool_result_end': {
      const reId = data.tool_call_id;
      const resultEl = document.querySelector('#result-' + CSS.escape(reId) + ' .tool-result-content');
      if (resultEl) {
        resultEl.classList.remove('streaming-cursor');
        resultEl.textContent = tryFormatJSON(resultEl.textContent);
      }
      // Update tool call status
      if (reId && ss.toolCalls[reId]) {
        const st = data.state || 'success';
        ss.toolCalls[reId].status = st === 'error' ? 'error' : 'done';
        const statusEl = document.querySelector('#tool-' + CSS.escape(reId) + ' .tool-call-status');
        if (statusEl) {
          statusEl.className = 'tool-call-status ' + (st === 'error' ? 'error' : 'done');
          statusEl.textContent = st === 'error' ? 'error' : 'done';
        }
      }
      break;
    }

    case 'require_user_confirm':
      showConfirmBar(data);
      break;

    case 'exceed_max_iters':
      const warnDiv = document.createElement('div');
      warnDiv.className = 'thinking-block';
      warnDiv.style.borderLeftColor = 'var(--warning)';
      warnDiv.innerHTML = '<div class="thinking-header" style="color:var(--warning)">Maximum iterations exceeded</div>';
      body.appendChild(warnDiv);
      break;

    case 'reply_end':
      endStream(ss);
      break;

    case 'hint_block':
      // Display hint as a subtle note
      const hintDiv = document.createElement('div');
      hintDiv.className = 'thinking-block';
      hintDiv.style.borderLeftColor = 'var(--text-muted)';
      const hintText = typeof data.hint === 'string' ? data.hint : JSON.stringify(data.hint);
      hintDiv.innerHTML = `
        <div class="thinking-header" style="color:var(--text-muted)" onclick="toggleCollapse(this)">
          <span class="toggle-icon">&#9660;</span>
          Hint (${escapeHtml(data.source || 'system')})
        </div>
        <div class="thinking-content">${escapeHtml(hintText)}</div>
      `;
      body.appendChild(hintDiv);
      break;

    default:
      // For unknown events, try to extract text
      if (data.delta && type !== 'message') {
        ss.textContent += data.delta;
        updateTextContent(body, ss);
        scrollToBottom();
      }
      break;
  }
}

function updateTextContent(body, ss) {
  let textEl = body.querySelector('.text-stream');
  if (!textEl) {
    textEl = document.createElement('div');
    textEl.className = 'text-stream streaming-cursor';
    body.appendChild(textEl);
  }
  textEl.innerHTML = renderMarkdown(ss.textContent);
}

function endStream(ss) {
  closeStream();

  // Remove any remaining streaming cursors
  document.querySelectorAll('.streaming-cursor').forEach(el => {
    el.classList.remove('streaming-cursor');
  });

  // If no content was generated, show a note
  const body = document.getElementById('streaming-body');
  if (body && !ss?.hasContent) {
    body.innerHTML = '<span style="color:var(--text-muted);font-style:italic">(No response)</span>';
  }
}

function closeStream() {
  if (state.eventSource) {
    state.eventSource.close();
    state.eventSource = null;
  }
  state.streaming = false;
  document.getElementById('btn-stop').classList.add('hidden');
  document.getElementById('btn-send').disabled = false;
}

function stopGeneration() {
  closeStream();
}

// ---------------------------------------------------------------------------
// Human-in-the-loop confirmation
// ---------------------------------------------------------------------------
function showConfirmBar(data) {
  const bar = document.getElementById('confirm-bar');
  const toolsDiv = document.getElementById('confirm-tools');

  const toolCalls = data.tool_calls || [];
  state.pendingConfirm = {
    session_id: state.activeSession,
    tool_calls: toolCalls.map(tc => ({ id: tc.id || tc.tool_call_id, name: tc.name || tc.tool_call_name })),
  };

  toolsDiv.innerHTML = toolCalls.map(tc =>
    `<span class="confirm-tool-chip">${escapeHtml(tc.name || tc.tool_call_name || 'tool')}</span>`
  ).join('');

  bar.classList.remove('hidden');
}

async function confirmAllTools(approved) {
  if (!state.pendingConfirm) return;

  try {
    await API.post('/api/confirm', {
      session_id: state.pendingConfirm.session_id,
      tool_calls: state.pendingConfirm.tool_calls.map(tc => ({
        id: tc.id,
        confirmed: approved,
      })),
    });
  } catch (err) {
    console.error('confirm error:', err);
  }

  state.pendingConfirm = null;
  document.getElementById('confirm-bar').classList.add('hidden');
}

// ---------------------------------------------------------------------------
// Models dialog
// ---------------------------------------------------------------------------
async function showModelsDialog() {
  document.getElementById('models-overlay').classList.remove('hidden');
  const content = document.getElementById('models-content');
  content.innerHTML = '<div class="loading">Loading models...</div>';

  try {
    const models = await API.get('/api/models');
    if (!models || !models.length) {
      content.innerHTML = '<div class="empty-state">No models available</div>';
      return;
    }

    content.innerHTML = models.map(m => `
      <div class="model-card">
        <div class="model-card-header">
          <span class="model-name">${escapeHtml(m.model || m.name || m.id || 'Unknown')}</span>
          <span class="model-provider">${escapeHtml(m.provider || '')}</span>
        </div>
        ${m.context_length ? `<div class="model-details">Context: ${m.context_length.toLocaleString()} tokens</div>` : ''}
      </div>
    `).join('');
  } catch (err) {
    content.innerHTML = `<div class="empty-state">Failed to load models: ${escapeHtml(err.message)}</div>`;
  }
}

function hideModelsDialog() {
  document.getElementById('models-overlay').classList.add('hidden');
}

function closeModelsDialog(e) {
  if (e.target === e.currentTarget) hideModelsDialog();
}

// ---------------------------------------------------------------------------
// UI helpers
// ---------------------------------------------------------------------------
function toggleCollapse(headerEl) {
  const icon = headerEl.querySelector('.toggle-icon');
  const content = headerEl.nextElementSibling;
  if (!content) return;

  if (content.classList.contains('collapsed')) {
    content.classList.remove('collapsed');
    if (icon) icon.classList.remove('collapsed');
  } else {
    content.classList.add('collapsed');
    if (icon) icon.classList.add('collapsed');
  }
}

function scrollToBottom() {
  const container = document.getElementById('chat-messages');
  requestAnimationFrame(() => {
    container.scrollTop = container.scrollHeight;
  });
}

function escapeHtml(text) {
  if (!text) return '';
  const div = document.createElement('div');
  div.textContent = text;
  return div.innerHTML;
}

function tryFormatJSON(text) {
  if (!text) return text;
  try {
    const obj = JSON.parse(text);
    return JSON.stringify(obj, null, 2);
  } catch {
    return text;
  }
}

// Simple Markdown renderer — handles basic formatting
function renderMarkdown(text) {
  if (!text) return '';

  let html = escapeHtml(text);

  // Code blocks (```)
  html = html.replace(/```(\w*)\n([\s\S]*?)```/g, (_, lang, code) => {
    return `<pre><code class="lang-${lang}">${code}</code></pre>`;
  });

  // Inline code
  html = html.replace(/`([^`]+)`/g, '<code>$1</code>');

  // Bold
  html = html.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');

  // Italic
  html = html.replace(/\*([^*]+)\*/g, '<em>$1</em>');

  // Line breaks
  html = html.replace(/\n/g, '<br>');

  // Wrap in paragraph if no block elements
  if (!html.includes('<pre>') && !html.includes('<br>')) {
    html = `<p>${html}</p>`;
  }

  return html;
}
