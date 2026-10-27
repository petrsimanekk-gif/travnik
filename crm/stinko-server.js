// Prima Shade CRM – adapter for the own server (PocketBase + /api/stinko/*).
// Provides the same window.claude.use("db" | "assets" | "user" | "downloads")
// interface the page was written against on claude.ai, so index.html runs
// unchanged apart from the login flow (window.STINKO_SERVER).
(() => {
  "use strict";
  const TK = "stinko-token";
  const lsGet = k => { try { return localStorage.getItem(k); } catch { return null; } };
  const lsSet = (k, v) => { try { v == null ? localStorage.removeItem(k) : localStorage.setItem(k, v); } catch {} };
  let token = lsGet(TK);

  class ApiError extends Error { constructor(msg, status, code){ super(msg); this.status = status; this.code = code; } }
  async function api(path, opt = {}){
    const headers = {...(opt.headers || {})};
    if (token) headers.Authorization = token;
    let body = opt.body;
    if (body && !(body instanceof FormData)){ headers["Content-Type"] = "application/json"; body = JSON.stringify(body); }
    const r = await fetch(path, {method: opt.method || (body ? "POST" : "GET"), headers, body, credentials: "same-origin"});
    if (r.status === 204) return null;
    const j = await r.json().catch(() => ({}));
    if (!r.ok){
      const code = r.status === 401 ? "unauthenticated" : r.status === 403 ? "permission_denied" : r.status === 413 ? "quota_exceeded" : "error";
      throw new ApiError(j.message || ("HTTP " + r.status), r.status, code);
    }
    return j;
  }

  const SV = window.STINKO_SERVER = { account: null, hasAccounts: true, api,
    async login(username, password){
      const j = await api("/api/stinko/login", {body: {username, password}});
      token = j.token; lsSet(TK, token); return j.account;
    },
    async setup(account){
      const j = await api("/api/stinko/setup", {body: {account}});
      token = j.token; lsSet(TK, token); return j.account;
    },
    async logout(){ try { await api("/api/stinko/logout", {method: "POST"}); } catch {} token = null; lsSet(TK, null); }
  };

  // ---------- realtime: one SSE stream for the whole docs collection ----------
  const cols = new Map(); // col -> {docs: Map(key -> data), subs: Set, loaded}
  const colState = c => { if (!cols.has(c)) cols.set(c, {docs: new Map(), subs: new Set(), loaded: false, loading: null}); return cols.get(c); };
  const snapshot = st => ({ docs: [...st.docs].map(([id, data]) => ({ id, data: () => data, exists: true })) });
  const emit = st => { const s = snapshot(st); st.subs.forEach(cb => { try { cb.next(s); } catch (e) { console.error(e); } }); };

  async function loadCol(c){
    const st = colState(c), docs = new Map();
    const f = encodeURIComponent(`col = "${c.replace(/["\\]/g, "\\$&")}"`);
    for (let page = 1; ; page++){
      const j = await api(`/api/collections/docs/records?filter=${f}&perPage=500&page=${page}&skipTotal=1&fields=key,data`);
      j.items.forEach(r => { if (!r.data?._hidden) docs.set(r.key, r.data || {}); }); // _hidden: outside this user's scope
      if (j.items.length < 500) break;
    }
    st.docs = docs; st.loaded = true; emit(st);
  }

  let es = null, clientId = null, resyncT = null;
  function connect(){
    if (es) return;
    es = new EventSource("/api/realtime");
    es.addEventListener("PB_CONNECT", async ev => {
      const first = clientId === null;
      clientId = JSON.parse(ev.data).clientId;
      try { await api("/api/realtime", {body: {clientId, subscriptions: ["docs/*"]}}); }
      catch (e) { console.warn("realtime subscribe", e); }
      // after a reconnect, changes may have been missed: reload what is watched
      if (!first){ clearTimeout(resyncT); resyncT = setTimeout(() => cols.forEach((st, c) => st.subs.size && loadCol(c).catch(console.warn)), 300); }
    });
    es.addEventListener("docs/*", ev => {
      const m = JSON.parse(ev.data), r = m.record || {};
      // own scope changed (e.g. promoted to see everyone's records): records hidden so far must be fetched again
      if (r.col === "accounts" && SV.account && r.key === SV.account.id && (r.data?.scope || "all") !== (SV.account.scope || "all")){
        SV.account = {...SV.account, scope: r.data.scope};
        clearTimeout(resyncT); resyncT = setTimeout(() => cols.forEach((st, c) => st.subs.size && loadCol(c).catch(console.warn)), 300);
      }
      const st = cols.get(r.col); if (!st || !st.loaded) return;
      if (m.action === "delete" || r.data?._hidden) st.docs.delete(r.key); else st.docs.set(r.key, r.data || {});
      emit(st);
    });
  }

  const db = {
    collection(c){
      return {
        doc(id){
          return {
            async get(){ const st = colState(c); if (!st.loaded) await (st.loading ||= loadCol(c)); const d = st.docs.get(id); return {id, exists: d !== undefined, data: () => d}; },
            async set(data){ await api("/api/stinko/set", {body: {col: c, id, data}}); const st = colState(c); st.docs.set(id, JSON.parse(JSON.stringify(data))); if (st.loaded) emit(st); },
            async delete(){ await api("/api/stinko/del", {body: {col: c, id}}); const st = colState(c); st.docs.delete(id); if (st.loaded) emit(st); }
          };
        },
        onSnapshot(next, error){
          const st = colState(c), sub = {next, error};
          st.subs.add(sub); connect();
          if (st.loaded) next(snapshot(st)); else (st.loading ||= loadCol(c)).catch(e => { st.loading = null; error && error(e); });
          return () => st.subs.delete(sub);
        }
      };
    }
  };

  const assets = {
    async upload(file){
      const fd = new FormData(); fd.append("file", file, file.name || "soubor");
      if (file.type) fd.append("type", file.type);
      return api("/api/stinko/upload", {body: fd});
    }
  };
  const user = { async id(){ return SV.account?.id || null; } };
  const downloads = {
    async save({filename, data}){
      const blob = data instanceof Blob ? data : new Blob([data]);
      const u = URL.createObjectURL(blob), a = document.createElement("a");
      a.href = u; a.download = filename || "soubor"; document.body.appendChild(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(u), 60000);
    }
  };

  // ---------- startup: who is signed in ----------
  const ready = (async () => {
    if (token){
      try { const j = await api("/api/stinko/me"); token = j.token; lsSet(TK, token); SV.account = j.account; }
      catch (e) { if (e.status === 401 || e.status === 403){ token = null; lsSet(TK, null); } else console.warn(e); }
    }
    if (!SV.account){ try { SV.hasAccounts = (await api("/api/stinko/status")).hasAccounts; } catch {} }
  })();
  // refresh the token (and cookie for /_blob) once a day while the page is open
  setInterval(async () => { if (!token) return; try { const j = await api("/api/stinko/me"); token = j.token; lsSet(TK, token); } catch {} }, 24 * 3600 * 1000);

  const caps = {db, assets, user, downloads};
  window.claude = { use: async name => { await ready; if (!SV.account && name !== "downloads") throw new ApiError("nepřihlášen", 401, "unauthenticated"); if (!caps[name]) throw new Error("unknown capability " + name); return caps[name]; } };
})();
