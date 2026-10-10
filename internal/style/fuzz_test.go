package style

import "testing"

// FuzzParseTheme checks that no theme file makes the parser panic.
func FuzzParseTheme(f *testing.F) {
	f.Add([]byte(`{"styles": {"accent": "color: red"}}`))
	f.Add([]byte(`{"styles": {":root": "--a: var(--a)", "muted": "color: var(--a)"}}`))
	f.Add([]byte(`{"styles": {"heading": {"color": "rgb(1,2,3", "font-weight": "900"}}}`))
	f.Add([]byte(`{"styles": {"x": "color: ansi(999); background: #12"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		ParseTheme(data, nil)
	})
}
