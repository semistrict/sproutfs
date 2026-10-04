package real

import "strings"

// EBSDevicePrefix is how the kernel's /dev/disk/by-id names an EBS volume
// attached to a Nitro instance: an NVMe device whose model is Amazon Elastic
// Block Store and whose serial is the volume's ID without its dash.
const EBSDevicePrefix = "nvme-Amazon_Elastic_Block_Store_"

// EBSDeviceName is the suffix of an EBS volume's path under
// /dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_: vol-0123abcd is
// vol0123abcd. A volume is named by its ID, which is also its CSI volume
// handle; anything else names no device.
func EBSDeviceName(volume string) string {
	id, found := strings.CutPrefix(volume, "vol-")
	if !found || id == "" {
		return ""
	}
	for _, r := range id {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') {
			return ""
		}
	}
	return "vol" + id
}
