package style

import (
	"strings"
	"testing"
)

func styled(t *testing.T) {
	t.Helper()
	t.Cleanup(SetEnabled(true))
	t.Cleanup(SetTrueColor(false))
}

func mustParse(t *testing.T, json string, base *Theme) *Theme {
	t.Helper()
	theme, problems, err := ParseTheme([]byte(json), base)
	if err != nil || len(problems) > 0 {
		t.Fatalf("ParseTheme: err=%v problems=%v", err, problems)
	}
	return theme
}

func TestDefaultThemeStylesEveryRole(t *testing.T) {
	styled(t)
	for _, role := range Roles {
		if Default().Style(role).IsZero() {
			t.Errorf("the default theme leaves %s unstyled", role)
		}
	}
	// The defaults are exact grayscale-ramp entries, so they keep their 256-color codes.
	want := map[Role]string{
		Accent:      "\x1b[38;5;255m",
		Muted:       "\x1b[38;5;245m",
		Subtle:      "\x1b[38;5;240m",
		Heading:     "\x1b[1;38;5;255m",
		Quote:       "\x1b[3;38;5;245m",
		UserMarker:  "\x1b[38;5;255;48;5;236m",
		UserMessage: "\x1b[1;48;5;236m",
	}
	for role, open := range want {
		if got := Default().Style(role).Open(); got != open {
			t.Errorf("%s opens with %q, want %q", role, got, open)
		}
	}
}

func TestThemeAcceptsDeclarationStringsAndObjects(t *testing.T) {
	styled(t)
	theme := mustParse(t, `{"styles": {
		"accent": "color: #ff0000; font-weight: bold; /* loud */",
		".muted": {"color": "rgb(0, 0, 255)", "font-weight": 300},
		"error": "{ text-decoration: underline line-through !important }"
	}}`, nil)
	if got := theme.Style(Accent).Open(); got != "\x1b[1;38;5;196m" {
		t.Errorf("accent = %q", got)
	}
	if got := theme.Style(Muted).Open(); got != "\x1b[2;38;5;21m" {
		t.Errorf("muted = %q", got)
	}
	if s := theme.Style(Error); !s.Underline || !s.Strike {
		t.Errorf("error = %+v", s)
	}
	if !theme.Style(Heading).IsZero() {
		t.Errorf("a role the file does not mention should be unstyled without a base")
	}
}

func TestThemeCascadesOverBase(t *testing.T) {
	styled(t)
	theme := mustParse(t, `{"styles": {
		":root": "--white: tomato",
		"heading": "font-weight: normal",
		"error": "color: red"
	}}`, Default())

	// Overriding a variable restyles every role that uses it.
	if got, want := theme.Style(Accent).Open(), "\x1b[38;5;203m"; got != want {
		t.Errorf("accent = %q, want %q", got, want)
	}
	// font-weight: normal turns the default's bold off but keeps its color.
	if s := theme.Style(Heading); s.Bold || !s.Fg.IsSet() {
		t.Errorf("heading = %+v", s)
	}
	if got := theme.Style(Error).Open(); got != "\x1b[38;5;196m" {
		t.Errorf("error = %q", got)
	}
	// Untouched roles keep the defaults.
	if theme.Style(Muted) != Default().Style(Muted) {
		t.Errorf("muted changed")
	}
	// The base theme itself is not modified.
	if Default().Style(Heading).Bold == false {
		t.Errorf("layering a theme modified the default theme")
	}
}

func TestThemeReportsProblemsAndKeepsTheRest(t *testing.T) {
	styled(t)
	theme, problems, err := ParseTheme([]byte(`{
		"$schema": "ignored",
		"colors": {},
		"styles": {
			"accent": "color: blu; font-weight: bold",
			"muted": "colour: red",
			"nope": "color: red",
			"subtle": "color: var(--missing)",
			"strong": "color: var(--missing, #000)",
			"code": "this is not css",
			"quote": ["red"]
		}
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{
		`unknown top-level key "colors"`,
		`accent: color: unknown color "blu"`,
		"muted: colour: unsupported property",
		`unknown style "nope"`,
		"subtle: color: undefined variable --missing",
		`code: "this is not css" is not a declaration`,
		"quote: a style must be",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems do not mention %q:\n%s", want, joined)
		}
	}
	if !theme.Style(Accent).Bold {
		t.Errorf("the usable declaration next to a bad one was dropped")
	}
	if got := theme.Style(Strong).Open(); got != "\x1b[38;5;16m" {
		t.Errorf("var() fallback: strong = %q", got)
	}

	if _, _, err := ParseTheme([]byte(`{"styles": `), nil); err == nil {
		t.Errorf("invalid JSON was accepted")
	}
}

func TestVarCycleIsReported(t *testing.T) {
	_, problems, err := ParseTheme([]byte(`{"styles": {":root": "--a: var(--b); --b: var(--a)", "accent": "color: var(--a)"}}`), nil)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "nest too deep") {
		t.Fatalf("err=%v problems=%v", err, problems)
	}
}

func TestParseColor(t *testing.T) {
	styled(t)
	cases := map[string]string{
		"#fff":                "38;5;231",
		"#FFFFFF80":           "38;5;231",
		"#808080":             "38;5;244",
		"rgb(95 135 175)":     "38;5;67",
		"rgba(100%,0%,0%,.5)": "38;5;196",
		"rgb(0 255 0 / 50%)":  "38;5;46",
		"RebeccaPurple":       "38;5;60",
		"ansi(3)":             "38;5;3",
	}
	for in, want := range cases {
		c, err := parseColor(in)
		if err != nil || c.sgr(38) != want {
			t.Errorf("parseColor(%q) = %q, %v; want %q", in, c.sgr(38), err, want)
		}
	}
	for _, unset := range []string{"none", "inherit", "currentColor", "transparent"} {
		if c, err := parseColor(unset); err != nil || c.IsSet() {
			t.Errorf("parseColor(%q) = %+v, %v; want unset", unset, c, err)
		}
	}
	for _, bad := range []string{"#ggg", "#12345", "rgb(1,2)", "ansi(256)", "hsl(0 0% 0%)", "blurple"} {
		if _, err := parseColor(bad); err == nil {
			t.Errorf("parseColor(%q) succeeded", bad)
		}
	}

	t.Cleanup(SetTrueColor(true))
	c, _ := parseColor("#123456")
	if got := c.sgr(48); got != "48;2;18;52;86" {
		t.Errorf("truecolor background = %q", got)
	}
}

func TestFontWeights(t *testing.T) {
	for value, want := range map[string][2]bool{
		"bold": {true, false}, "700": {true, false}, "normal": {false, false},
		"400": {false, false}, "lighter": {false, true}, "200": {false, true},
	} {
		var s Style
		if err := applyProperty(&s, "font-weight", value); err != nil || s.Bold != want[0] || s.Faint != want[1] {
			t.Errorf("font-weight %s: bold=%v faint=%v err=%v", value, s.Bold, s.Faint, err)
		}
	}
	var s Style
	if err := applyProperty(&s, "font-weight", "heavy"); err == nil {
		t.Errorf("font-weight heavy accepted")
	}
}

func TestRenderAndMerge(t *testing.T) {
	styled(t)
	bold := Style{Bold: true}
	if got := bold.Render("a\n\nb"); got != "\x1b[1ma"+Reset+"\n\n\x1b[1mb"+Reset {
		t.Errorf("multi-line render = %q", got)
	}
	if got := (Style{}).Render("plain"); got != "plain" {
		t.Errorf("empty style render = %q", got)
	}

	quote := Default().Style(Quote)
	code := Default().Style(InlineCode)
	merged := quote.Merge(code)
	if !merged.Italic || merged.Fg != code.Fg {
		t.Errorf("inline code inside a quote should keep italic and take the code color: %+v", merged)
	}

	restore := SetEnabled(false)
	defer restore()
	if got := Accent.Render("x"); got != "x" {
		t.Errorf("styling off still styled: %q", got)
	}
	if got := Glyphs().Bullet; got != "*" {
		t.Errorf("styling off uses Unicode glyphs: %q", got)
	}
}

func TestActiveAfter(t *testing.T) {
	if got := ActiveAfter("", "a\x1b[1mb\x1b[3mc"); got != "\x1b[1m\x1b[3m" {
		t.Errorf("got %q", got)
	}
	if got := ActiveAfter("\x1b[1m", "x"+Reset+"y\x1b[2A"); got != "" {
		t.Errorf("reset and cursor moves: got %q", got)
	}
}

func TestWrapBreaksAtWidthAndCarriesOpenStyles(t *testing.T) {
	if got := Wrap("one two three", 8, "- ", "  "); got != "- one\n  two\n  three" {
		t.Fatalf("plain wrap: %q", got)
	}
	bold := "\x1b[1m"
	got := Wrap(bold+"aaa bbb"+Reset, 5, "", "> ")
	want := bold + "aaa" + Reset + "\n> " + bold + "bbb" + Reset
	if got != want {
		t.Fatalf("styled wrap:\n got %q\nwant %q", got, want)
	}
	if got := Wrap("averyveryverylongword", 4, "", ""); got != "averyveryverylongword" {
		t.Fatalf("long words should overflow, got %q", got)
	}
}
