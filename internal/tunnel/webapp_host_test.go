package tunnel

import "testing"

func TestBuildAndParseWebAppPublicHost(t *testing.T) {
	host, err := BuildWebAppPublicHost("abcdefghijk23", " Example.Test. ")
	if err != nil {
		t.Fatal(err)
	}
	if host != "abcdefghijk23-wa.example.test" {
		t.Fatalf("host = %q", host)
	}
	for _, candidate := range []string{
		"abcdefghijk23-wa.example.test",
		"ABCDEFGHIJK23-WA.EXAMPLE.TEST:443",
		"abcdefghijk23-wa.example.test.",
	} {
		label, ok := ParseWebAppPublicHost(candidate, "example.test")
		if !ok || label != "abcdefghijk23" {
			t.Fatalf("ParseWebAppPublicHost(%q) = %q, %t", candidate, label, ok)
		}
	}
}

func TestParseWebAppPublicHostRejectsOtherShapes(t *testing.T) {
	for _, host := range []string{
		"abcdefghijk23.wa.example.test",
		"nested.abcdefghijk23-wa.example.test",
		"abcdefghijk23.example.test",
		"short-wa.example.test",
		"abcdefghijk10-wa.example.test",
		"example.test",
	} {
		if label, ok := ParseWebAppPublicHost(host, "example.test"); ok {
			t.Fatalf("ParseWebAppPublicHost(%q) accepted label %q", host, label)
		}
	}
}

func TestBuildWebAppPublicHostRejectsInvalidInput(t *testing.T) {
	for _, test := range []struct {
		label string
		base  string
	}{
		{label: "short", base: "example.test"},
		{label: "abcdefghijk10", base: "example.test"},
		{label: "abcdefghijk23", base: ""},
	} {
		if _, err := BuildWebAppPublicHost(test.label, test.base); err == nil {
			t.Fatalf("BuildWebAppPublicHost(%q, %q) succeeded", test.label, test.base)
		}
	}
}
