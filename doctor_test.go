package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMacUSBUsesAdvertisedReport(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "system_profiler")
	profiler := `#!/bin/sh
if [ "$1" = "-listDataTypes" ]; then
    printf '%s\n' "Available Datatypes:" "$REPORT_LIST"
elif [ "$1" = "$EXPECTED_REPORT" ] && [ "$2" = "-json" ]; then
    printf '%s\n' "$REPORT_JSON"
else
    exit 2
fi
`
	if err := os.WriteFile(path, []byte(profiler), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	const host, legacy = "SPUSBHostDataType", "SPUSBDataType"
	for _, tc := range []struct {
		name, list, report, data string
		want                     []string
		fail                     bool
	}{
		{"Tahoe", host, host, `{"SPUSBHostDataType":[{"_items":[{"USBDeviceKeyVendorID":"0x0416","USBDeviceKeyProductID":"0x5409"}]}]}`, []string{"0416:5409"}, false},
		{"legacy", legacy, legacy, `{"SPUSBDataType":[{"_items":[{"vendor_id":"0x0416 (Nuvoton)","product_id":"0x5408"},{"vendor_id":"apple_vendor_id","product_id":"0x1234"}]}]}`, []string{"0416:5408"}, false},
		{"empty bus", host, host, `{"SPUSBHostDataType":[]}`, nil, false},
		{"missing list", host, host, `{"SPUSBDataType":[]}`, nil, true},
		{"missing product ID", host, host, `{"SPUSBHostDataType":[{"USBDeviceKeyVendorID":"0x0416"}]}`, nil, true},
		{"unavailable", "SPHardwareDataType", "", "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("REPORT_LIST", tc.list)
			t.Setenv("EXPECTED_REPORT", tc.report)
			t.Setenv("REPORT_JSON", tc.data)
			ids, err := macUSB(context.Background())
			if (err != nil) != tc.fail || !slices.Equal(ids, tc.want) {
				t.Fatalf("ids = %v, error = %v", ids, err)
			}
		})
	}
}
