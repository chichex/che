package wizard

import "testing"

func TestSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"basico", "Triage Checkout Flow", "triage-checkout-flow"},
		{"acentos", "Análisis de Café", "analisis-de-cafe"},
		{"signos colapsan", "  hola!! mundo??  ", "hola-mundo"},
		{"signos en bordes se trimean", "---hola---", "hola"},
		{"unicode no-latin se descarta", "テスト pipeline", "pipeline"},
		{"vacio", "", ""},
		{"solo simbolos", "!!@#$", ""},
		{"solo emoji", "🚀🚀", ""},
		{"alfanumerico mix", "Foo42_Bar", "foo42-bar"},
		{"numeros pegados", "v1.2.3", "v1-2-3"},
		{"colapsa runs", "a___b___c", "a-b-c"},
		{"upper unicode", "ÑoNo", "nono"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Slug(tc.in)
			if got != tc.want {
				t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
