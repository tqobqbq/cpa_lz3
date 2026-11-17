const accessView = (() => {
  'use strict';
  const base = __EXTENSION_BASE__, configPath = __CONFIG_PATH__, $ = id => document.getElementById(id);
  const drafts = new Map(), selections = {credentials: '', keys: ''};
  let state = null, scope = 'credentials', busy = false, saving = false;
  const api = (...args) => managementAuth.request(...args);
  const field = kind => kind === 'credentials' ? 'credential_rules' : 'api_key_rules';
  const draftID = (kind, id) => kind + '\u0000' + id;
  const message = text => { $('accessMessage').textContent = text || ''; };
  function element(tag, text, className) {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (className) node.className = className;
    return node;
  }
  function storedRule(kind, id) {
    const rules = state?.config?.[field(kind)];
    const rule = rules && Object.hasOwn(rules, id) ? rules[id] : null;
    return {mode: rule?.mode || 'blacklist', entries: [...(rule?.entries || [])]};
  }
  function ruleFor(kind, id) { return drafts.get(draftID(kind, id))?.rule || storedRule(kind, id); }
  function sameRule(a, b) { return a.mode === b.mode && JSON.stringify([...a.entries].sort()) === JSON.stringify([...b.entries].sort()); }
  function records(kind) {
    if (!state) return [];
    const result = new Map();
    if (kind === 'credentials') {
      for (const auth of state.credentials) {
        if (!auth.auth_index) continue;
        const name = auth.email || auth.account || auth.label || auth.name || auth.id;
        result.set(auth.auth_index, {id: auth.auth_index, label: name, detail: auth.provider + ' · ' + auth.auth_index + (auth.disabled ? ' · 已禁用' : ''), current: true});
      }
    } else {
      for (const key of state.api_keys || []) result.set(key, {id: key, label: key, detail: '客户端 API key', current: true});
    }
    const ids = new Set(Object.keys(state.config?.[field(kind)] || {}));
    const opposite = kind === 'credentials' ? 'keys' : 'credentials';
    for (const rule of Object.values(state.config?.[field(opposite)] || {})) for (const id of rule.entries || []) ids.add(id);
    for (const [key, draft] of drafts) {
      if (key.startsWith(kind + '\u0000')) ids.add(key.slice(kind.length + 1));
      else for (const id of draft.rule.entries) ids.add(id);
    }
    for (const id of ids) if (!result.has(id)) result.set(id, {id, label: id, detail: kind === 'credentials' ? '当前已移除的 Credential' : '当前未配置的 API key', current: false});
    return [...result.values()].sort((a, b) => Number(b.current) - Number(a.current) || a.label.localeCompare(b.label));
  }
  function matches(record, query) { return (record.label + ' ' + record.detail).toLowerCase().includes(query.toLowerCase()); }
  function denial(key, index) {
    const credentialRule = ruleFor('credentials', index), keyRule = ruleFor('keys', key);
    const allows = (rule, value) => rule.mode === 'whitelist' ? rule.entries.includes(value) : !rule.entries.includes(value);
    if (!allows(credentialRule, key)) return credentialRule.mode === 'whitelist' ? 'API key 不在 Credential 白名单中' : 'API key 命中 Credential 黑名单';
    if (!allows(keyRule, index)) return keyRule.mode === 'whitelist' ? 'Credential 不在 API key 白名单中' : 'Credential 命中 API key 黑名单';
    return '';
  }
  function edit(change) {
    const id = selections[scope];
    if (!id || saving) return;
    const rule = ruleFor(scope, id), next = {mode: rule.mode, entries: [...rule.entries]};
    change(next);
    drafts.set(draftID(scope, id), {rule: next, pending: false});
    render();
  }
  function members() {
    const opposite = scope === 'credentials' ? 'keys' : 'credentials';
    return records(opposite).filter(record => matches(record, $('accessMemberSearch').value));
  }
  function render() {
    const list = records(scope), selected = selections[scope];
    $('accessCredentials').className = scope === 'credentials' ? 'active-tab' : 'secondary';
    $('accessKeys').className = scope === 'keys' ? 'active-tab' : 'secondary';
    $('accessCredentials').setAttribute('aria-pressed', String(scope === 'credentials'));
    $('accessKeys').setAttribute('aria-pressed', String(scope === 'keys'));
    if (!selected && list.length) selections[scope] = list[0].id;
    const focused = document.activeElement?.dataset.accessEntity;
    $('accessEntities').replaceChildren();
    for (const record of list.filter(record => matches(record, $('accessEntitySearch').value))) {
      const button = element('button', undefined, record.id === selections[scope] ? 'active-tab' : 'secondary');
      button.type = 'button'; button.dataset.accessEntity = record.id;
      button.setAttribute('aria-pressed', String(record.id === selections[scope]));
      button.append(element('strong', record.label), element('small', record.detail));
      const rule = ruleFor(scope, record.id);
      button.append(element('small', (rule.mode === 'whitelist' ? '白名单' : '黑名单') + ' · ' + rule.entries.length + ' 项'));
      button.onclick = () => { selections[scope] = record.id; $('accessMemberSearch').value = ''; render(); };
      $('accessEntities').append(button);
      if (focused === record.id) button.focus({preventScroll: true});
    }
    if (!$('accessEntities').childElementCount) $('accessEntities').append(element('small', state ? '没有匹配对象' : '连接后读取配置对象'));
    const id = selections[scope], current = list.find(record => record.id === id);
    $('accessEditor').hidden = !state || !current; $('accessEmpty').hidden = !!state && !!current;
    if (!state || !current) return;
    const rule = ruleFor(scope, id), draft = drafts.get(draftID(scope, id));
    $('accessTitle').textContent = scope === 'credentials' ? current.label : 'API key';
    $('accessIdentity').textContent = id; $('accessEntityStatus').textContent = current.detail;
    $('accessMode').value = rule.mode; $('accessMode').disabled = saving;
    $('accessModeHint').textContent = rule.mode === 'blacklist' ? '名单中的对象禁止使用；空名单允许全部。' : '仅允许名单中的对象；空名单拒绝全部。';
    $('accessDraft').textContent = draft?.pending ? '已保存，等待热更新' : draft ? '有未保存修改' : '当前生效配置';
    const body = $('accessMembers'), scrollTop = body.scrollTop, focusedMember = document.activeElement?.dataset.accessMember;
    body.replaceChildren(); $('accessPreview').replaceChildren();
    const visible = members();
    for (const record of visible) {
      const label = element('label', undefined, 'access-member'), checkbox = element('input'), identity = element('span');
      checkbox.type = 'checkbox'; checkbox.checked = rule.entries.includes(record.id); checkbox.disabled = saving;
      checkbox.dataset.accessMember = record.id;
      checkbox.onchange = () => edit(next => { next.entries = checkbox.checked ? [...new Set([...next.entries, record.id])] : next.entries.filter(value => value !== record.id); });
      identity.append(element('strong', record.label), element('small', record.detail)); label.append(checkbox, identity); body.append(label);
      if (focusedMember === record.id) checkbox.focus({preventScroll: true});
      const reason = scope === 'credentials' ? denial(record.id, id) : denial(id, record.id);
      const row = element('tr');
      row.append(element('td', record.label), element('td', reason ? '拒绝' : '允许', reason ? 'bad' : 'good'), element('td', reason || '两侧名单均允许'));
      $('accessPreview').append(row);
    }
    if (!visible.length) body.append(element('small', '没有匹配对象；可调整搜索条件。'));
    body.scrollTop = scrollTop;
    $('accessMemberCount').textContent = '名单共 ' + rule.entries.length + ' 项 · 当前显示 ' + visible.length + ' 项';
    $('accessSave').disabled = saving || !draft || draft.pending;
    $('accessReset').disabled = saving || !draft || draft.pending;
    $('accessSelectVisible').disabled = saving; $('accessClear').disabled = saving;
  }
  async function refresh() {
    if (!managementAuth.ready || busy) return;
    busy = true; const revision = managementAuth.revision;
    try {
      const next = await api(base + '/access');
      if (revision !== managementAuth.revision) return;
      state = next;
      for (const [key, draft] of drafts) {
        if (!draft.pending) continue;
        const split = key.indexOf('\u0000'), kind = key.slice(0, split), id = key.slice(split + 1);
        if (sameRule(draft.rule, storedRule(kind, id))) drafts.delete(key);
      }
      $('connection').textContent = '已连接 · 访问控制'; render();
    } catch (error) { message(error.message); }
    finally { busy = false; }
  }
  $('accessEditor').onsubmit = async event => {
    event.preventDefault();
    const kind = scope, id = selections[kind], key = draftID(kind, id), draft = drafts.get(key);
    if (saving || !draft || draft.pending || !managementAuth.ready) return;
    saving = true; render(); const revision = managementAuth.revision;
    try {
      await api(configPath + '/access_control', 'PATCH', {[field(kind)]: {[id]: draft.rule}});
      if (revision !== managementAuth.revision) return;
      draft.pending = true; message('名单已保存，等待配置热更新。');
      await refresh();
    } catch (error) { message(error.message); }
    finally { saving = false; render(); }
  };
  function changeScope(next) { scope = next; $('accessEntitySearch').value = ''; $('accessMemberSearch').value = ''; render(); }
  $('accessCredentials').onclick = () => changeScope('credentials');
  $('accessKeys').onclick = () => changeScope('keys');
  $('accessEntitySearch').oninput = render; $('accessMemberSearch').oninput = render;
  $('accessMode').onchange = () => edit(rule => { rule.mode = $('accessMode').value; });
  $('accessSelectVisible').onclick = () => edit(rule => { rule.entries = [...new Set([...rule.entries, ...members().map(record => record.id)])]; });
  $('accessClear').onclick = () => edit(rule => { rule.entries = []; });
  $('accessReset').onclick = () => { drafts.delete(draftID(scope, selections[scope])); render(); };
  $('accessRefresh').onclick = refresh;
  setInterval(() => { if (!document.hidden && !$('accessPanel').hidden) refresh(); }, 2000);
  return {refresh, reset() { state = null; drafts.clear(); selections.credentials = ''; selections.keys = ''; message(''); render(); },
    openCredential(index) { changeScope('credentials'); selections.credentials = index; location.hash = 'access'; render(); refresh(); }};
})();
