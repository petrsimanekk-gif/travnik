package main

// Record scope: an account with scope "own" receives only the records it owns
// and what belongs to them (same rule as applyScope in the CRM page). Other
// records reach it as {"_hidden": true}, which the page adapter drops.

import (
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type scopeSets struct {
	own                   bool
	name                  string
	deals, leads, clients map[string]bool
	at                    time.Time
}

var (
	scopeMu    sync.Mutex
	scopeCache = map[string]*scopeSets{}
)

func clearScopeCache() {
	scopeMu.Lock()
	scopeCache = map[string]*scopeSets{}
	scopeMu.Unlock()
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func scopeFor(app core.App, accID string) *scopeSets {
	scopeMu.Lock()
	if s, ok := scopeCache[accID]; ok && time.Since(s.at) < 2*time.Second {
		scopeMu.Unlock()
		return s
	}
	scopeMu.Unlock()
	s := &scopeSets{at: time.Now()}
	a, _ := loadAccount(app, accID)
	if a != nil && a["role"] != "admin" && a["scope"] == "own" {
		s.own, s.name = true, str(a, "name")
		load := func(col string) map[string]map[string]any {
			out := map[string]map[string]any{}
			recs, _ := app.FindAllRecords("docs", dbx.HashExp{"col": col})
			for _, r := range recs {
				out[r.GetString("key")] = docData(r)
			}
			return out
		}
		s.deals, s.leads, s.clients = map[string]bool{}, map[string]bool{}, map[string]bool{}
		for id, d := range load("deals") {
			if str(d, "owner") == s.name {
				s.deals[id] = true
				s.clients[str(d, "clientId")] = true
			}
		}
		for id, d := range load("leads") {
			if str(d, "owner") == s.name {
				s.leads[id] = true
				s.clients[str(d, "clientId")] = true
			}
		}
		for _, d := range load("offers") {
			if str(d, "owner") == s.name || s.deals[str(d, "dealId")] {
				s.clients[str(d, "clientId")] = true
			}
		}
		for id, d := range load("clients") {
			if str(d, "owner") == s.name {
				s.clients[id] = true
			}
		}
		delete(s.clients, "")
	}
	scopeMu.Lock()
	scopeCache[accID] = s
	scopeMu.Unlock()
	return s
}

func (s *scopeSets) visible(col, key string, d map[string]any) bool {
	if !s.own {
		return true
	}
	mine := str(d, "owner") == s.name
	switch col {
	case "deals", "leads":
		return mine
	case "offers":
		return mine || s.deals[str(d, "dealId")]
	case "clients":
		return mine || s.clients[key]
	case "activities":
		return mine || s.deals[str(d, "dealId")] || s.leads[str(d, "leadId")]
	case "installs":
		if s.deals[str(d, "dealId")] {
			return true
		}
		techs, _ := d["techs"].([]any)
		for _, t := range techs {
			if t == s.name {
				return true
			}
		}
		return false
	case "comms":
		return s.deals[str(d, "dealId")] || s.leads[str(d, "leadId")] || s.clients[str(d, "clientId")]
	}
	return true
}
