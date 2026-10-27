package main

import "testing"

func TestFillSalutation(t *testing.T) {
	for _, c := range [][3]string{
		{"{osloveni}\n\nděkujeme", "Dobrý den, pane Nováku,", "Dobrý den, pane Nováku,\n\nděkujeme"},
		{"Dobrý den, {osloveni},\n\nděkujeme", "Dobrý den, pane Nováku,", "Dobrý den, pane Nováku,\n\nděkujeme"},
		{"Dobrý den {osloveni}\nx", "Dobrý den, pane Nováku,", "Dobrý den, pane Nováku,\nx"},
		{"Dobrý den, {osloveni},\nx", "Dobrý den,", "Dobrý den,\nx"},
		{"{osloveni},\nx", "Dobrý den, paní Malá,", "Dobrý den, paní Malá,\nx"},
		{"Dobrý den,\n{osloveni}", "Dobrý den, pane X,", "Dobrý den,\nDobrý den, pane X,"},
		{"Dobrý den, {osloveni}", "Vážený pane inženýre,", "Dobrý den, Vážený pane inženýre,"},
	} {
		if got := fillAuto(c[0], map[string]string{"osloveni": c[1]}); got != c[2] {
			t.Errorf("%q + %q → %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestSalutation(t *testing.T) {
	for _, c := range [][2]string{{"Jan Novák", "Dobrý den, pane Nováku,"}, {"Petr Svoboda", "Dobrý den, pane Svobodo,"}, {"Eva Svobodová", "Dobrý den, paní Svobodová,"}, {"Ing. Pavel Černý", "Dobrý den, pane Černý,"},
		{"Tomáš Beneš", "Dobrý den, pane Beneši,"}, {"Jiří Němec", "Dobrý den, pane Němče,"}, {"Karel Vaněk", "Dobrý den, pane Vaňku,"}, {"Václav Havel", "Dobrý den, pane Havle,"},
		{"Novák Jan", "Dobrý den, pane Nováku,"}, {"Mgr. Lenka Dvořáková, Ph.D.", "Dobrý den, paní Dvořáková,"}, {"Stavby Novák s.r.o.", "Dobrý den,"}, {"Restaurace Malá", "Dobrý den,"}, {"Hájek", "Dobrý den,"}} {
		if got := salutation(c[0], ""); got != c[1] {
			t.Errorf("%q → %q, want %q", c[0], got, c[1])
		}
	}
}
