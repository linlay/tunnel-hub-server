package devicelink

import (
	"errors"
	"testing"
	"time"
)

func TestMergeDesktopPresenceKeepsOfflineDesktopAndDoesNotConnectMobile(t *testing.T) {
	fixtures := loadContractFixtures(t)
	list, err := ParseAccountDeviceListV1(fixtures.AccountDeviceLists.Valid[0])
	if err != nil {
		t.Fatal(err)
	}
	connectedAt := time.Date(2030, 1, 1, 0, 0, 30, 0, time.UTC)
	merged := MergeDesktopPresence(list.Items, map[string]DesktopPresence{
		"22222222-2222-4222-8222-222222222222": {ConnectedAt: connectedAt},
	})
	if len(merged) != 3 || merged[0].Online != nil {
		t.Fatalf("mobile presence must stay unset: %#v", merged)
	}
	if merged[1].Online == nil || !*merged[1].Online || merged[1].ConnectedAt == nil || !merged[1].ConnectedAt.Equal(connectedAt) {
		t.Fatalf("online Desktop was not projected: %#v", merged[1])
	}
	if merged[2].Online == nil || *merged[2].Online {
		t.Fatalf("offline Desktop disappeared or became online: %#v", merged[2])
	}
}

func TestResolveActiveDesktopFailsClosed(t *testing.T) {
	fixtures := loadContractFixtures(t)
	list, err := ParseAccountDeviceListV1(fixtures.AccountDeviceLists.Valid[0])
	if err != nil {
		t.Fatal(err)
	}
	merged := MergeDesktopPresence(list.Items, map[string]DesktopPresence{
		"22222222-2222-4222-8222-222222222222": {},
	})
	if _, err := ResolveActiveDesktop(merged, "22222222-2222-4222-8222-222222222222"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		deviceID string
		code     string
	}{
		{"33333333-3333-4333-8333-333333333333", ErrorTargetOffline},
		{"11111111-1111-4111-8111-111111111111", ErrorProtocolIncompatible},
		{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ErrorProtocolIncompatible},
	} {
		_, err := ResolveActiveDesktop(merged, test.deviceID)
		var routeError *RouteSelectionError
		if !errors.As(err, &routeError) || routeError.Code != test.code {
			t.Fatalf("%s: got %v, want %s", test.deviceID, err, test.code)
		}
	}
}
