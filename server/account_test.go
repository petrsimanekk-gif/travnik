package main

import "testing"

func TestOwnAccountWrite(t *testing.T) {
	a := map[string]any{"id": "u1", "name": "Jan", "role": "tech", "salt": "s", "hash": "h", "perms": map[string]any{}}
	ok := func(d map[string]any) bool { return canWrite(a, "accounts", "u1", d, nil) }
	if !ok(map[string]any{"name": "Jan", "role": "tech", "salt": "s2", "hash": "h2", "perms": map[string]any{}, "phone": "+420 777 000 000"}) {
		t.Error("own phone and password should be allowed")
	}
	if ok(map[string]any{"name": "Jan", "role": "admin", "salt": "s", "hash": "h", "perms": map[string]any{}, "phone": "1"}) {
		t.Error("role change must be refused")
	}
	if ok(map[string]any{"name": "Jan", "role": "tech", "salt": "s", "hash": "h", "perms": map[string]any{}, "phone": map[string]any{"x": 1}}) {
		t.Error("phone must be text")
	}
	if canWrite(a, "accounts", "u2", map[string]any{"phone": "1"}, nil) {
		t.Error("someone else's account must be refused")
	}
}

func TestNewsWrite(t *testing.T) {
	a := map[string]any{"id": "u1", "name": "Jan", "role": "tech", "perms": map[string]any{}}
	if canWrite(a, "news", "n1", map[string]any{"title": "x"}, nil) {
		t.Error("only admins write news")
	}
	if !canWrite(a, "newsreads", "n1_u1", map[string]any{"news": "n1", "acc": "u1"}, nil) {
		t.Error("own read mark should be allowed")
	}
	if canWrite(a, "newsreads", "n1_u2", map[string]any{"news": "n1", "acc": "u2"}, nil) || canWrite(a, "newsreads", "n1_u1", map[string]any{"news": "n1", "acc": "u2"}, nil) {
		t.Error("someone else's read mark must be refused")
	}
}
