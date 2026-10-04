package real

import "testing"

// An EBS volume's device is named by its ID without the dash; anything that
// is not a volume ID names no device, so Open refuses it.
func TestEBSDeviceName(t *testing.T) {
	for volume, want := range map[string]string{
		"vol-0123456789abcdef0": "vol0123456789abcdef0",
		"vol-1a2b":              "vol1a2b",
		"vol-":                  "",
		"vol-0123/../x":         "",
		"0123456789abcdef0":     "",
		"shard-0":               "",
		"vol-ABCDEF":            "",
	} {
		if got := EBSDeviceName(volume); got != want {
			t.Errorf("EBSDeviceName(%q) = %q, want %q", volume, got, want)
		}
	}
}
