package main

import (
	"strings"
	"testing"
)

func TestCleanHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<b>Tučně</b> a <u>podtržené</u>`, `<b>Tučně</b> a <u>podtržené</u>`},
		{`<span style="color: rgb(200, 0, 0); font-size: 18px; position:fixed">x</span>`, `<span style="color:rgb(200, 0, 0);font-size:18px">x</span>`},
		{`<font face="Georgia" color="#c00" size="4">x</font>`, `<font color="#c00" face="Georgia" size="4">x</font>`},
		{`<script>alert(1)</script><p onclick="x()">a</p>`, `<p>a</p>`},
		{`<a href="javascript:alert(1)">a</a><a href="https://x.cz">b</a>`, `<a target="_blank">a</a><a href="https://x.cz" target="_blank">b</a>`},
		{`<img src=x onerror=alert(1)><div style="background:url(http://x)">d</div>`, `<div>d</div>`},
		{`<table><tr><td>a&amp;b</td></tr></table>`, `a&amp;b`},
	}
	for _, c := range cases {
		if got := cleanHTML(c.in); got != c.want {
			t.Errorf("cleanHTML(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRichToText(t *testing.T) {
	got := richToText(`<div>Dobrý den,</div><div><br></div><div><b>děkujeme</b> &amp; zdravíme&nbsp;</div>`)
	if want := "Dobrý den,\n\n\nděkujeme & zdravíme"; strings.ReplaceAll(got, "\n\n\n", "\n\n") != strings.ReplaceAll(want, "\n\n\n", "\n\n") {
		t.Errorf("richToText = %q", got)
	}
}

func TestFillAutoHTML(t *testing.T) {
	v := map[string]string{"osloveni": "Dobrý den, pane Nováku,", "jmeno": "Jan <Novák>"}
	cases := []struct{ in, want string }{
		{`<div><b>{osloveni}</b></div><div>{jmeno}</div>`, `<div><b>Dobrý den, pane Nováku,</b></div><div>Jan &lt;Novák&gt;</div>`},
		{`<div>Dobrý den, {osloveni}</div>`, `<div>Dobrý den, pane Nováku,</div>`},
		{`<div>x</div><div>Dobrý den,&nbsp;{osloveni}</div>`, `<div>x</div><div>Dobrý den,&nbsp;pane Nováku,</div>`},
	}
	for _, c := range cases {
		if got := fillAutoHTML(c.in, v); got != c.want {
			t.Errorf("fillAutoHTML(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	if got := fillAutoHTML(`<div>Dobrý den, {osloveni}</div>`, map[string]string{"osloveni": "Dobrý den,"}); got != `<div>Dobrý den,</div>` {
		t.Errorf("company: %q", got)
	}
}
