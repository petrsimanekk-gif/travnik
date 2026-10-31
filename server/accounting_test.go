package main

import "testing"

func TestAccPerm(t *testing.T) {
	cases := []struct {
		a    map[string]any
		want string
	}{
		{nil, "none"},
		{map[string]any{"role": "admin"}, "edit"},
		{map[string]any{"role": "sales", "perms": map[string]any{"deals": "edit"}}, "none"},
		{map[string]any{"role": "read", "perms": map[string]any{"accounting": "view"}}, "view"},
		{map[string]any{"role": "acct", "perms": map[string]any{"accounting": "edit"}}, "edit"},
		{map[string]any{"role": "custom", "perms": map[string]any{"accounting": "bogus"}}, "none"},
	}
	for i, c := range cases {
		if got := accPerm(c.a); got != c.want {
			t.Errorf("case %d: accPerm = %q, want %q", i, got, c.want)
		}
	}
	for col, want := range map[string]bool{"acc_out": true, "acc_in": true, "acc_bank": true, "acc_cfg": true, "accounts": false, "deals": false} {
		if isAccCol(col) != want {
			t.Errorf("isAccCol(%q) != %v", col, want)
		}
	}
	for _, col := range []string{"acc_out", "acc_in", "acc_bank", "acc_cfg"} {
		if s := writeSections[col]; len(s) != 1 || s[0] != "accounting" {
			t.Errorf("writeSections[%q] = %v", col, s)
		}
	}
}
