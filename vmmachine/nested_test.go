package vmmachine

import (
	"encoding/json"
	"strings"
	"testing"
)

// document is a boot configuration parsed from JSON, as a Starter builds it.
func document(t *testing.T, raw string) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// encoded is a document as JSON, for comparing.
func encoded(t *testing.T, d map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A guest that is not nested is offered neither VMX nor SVM, whatever else its
// Starter's template says, and a nested one is offered both unless something
// hides them, which it refuses. See nested.go for why.
func TestOnlyANestedVMIsOfferedHardwareVirtualisation(t *testing.T) {
	const vmx = `"0bxxxxxxxxxxxxxxxxxxxxxxxxxx0xxxxx"`
	const svm = `"0bxxxxxxxxxxxxxxxxxxxxxxxxxxxxx0xx"`
	for _, c := range []struct {
		name, arch, in string
		nested         bool
		want           string
		refused        string
	}{
		{name: "no template, not nested", arch: "amd64", in: `{}`,
			want: `{"cpu-config":{"cpuid_modifiers":[` +
				`{"flags":0,"leaf":"0x1","modifiers":[{"bitmap":` + vmx + `,"register":"ecx"}],"subleaf":"0x0"},` +
				`{"flags":0,"leaf":"0x80000001","modifiers":[{"bitmap":` + svm + `,"register":"ecx"}],"subleaf":"0x0"}]}}`},
		{name: "no template, nested", arch: "amd64", in: `{}`, nested: true, want: `{}`},
		{name: "a template's other bits are kept", arch: "amd64", nested: false,
			in: `{"cpu-config":{"cpuid_modifiers":[{"leaf":"0x01","subleaf":"0x0","flags":0,` +
				`"modifiers":[{"register":"ecx","bitmap":"0b1_0000"},{"register":"eax","bitmap":"0b1"}]}]}}`,
			want: `{"cpu-config":{"cpuid_modifiers":[{"flags":0,"leaf":"0x01","modifiers":[` +
				`{"bitmap":"0bxxxxxxxxxxxxxxxxxxxxxxxxxx010000","register":"ecx"},{"bitmap":"0b1","register":"eax"}],"subleaf":"0x0"},` +
				`{"flags":0,"leaf":"0x80000001","modifiers":[{"bitmap":` + svm + `,"register":"ecx"}],"subleaf":"0x0"}]}}`},
		{name: "a leaf without the register", arch: "amd64",
			in: `{"cpu-config":{"cpuid_modifiers":[{"leaf":"0x80000001","subleaf":"0x0","flags":0,` +
				`"modifiers":[{"register":"edx","bitmap":"0b0"}]}]}}`,
			want: `{"cpu-config":{"cpuid_modifiers":[{"flags":0,"leaf":"0x80000001","modifiers":[` +
				`{"bitmap":"0b0","register":"edx"},{"bitmap":` + svm + `,"register":"ecx"}],"subleaf":"0x0"},` +
				`{"flags":0,"leaf":"0x1","modifiers":[{"bitmap":` + vmx + `,"register":"ecx"}],"subleaf":"0x0"}]}}`},
		{name: "a template that offers VMX to a VM not nested", arch: "amd64",
			in: `{"cpu-config":{"cpuid_modifiers":[{"leaf":"0x1","subleaf":"0x0","flags":0,` +
				`"modifiers":[{"register":"ecx","bitmap":"0b100000"}]}]}}`,
			refused: "offers VMX to a VM that is not nested"},
		{name: "a template that hides SVM from a nested VM", arch: "amd64", nested: true,
			in: `{"cpu-config":{"cpuid_modifiers":[{"leaf":"0x80000001","subleaf":"0x0","flags":0,` +
				`"modifiers":[{"register":"ecx","bitmap":"0b000"}]}]}}`,
			refused: "hides SVM from a nested VM"},
		{name: "a static template, not nested", arch: "amd64",
			in: `{"machine-config":{"cpu_template":"T2"}}`, want: `{"machine-config":{"cpu_template":"T2"}}`},
		{name: "a static template, nested", arch: "amd64", nested: true,
			in: `{"machine-config":{"cpu_template":"T2"}}`, refused: "takes no static CPU template"},
		{name: "a template by path", arch: "amd64", in: `{"cpu-config":"/etc/template.json"}`,
			refused: "cannot be checked"},
		{name: "aarch64, not nested", arch: "arm64", in: `{}`, want: `{}`},
		{name: "aarch64, nested", arch: "arm64", in: `{}`, nested: true, refused: "only on x86_64"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := document(t, c.in)
			err := configureNested(d, c.nested, c.arch)
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("configureNested = %v, want a refusal saying %q", err, c.refused)
				}
				if got := encoded(t, d); got != encoded(t, document(t, c.in)) {
					t.Fatalf("a refused configuration was changed to %s", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := encoded(t, d); got != c.want {
				t.Fatalf("configureNested made\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}
