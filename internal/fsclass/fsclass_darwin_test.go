//go:build darwin

package fsclass

import "testing"

func TestClassifyDarwinName(t *testing.T) {
	cases := []struct {
		name string
		want Class
	}{
		{"apfs", SupportedLocal},
		{"hfs", SupportedLocal},
		{"exfat", SupportedLocal},
		{"nfs", KnownUnsupported},
		{"smbfs", KnownUnsupported},
		{"webdav", KnownUnsupported},
		{"osxfuse", Unknown},
		{"", Unknown},
	}
	for _, c := range cases {
		if got := classifyDarwinName(c.name).Class; got != c.want {
			t.Fatalf("classifyDarwinName(%q) = %s, want %s", c.name, got, c.want)
		}
	}
}
