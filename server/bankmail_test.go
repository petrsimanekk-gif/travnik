package main

import "testing"

const csobSample = `Vážený kliente,

toto je automaticky generovaný e-mail ze služby ČSOB CEB, neodpovídejte na něj.

Dne 31.10.2026 byla na účtu 378833629 zaúčtována transakce typu: Příchozí úhrada okamžitá.

Název smlouvy: Prima Shade s.r.o.
Číslo smlouvy: 42909701
Majitel smlouvy: Prima Shade s.r.o.
Účet: 378833629, CZK, PRIMA SHADE S.R.O.
Částka: +2,00 CZK
Účet protistrany: 293316219/0300
Název protistrany: SIMANEK PETR
Variabilní symbol: 123

Zůstatek na účtu po zaúčtování transakce: +12,00 CZK.

S přáním krásného dne
Vaše ČSOB`

func TestParseBankMailCSOB(t *testing.T) {
	m := parseBankMail(&parsedMail{Subject: "ČSOB CEB info", Text: csobSample})
	if m == nil {
		t.Fatal("not recognised")
	}
	want := bankMove{Date: "2026-10-31", Account: "378833629", Counter: "293316219/0300", Name: "SIMANEK PETR", VS: "123", Amount: 2, Bank: "ČSOB"}
	if *m != want {
		t.Fatalf("got %+v\nwant %+v", *m, want)
	}
	// a bigger outgoing one, with a non-breaking space in the thousands
	out := parseBankMail(&parsedMail{Text: "ČSOB\nDne 1.11.2026 byla na účtu 378833629 zaúčtována transakce typu: Odchozí úhrada.\nČástka: -12 100,50 CZK\nVariabilní symbol: 0000777001\n"})
	if out == nil || out.Amount != -12100.5 || out.VS != "777001" || out.Date != "2026-11-01" {
		t.Fatalf("outgoing: %+v", out)
	}
	if parseBankMail(&parsedMail{Text: "Dobrý den, posílám nabídku. Částka: 120 000 Kč"}) != nil {
		t.Fatal("an ordinary e-mail must not be taken for a payment")
	}
	if parseBankMail(&parsedMail{Subject: "Re: platba", Text: csobSample}) != nil {
		t.Fatal("a reply must not count")
	}
}

func TestBaseAcct(t *testing.T) {
	for in, want := range map[string]string{"378833629/0300": "378833629", "0000378833629": "378833629", "19-123457/0100": "19-123457", "000019-0000123457": "19-123457"} {
		if got := baseAcct(in); got != want {
			t.Errorf("baseAcct(%q) = %q, want %q", in, got, want)
		}
	}
}
