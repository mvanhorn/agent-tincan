// A small DOM for running the extension's page functions against HTML
// fixtures in node --test: an HTML parser (elements, attributes, text,
// comments, void elements, a few entities) and querySelector with the
// selector forms the page functions use: tag, #id, .class, [attr],
// [attr=v], [attr*=v], [attr^=v], [attr$=v] (each with an optional i
// flag), compounds, descendant and child combinators, and comma lists.
// Test-only; never shipped.

const VOID = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'source', 'track', 'wbr']);
const ENTITIES = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' ' };

function decode(s) {
  return s.replace(/&(#x[0-9a-f]+|#\d+|[a-z]+);/gi, (m, e) => {
    if (e[0] === '#') return String.fromCodePoint(e[1].toLowerCase() === 'x' ? parseInt(e.slice(2), 16) : Number(e.slice(1)));
    return ENTITIES[e.toLowerCase()] ?? m;
  });
}

class Node {
  constructor(doc, nodeType) {
    this.ownerDocument = doc;
    this.nodeType = nodeType;
    this.parentNode = null;
    this.childNodes = [];
  }
  get parentElement() {
    return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null;
  }
  get textContent() {
    if (this.nodeType === 3) return this.nodeValue;
    return this.childNodes.map((c) => c.textContent).join('');
  }
  contains(n) {
    for (let x = n; x; x = x.parentNode) if (x === this) return true;
    return false;
  }
  appendChild(n) {
    if (n.parentNode) n.parentNode.childNodes.splice(n.parentNode.childNodes.indexOf(n), 1);
    n.parentNode = this;
    this.childNodes.push(n);
    return n;
  }
}

class Text extends Node {
  constructor(doc, value) {
    super(doc, 3);
    this.nodeValue = value;
  }
}

export class Element extends Node {
  constructor(doc, tag) {
    super(doc, 1);
    this.localName = tag.toLowerCase();
    this.tagName = tag.toUpperCase();
    this.attrs = new Map();
  }
  get children() {
    return this.childNodes.filter((c) => c.nodeType === 1);
  }
  get id() {
    return this.getAttribute('id') || '';
  }
  get hidden() {
    return this.hasAttribute('hidden');
  }
  get innerText() {
    return this.textContent;
  }
  getAttribute(n) {
    return this.attrs.has(n) ? this.attrs.get(n) : null;
  }
  hasAttribute(n) {
    return this.attrs.has(n);
  }
  setAttribute(n, v) {
    this.attrs.set(n, String(v));
  }
  checkVisibility() {
    for (let x = this; x && x.nodeType === 1; x = x.parentNode) {
      if (x.hidden || /display\s*:\s*none/.test(x.getAttribute('style') || '')) return false;
    }
    return true;
  }
  scrollIntoView(opts) {
    const hook = this.ownerDocument.onScrollIntoView;
    if (hook) hook(this, opts);
  }
  matches(sel) {
    return parseList(sel).some((chain) => matchChain(this, chain));
  }
  closest(sel) {
    for (let n = this; n; n = n.parentElement) if (n.matches(sel)) return n;
    return null;
  }
  querySelectorAll(sel) {
    const chains = parseList(sel);
    const out = [];
    const walk = (n) => {
      for (const c of n.children) {
        if (chains.some((chain) => matchChain(c, chain))) out.push(c);
        walk(c);
      }
    };
    walk(this);
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
  insertHTML(html) {
    for (const n of parseFragment(this.ownerDocument, html)) this.appendChild(n);
  }
}

// ---- Selectors.

function parseCompound(s) {
  const c = { tag: null, attrs: [] };
  let i = 0;
  const ident = () => {
    const m = /^[A-Za-z0-9_-]+/.exec(s.slice(i));
    if (!m) throw new SyntaxError(`bad selector ${s}`);
    i += m[0].length;
    return m[0];
  };
  if (/[A-Za-z*]/.test(s[0])) {
    if (s[0] === '*') i++;
    else c.tag = ident().toLowerCase();
  }
  while (i < s.length) {
    const ch = s[i];
    if (ch === '#') {
      i++;
      c.attrs.push({ name: 'id', op: '=', value: ident() });
    } else if (ch === '.') {
      i++;
      c.attrs.push({ name: 'class', op: '~=', value: ident() });
    } else if (ch === '[') {
      const end = s.indexOf(']', i);
      const m = /^\[\s*([A-Za-z0-9_:-]+)\s*(?:([*^$~|]?=)\s*(?:"([^"]*)"|'([^']*)'|([^\s\]]+))\s*(i)?)?\s*\]$/.exec(s.slice(i, end + 1));
      if (!m) throw new SyntaxError(`bad selector ${s}`);
      c.attrs.push({ name: m[1], op: m[2] || null, value: m[3] ?? m[4] ?? m[5] ?? '', ci: Boolean(m[6]) });
      i = end + 1;
    } else {
      throw new SyntaxError(`bad selector ${s}`);
    }
  }
  return c;
}

// parseList returns each comma-separated selector as a chain of
// [{compound, combinator}] from left to right.
function parseList(sel) {
  const chains = [];
  for (const part of splitTop(sel, ',')) {
    const tokens = [];
    let buf = '';
    let depth = 0;
    let quote = '';
    const flush = () => {
      if (buf.trim()) tokens.push(buf.trim());
      buf = '';
    };
    for (const ch of part.trim()) {
      if (quote) {
        buf += ch;
        if (ch === quote) quote = '';
        continue;
      }
      if (ch === '"' || ch === "'") quote = ch;
      if (ch === '[') depth++;
      if (ch === ']') depth--;
      if (depth === 0 && (ch === ' ' || ch === '>')) {
        flush();
        if (ch === '>') tokens.push('>');
        continue;
      }
      buf += ch;
    }
    flush();
    const chain = [];
    let comb = ' ';
    for (const t of tokens) {
      if (t === '>') {
        comb = '>';
        continue;
      }
      chain.push({ compound: parseCompound(t), comb });
      comb = ' ';
    }
    chains.push(chain);
  }
  return chains;
}

function splitTop(s, sep) {
  const out = [];
  let buf = '';
  let depth = 0;
  let quote = '';
  for (const ch of s) {
    if (quote) {
      if (ch === quote) quote = '';
    } else if (ch === '"' || ch === "'") quote = ch;
    else if (ch === '[') depth++;
    else if (ch === ']') depth--;
    else if (ch === sep && depth === 0) {
      out.push(buf);
      buf = '';
      continue;
    }
    buf += ch;
  }
  out.push(buf);
  return out;
}

function matchCompound(el, c) {
  if (c.tag && el.localName !== c.tag) return false;
  for (const a of c.attrs) {
    const v = el.getAttribute(a.name);
    if (v === null) return false;
    if (!a.op) continue;
    const have = a.ci ? v.toLowerCase() : v;
    const want = a.ci ? a.value.toLowerCase() : a.value;
    const ok = {
      '=': () => have === want,
      '*=': () => want !== '' && have.includes(want),
      '^=': () => want !== '' && have.startsWith(want),
      '$=': () => want !== '' && have.endsWith(want),
      '~=': () => have.split(/\s+/).includes(want),
      '|=': () => have === want || have.startsWith(want + '-'),
    }[a.op]();
    if (!ok) return false;
  }
  return true;
}

// matchChain matches el against chain from its last compound, walking up
// its ancestors as querySelectorAll does (the whole document counts).
function matchChain(el, chain) {
  const at = (node, i) => {
    if (!matchCompound(node, chain[i].compound)) return false;
    if (i === 0) return true;
    const comb = chain[i].comb;
    for (let p = node.parentNode; p && p.nodeType === 1; p = p.parentNode) {
      if (at(p, i - 1)) return true;
      if (comb === '>') return false;
    }
    return false;
  };
  return at(el, chain.length - 1);
}

// ---- Parsing.

function parseFragment(doc, html) {
  const root = new Element(doc, 'fragment');
  let cur = root;
  const re = /<!--[\s\S]*?-->|<!doctype[^>]*>|<\/([A-Za-z0-9-]+)\s*>|<([A-Za-z0-9-]+)((?:\s+[^\s"'>/=]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'>]+))?)*)\s*(\/?)>|([^<]+)/gi;
  let m;
  while ((m = re.exec(html))) {
    if (m[5] !== undefined) {
      cur.appendChild(new Text(doc, decode(m[5])));
    } else if (m[1]) {
      const tag = m[1].toLowerCase();
      for (let x = cur; x !== root; x = x.parentNode) {
        if (x.localName === tag) {
          cur = x.parentNode;
          break;
        }
      }
    } else if (m[2]) {
      const el = new Element(doc, m[2]);
      const attrRe = /([^\s"'>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?/g;
      let a;
      while ((a = attrRe.exec(m[3] || ''))) el.setAttribute(a[1].toLowerCase(), decode(a[2] ?? a[3] ?? a[4] ?? ''));
      cur.appendChild(el);
      if (!VOID.has(el.localName) && !m[4]) cur = el;
    }
  }
  return [...root.childNodes];
}

// parseHTML returns a document for html: documentElement, body, title,
// querySelector(All), and onScrollIntoView, which a test sets to see
// scrollIntoView calls.
export function parseHTML(html) {
  const doc = { onScrollIntoView: null };
  const top = new Element(doc, 'html');
  for (const n of parseFragment(doc, html)) top.appendChild(n);
  const htmlEl = top.children.length === 1 && top.children[0].localName === 'html' ? top.children[0] : top;
  doc.documentElement = htmlEl;
  doc.body = htmlEl.querySelector('body') || htmlEl;
  const t = htmlEl.querySelector('title');
  doc.title = t ? t.textContent : '';
  doc.querySelector = (s) => htmlEl.querySelector(s);
  doc.querySelectorAll = (s) => htmlEl.querySelectorAll(s);
  doc.getElementById = (id) => htmlEl.querySelector(`#${id}`);
  doc.createElement = (tag) => new Element(doc, tag);
  return doc;
}
