package vmmachine

import (
	"fmt"
	"strings"
)

// Nested virtualisation is offered to a guest only when its VM asks for it,
// which is experimental (see Launch.Nested). A guest that may run VMs of its
// own gets pages of its RAM written by KVM behind the host page tables, so its
// RAM is kept in place and it can never be captured, forked or migrated: see
// vmmemory/fixed.go and plans/nested-kvm-2026-09-27.md. Every other guest must
// not be able to start a VM at all, or it would get those writes without those
// limits. KVM lets a guest turn VMX or SVM on only when its CPUID offers it, so
// the CPUID is where both are decided.

// virtualisationBit is one CPUID feature bit that offers a guest hardware
// virtualisation.
type virtualisationBit struct {
	name     string
	leaf     string
	register string
	bit      int
}

// virtualisationBits are Intel's VMX and AMD's SVM.
var virtualisationBits = []virtualisationBit{
	{name: "VMX", leaf: "0x1", register: "ecx", bit: 5},
	{name: "SVM", leaf: "0x80000001", register: "ecx", bit: 2},
}

// checkNested reports whether configureNested would refuse the document,
// without changing it, so that Configure refuses before it changes anything.
func checkNested(document map[string]any, nested bool, arch string) error {
	return configureNested(clone(document), nested, arch)
}

// clone copies a configuration document deeply enough that configureNested
// may change the copy: its maps and lists.
func clone(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, v := range value {
		out[key] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return clone(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneValue(e)
		}
		return out
	}
	return v
}

// configureNested makes a boot's configuration offer hardware virtualisation
// exactly when the VM is nested. Off x86_64 there is nothing to withhold: the
// VMM offers no virtualisation there unless asked, and a nested VM is refused
// before it gets here. A static CPU template hides both bits, so a nested VM
// refuses one. A custom template, which the configuration's cpu-config holds,
// has the two bits cleared in it, or refused where it sets them, and a nested
// VM refuses a template that clears them. A template given by path cannot be
// checked, so it is refused.
func configureNested(document map[string]any, nested bool, arch string) error {
	if arch != "amd64" {
		if nested {
			return fmt.Errorf("vmmachine: a nested VM runs only on x86_64, not %s", arch)
		}
		return nil
	}
	if machine, ok := document["machine-config"].(map[string]any); ok {
		if template, set := machine["cpu_template"]; set && template != "" && template != "None" {
			if nested {
				return fmt.Errorf("vmmachine: a nested VM takes no static CPU template, and %v hides VMX and SVM", template)
			}
			return nil
		}
	}
	config := map[string]any{}
	if existing, found := document["cpu-config"]; found {
		object, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("vmmachine: a CPU template given as %T cannot be checked for VMX and SVM", existing)
		}
		config = object
	}
	var entries []any
	if existing, found := config["cpuid_modifiers"]; found {
		list, ok := existing.([]any)
		if !ok {
			return fmt.Errorf("vmmachine: the CPU template's cpuid_modifiers is %T, not a list", existing)
		}
		entries = list
	}
	// Every bit is checked before anything is changed, so a configuration
	// this refuses is left as it was.
	var edits []func()
	for _, want := range virtualisationBits {
		leaf, modifier, err := findModifier(entries, want)
		if err != nil {
			return err
		}
		if modifier == nil {
			if nested {
				continue
			}
			register := map[string]any{"register": want.register, "bitmap": hidden(want.bit)}
			if leaf == nil {
				edits = append(edits, func() {
					entries = append(entries, map[string]any{"leaf": want.leaf, "subleaf": "0x0", "flags": 0,
						"modifiers": []any{register}})
				})
			} else {
				edits = append(edits, func() {
					registers, _ := leaf["modifiers"].([]any)
					leaf["modifiers"] = append(registers, register)
				})
			}
			continue
		}
		bits, err := expand(modifier["bitmap"])
		if err != nil {
			return err
		}
		at := len(bits) - 1 - want.bit
		switch {
		case nested && bits[at] == '0':
			return fmt.Errorf("vmmachine: the CPU template hides %s from a nested VM", want.name)
		case !nested && bits[at] == '1':
			return fmt.Errorf("vmmachine: the CPU template offers %s to a VM that is not nested", want.name)
		case !nested:
			bits[at] = '0'
			edits = append(edits, func() { modifier["bitmap"] = "0b" + string(bits) })
		}
	}
	if len(edits) == 0 {
		return nil
	}
	for _, edit := range edits {
		edit()
	}
	config["cpuid_modifiers"] = entries
	document["cpu-config"] = config
	return nil
}

// findModifier finds one register of one leaf in a template's CPUID modifiers:
// the leaf's entry, nil where the template has none, and the register's
// modifier, nil where the template leaves that register alone.
func findModifier(entries []any, want virtualisationBit) (leaf, modifier map[string]any, err error) {
	for _, entry := range entries {
		object, ok := entry.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("vmmachine: a CPU template's CPUID modifier is %T, not an object", entry)
		}
		if !sameHex(object["leaf"], want.leaf) || !sameHex(object["subleaf"], "0x0") {
			continue
		}
		registers, _ := object["modifiers"].([]any)
		for _, r := range registers {
			if m, ok := r.(map[string]any); ok && m["register"] == want.register {
				return object, m, nil
			}
		}
		return object, nil, nil
	}
	return nil, nil, nil
}

// sameHex compares a template's hexadecimal string with one of ours.
func sameHex(value any, want string) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	return strings.TrimLeft(strings.TrimPrefix(strings.ToLower(s), "0x"), "0") ==
		strings.TrimLeft(strings.TrimPrefix(want, "0x"), "0")
}

// expand is a template's 32-bit bitmap written out in full: its separators
// removed and the bits it leaves out, which are the high ones, left alone.
func expand(value any) ([]byte, error) {
	s, ok := value.(string)
	if !ok || !strings.HasPrefix(s, "0b") {
		return nil, fmt.Errorf("vmmachine: a CPU template's bitmap %v is not 0b...", value)
	}
	bits := strings.ReplaceAll(s[2:], "_", "")
	if len(bits) > 32 || strings.Trim(bits, "01x") != "" {
		return nil, fmt.Errorf("vmmachine: a CPU template's bitmap %q is not 32 bits of 0, 1 and x", s)
	}
	return []byte(strings.Repeat("x", 32-len(bits)) + bits), nil
}

// hidden is the bitmap that clears one bit and leaves the rest alone.
func hidden(bit int) string {
	bits := []byte(strings.Repeat("x", 32))
	bits[31-bit] = '0'
	return "0b" + string(bits)
}
