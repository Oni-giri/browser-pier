/*
 * Minimal stand-in runtime for BrowserPier's *.dc.html design files.
 *
 * The files were made on a design canvas whose own runtime isn't
 * distributed. This script renders them in a plain browser so they can be
 * viewed and clicked through. It supports exactly what the files use:
 *   - <helmet> contents moved into <head>
 *   - {{path.to.value}} in text and attributes
 *   - <sc-if value="{{x}}"> and <sc-for list="{{xs}}" as="item">
 *   - onClick / onInput / onChange="{{handler}}"
 *   - a Component class extending DCLogic, with props, state,
 *     setState, renderVals and componentDidMount
 *
 * It reads the page's own source over HTTP, so serve the folder:
 *   python3 -m http.server   (then open http://localhost:8000/Main.dc.html)
 * Optional: ?props={"empty":true} overrides a screen's props.
 */
(function () {
  class DCLogic {
    constructor(props) { this.props = props || {}; this.state = {}; }
    setState(patch) {
      const p = typeof patch === 'function' ? patch(this.state, this.props) : patch;
      this.state = Object.assign({}, this.state, p);
      if (this.__render) this.__render();
    }
  }
  window.DCLogic = DCLogic;

  const BOOL_PROPS = new Set(['disabled', 'checked', 'selected', 'hidden', 'readonly', 'required']);
  const EVENTS = { onclick: 'click', oninput: 'input', onchange: 'change' };
  const WHOLE = /^\s*\{\{\s*([^}]+?)\s*\}\}\s*$/;

  function lookup(scope, path) {
    path = path.trim();
    if (path === 'true') return true;
    if (path === 'false') return false;
    if (path === 'null') return null;
    if (/^-?\d+(\.\d+)?$/.test(path)) return Number(path);
    let v = scope;
    for (const part of path.split('.')) {
      if (v == null) return undefined;
      v = v[part];
    }
    return v;
  }
  function interp(str, scope) {
    return str.replace(/\{\{\s*([^}]+?)\s*\}\}/g, (_, p) => {
      const v = lookup(scope, p);
      return v == null ? '' : String(v);
    });
  }

  function renderNodes(src, scope, out) {
    for (const node of Array.from(src.childNodes)) renderNode(node, scope, out);
  }
  function renderNode(node, scope, out) {
    if (node.nodeType === 3) { out.appendChild(document.createTextNode(interp(node.nodeValue, scope))); return; }
    if (node.nodeType !== 1) return;
    const kind = node.getAttribute('data-sc');
    const inner = node.content || node; // <template> holds its children in .content
    if (kind === 'if') {
      const m = (node.getAttribute('value') || '').match(WHOLE);
      if (m && lookup(scope, m[1])) renderNodes(inner, scope, out);
      return;
    }
    if (kind === 'for') {
      const m = (node.getAttribute('list') || '').match(WHOLE);
      const name = node.getAttribute('as') || 'item';
      const list = (m && lookup(scope, m[1])) || [];
      list.forEach((item) => renderNodes(inner, Object.assign(Object.create(scope), { [name]: item }), out));
      return;
    }
    const el = node.namespaceURI === 'http://www.w3.org/2000/svg'
      ? document.createElementNS(node.namespaceURI, node.localName)
      : document.createElement(node.localName);
    for (const attr of Array.from(node.attributes)) {
      const n = attr.name;
      if (n.startsWith('hint-placeholder')) continue;
      const whole = attr.value.match(WHOLE);
      if (EVENTS[n.toLowerCase()]) {
        const fn = whole && lookup(scope, whole[1]);
        if (typeof fn === 'function') el.addEventListener(EVENTS[n.toLowerCase()], fn);
        continue;
      }
      if (whole) {
        const v = lookup(scope, whole[1]);
        if (BOOL_PROPS.has(n.toLowerCase())) { if (v && v !== 'false') el.setAttribute(n, ''); continue; }
        if (n === 'value' && 'value' in el) { el.setAttribute('value', v == null ? '' : String(v)); continue; }
        el.setAttribute(n, v == null ? '' : String(v));
      } else {
        el.setAttribute(n, interp(attr.value, scope));
      }
    }
    renderNodes(inner, scope, el);
    out.appendChild(el);
  }

  function boot() {
    const xhr = new XMLHttpRequest();
    xhr.open('GET', location.href.split('#')[0], false);
    xhr.send();
    let src = xhr.responseText;

    const body = src.match(/<x-dc>([\s\S]*?)<\/x-dc>/);
    const scriptTag = src.match(/<script type="text\/x-dc"[^>]*data-props='([^']*)'[^>]*>([\s\S]*?)<\/script>/)
      || src.match(/<script type="text\/x-dc"[^>]*>([\s\S]*?)<\/script>/);
    if (!body) return;
    let markup = body[1];

    const helmet = markup.match(/<helmet>([\s\S]*?)<\/helmet>/);
    if (helmet) {
      const h = document.createElement('template');
      h.innerHTML = helmet[1];
      document.head.appendChild(h.content);
      markup = markup.replace(helmet[0], '');
    }
    // <template> is valid anywhere (even inside <tbody>), so the parser keeps loops in place.
    markup = markup
      .replace(/<sc-(if|for)\b/g, '<template data-sc="$1"')
      .replace(/<\/sc-(if|for)>/g, '</template>');
    const tpl = document.createElement('template');
    tpl.innerHTML = markup;

    let props = {};
    let code = '';
    if (scriptTag && scriptTag.length === 3) {
      try {
        const spec = JSON.parse(scriptTag[1].replace(/&quot;/g, '"'));
        for (const k of Object.keys(spec)) if (k[0] !== '$' && spec[k] && 'default' in spec[k]) props[k] = spec[k].default;
      } catch (e) { /* no props */ }
      code = scriptTag[2];
    } else if (scriptTag) {
      code = scriptTag[1];
    }
    try {
      const q = new URLSearchParams(location.search).get('props');
      if (q) Object.assign(props, JSON.parse(q));
    } catch (e) { /* ignore bad override */ }

    let comp = null;
    if (code.trim()) {
      const Component = new Function('DCLogic', code + '\n;return Component;')(DCLogic);
      comp = new Component(props);
    }

    const root = document.createElement('div');
    root.id = 'dc-root';
    document.body.appendChild(root);
    const render = () => {
      const vals = comp && comp.renderVals ? comp.renderVals() : {};
      const frag = document.createDocumentFragment();
      renderNodes(tpl.content, vals, frag);
      root.replaceChildren(frag);
    };
    if (comp) comp.__render = render;
    render();
    if (comp && comp.componentDidMount) comp.componentDidMount();
  }

  // Hide the raw markup the browser parsed before this script swaps in the render.
  const hide = document.createElement('style');
  hide.textContent = 'x-dc{display:none!important}';
  document.head.appendChild(hide);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
