package main

import (
	"bytes"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Formatted e-mail text (templates, the compose editor) arrives as HTML. cleanHTML keeps
// only plain formatting — no scripts, forms, images or event handlers — so a template can
// not carry anything else into the e-mails or into the copy kept in Odeslané.

var richTags = map[atom.Atom]bool{
	atom.B: true, atom.Strong: true, atom.I: true, atom.Em: true, atom.U: true, atom.S: true, atom.Strike: true, atom.Del: true,
	atom.Span: true, atom.Font: true, atom.Div: true, atom.P: true, atom.Br: true, atom.Ul: true, atom.Ol: true, atom.Li: true,
	atom.A: true, atom.Blockquote: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.Sub: true, atom.Sup: true, atom.Hr: true,
}

// dropped together with everything inside
var richDrop = map[atom.Atom]bool{atom.Script: true, atom.Style: true, atom.Head: true, atom.Title: true, atom.Iframe: true, atom.Object: true,
	atom.Embed: true, atom.Template: true, atom.Svg: true, atom.Math: true, atom.Noscript: true, atom.Textarea: true, atom.Select: true}

var (
	cssPropRe = regexp.MustCompile(`^(color|background-color|font-family|font-size|font-weight|font-style|text-decoration|text-decoration-line|text-align|margin-left|padding-left|line-height)$`)
	cssValRe  = regexp.MustCompile(`^[#%,.'"\w\s()-]{1,120}$`)
	cssBadRe  = regexp.MustCompile(`(?i)url\s*\(|expression|javascript|@import|\\`)
	hrefRe    = regexp.MustCompile(`(?i)^(https?://|mailto:|tel:)`)
)

func cleanStyle(s string) string {
	var out []string
	for _, d := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(d, ":")
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if ok && cssPropRe.MatchString(k) && cssValRe.MatchString(v) && !cssBadRe.MatchString(v) {
			out = append(out, k+":"+v)
		}
	}
	return strings.Join(out, ";")
}

func cleanHTML(s string) string {
	ctx := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(s), ctx)
	if err != nil {
		return textToHTML(htmlToText(s))
	}
	var b bytes.Buffer
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(html.EscapeString(n.Data))
		case html.ElementNode:
			if richDrop[n.DataAtom] {
				return
			}
			keep := richTags[n.DataAtom]
			if keep {
				b.WriteString("<" + n.Data)
				for _, a := range n.Attr {
					k, v := strings.ToLower(a.Key), a.Val
					switch {
					case k == "style":
						v = cleanStyle(v)
					case k == "href" && n.DataAtom == atom.A:
						if !hrefRe.MatchString(strings.TrimSpace(v)) {
							continue
						}
					case n.DataAtom == atom.Font && (k == "color" || k == "face" || k == "size"):
						if !cssValRe.MatchString(v) || cssBadRe.MatchString(v) {
							continue
						}
					case k == "align" && cssValRe.MatchString(v):
					default:
						continue
					}
					if v == "" {
						continue
					}
					b.WriteString(" " + k + `="` + html.EscapeString(v) + `"`)
				}
				if n.DataAtom == atom.A {
					b.WriteString(` target="_blank"`)
				}
				b.WriteString(">")
				if n.DataAtom == atom.Br || n.DataAtom == atom.Hr {
					return
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			if keep {
				b.WriteString("</" + n.Data + ">")
			}
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return b.String()
}

// richToText is the plain-text twin of a formatted body (line breaks kept).
var (
	richBreakRe = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|h\d|blockquote)>`)
	richBlankRe = regexp.MustCompile(`\n{3,}`)
)

func richToText(s string) string {
	s = richBreakRe.ReplaceAllString(s, "\n")
	s = html.UnescapeString(tagRe.ReplaceAllString(s, ""))
	s = strings.ReplaceAll(s, " ", " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.TrimSpace(richBlankRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// richWrap gives the formatted body the same default font as plain e-mails.
func richWrap(s string) string {
	return `<div style="font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:1.45">` + cleanHTML(s) + `</div>`
}
