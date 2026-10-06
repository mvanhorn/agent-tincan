// Agent Tincan History options page: lists each site in SITE_ACCESS, says
// whether Chrome has granted the extension its origins, and offers a Grant
// button for a site that is not granted. A site whose pages are granted
// but not every origin (ChatGPT without its file host) shows as granted
// with files needing more access, and keeps a button for the rest.
//
// Chrome shows its own prompt for a grant and accepts a request only
// during a user gesture, so the button's click handler calls
// permissions.request before anything else (no await first). The page is
// built with createElement and textContent only, runs as an extension page
// (never in a site's page), and holds no state of its own: it reads the
// grants from Chrome each time. A grant or revocation also reaches the
// worker (chrome.permissions.onAdded/onRemoved), which tells the native
// host.

import { SITE_ACCESS, siteGranted } from './ops.js';

// siteStates lists every site with whether all its origins are granted,
// and partial when only its page origins are (its operations work, but
// not the fetches to its other origins, such as ChatGPT's file host). A
// site not fully granted keeps its Grant button, which asks for all of
// them.
export async function siteStates(permissions) {
  const out = [];
  for (const [site, s] of Object.entries(SITE_ACCESS)) {
    const granted = await siteGranted(permissions, site, { all: true });
    const partial = !granted && (await siteGranted(permissions, site));
    out.push({ site, label: s.label, granted, partial });
  }
  return out;
}

// STATUS is the text, status class and button label for each state.
const STATUS = {
  granted: { text: 'Granted', className: 'status granted', button: 'Grant' },
  partial: { text: 'Granted (images and files need file access)', className: 'status partial', button: 'Grant file access' },
  none: { text: 'Not granted', className: 'status', button: 'Grant' },
};

// renderOptions fills the page's #sites list and keeps it current. It
// returns {ready}, a promise for the first render.
export function renderOptions({ document, permissions }) {
  const list = document.getElementById('sites');
  const rows = new Map();

  function row(site, label) {
    const li = document.createElement('li');
    li.setAttribute('data-site', site);
    const name = document.createElement('span');
    name.className = 'label';
    name.textContent = label;
    const status = document.createElement('span');
    status.className = 'status';
    const button = document.createElement('button');
    button.textContent = 'Grant';
    button.addEventListener('click', () => {
      button.disabled = true;
      let asked;
      try {
        asked = Promise.resolve(permissions.request({ origins: [...SITE_ACCESS[site].origins] }));
      } catch (e) {
        asked = Promise.reject(e);
      }
      // Chrome refuses a request for an origin the loaded manifest does
      // not list (a newer ops.js before the extension reloads); say so
      // instead of leaving the click looking like it did nothing.
      asked
        .then(() => {
          r.error = '';
        })
        .catch((e) => {
          r.error = `Chrome refused: ${(e && e.message) || e}. Reload the extension in chrome://extensions and try again.`;
        })
        .finally(() => {
          button.disabled = false;
          refresh();
        });
    });
    li.append(name, status, button);
    const r = { li, status, button, error: '' };
    return r;
  }

  async function refresh() {
    for (const s of await siteStates(permissions)) {
      let r = rows.get(s.site);
      if (!r) {
        r = row(s.site, s.label);
        rows.set(s.site, r);
      }
      const st = STATUS[s.granted ? 'granted' : s.partial ? 'partial' : 'none'];
      r.status.textContent = r.error && !s.granted ? r.error : st.text;
      r.status.className = r.error && !s.granted ? 'status error' : st.className;
      r.button.textContent = st.button;
      r.button.hidden = s.granted;
    }
    list.replaceChildren(...[...rows.values()].map((r) => r.li));
  }

  // A change made elsewhere (chrome://extensions site access) shows here
  // too.
  if (permissions.onAdded) permissions.onAdded.addListener(() => refresh());
  if (permissions.onRemoved) permissions.onRemoved.addListener(() => refresh());
  return { ready: refresh() };
}

if (typeof document !== 'undefined' && typeof chrome !== 'undefined' && chrome.permissions) {
  renderOptions({ document, permissions: chrome.permissions });
}
